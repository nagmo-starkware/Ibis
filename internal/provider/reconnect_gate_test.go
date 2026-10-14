package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A mass drop of the shared socket resubscribes every address-sub; the
// post-subscribe RPCs they issue must stay within the resubscribe bound, not
// scale with N.
func TestReconnectWavePeakConcurrency(t *testing.T) {
	const n = 500
	w := &waveNode{latency: 20 * time.Millisecond}
	sub, ss := startReconnectWave(t, n, w, SubscriberConfig{})
	took := dropAndRecover(t, n, w, sub, ss, 10*time.Second)

	peak := w.post.peak.Load()
	t.Logf("dropped %d streams: recovered in %v, peak post-subscribe RPCs %d", n, took, peak)
	if peak > maxConcurrentResubscribe {
		t.Fatalf("peak post-subscribe RPCs = %d, want <= %d", peak, maxConcurrentResubscribe)
	}
	assertSlotsFree(t, sub)
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
			assertSlotsFree(t, sub)
		})
	}
}

// Waiting for P to be accepted holds no slot: with a bound of 1 both streams
// still subscribe, and a session dropped there recovers.
func TestResubscribeGateNotHeldWhileWaitingAccepted(t *testing.T) {
	chain := newGapChain(100, 121) // P not accepted yet
	node := newGapNode()
	sub, _ := startGapSite(t, gateSite(), chain, node)

	first := node.next(t)
	node.next(t) // only reachable if the first stream's wait holds no slot
	first.errs <- fmt.Errorf("socket dropped")
	chain.tip.Store(121)
	waitLive(t, sub)
	assertSlotsFree(t, sub)
}

// A dial that hangs (subscribe reply never comes) holds no slot: with a bound
// of 1 the other stream still goes live.
func TestResubscribeGateNotHeldByHungDial(t *testing.T) {
	chain := newGapChain(121, 121)
	node := newGapNode()
	node.hangKeys.Store(true)
	sub, _ := startGapSite(t, gateSite(), chain, node)

	deadline := time.Now().Add(5 * time.Second)
	for live, _, _ := sub.TransportStatus(); live < 1; live, _, _ = sub.TransportStatus() {
		if time.Now().After(deadline) {
			t.Fatal("address-sub starved behind a hung dial")
		}
		time.Sleep(2 * time.Millisecond)
	}
	assertSlotsFree(t, sub)
}

// A stream canceled while blocked acquiring the slot returns promptly and
// closes its session.
func TestResubscribeGateCancelWhileAcquiring(t *testing.T) {
	chain := newGapChain(121, 121)
	node := newGapNode()
	node.hold = make(chan struct{})
	site := gateSite()
	sub, _ := startGapSite(t, site, chain, node)

	sub.resubSem <- struct{}{} // the test holds the only slot
	close(node.hold)
	// Both subscribed with the slot taken: each can only be blocked acquiring it.
	waitFor(t, "both subscribes", func() bool { return node.dials.Load() == 2 })

	sub.RemoveContract(newTestFelt(site.fromAddr).String())
	waitFor(t, "canceled stream's session closed", func() bool { return node.closed.Load() == 1 })
	if n := chain.preReads.Load(); n != 0 {
		t.Fatalf("pre_confirmed reads = %d while the slot was held", n)
	}
	<-sub.resubSem
	waitLive(t, sub) // the keys-sub
	assertSlotsFree(t, sub)
}

// A stream canceled while holding the slot (mid-backfill) releases it.
func TestResubscribeGateCancelWhileHolding(t *testing.T) {
	chain := newGapChain(121, 121)
	chain.gate = make(chan struct{}) // backfills block holding their slot
	node := newGapNode()
	site := gateSite()
	site.cfg.MaxConcurrentResubscribes = 2
	sub, _ := startGapSite(t, site, chain, node)

	waitFor(t, "both backfills holding a slot", func() bool {
		return chain.fetches.Load() == 2 && len(sub.resubSem) == 2
	})
	sub.RemoveContract(newTestFelt(site.fromAddr).String())
	waitFor(t, "slot released", func() bool { return len(sub.resubSem) == 1 && node.closed.Load() == 1 })
	close(chain.gate)
	waitLive(t, sub) // the keys-sub
	assertSlotsFree(t, sub)
}

