package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/client"
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

// reconAdd puts an event of the site's scope on the chain.
func reconAdd(chain *gapChain, site gapSite, block uint64) { chain.add(block, site.addr) }

// reconLive is a live event whose identity matches what gapChain serves over
// HTTP for (addr, block): tx hash = block, keys [1], data [2].
func reconLive(addr, block uint64) *rpc.EmittedEventWithFinalityStatus {
	e := fhEvent(addr, block)
	e.TransactionHash = newTestFelt(block)
	return e
}

// startReconSite is startGapSite with reconcile enabled at interval (0 = off)
// and the subscriber's log captured.
func startReconSite(t *testing.T, site gapSite, chain *gapChain, node *gapNode, interval time.Duration, mods ...func(*SubscriberConfig)) (*EventSubscriber, chan RawEvent, *syncBuf) {
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
	lag := uint64(2)
	cfg.ReconcileLag = &lag
	for _, m := range mods {
		m(&cfg)
	}
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
	r.advance(80, 0)
	r.mu.Lock()
	defer r.mu.Unlock()
	for b := range r.seen {
		if b <= 80 {
			t.Errorf("block %d still held after.advance(80, 0)", b)
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
	got, _ := r.missing([]RawEvent{e, e, e, other}, 0)
	if len(got) != 2 || rawID(got[0]) != rawID(e) || rawID(got[1]) != rawID(other) {
		t.Fatalf("missing() = %d events, want the 3rd identical one and the other-data one", len(got))
	}
	for _, g := range got {
		r.mark(g, 0)
	}
	if again, _ := r.missing([]RawEvent{e, e, e, other}, 0); len(again) != 0 {
		t.Errorf("second pass re-reported %d events", len(again))
	}
}

// --- identity, epoch, send failure --------------------------------------------

// The identity of an event as the WSS notification delivers it equals the one
// of the same event read back over HTTP, whatever the hex padding, and for
// multi-key events. nil and zero felts are the same identity.
func TestEventIdentityParity(t *testing.T) {
	const ws = `{"from_address":"0x00abc","keys":["0x0001","0x02","0x0"],"data":["0x0","0x00ff","0x0123456789"],
		"block_number":5,"block_hash":"0x01","transaction_hash":"0x000123","finality_status":"ACCEPTED_ON_L2"}`
	var live rpc.EmittedEventWithFinalityStatus
	if err := json.Unmarshal([]byte(ws), &live); err != nil {
		t.Fatalf("unmarshal WSS event: %v", err)
	}

	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_getEvents": func(json.RawMessage) (interface{}, error) {
			return map[string]interface{}{"events": []map[string]interface{}{{
				"from_address": "0xabc", "keys": []string{"0x1", "0x2", "0x0000"}, "data": []string{"0x00", "0xff", "0x123456789"},
				"block_number": 5, "block_hash": "0x1", "transaction_hash": "0x123",
			}}}, nil
		},
	})
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	got, err := p.GetEvents(context.Background(), GetEventsOptions{FromBlock: 5, ToBlock: 5})
	if err != nil || len(got) != 1 {
		t.Fatalf("GetEvents: %v %v", got, err)
	}
	if emittedID(&live) != rawID(got[0]) {
		t.Error("WSS and HTTP identities of the same event differ")
	}

	// Different keys / data / tx must differ.
	other := got[0]
	other.Keys = other.Keys[:2]
	if rawID(other) == rawID(got[0]) {
		t.Error("a dropped key did not change the identity")
	}
	other = got[0]
	other.Data = []*felt.Felt{newTestFelt(0), newTestFelt(0xff), newTestFelt(0x123456780)}
	if rawID(other) == rawID(got[0]) {
		t.Error("changed data did not change the identity")
	}

	// nil == zero.
	if idOf(nil, newTestFelt(1), []*felt.Felt{nil}, nil) != idOf(newTestFelt(0), newTestFelt(1), []*felt.Felt{newTestFelt(0)}, nil) {
		t.Error("nil and zero felts have different identities")
	}
}

