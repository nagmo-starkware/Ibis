package provider

import (
	"fmt"
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
}

// A stream canceled while it holds the slot, or while blocked acquiring it,
// returns promptly, closes its session, and leaves the slot usable.
func TestResubscribeGateCancel(t *testing.T) {
	chain := newGapChain(121, 121)
	chain.gate = make(chan struct{}) // the slot holder's backfill blocks
	node := newGapNode()
	site := gateSite()
	sub, _ := startGapSite(t, site, chain, node)

	select {
	case <-chain.entered: // one stream holds the slot
	case <-time.After(5 * time.Second):
		t.Fatal("no backfill started")
	}
	for deadline := time.Now().Add(5 * time.Second); node.dials.Load() < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("second stream never subscribed")
		}
	}
	time.Sleep(50 * time.Millisecond) // the other reaches acquire

	sub.RemoveContract(newTestFelt(site.fromAddr).String())
	deadline := time.Now().Add(2 * time.Second)
	for node.closed.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("canceled stream did not close its session")
		}
		time.Sleep(time.Millisecond)
	}
	close(chain.gate)
	waitLive(t, sub) // the keys-sub, through the same slot
	if _, total, _ := sub.TransportStatus(); total != 1 {
		t.Fatalf("total streams = %d, want 1 after removing the address-sub", total)
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
}