// waitFor polls cond for up to 5s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// assertSlotsFree checks every acquired slot was released (none leaked, none
// released twice: a double release would block or panic instead).
func assertSlotsFree(t *testing.T, sub *EventSubscriber) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(sub.resubSem) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d resubscribe slots still held after settling", len(sub.resubSem))
		}
		time.Sleep(time.Millisecond)
	}
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
	for _, d := range []time.Duration{0, -time.Second} {
		if got := jitter(d); got != d {
			t.Fatalf("jitter(%v) = %v, want it unchanged", d, got)
		}
	}
}

// MaxConcurrentResubscribes sizes the gate; unset or 0 means the default.
func TestResubscribeGateSize(t *testing.T) {
	p := &StarknetProvider{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, tc := range []struct {
		cfg  *SubscriberConfig
		want int
	}{
		{nil, maxConcurrentResubscribe},
		{&SubscriberConfig{}, maxConcurrentResubscribe},
		{&SubscriberConfig{MaxConcurrentResubscribes: 7}, 7},
	} {
		if got := cap(p.NewSubscriber(nil, nil, tc.cfg).resubSem); got != tc.want {
			t.Errorf("cfg %+v: gate size = %d, want %d", tc.cfg, got, tc.want)
		}
	}
}

// newTipSub is a subscriber over a node whose blockNumber is *tip, with the
// shared tip poller running (every interval) when shared.
func newTipSub(t *testing.T, shared bool, interval time.Duration) (*EventSubscriber, *atomic.Uint64, *atomic.Int64) {
	t.Helper()
	var tip atomic.Uint64
	var reads atomic.Int64
	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(json.RawMessage) (interface{}, error) { reads.Add(1); return tip.Load(), nil },
	})
	t.Cleanup(server.Close)
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tip.Store(100)
	sub := p.NewSubscriber(nil, nil, &SubscriberConfig{SharedTipPoller: shared, TipPollInterval: interval})
	if shared {
		p.StartTipPoller(ctx, interval)
	}
	return sub, &tip, &reads
}

// waitAccepted reads the shared cached tip when the poller runs (no RPC per
// waiting stream), and the node directly when it does not.
func TestWaitAcceptedUsesCachedTip(t *testing.T) {
	for _, shared := range []bool{true, false} {
		t.Run(fmt.Sprint("shared=", shared), func(t *testing.T) {
			sub, _, reads := newTipSub(t, shared, time.Hour)
			before := reads.Load()
			for range 10 {
				if err := sub.waitAccepted(context.Background(), 100); err != nil {
					t.Fatalf("waitAccepted: %v", err)
				}
			}
			want := int64(10)
			if shared {
				want = 0
			}
			if got := reads.Load() - before; got != want {
				t.Fatalf("blockNumber RPCs = %d, want %d", got, want)
			}
		})
	}
}

// A cached tip lagging P holds waitAccepted until it reaches P.
func TestWaitAcceptedWaitsForCachedTip(t *testing.T) {
	sub, tip, _ := newTipSub(t, true, 5*time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- sub.waitAccepted(context.Background(), 101) }()

	select {
	case err := <-done:
		t.Fatalf("returned (%v) while the tip was below P", err)
	case <-time.After(50 * time.Millisecond):
	}
	tip.Store(101)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitAccepted: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not return once the tip reached P")
	}
}

// A cached tip stuck below P ends the wait with the acceptWaitTimeout error.
func TestWaitAcceptedTimesOutOnStuckTip(t *testing.T) {
	old := acceptWaitTimeout
	acceptWaitTimeout = 100 * time.Millisecond
	t.Cleanup(func() { acceptWaitTimeout = old })

	sub, _, _ := newTipSub(t, true, 5*time.Millisecond)
	err := sub.waitAccepted(context.Background(), 101)
	if err == nil || !strings.Contains(err.Error(), "not accepted within") {
		t.Fatalf("waitAccepted = %v, want the accept-wait timeout", err)
	}
}
