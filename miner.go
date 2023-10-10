// Copyright (c) 2016-2023 The Decred developers.

package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/rpcclient/v8"
	"github.com/decred/gominer/stratum"
	"github.com/decred/gominer/util"
	"github.com/decred/gominer/work"
)

type Miner struct {
	// The following variables must only be used atomically.
	validShares   uint64
	staleShares   uint64
	invalidShares uint64

	started          uint32
	devices          []*Device
	workDone         chan []byte
	needsWorkRefresh chan struct{}
	wg               sync.WaitGroup
	pool             *stratum.Stratum

	rpc *rpcclient.Client
}

func newStratum() (*Miner, error) {
	s, err := stratum.StratumConn(cfg.Pool, cfg.PoolUser, cfg.PoolPassword, cfg.Proxy, cfg.ProxyUser, cfg.ProxyPass, version(), chainParams)
	if err != nil {
		return nil, err
	}
	m := &Miner{
		pool:             s,
		workDone:         make(chan []byte, 10),
		needsWorkRefresh: make(chan struct{}),
	}
	return m, nil
}

func newMiner(ctx context.Context) (*Miner, error) {
	var rpc *rpcclient.Client
	ntfnHandlers := rpcclient.NotificationHandlers{
		OnBlockConnected: func(blockHeader []byte, transactions [][]byte) {
			minrLog.Infof("Block connected: %x (%d transactions)", blockHeader, len(transactions))
		},
		OnBlockDisconnected: func(blockHeader []byte) {
			minrLog.Infof("Block disconnected: %x", blockHeader)
		},
		OnWork: func(data, target []byte, reason string) {
			minrLog.Infof("Work received: %x %x %s", data, target, reason)

			bigTarget := new(big.Int)
			bigTarget.SetBytes(util.Reverse(target))

			var workData [192]byte
			copy(workData[:], data)
			ts := workData[128+4*work.TimestampWord : 132+4*work.TimestampWord]
			givenTs := binary.LittleEndian.Uint32(ts)
			w := work.NewWork(workData, bigTarget, givenTs, uint32(time.Now().Unix()), true)

			w.Target = bigTarget

			// Solo
			for _, d := range m.devices {
				d.SetWork(ctx, w)
			}
		},
	}
	// Connect to local dcrd RPC server using websockets.
	certs, err := os.ReadFile(cfg.RPCCert)
	if err != nil {
		return nil, fmt.Errorf("failed to read rpc certificate %v: %w",
			cfg.RPCCert, err)
	}

	connCfg := &rpcclient.ConnConfig{
		Host:         cfg.RPCServer,
		Endpoint:     "ws",
		User:         cfg.RPCUser,
		Pass:         cfg.RPCPassword,
		Certificates: certs,
		Proxy:        cfg.Proxy,
		ProxyUser:    cfg.ProxyUser,
		ProxyPass:    cfg.ProxyPass,
	}
	rpc, err = rpcclient.New(connCfg, &ntfnHandlers)
	if err != nil {
		return nil, err
	}

	m := &Miner{
		rpc:      rpc,
		workDone: make(chan []byte, 10),
	}

	return m, nil
}

func NewMiner(ctx context.Context) (*Miner, error) {
	var m *Miner
	var err error
	if cfg.Pool == "" {
		m, err = newMiner(ctx)
	} else {
		m, err = newStratum()
	}
	if err != nil {
		return nil, err
	}

	m, deviceListEnabledCount, err := newMinerDevs(m)
	if err != nil {
		return nil, err
	}
	if deviceListEnabledCount == 0 {
		return nil, fmt.Errorf("no devices started")
	}

	m.started = uint32(time.Now().Unix())

	return m, nil
}

