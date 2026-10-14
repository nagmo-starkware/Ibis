package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/client"
	"github.com/NethermindEth/starknet.go/rpc"
)

// inflight tracks how many RPCs of one kind are being served at once, and the
// peak.
type inflight struct {
	cur, peak atomic.Int64
}

func (f *inflight) enter() {
	n := f.cur.Add(1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			return
		}
	}
}

func (f *inflight) leave() { f.cur.Add(-1) }

// sharedSocket fakes the address-sub shared WSS: drop ends every open session
// at once, as a provider closing the socket does.
type sharedSocket struct {
	mu    sync.Mutex
	gone  chan struct{}
	dials atomic.Int64
}

func newSharedSocket() *sharedSocket { return &sharedSocket{gone: make(chan struct{})} }

func (ss *sharedSocket) drop() {
	ss.mu.Lock()
	close(ss.gone)
	ss.gone = make(chan struct{})
	ss.mu.Unlock()
}

func (ss *sharedSocket) dialer() wssDialer {
	return func(ctx context.Context, _ string, _ *rpc.EventSubscriptionInput) (*wssSession, error) {
		ss.dials.Add(1)
		ss.mu.Lock()
		gone := ss.gone
		ss.mu.Unlock()
		errs := make(chan error, 1)
		go func() {
			select {
			case <-gone:
				errs <- fmt.Errorf("websocket: close 1006 (abnormal closure): unexpected EOF")
			case <-ctx.Done():
			}
		}()
		return &wssSession{
			events: make(chan *rpc.EmittedEventWithFinalityStatus),
			errs:   errs,
			reorgs: make(chan *client.ReorgEvent),
			close:  func() {},
		}, nil
	}
}

// waveNode is the fake node: every RPC takes latency; with blockTime > 0 the
// accepted tip advances one block per blockTime and pre_confirmed is tip+1.
type waveNode struct {
	latency, blockTime time.Duration
	tip                atomic.Uint64
	post               inflight // pre_confirmed reads + getEvents
	tipReads           inflight // blockNumber
}

func (w *waveNode) handlers() map[string]func(json.RawMessage) (interface{}, error) {
	slow := func(f *inflight, res interface{}, err error) (interface{}, error) {
		f.enter()
		defer f.leave()
		time.Sleep(w.latency)
		return res, err
	}
	return map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(json.RawMessage) (interface{}, error) {
			return slow(&w.tipReads, w.tip.Load(), nil)
		},
		"starknet_getBlockWithTxHashes": func(params json.RawMessage) (interface{}, error) {
			if !strings.Contains(string(params), "pre_confirmed") {
				return map[string]interface{}{"block_number": 1, "timestamp": 1, "block_hash": "0x1"}, nil
			}
			pre := w.tip.Load()
			if w.blockTime > 0 {
				pre++
			}
			return slow(&w.post, map[string]interface{}{"block_number": pre, "timestamp": 1}, nil)
		},
		"starknet_getEvents": func(params json.RawMessage) (interface{}, error) {
			var q []struct {
				To struct {
					N uint64 `json:"block_number"`
				} `json:"to_block"`
			}
			if err := json.Unmarshal(params, &q); err != nil || len(q) != 1 {
				return nil, fmt.Errorf("bad params %s", params)
			}
			if q[0].To.N > w.tip.Load() {
				return slow(&w.post, nil, fmt.Errorf("block %d not found", q[0].To.N))
			}
			return slow(&w.post, map[string]interface{}{"events": []interface{}{}}, nil)
		},
	}
}

// startReconnectWave starts n address-sub streams at tip on one shared socket,
// with the shared tip poller running as in prod.
func startReconnectWave(t *testing.T, n int, w *waveNode, cfg SubscriberConfig) (*EventSubscriber, *sharedSocket) {
	t.Helper()
	w.tip.Store(1000)
	server := mockRPCServer(t, w.handlers())
	p, err := New(context.Background(), server.URL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		server.Close()
		t.Fatalf("New() error: %v", err)
	}
	contracts := make([]ContractSubscription, n)
	for i := range contracts {
		contracts[i] = ContractSubscription{Address: newTestFelt(0x10000 + uint64(i)), StartBlock: 1000}
	}
	cfg.KeysFirehose = true
	cfg.OptionSelectors = []*felt.Felt{newTestFelt(0x999)}
	cfg.TipPollInterval = 20 * time.Millisecond
	cfg.CatchupPollInterval = 5 * time.Millisecond
	sub := p.NewSubscriber(contracts, make(chan RawEvent, 64), &cfg)
	ss := newSharedSocket()
	sub.dialWSS = ss.dialer()

	ctx, cancel := context.WithCancel(context.Background())
	if w.blockTime > 0 {
		go func() {
			tick := time.NewTicker(w.blockTime)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					w.tip.Add(1)
				}
			}
		}()
	}
	p.StartTipPoller(ctx, cfg.TipPollInterval)
	done := make(chan struct{})
	go func() { defer close(done); _ = sub.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
		p.Close()
		server.Close()
	})
	return sub, ss
}

// dropAndRecover drops the shared socket once every stream is live and returns
// how long until every stream is live again. Peaks are reset at the drop.
func dropAndRecover(t *testing.T, n int, w *waveNode, sub *EventSubscriber, ss *sharedSocket, limit time.Duration) time.Duration {
	t.Helper()
	waitAllLive(t, sub, limit)
	w.post.peak.Store(0)
	w.tipReads.peak.Store(0)
	dials := ss.dials.Load()
	start := time.Now()
	ss.drop()
	for !isLive(sub) || ss.dials.Load() < dials+int64(n) {
		if time.Since(start) > limit {
			live, total, _ := sub.TransportStatus()
			t.Fatalf("not recovered after %v: live=%d total=%d", limit, live, total)
		}
		time.Sleep(time.Millisecond)
	}
	return time.Since(start)
}

func waitAllLive(t *testing.T, sub *EventSubscriber, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !isLive(sub) {
		if time.Now().After(deadline) {
			live, total, _ := sub.TransportStatus()
			t.Fatalf("never went live: live=%d total=%d", live, total)
		}
		time.Sleep(time.Millisecond)
	}
}

// Opt-in measurement (IBIS_RECONNECT_BENCH=1): recovery time and peak RPC
// concurrency after a shared-socket drop, scaled to 20ms RPCs and 1s blocks.
func TestReconnectRecoveryBench(t *testing.T) {
	if os.Getenv("IBIS_RECONNECT_BENCH") == "" {
		t.Skip("set IBIS_RECONNECT_BENCH=1 to run")
	}
	for _, n := range []int{500, 3750} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			w := &waveNode{latency: 20 * time.Millisecond, blockTime: time.Second}
			sub, ss := startReconnectWave(t, n, w, SubscriberConfig{})
			took := dropAndRecover(t, n, w, sub, ss, 10*time.Minute)
			t.Logf("N=%d recovery=%v peak_post_rpcs=%d peak_blockNumber=%d",
				n, took.Round(10*time.Millisecond), w.post.peak.Load(), w.tipReads.peak.Load())
		})
	}
}
