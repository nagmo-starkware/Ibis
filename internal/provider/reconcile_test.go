package provider

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/rpc"
)

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// reconSites are the gap sites whose live stream is reconciled.
func reconSites(names ...string) []gapSite {
	var out []gapSite
	for _, s := range gapSites() {
		for _, n := range names {
			if s.name == n {
				out = append(out, s)
			}
		}
	}
	return out
}

// reconLabel is the stream label the site's reconcile logs under.
func reconLabel(site gapSite) string {
	switch site.name {
	case "keys-sub":
		return "keys-sub"
	}
	return site.name
}

// reconLive is a live event whose identity matches what gapChain serves over
// HTTP for (addr, block): tx hash = block, keys [1], data [2].
func reconLive(addr, block uint64) *rpc.EmittedEventWithFinalityStatus {
	e := fhEvent(addr, block)
	e.TransactionHash = newTestFelt(block)
	return e
}

// startReconSite is startGapSite with reconcile enabled at interval (0 = off)
// and the subscriber's log captured.
func startReconSite(t *testing.T, site gapSite, chain *gapChain, node *gapNode, interval time.Duration) (*EventSubscriber, chan RawEvent, *syncBuf) {
	t.Helper()
	server := mockRPCServer(t, chain.handlers())
	logs := &syncBuf{}
	p, err := New(context.Background(), server.URL, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		server.Close()
		t.Fatalf("New() error: %v", err)
	}
	cfg := site.cfg
	cfg.TipPollInterval = 5 * time.Millisecond
	cfg.CatchupPollInterval = 5 * time.Millisecond
	cfg.ReconcileInterval = interval
	cfg.ReconcileLag = 2
	events := make(chan RawEvent, 64)
	sub := p.NewSubscriber(site.contracts, events, &cfg)
	sub.dialWSS = node.dialer()

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
	return sub, events, logs
}

// collect drains events for d and returns their blocks (by catchup flag).
func collect(ch <-chan RawEvent, d time.Duration) (live, catchup []uint64) {
	deadline := time.After(d)
	for {
		select {
		case e := <-ch:
			if e.IsCatchup {
				catchup = append(catchup, e.BlockNumber)
			} else {
				live = append(live, e.BlockNumber)
			}
		case <-deadline:
			return
		}
	}
}

// The live node drops the middle event (delivers its neighbours): reconcile
// recovers exactly that one, flagged IsCatchup, logs it at WARN, and neither
// re-delivers what the live path delivered nor what the backfill delivered.
func TestReconcileRecoversDroppedLiveEvent(t *testing.T) {
	for _, site := range reconSites("keys-sub") {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(121, 121)
			chain.add(105, site.addr) // gap event: delivered by the post-subscribe backfill
			for _, b := range []uint64{130, 131, 132} {
				chain.add(b, site.addr)
			}
			node := newGapNode()
			sub, events, logs := startReconSite(t, site, chain, node, 20*time.Millisecond)

			sess := node.nextFor(t, site)
			waitLive(t, sub)
			waitEvent(t, events, "backfilled @105", func(e RawEvent) bool { return e.BlockNumber == 105 })

			sess.events <- reconLive(site.addr, 130)
			sess.events <- reconLive(site.addr, 132) // 131 is dropped by the node
			waitEvent(t, events, "live @130", func(e RawEvent) bool { return e.BlockNumber == 130 && !e.IsCatchup })
			waitEvent(t, events, "live @132", func(e RawEvent) bool { return e.BlockNumber == 132 && !e.IsCatchup })

			chain.tip.Store(140) // blocks <= 138 are now reconciled
			got := waitEvent(t, events, "recovered @131", func(e RawEvent) bool { return e.BlockNumber == 131 })
			if !got.IsCatchup {
				t.Error("recovered event must be flagged IsCatchup")
			}
			live, catchup := collect(events, 300*time.Millisecond) // several more ticks
			if len(live) != 0 || len(catchup) != 0 {
				t.Errorf("duplicate delivery after recovery: live=%v catchup=%v", live, catchup)
			}
			out := logs.String()
			for _, want := range []string{"live stream missed event; recovered by reconcile", "block=131", "stream=" + reconLabel(site)} {
				if !strings.Contains(out, want) {
					t.Errorf("log missing %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "block=130 ") || strings.Contains(out, "block=132 ") || strings.Contains(out, "block=105 ") {
				t.Errorf("a delivered event was reported as missed:\n%s", out)
			}
		})
	}
}