func (m *Miner) workSubmitThread(ctx context.Context) {
	defer m.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case data := <-m.workDone:
			// Only use that is we are not using a pool.
			if m.pool == nil {
				// Solo
				accepted, err := m.rpc.GetWorkSubmit(ctx, hex.EncodeToString(data))
				if err != nil {
					atomic.AddUint64(&m.invalidShares, 1)
					minrLog.Errorf("failed to submit work: %w", err)
					continue
				} else if !accepted {
					atomic.AddUint64(&m.invalidShares, 1)
					minrLog.Error("work not accepted")
					continue
				}
				atomic.AddUint64(&m.validShares, 1)
				minrLog.Infof("Submitted work successfully: block hash %v",
					chainhash.Hash(blake256.Sum256(data[:180])))
			} else {
				submitted, err := GetPoolWorkSubmit(data, m.pool)
				if err != nil {
					switch {
					case errors.Is(err, stratum.ErrStratumStaleWork):
						atomic.AddUint64(&m.staleShares, 1)
						minrLog.Debugf("Share submitted to pool was stale")

					default:
						atomic.AddUint64(&m.invalidShares, 1)
						minrLog.Errorf("Error submitting work to pool: %v", err)
					}
				} else {
					if submitted {
						minrLog.Debugf("Submitted work to pool successfully: %v",
							submitted)
					}

					select {
					case m.needsWorkRefresh <- struct{}{}:
					case <-ctx.Done():
					}
				}
			}
		}
	}
}

func (m *Miner) workRefreshThread(ctx context.Context) {
	defer m.wg.Done()

	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()

	for {
		// Stratum only code.
		m.pool.Lock()
		if m.pool.PoolWork.NewWork {
			work, err := GetPoolWork(m.pool)
			m.pool.Unlock()
			if err != nil {
				minrLog.Errorf("Error in getpoolwork: %v", err)
			} else {
				for _, d := range m.devices {
					d.SetWork(ctx, work)
				}
			}
		} else {
			m.pool.Unlock()
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.needsWorkRefresh:
		}
	}
}

func (m *Miner) printStatsThread(ctx context.Context) {
	defer m.wg.Done()

	t := time.NewTicker(time.Second * 5)
	defer t.Stop()

	for {
		if !cfg.Benchmark {
			valid, rejected, stale, total, utility := m.Status()

			if cfg.Pool != "" {
				minrLog.Infof("Global stats: Accepted: %v, Rejected: %v, Stale: %v, Total: %v",
					valid,
					rejected,
					stale,
					total,
				)
				secondsElapsed := uint32(time.Now().Unix()) - m.started
				if (secondsElapsed / 60) > 0 {
					minrLog.Infof("Global utility (accepted shares/min): %v", utility)
				}
			} else {
				minrLog.Infof("Global stats: Accepted: %v, Rejected: %v, Total: %v",
					valid,
					rejected,
					total,
				)
			}
		}

		for _, d := range m.devices {
			d.UpdateFanTemp()
			d.PrintStats()
			if d.fanControlActive {
				d.fanControl()
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.needsWorkRefresh:
		}
	}
}

func (m *Miner) Run(ctx context.Context) {
	m.wg.Add(len(m.devices))

	for _, d := range m.devices {
		device := d
		go func() {
			device.Run(ctx)
			device.Release()
			m.wg.Done()
		}()
	}

	m.wg.Add(1)
	go m.workSubmitThread(ctx)

	if cfg.Benchmark {
		minrLog.Warn("Running in BENCHMARK mode! No real mining taking place!")
		work := &work.Work{}
		for _, d := range m.devices {
			d.SetWork(ctx, work)
		}
	} else if m.pool != nil {
		m.wg.Add(1)
		go m.workRefreshThread(ctx)
	}

	m.wg.Add(1)
	go m.printStatsThread(ctx)

	m.wg.Wait()
}

func (m *Miner) Status() (uint64, uint64, uint64, uint64, float64) {
	if cfg.Pool != "" {
		valid := atomic.LoadUint64(&m.pool.ValidShares)
		rejected := atomic.LoadUint64(&m.pool.InvalidShares)
		stale := atomic.LoadUint64(&m.staleShares)
		total := valid + rejected + stale

		secondsElapsed := uint32(time.Now().Unix()) - m.started
		utility := float64(valid) / (float64(secondsElapsed) / float64(60))

		return valid, rejected, stale, total, utility
	}

	valid := atomic.LoadUint64(&m.validShares)
	rejected := atomic.LoadUint64(&m.invalidShares)
	total := valid + rejected

	return valid, rejected, 0, total, 0
}