func newBareSub(t *testing.T, events chan RawEvent, onBlockRead ...func()) *EventSubscriber {
	t.Helper()
	h := newGapChain(1, 1).handlers()
	if len(onBlockRead) > 0 {
		inner := h["starknet_getBlockWithTxHashes"]
		h["starknet_getBlockWithTxHashes"] = func(params json.RawMessage) (interface{}, error) {
			onBlockRead[0]() // runs inside resolveTimestamps: after missing(), before the send
			return inner(params)
		}
	}
	server := mockRPCServer(t, h)
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p.NewSubscriber(nil, events, &SubscriberConfig{ReconcileInterval: time.Second})
}

var testLogger = slog.New(slog.NewTextHandler(&syncBuf{}, nil))

// A tick that was in flight when a reorg rolled the reconciler back neither
// advances lastReconciled nor marks its (now stale) identities seen, whichever
// step the reorg lands in (fetch, between diff and send, after the send).
func TestReconcileRollbackInvalidatesInFlightTick(t *testing.T) {
	for _, stage := range []string{"fetch", "before-send", "after-send"} {
		for _, tc := range []struct{ rollbackAt, wantLast uint64 }{{12, 10}, {8, 7}} {
			t.Run(fmt.Sprintf("%s/rollback@%d", stage, tc.rollbackAt), func(t *testing.T) {
				events := make(chan RawEvent, 8)
				rec := newReconciler()
				rec.start(10)
				fire := func() { rec.rollback(tc.rollbackAt) }
				var sub *EventSubscriber
				if stage == "before-send" {
					sub = newBareSub(t, events, fire)
				} else {
					sub = newBareSub(t, events)
				}
				sc := reconcileScope{
					label: "x",
					fetch: func(context.Context, uint64, uint64) ([]RawEvent, error) {
						if stage == "fetch" {
							fire() // the reorg lands mid-fetch
						}
						return []RawEvent{mkEv(15, 15, 2)}, nil
					},
					keep: func(RawEvent) bool { return true },
				}
				if stage == "after-send" {
					sc.delivered = func(RawEvent) { fire() }
				}
				_, epoch := rec.snapshot()
				err := sub.reconcileRange(context.Background(), rec, epoch, sc, 11, 20, testLogger)
				if !errors.Is(err, errReconcileStale) {
					t.Fatalf("err = %v, want errReconcileStale", err)
				}
				if last, _ := rec.state(); last != tc.wantLast {
					t.Errorf("lastReconciled = %d, want %d (not advanced by the stale tick)", last, tc.wantLast)
				}
				_, fresh := rec.snapshot()
				if stage != "after-send" { // there the send itself was valid (and is redelivered by the next tick)
					if miss, _ := rec.missing([]RawEvent{mkEv(15, 15, 2)}, fresh); len(miss) != 1 {
						t.Error("stale tick marked an identity seen")
					}
				}
			})
		}
	}
}

// An event is marked seen only after its send succeeded: a send that fails
// part-way leaves the unsent rest to the next tick.
func TestReconcileSendFailureLeavesRestUnseen(t *testing.T) {
	events := make(chan RawEvent, 1) // room for exactly one
	sub := newBareSub(t, events)
	rec := newReconciler()
	rec.start(0)
	_, epoch := rec.snapshot()
	kept := []RawEvent{mkEv(1, 1, 2), mkEv(2, 2, 2), mkEv(3, 3, 2)}

	ctx, cancel := context.WithCancel(context.Background())
	type res struct {
		n   int
		err error
	}
	done := make(chan res, 1)
	go func() {
		n, err := sub.deliverMissed(ctx, rec, epoch, reconcileScope{label: "x"}, kept, testLogger)
		done <- res{n, err}
	}()
	for len(events) == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond) // the second send is now blocked
	cancel()
	r := <-done
	if !errors.Is(r.err, context.Canceled) || r.n != 1 {
		t.Fatalf("deliverMissed = %d, %v; want 1, context canceled", r.n, r.err)
	}
	miss, _ := rec.missing(kept, epoch)
	if len(miss) != 2 || miss[0].BlockNumber != 2 || miss[1].BlockNumber != 3 {
		t.Errorf("after the failed send, missing = %v, want blocks 2 and 3 only", miss)
	}
}

func TestCapCursorsToNeverMovesForward(t *testing.T) {
	st := newFirehoseKeysStream("x", nil, nil)
	st.setCursor("a", 50, true)
	st.setCursor("b", 10, true)
	st.capCursorsTo(30)
	if a, b := st.cursor("a"), st.cursor("b"); a != 30 || b != 10 {
		t.Errorf("cursors a=%d b=%d, want 30 and 10", a, b)
	}
}

