package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/client"
	"github.com/NethermindEth/starknet.go/rpc"
)

// inflight tracks how many post-reconnect RPCs (pre_confirmed reads and
// getEvents) are being served at once, and the peak.
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

// startReconnectWave starts n address-sub streams at tip against a node whose
// post-reconnect RPCs each take a few ms, so overlapping ones are counted.
func startReconnectWave(t *testing.T, n int, cfg SubscriberConfig) (*EventSubscriber, *sharedSocket, *inflight) {
	t.Helper()
	const tip = 1000
	f := &inflight{}
	slow := func(res interface{}) (interface{}, error) {
		f.enter()
		defer f.leave()
		time.Sleep(20 * time.Millisecond)
		return res, nil
	}
	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(json.RawMessage) (interface{}, error) { return uint64(tip), nil },
		"starknet_getBlockWithTxHashes": func(params json.RawMessage) (interface{}, error) {
			if !strings.Contains(string(params), "pre_confirmed") {
				return map[string]interface{}{"block_number": 1, "timestamp": 1, "block_hash": "0x1"}, nil
			}
			return slow(map[string]interface{}{"block_number": tip, "timestamp": 1})
		},
		"starknet_getEvents": func(json.RawMessage) (interface{}, error) {
			return slow(map[string]interface{}{"events": []interface{}{}})
		},
	})
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		server.Close()
		t.Fatalf("New() error: %v", err)
	}
	contracts := make([]ContractSubscription, n)
	for i := range contracts {
		contracts[i] = ContractSubscription{Address: newTestFelt(0x10000 + uint64(i)), StartBlock: tip}
	}
	cfg.KeysFirehose = true
	cfg.OptionSelectors = []*felt.Felt{newTestFelt(0x999)}
	cfg.TipPollInterval = 5 * time.Millisecond
	cfg.CatchupPollInterval = 5 * time.Millisecond
	sub := p.NewSubscriber(contracts, make(chan RawEvent, 64), &cfg)
	ss := newSharedSocket()
	sub.dialWSS = ss.dialer()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = sub.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		p.Close()
		server.Close()
	})
	return sub, ss, f
}

// A mass drop of the shared socket resubscribes every address-sub; the
// post-reconnect RPCs they issue must stay within the resubscribe bound, not
// scale with N.
func TestReconnectWavePeakConcurrency(t *testing.T) {
	const n = 500
	sub, ss, f := startReconnectWave(t, n, SubscriberConfig{})
	waitLive(t, sub)

	f.peak.Store(0)
	dials := ss.dials.Load()
	ss.drop()
	deadline := time.Now().Add(10 * time.Second)
	for ss.dials.Load() < dials+n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d streams resubscribed", ss.dials.Load()-dials, n)
		}
		time.Sleep(time.Millisecond)
	}
	waitLive(t, sub)

	peak := f.peak.Load()
	t.Logf("peak in-flight post-reconnect RPCs after dropping %d streams: %d", n, peak)
	if peak > maxConcurrentResubscribe {
		t.Fatalf("peak in-flight post-reconnect RPCs = %d, want <= %d", peak, maxConcurrentResubscribe)
	}
}

// gateSite is a keys-sub plus one address-sub sharing a resubscribe bound of
// 1: a slot leaked on any path leaves the other stream never live.
func gateSite() gapSite {
	site := gapSites()[1] // keys-address-sub
	site.cfg.MaxConcurrentResubscribes = 1
	return site
}

// Each failure path between dial and backfill gives the slot back.
func TestResubscribeGateReleasedOnFailure(t *testing.T) {
	cases := []struct {
		name string
		fail func(t *testing.T, chain *gapChain, node *gapNode)
	}{
		{"dial_error", func(t *testing.T, _ *gapChain, node *gapNode) {
			node.failDials.Store(2)
		}},
		{"pre_confirmed_error", func(t *testing.T, chain *gapChain, _ *gapNode) {
			chain.preFails.Store(2 * preConfirmedAttempts)
		}},
		{"backfill_error", func(t *testing.T, chain *gapChain, node *gapNode) {
			chain.failFetch.Store(true)
			go func() {
				for node.closed.Load() < 2 { // both streams' backfills failed
					time.Sleep(2 * time.Millisecond)
				}
				chain.failFetch.Store(false)
			}()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			chain := newGapChain(121, 121)
			node := newGapNode()
			tc.fail(t, chain, node)
			sub, _ := startGapSite(t, gateSite(), chain, node)
			waitLive(t, sub)
		})
	}
}

// A session that ends while waiting for P to be accepted (before its backfill
// runs) releases the slot to the other stream.
func TestResubscribeGateReleasedWhenSessionEndsBeforeBackfill(t *testing.T) {
	chain := newGapChain(100, 121) // P never accepted: stuck in waitAccepted
	node := newGapNode()
	sub, _ := startGapSite(t, gateSite(), chain, node)

	first := node.next(t)
	first.errs <- fmt.Errorf("socket dropped")
	second := node.next(t) // only reachable once the slot is free
	if second.in.FromAddress.String() == first.in.FromAddress.String() {
		t.Fatalf("slot went back to the dropped stream (%v), not the waiting one", first.in.FromAddress)
	}
	chain.tip.Store(121)
	waitLive(t, sub)
}

// A stream canceled while holding the slot (its contract removed) releases it.
func TestResubscribeGateReleasedOnCancel(t *testing.T) {
	chain := newGapChain(100, 121) // P never accepted: stuck in waitAccepted
	node := newGapNode()
	site := gateSite()
	sub, _ := startGapSite(t, site, chain, node)

	// The keys-sub may win the slot first; drop it until the address-sub holds it.
	for {
		s := node.next(t)
		if s.in.FromAddress != nil {
			break
		}
		s.errs <- fmt.Errorf("socket dropped")
	}
	sub.RemoveContract(newTestFelt(site.fromAddr).String())
	if s := node.next(t); s.in.FromAddress != nil {
		t.Fatalf("got a dial for %v, want the keys-sub", s.in.FromAddress)
	}
	chain.tip.Store(121)
	waitLive(t, sub)
}

// Reconnect sleeps never undercut the backoff and are spread across streams.
func TestJitterSpreadsReconnects(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 100 {
		d := jitter(time.Second)
		if d < time.Second || d > 1500*time.Millisecond {
			t.Fatalf("jitter(1s) = %v, want in [1s, 1.5s]", d)
		}
		seen[d] = true
	}
	if len(seen) < 50 {
		t.Fatalf("jitter(1s) gave only %d distinct sleeps in 100 calls; streams would reconnect in lockstep", len(seen))
	}
}