// A failed reconcile does not advance lastReconciled: the next successful tick
// still covers the range and recovers the event.
func TestReconcileFailureDoesNotAdvance(t *testing.T) {
	for _, site := range reconSites("keys-sub") {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(121, 121)
			chain.add(131, site.addr)
			node := newGapNode()
			sub, events, logs := startReconSite(t, site, chain, node, 20*time.Millisecond)

			node.nextFor(t, site)
			waitLive(t, sub)
			base := chain.fetches.Load()
			chain.failFetch.Store(true)
			chain.tip.Store(140)

			deadline := time.After(5 * time.Second)
			for chain.fetches.Load() < base+3 { // several failed ticks
				select {
				case <-deadline:
					t.Fatal("reconcile did not retry")
				case <-time.After(5 * time.Millisecond):
				}
			}
			if _, catchup := collect(events, 50*time.Millisecond); len(catchup) != 0 {
				t.Fatalf("event delivered while every fetch failed: %v", catchup)
			}
			if !strings.Contains(logs.String(), "reconcile failed; will retry") {
				t.Error("failure not logged")
			}

			chain.failFetch.Store(false)
			waitEvent(t, events, "recovered @131 after failures", func(e RawEvent) bool { return e.BlockNumber == 131 && e.IsCatchup })
		})
	}
}

// A session that drops before reconcile caught up resumes at/below the first
// unreconciled block, though live events advanced the cursors past it.
func TestReconcileDropResumesBelowUnreconciled(t *testing.T) {
	for _, site := range reconSites("keys-sub") {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(121, 121)
			node := newGapNode()
			// Interval too long for a tick to run: lastReconciled stays at P=121.
			sub, events, _ := startReconSite(t, site, chain, node, time.Hour)

			sess := node.nextFor(t, site)
			waitLive(t, sub)
			sess.events <- reconLive(site.addr, 130)
			waitEvent(t, events, "live @130", func(e RawEvent) bool { return e.BlockNumber == 130 })
			sess.errs <- fmt.Errorf("socket dropped")

			second := node.nextFor(t, site)
			n := second.in.SubBlockID.Number
			if n == nil {
				t.Fatal("second subscribe has no block_id")
			}
			if *n > 122 {
				t.Errorf("second subscribe block_id %d, want <= 122 (P+1, the first unreconciled block)", *n)
			}
		})
	}
}

// Interval 0 turns reconcile off: no fetch after the backfill and the dropped
// event stays dropped.
func TestReconcileDisabled(t *testing.T) {
	for _, site := range reconSites("keys-sub") {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(121, 121)
			chain.add(131, site.addr)
			node := newGapNode()
			sub, events, _ := startReconSite(t, site, chain, node, 0)

			node.nextFor(t, site)
			waitLive(t, sub)
			base := chain.fetches.Load()
			chain.tip.Store(140)
			time.Sleep(200 * time.Millisecond)
			if n := chain.fetches.Load(); n != base {
				t.Errorf("fetches grew %d -> %d with reconcile off", base, n)
			}
			if live, catchup := collect(events, 10*time.Millisecond); len(live)+len(catchup) != 0 {
				t.Errorf("events delivered with reconcile off: %v %v", live, catchup)
			}
		})
	}
}

// --- reconciler unit tests ----------------------------------------------------

func mkEv(block, tx, data uint64) RawEvent {
	return RawEvent{
		BlockNumber: block, TransactionHash: newTestFelt(tx), ContractAddress: newTestFelt(0xA),
		Keys: []*felt.Felt{newTestFelt(1)}, Data: []*felt.Felt{newTestFelt(data)},
	}
}

// The seen set never holds more than the window, however many blocks pass
// without a reconcile.
func TestReconcilerSeenSetBounded(t *testing.T) {
	r := newReconciler()
	for b := uint64(1); b <= 10*reconcileWindow; b++ {
		e := mkEv(b, b, 2)
		r.observe(b, rawID(e))
	}
	r.mu.Lock()
	n := len(r.seen)
	r.mu.Unlock()
	if n > int(reconcileWindow)+1 {
		t.Errorf("seen set holds %d blocks, want <= %d", n, reconcileWindow+1)
	}
}

// advance forgets reconciled blocks; start forgets everything at/below P.
func TestReconcilerPrunesOnAdvance(t *testing.T) {
	r := newReconciler()
	for b := uint64(1); b <= 100; b++ {
		r.observe(b, rawID(mkEv(b, b, 2)))
	}
	r.start(50)
	r.advance(80)
	r.mu.Lock()
	defer r.mu.Unlock()
	for b := range r.seen {
		if b <= 80 {
			t.Errorf("block %d still held after advance(80)", b)
		}
	}
	if len(r.seen) != 20 {
		t.Errorf("held %d blocks, want 20 (81..100)", len(r.seen))
	}
}

// Identical events in one tx are told apart by count: live delivered 2 of 3.
func TestReconcilerMissingCountsDuplicates(t *testing.T) {
	r := newReconciler()
	e := mkEv(10, 7, 2)
	r.observe(10, rawID(e))
	r.observe(10, rawID(e))
	other := mkEv(10, 7, 3) // same tx, different data: fully missed
	got := r.missing([]RawEvent{e, e, e, other})
	if len(got) != 2 || rawID(got[0]) != rawID(e) || rawID(got[1]) != rawID(other) {
		t.Fatalf("missing() = %d events, want the 3rd identical one and the other-data one", len(got))
	}
	if again := r.missing([]RawEvent{e, e, e, other}); len(again) != 0 {
		t.Errorf("second pass re-reported %d events", len(again))
	}
}
