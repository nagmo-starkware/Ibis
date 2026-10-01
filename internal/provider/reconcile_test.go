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
	"sync/atomic"
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

// reconTransports names the gap sites whose live stream is reconciled at this
// point of the series; tests over "all transports" range over it.
var reconTransports = []string{"keys-sub", "keys-address-sub", "keys-child-transfer", "shared-firehose"}

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
	case "keys-address-sub":
		return "token:" + newTestFelt(site.addr).String()
	case "keys-child-transfer":
		return "child-transfer:" + newTestFelt(site.addr).String()
	case "shared-firehose":
		return "firehose"
	case "per-contract":
		return "contract:" + newTestFelt(site.addr).String()
	}
	return site.name
}

// reconKey is keys[0] of the site's events: a Transfer for the child stream (so
// only its filter selects them), else the match-any 0x1.
func reconKey(site gapSite) *felt.Felt {
	if site.name == "keys-child-transfer" {
		return transferSelector
	}
	return newTestFelt(1)
}

// reconAdd puts an event of the site's scope on the chain.
func reconAdd(chain *gapChain, site gapSite, block uint64) {
	switch site.name {
	case "keys-child-transfer":
		chain.addKeyed(block, site.addr, transferSelector)
	case "keys-address-sub":
		// Keyed, so the keys-sub's filter (0x999) does not also select it.
		chain.addKeyed(block, site.addr, newTestFelt(1))
	default:
		chain.add(block, site.addr)
	}
}