// --- split, lag, reorg, reconnect, membership ------------------------------------

// A range the RPC cannot serve in one call (too wide for its timeout) is split
// until it can, so reconcile keeps making progress instead of retrying the same
// range forever.
func TestReconcileSplitsOversizedRange(t *testing.T) {
	for _, site := range reconSites("keys-sub") {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(121, 121)
			reconAdd(chain, site, 131)
			node := newGapNode()
			sub, events, logs := startReconSite(t, site, chain, node, 20*time.Millisecond)
			node.nextFor(t, site)
			waitLive(t, sub)
			chain.maxSpan.Store(4) // 18 blocks to reconcile: only <= 4 succeed
			chain.tip.Store(140)
			waitEvent(t, events, "recovered @131 via split ranges", func(e RawEvent) bool { return e.BlockNumber == 131 && e.IsCatchup })
			if !strings.Contains(logs.String(), "reconcile range failed; splitting") {
				t.Error("split not logged")
			}
		})
	}
}

// reconcile_lag 0 is honoured: the tip block itself is reconciled.
func TestReconcileLagZero(t *testing.T) {
	for _, site := range reconSites("keys-sub") {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(121, 121)
			reconAdd(chain, site, 140)
			node := newGapNode()
			zero := uint64(0)
			sub, events, _ := startReconSite(t, site, chain, node, 20*time.Millisecond, func(c *SubscriberConfig) { c.ReconcileLag = &zero })
			node.nextFor(t, site)
			waitLive(t, sub)
			chain.tip.Store(140)
			waitEvent(t, events, "recovered @140 with lag 0", func(e RawEvent) bool { return e.BlockNumber == 140 && e.IsCatchup })
		})
	}
}

// A reorg at R: events of blocks >= R the live path had delivered are no longer
// counted as seen and the reconciled range is rolled back, so reconcile
// re-delivers them (the consumer reverted them); older blocks are untouched.
// "after" = the reorg lands once reconcile had covered the blocks; "before" =
// while they were still unreconciled (the seen set, not lastReconciled, matters).
func TestReconcileReorgRecoversFromReorgStart(t *testing.T) {
	for _, phase := range []string{"after-reconcile", "before-reconcile"} {
		for _, site := range reconSites("keys-sub") {
			t.Run(phase+"/"+site.name, func(t *testing.T) {
				interval := 20 * time.Millisecond
				if phase == "before-reconcile" {
					interval = 400 * time.Millisecond
				}
				chain := newGapChain(121, 121)
				reconAdd(chain, site, 130)
				reconAdd(chain, site, 131)
				node := newGapNode()
				sub, events, _ := startReconSite(t, site, chain, node, interval)
				sess := node.nextFor(t, site)
				waitLive(t, sub)
				sess.events <- reconLive(site.addr, 130)
				sess.events <- reconLive(site.addr, 131)
				waitEvent(t, events, "live @131", func(e RawEvent) bool { return e.BlockNumber == 131 })

				if phase == "after-reconcile" {
					chain.tip.Store(140)
					base := chain.fetches.Load()
					deadline := time.After(5 * time.Second)
					for chain.fetches.Load() == base { // the tick that reconciles through 138
						select {
						case <-deadline:
							t.Fatal("no reconcile fetch")
						case <-time.After(5 * time.Millisecond):
						}
					}
					time.Sleep(50 * time.Millisecond)
					if _, c := collect(events, 30*time.Millisecond); len(c) != 0 {
						t.Fatalf("unexpected recovery before the reorg: %v", c)
					}
				}

				sess.reorgs <- &client.ReorgEvent{StartBlockNum: 131, EndBlockNum: 131}
				if phase == "before-reconcile" {
					time.Sleep(50 * time.Millisecond) // the session has processed the reorg
					chain.tip.Store(140)
				}
				waitEvent(t, events, "re-delivered @131 after reorg", func(e RawEvent) bool { return e.BlockNumber == 131 && e.IsCatchup })
				if live, c := collect(events, 600*time.Millisecond); len(live)+len(c) != 0 {
					t.Errorf("after the reorg only block 131 may be re-delivered, once; also got live=%v catchup=%v", live, c)
				}
			})
		}
	}
}

// Healthy live stream, repeated reconnects: each reconnect re-delivers only the
// few blocks past lastReconciled, a bounded number that does not grow.
func TestReconcileReconnectRedeliveryBounded(t *testing.T) {
	for _, site := range reconSites("keys-sub") {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(121, 121)
			node := newGapNode()
			sub, events, _ := startReconSite(t, site, chain, node, 20*time.Millisecond)
			sess := node.nextFor(t, site)
			waitLive(t, sub)
			tip := uint64(121)
			for k := 0; k < 4; k++ {
				for b := tip + 1; b <= tip+10; b++ {
					reconAdd(chain, site, b)
					sess.events <- reconLive(site.addr, b)
				}
				tip += 10
				chain.tip.Store(tip)
				chain.pre.Store(tip)
				waitEvent(t, events, "live tip event", func(e RawEvent) bool { return e.BlockNumber == tip && !e.IsCatchup })
				time.Sleep(150 * time.Millisecond) // reconcile catches up to tip-2
				collect(events, 10*time.Millisecond)

				sess.errs <- fmt.Errorf("socket dropped")
				sess = node.nextFor(t, site)
				_, catchup := collect(events, 300*time.Millisecond)
				// lag(2)+1 blocks of overlap at most; one event per block here.
				if len(catchup) > 4 {
					t.Errorf("reconnect %d re-delivered %d events (%v), want a small bounded number", k, len(catchup), catchup)
				}
			}
		})
	}
}

// A contract added mid-session is reconciled from its join point: its history
// (delivered by its own backfill) is not delivered again, its later events are
// recovered. A removed contract is no longer reconciled.
func TestReconcileLateJoinerAndRemoved(t *testing.T) {
	site := reconSites("keys-sub")[0]
	chain := newGapChain(121, 121)
	chain.add(128, 0xB1) // B's history, below its join point
	chain.add(143, 0xB1) // after the join: the live stream drops it
	chain.add(143, 0xB2) // B2 is removed first
	chain.add(143, 0xA)  // control
	node := newGapNode()
	site.contracts = append(site.contracts, ContractSubscription{Address: newTestFelt(0xB2), StartBlock: 100, Wildcard: true})
	sub, events, _ := startReconSite(t, site, chain, node, 300*time.Millisecond)
	node.nextFor(t, site)
	waitLive(t, sub)

	// Join at tip 140 (the subscriber's cached tip is what seeds the join point).
	chain.tip.Store(140)
	sub.provider.tipBlock.Store(140)
	sub.AddContract(context.Background(), ContractSubscription{Address: newTestFelt(0xB1), StartBlock: 125, Wildcard: true})
	sub.RemoveContract(newTestFelt(0xB2).String())

	chain.tip.Store(150)
	got := map[string]int{}
	deadline := time.After(3 * time.Second)
	for got[newTestFelt(0xB1).String()+"@143"] == 0 || got[newTestFelt(0xA).String()+"@143"] == 0 {
		select {
		case e := <-events:
			if !e.BackfillDone {
				got[fmt.Sprintf("%s@%d", e.ContractAddress, e.BlockNumber)]++
			}
		case <-deadline:
			t.Fatalf("timed out; got %v", got)
		}
	}
	// Let further ticks pass, then check exactly-once and the removed contract.
	deadline = time.After(900 * time.Millisecond)
loop:
	for {
		select {
		case e := <-events:
			if !e.BackfillDone {
				got[fmt.Sprintf("%s@%d", e.ContractAddress, e.BlockNumber)]++
			}
		case <-deadline:
			break loop
		}
	}
	b1 := newTestFelt(0xB1).String()
	if got[b1+"@128"] != 1 {
		t.Errorf("B's history @128 delivered %d times, want 1 (its own backfill only)", got[b1+"@128"])
	}
	if got[b1+"@143"] != 1 {
		t.Errorf("B's post-join event @143 delivered %d times, want 1", got[b1+"@143"])
	}
	if n := got[newTestFelt(0xB2).String()+"@143"]; n != 0 {
		t.Errorf("removed contract's event delivered %d times", n)
	}
}