// reconLive is a live event whose identity matches what gapChain serves over
// HTTP for the site at block: tx hash = block, keys [reconKey], data [2].
func reconLive(site gapSite, block uint64) *rpc.EmittedEventWithFinalityStatus {
	e := fhEvent(site.addr, block)
	e.TransactionHash = newTestFelt(block)
	e.Keys = []*felt.Felt{reconKey(site)}
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
	for _, site := range reconSites("keys-sub", "keys-address-sub", "keys-child-transfer", "shared-firehose", "per-contract") {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(121, 121)
			reconAdd(chain, site, 105) // gap event: delivered by the post-subscribe backfill
			for _, b := range []uint64{130, 131, 132} {
				reconAdd(chain, site, b)
			}
			node := newGapNode()
			sub, events, logs := startReconSite(t, site, chain, node, 20*time.Millisecond)

			sess := node.nextFor(t, site)
			waitLive(t, sub)
			waitEvent(t, events, "backfilled @105", func(e RawEvent) bool { return e.BlockNumber == 105 })

			sess.events <- reconLive(site, 130)
			sess.events <- reconLive(site, 132) // 131 is dropped by the node
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

// Child Transfer/Approval streams are reconciled with one address-less query per
// tick, not one per child: every child's dropped event is recovered exactly
// once and no per-address query is issued.
func TestReconcileChildrenShareOneQuery(t *testing.T) {
	const n = 4
	var subs []ContractSubscription
	chain := newGapChain(121, 121)
	for i := uint64(0); i < n; i++ {
		subs = append(subs, ContractSubscription{Address: newTestFelt(0xD0 + i), StartBlock: 100, Wildcard: true, ERC20: true})
		chain.addKeyed(131, 0xD0+i, transferSelector)
	}
	site := gapSite{"children", subs, SubscriberConfig{KeysFirehose: true, OptionSelectors: []*felt.Felt{newTestFelt(0x999)}}, 0xD0, 100, 0, n + 1}
	node := newGapNode()
	sub, events, _ := startReconSite(t, site, chain, node, 20*time.Millisecond)
	waitLive(t, sub)

	base := chain.addrCalls.Load()
	chain.tip.Store(140)
	seen := map[string]int{}
	for i := 0; i < n; i++ {
		e := waitEvent(t, events, "recovered child event", func(e RawEvent) bool { return e.BlockNumber == 131 && e.IsCatchup })
		seen[e.ContractAddress.String()]++
	}
	if len(seen) != n {
		t.Errorf("recovered events per child = %v, want one for each of %d children", seen, n)
	}
	if live, catchup := collect(events, 200*time.Millisecond); len(live)+len(catchup) != 0 {
		t.Errorf("duplicate delivery: %v %v", live, catchup)
	}
	if got := chain.addrCalls.Load() - base; got != 0 {
		t.Errorf("%d per-address getEvents calls during reconcile, want 0 (one shared query)", got)
	}
}

// A failed reconcile does not advance lastReconciled: the next successful tick
// still covers the range and recovers the event.
func TestReconcileFailureDoesNotAdvance(t *testing.T) {
	for _, site := range reconSites("keys-sub", "keys-address-sub", "keys-child-transfer", "shared-firehose", "per-contract") {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(121, 121)
			reconAdd(chain, site, 131)
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
	for _, site := range reconSites("keys-sub", "keys-address-sub", "keys-child-transfer", "shared-firehose", "per-contract") {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(121, 121)
			node := newGapNode()
			// Interval too long for a tick to run: lastReconciled stays at P=121.
			sub, events, _ := startReconSite(t, site, chain, node, time.Hour)

			sess := node.nextFor(t, site)
			waitLive(t, sub)
			sess.events <- reconLive(site, 130)
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
	for _, site := range reconSites("keys-sub", "keys-address-sub", "keys-child-transfer", "shared-firehose", "per-contract") {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(121, 121)
			reconAdd(chain, site, 131)
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
	for _, site := range reconSites("keys-sub", "keys-address-sub", "keys-child-transfer", "shared-firehose") {
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
	for _, site := range reconSites("keys-sub", "keys-address-sub", "keys-child-transfer", "shared-firehose") {
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
		for _, site := range reconSites("keys-sub", "keys-address-sub", "keys-child-transfer", "shared-firehose") {
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
				sess.events <- reconLive(site, 130)
				sess.events <- reconLive(site, 131)
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
	for _, site := range reconSites("keys-sub", "keys-address-sub", "keys-child-transfer", "shared-firehose") {
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
					sess.events <- reconLive(site, b)
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
	for _, site := range reconSites("keys-sub", "shared-firehose") {
		t.Run(site.name, func(t *testing.T) { lateJoinerAndRemoved(t, site) })
	}
}

func lateJoinerAndRemoved(t *testing.T, site gapSite) {
	chain := newGapChain(121, 121)
	chain.add(128, 0xB1) // B's history, below its join point
	chain.add(143, 0xB1) // after the join: the live stream drops it
	chain.add(143, 0xB2) // B2 is removed first
	chain.add(143, 0xA)  // control
	node := newGapNode()
	site.contracts = append(append([]ContractSubscription(nil), site.contracts...), ContractSubscription{Address: newTestFelt(0xB2), StartBlock: 100, Wildcard: true})
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

// --- split bounds, backoff, redaction, pending rollback ---------------------------

// countEvents tallies non-sentinel events by "address@block" until d elapses.
func countEvents(ch <-chan RawEvent, d time.Duration) map[string]int {
	got := map[string]int{}
	deadline := time.After(d)
	for {
		select {
		case e := <-ch:
			if !e.BackfillDone {
				got[fmt.Sprintf("%s@%d", e.ContractAddress, e.BlockNumber)]++
			}
		case <-deadline:
			return got
		}
	}
}

func bareRange(t *testing.T, fetch func(from, to uint64) ([]RawEvent, error), rec *reconciler, from, to uint64) error {
	t.Helper()
	sub := newBareSub(t, make(chan RawEvent, 4))
	sc := reconcileScope{
		label: "x",
		fetch: func(_ context.Context, f, t uint64) ([]RawEvent, error) { return fetch(f, t) },
		keep:  func(RawEvent) bool { return true },
	}
	_, epoch := rec.snapshot()
	return sub.reconcileRange(context.Background(), rec, epoch, sc, from, to, testLogger)
}

var errTimeout = errors.New("fetching events: context deadline exceeded")

// A range that keeps failing costs O(log2(range)) calls per tick, never one per
// block, and does not advance; a mixed result advances exactly through the last
// contiguous success.
func TestReconcileSplitRetryBounds(t *testing.T) {
	t.Run("persistent failure", func(t *testing.T) {
		rec := newReconciler()
		rec.start(0)
		var calls int
		for tick := 0; tick < 2; tick++ { // retried next tick, same bound, no progress
			calls = 0
			err := bareRange(t, func(f, to uint64) ([]RawEvent, error) { calls++; return nil, errTimeout }, rec, 1, 100)
			if err == nil {
				t.Fatal("want an error")
			}
			if calls > 9 { // log2(100) ~ 6.6: 8 descents + the first call
				t.Errorf("tick %d: %d calls for a persistently failing 100-block range, want O(log2) (<= 9)", tick, calls)
			}
			if last, _ := rec.state(); last != 0 {
				t.Errorf("lastReconciled = %d after only failures, want 0", last)
			}
		}
	})
	t.Run("mixed result", func(t *testing.T) {
		rec := newReconciler()
		rec.start(0)
		calls := 0
		// Wide ranges time out; block 20 is bad on its own; everything else works.
		err := bareRange(t, func(f, to uint64) ([]RawEvent, error) {
			calls++
			if to-f+1 > 8 || (f <= 20 && 20 <= to) {
				return nil, errTimeout
			}
			return nil, nil
		}, rec, 1, 64)
		if err == nil {
			t.Fatal("want an error from block 20")
		}
		if last, _ := rec.state(); last != 19 {
			t.Errorf("lastReconciled = %d, want 19 (last contiguous success)", last)
		}
		if calls > 20 {
			t.Errorf("%d calls, want a small multiple of log2(64)", calls)
		}
	})
	t.Run("429 is not split", func(t *testing.T) {
		rec := newReconciler()
		rec.start(0)
		calls := 0
		err := bareRange(t, func(f, to uint64) ([]RawEvent, error) {
			calls++
			return nil, errors.New("429 Too Many Requests")
		}, rec, 1, 100)
		if err == nil || calls != 1 {
			t.Errorf("err=%v calls=%d, want 1 call and an error (no splitting on a rate limit)", err, calls)
		}
	})
}

func TestSplittable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{context.DeadlineExceeded, true},
		{fmt.Errorf("x: %w", context.DeadlineExceeded), true},
		{errors.New("Post: i/o timeout"), true},
		{errors.New("response size exceeded"), true},
		{errors.New("429 Too Many Requests"), false},
		{errors.New("rate limit exceeded"), false},
		{errors.New("503 Service Unavailable"), false},
		{errors.New("connection refused"), false},
	} {
		if got := splittable(tc.err); got != tc.want {
			t.Errorf("splittable(%q) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// The retry backoff is capped at the interval: after many consecutive failures
// the loop still waits (it used to overflow to 0 and spin).
func TestReconcileLoopBackoffNeverCollapses(t *testing.T) {
	sub := newBareSub(t, make(chan RawEvent, 1))
	sub.reconcileInterval = 5 * time.Millisecond
	var calls atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	sub.reconcileLoop(ctx, "x", func(context.Context, *slog.Logger) error {
		calls.Add(1)
		return errors.New("rpc down")
	})
	if n := calls.Load(); n > 200 || n < 20 { // ~140 at one tick per 5ms
		t.Errorf("%d ticks in 700ms at a 5ms interval, want ~140 (a collapsed backoff spins)", n)
	}
}

func TestRedactURLs(t *testing.T) {
	in := `Post "https://starknet-mainnet.g.alchemy.com/starknet/version/rpc/v0_10/SECRETKEY": dial tcp: lookup failed; ws: wss://user:pw@node.example/ws?apikey=SECRET2 and http://127.0.0.1:8080/`
	out := redactURLs(in)
	for _, leak := range []string{"SECRETKEY", "SECRET2", "pw@", "v0_10"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q leaked in %q", leak, out)
		}
	}
	for _, keep := range []string{"https://starknet-mainnet.g.alchemy.com/<redacted>", "wss://node.example/<redacted>", "http://127.0.0.1:8080"} {
		if !strings.Contains(out, keep) {
			t.Errorf("%q missing in %q", keep, out)
		}
	}
}

// A failing reconcile against an RPC URL with a key in its path never logs the key.
func TestReconcileLogMasksRPCURL(t *testing.T) {
	server := mockRPCServer(t, newGapChain(1, 1).handlers())
	logs := &syncBuf{}
	p, err := New(context.Background(), server.URL+"/v0_10/SECRETTOKEN", slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	server.Close() // every call now fails with an error that embeds the URL
	sub := p.NewSubscriber(nil, make(chan RawEvent, 1), &SubscriberConfig{ReconcileInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	rec := newReconciler()
	rec.start(0)
	sub.reconcileLoop(ctx, "x", func(c context.Context, l *slog.Logger) error {
		return sub.reconcileTick(c, rec, reconcileScope{label: "x"}, l)
	})
	out := logs.String()
	if !strings.Contains(out, "reconcile failed") {
		t.Fatalf("no failure logged:\n%s", out)
	}
	if strings.Contains(out, "SECRETTOKEN") {
		t.Errorf("RPC URL key leaked in logs:\n%s", out)
	}
}

// A reorg seen before start() (during the post-subscribe backfill, which may have
// fetched orphaned data) is honoured: reconcile starts below the reorg.
func TestReconcilerStartHonoursPendingRollback(t *testing.T) {
	for _, tc := range []struct{ rollbackAt, p, want uint64 }{{50, 60, 49}, {50, 40, 40}, {0, 60, 0}} {
		r := newReconciler()
		r.rollback(tc.rollbackAt)
		r.start(tc.p)
		if last, _ := r.state(); last != tc.want {
			t.Errorf("rollback(%d) then start(%d): last = %d, want %d", tc.rollbackAt, tc.p, last, tc.want)
		}
	}
}

// A reorg landing while the post-subscribe backfill runs makes reconcile re-read
// from the reorg start, so the (possibly orphaned) backfilled events are redone.
func TestReconcileReorgDuringBackfill(t *testing.T) {
	for _, site := range reconSites(reconTransports...) {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(121, 121)
			chain.gate = make(chan struct{})
			reconAdd(chain, site, 120)
			node := newGapNode()
			sub, events, _ := startReconSite(t, site, chain, node, 20*time.Millisecond)
			sess := node.nextFor(t, site)
			<-chain.entered // the backfill is in flight
			sess.reorgs <- &client.ReorgEvent{StartBlockNum: 119, EndBlockNum: 119}
			for len(sess.reorgs) != 0 {
				time.Sleep(time.Millisecond)
			}
			time.Sleep(30 * time.Millisecond) // the rollback has run
			close(chain.gate)
			waitLive(t, sub)
			chain.tip.Store(130)
			got := countEvents(events, 800*time.Millisecond)
			key := fmt.Sprintf("%s@120", newTestFelt(site.addr))
			if got[key] != 2 {
				t.Errorf("event @120 delivered %d times, want 2 (backfill, then re-read after the reorg): %v", got[key], got)
			}
		})
	}
}

// Child Transfer/Approval streams: a child added mid-session is reconciled from
// its join point (history delivered once by its own backfill, later events
// recovered once); a removed child is no longer reconciled.
func TestReconcileChildJoinAndRemove(t *testing.T) {
	site := reconSites("keys-child-transfer")[0] // child 0xD from the start
	site.contracts = append(site.contracts, ContractSubscription{Address: newTestFelt(0xE), StartBlock: 100, Wildcard: true, ERC20: true})
	chain := newGapChain(121, 121)
	node := newGapNode()
	sub, events, _ := startReconSite(t, site, chain, node, 300*time.Millisecond)
	node.nextFor(t, site)
	waitLive(t, sub)

	chain.tip.Store(140) // pre stays 121: the joined child's P is below its join point
	sub.provider.tipBlock.Store(140)
	for _, a := range []uint64{0xD, 0xE, 0xF} {
		chain.addKeyed(143, a, transferSelector)
	}
	chain.addKeyed(128, 0xF, transferSelector) // F's history, below its join point
	sub.AddContract(context.Background(), ContractSubscription{Address: newTestFelt(0xF), StartBlock: 125, Wildcard: true, ERC20: true})
	sub.RemoveContract(newTestFelt(0xE).String())
	waitLive(t, sub)

	chain.tip.Store(150)
	got := countEvents(events, 1500*time.Millisecond)
	f, d, e := newTestFelt(0xF).String(), newTestFelt(0xD).String(), newTestFelt(0xE).String()
	if got[f+"@128"] != 1 {
		t.Errorf("joined child's history delivered %d times, want 1 (its own backfill)", got[f+"@128"])
	}
	if got[f+"@143"] != 1 || got[d+"@143"] != 1 {
		t.Errorf("recovered: F@143=%d D@143=%d, want 1 each (%v)", got[f+"@143"], got[d+"@143"], got)
	}
	if got[e+"@143"] != 0 {
		t.Errorf("removed child's event delivered %d times", got[e+"@143"])
	}
}

// --- child hub: per-member handling inside one tick --------------------------------

// hubFixture is a subscriber with hand-registered collective child streams.
type hubFixture struct {
	sub     *EventSubscriber
	chain   *gapChain
	events  chan RawEvent
	streams map[uint64]*firehoseKeysStream
	recs    map[uint64]*reconciler
}

func newHubFixture(t *testing.T, addrs ...uint64) *hubFixture {
	t.Helper()
	chain := newGapChain(150, 150)
	server := mockRPCServer(t, chain.handlers())
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	events := make(chan RawEvent, 64)
	lag := uint64(2)
	sub := p.NewSubscriber(nil, events, &SubscriberConfig{KeysFirehose: true, ReconcileInterval: time.Second, ReconcileLag: &lag})
	f := &hubFixture{sub: sub, chain: chain, events: events, streams: map[uint64]*firehoseKeysStream{}, recs: map[uint64]*reconciler{}}
	for _, a := range addrs {
		f.add(a)
	}
	return f
}

// add registers a live child stream (reconciler started at 100) for addr.
func (f *hubFixture) add(addr uint64) {
	st := newFirehoseKeysStream("child-transfer:"+newTestFelt(addr).String(), newTestFelt(addr), childTransferKeys())
	st.collective = true
	st.setCursor(newTestFelt(addr).String(), 100, true)
	rec := newReconciler()
	rec.start(100)
	st.rec.Store(rec)
	f.sub.trackContract(ContractSubscription{Address: newTestFelt(addr)})
	f.sub.streamsMu.Lock()
	f.sub.addrStreams[newTestFelt(addr).String()] = st
	f.sub.streamsMu.Unlock()
	f.streams[addr] = st
	f.recs[addr] = rec
	f.chain.addKeyed(120, addr, transferSelector)
}

func (f *hubFixture) last(addr uint64) uint64 {
	l, _ := f.recs[addr].state()
	return l
}

func (f *hubFixture) tick(t *testing.T) error {
	t.Helper()
	return f.sub.childReconcileTick(context.Background(), testLogger)
}

// Members that go stale (rolled back), are removed, or join while a tick is in
// flight do not affect the healthy ones: those recover their missed events
// exactly once and advance; the stale and removed ones neither deliver nor
// advance; the next tick picks up the stale one and the newcomer.
func TestReconcileChildHubPerMemberStale(t *testing.T) {
	f := newHubFixture(t, 0x1, 0x2, 0x3, 0x4) // 1,2 healthy; 3 rolled back; 4 removed
	f.chain.onFetch = func(uint64, uint64) {
		f.chain.onFetch = nil
		f.streams[0x3].rec.Load().rollback(110) // reorg lands mid-tick
		f.sub.streamsMu.Lock()
		delete(f.sub.addrStreams, newTestFelt(0x4).String())
		f.sub.streamsMu.Unlock()
		f.streams[0x4].rec.Store(nil) // 4's session ended
		f.add(0x5)                    // joins mid-tick
	}
	if err := f.tick(t); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got := countEvents(f.events, 100*time.Millisecond)
	k := func(a uint64) string { return fmt.Sprintf("%s@120", newTestFelt(a)) }
	if got[k(1)] != 1 || got[k(2)] != 1 {
		t.Errorf("healthy members recovered %d and %d times, want 1 each (%v)", got[k(1)], got[k(2)], got)
	}
	if got[k(3)] != 0 || got[k(4)] != 0 || got[k(5)] != 0 {
		t.Errorf("stale/removed/new member delivered during the tick: %v", got)
	}
	if f.last(1) != 148 || f.last(2) != 148 {
		t.Errorf("healthy members at %d and %d, want 148", f.last(1), f.last(2))
	}
	if f.last(3) != 100 || f.last(4) != 100 || f.last(5) != 100 {
		t.Errorf("stale/removed/new members advanced: %d %d %d, want 100", f.last(3), f.last(4), f.last(5))
	}

	// Next tick: the rolled-back member and the newcomer catch up; the removed one stays out.
	if err := f.tick(t); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	got = countEvents(f.events, 100*time.Millisecond)
	if got[k(3)] != 1 || got[k(5)] != 1 || got[k(4)] != 0 || got[k(1)] != 0 {
		t.Errorf("second tick delivered %v, want 3 and 5 once, nothing for 1 or 4", got)
	}
}

// A range that has to be split and retried, with a member rolled back during
// the first attempt, still advances every healthy member exactly through the
// range and recovers its events once; the rolled-back one is left untouched.
func TestReconcileChildHubRetryKeepsMembership(t *testing.T) {
	f := newHubFixture(t, 0x1, 0x2, 0x3)
	f.chain.maxSpan.Store(8) // 48 blocks to reconcile: needs splitting
	first := true
	f.chain.onFetch = func(uint64, uint64) {
		if first {
			first = false
			f.streams[0x3].rec.Load().rollback(110)
		}
	}
	if err := f.tick(t); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got := countEvents(f.events, 100*time.Millisecond)
	k := func(a uint64) string { return fmt.Sprintf("%s@120", newTestFelt(a)) }
	if got[k(1)] != 1 || got[k(2)] != 1 || got[k(3)] != 0 {
		t.Errorf("deliveries %v, want 1 and 2 once, 3 none", got)
	}
	if f.last(1) != 148 || f.last(2) != 148 || f.last(3) != 100 {
		t.Errorf("last = %d %d %d, want 148 148 100", f.last(1), f.last(2), f.last(3))
	}
}

func TestCapSinksToNeverMovesForward(t *testing.T) {
	s := &EventSubscriber{router: map[string]*firehoseSink{}}
	s.addSink(ContractSubscription{Address: newTestFelt(1)}, 50)
	s.addSink(ContractSubscription{Address: newTestFelt(2)}, 10)
	s.capSinksTo(30)
	if a, b := s.router[newTestFelt(1).String()].lastBlock, s.router[newTestFelt(2).String()].lastBlock; a != 30 || b != 10 {
		t.Errorf("sink cursors %d and %d, want 30 and 10", a, b)
	}
}
