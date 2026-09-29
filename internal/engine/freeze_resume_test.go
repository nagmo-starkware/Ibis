package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/provider"
	"github.com/b-j-roberts/ibis/internal/store/memory"
	"github.com/b-j-roberts/ibis/internal/types"
)

// eventsRPC is a node at tip 1000 that records each getEvents range and
// answers with one event at block 950 when the range contains it. If hold is
// set, getEvents blocks on it.
type eventsRPC struct {
	*httptest.Server
	mu     sync.Mutex
	ranges [][2]uint64
	hold   chan struct{}
}

func (s *eventsRPC) requests() [][2]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][2]uint64(nil), s.ranges...)
}

// covers reports whether the recorded requests span [from, to] as their
// lowest start and highest end.
func (s *eventsRPC) spans(from, to uint64) bool {
	rs := s.requests()
	if len(rs) == 0 {
		return false
	}
	lo, hi := rs[0][0], rs[0][1]
	for _, r := range rs {
		lo, hi = min(lo, r[0]), max(hi, r[1])
	}
	return lo == from && hi == to
}

func newEventsRPC(addr *felt.Felt, sel *felt.Felt) *eventsRPC {
	s := &eventsRPC{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var result interface{}
		switch req.Method {
		case "starknet_specVersion":
			result = "0.9.0"
		case "starknet_blockNumber":
			result = 1000
		case "starknet_getBlockWithTxHashes":
			result = map[string]interface{}{"timestamp": 1}
		case "starknet_getEvents":
			var p []struct {
				From struct {
					N uint64 `json:"block_number"`
				} `json:"from_block"`
				To struct {
					N uint64 `json:"block_number"`
				} `json:"to_block"`
			}
			_ = json.Unmarshal(req.Params, &p)
			var from, to uint64
			if len(p) == 1 {
				from, to = p[0].From.N, p[0].To.N
			}
			s.mu.Lock()
			s.ranges = append(s.ranges, [2]uint64{from, to})
			hold := s.hold
			s.mu.Unlock()
			if hold != nil {
				<-hold
			}
			evs := []map[string]interface{}{}
			if from <= 950 && 950 <= to {
				evs = append(evs, map[string]interface{}{
					"block_number": 950, "block_hash": "0x1", "transaction_hash": "0x2",
					"from_address": addr.String(), "keys": []string{sel.String(), "0x7"}, "data": []string{"0x5"},
				})
			}
			result = map[string]interface{}{"events": evs}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	return s
}

// resumeHarness builds an engine holding one frozen dynamic contract, as
// setup() would rehydrate it, with `saved` as its persisted record.
func resumeHarness(t *testing.T, saved config.ContractConfig, cursor uint64, transport string) (*Engine, *contractState, *eventsRPC) {
	t.Helper()
	addr := new(felt.Felt).SetUint64(0xC0FFEE)
	ev := testEventDef("Transfer")
	cs := testContractState(addr, "Child_c0ffee", []*abi.EventDef{ev}, types.TableTypeLog)
	saved.Name, saved.Address, saved.Dynamic = "Child_c0ffee", addr.String(), true
	cs.config = saved

	st := memory.New()
	ctx := context.Background()
	if err := st.CreateTable(ctx, cs.schemas["Transfer"]); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveDynamicContract(ctx, &saved); err != nil {
		t.Fatal(err)
	}
	if cursor > 0 {
		if err := st.SetCursor(ctx, cs.config.Name, cursor); err != nil {
			t.Fatal(err)
		}
	}

	srv := newEventsRPC(addr, ev.Selector)
	t.Cleanup(srv.Close)
	p, err := provider.New(ctx, srv.URL, nil)
	if err != nil {
		t.Fatalf("provider.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	cfg := &config.Config{}
	cfg.Indexer.Transport = transport
	e := New(cfg, st, p, noopLogger())
	e.contracts = []*contractState{cs}
	e.setupDone = true
	return e, cs, srv
}

// runEngine runs e until the test ends and returns a channel of delivered
// events' block numbers.
func runEngine(t *testing.T, e *Engine) <-chan uint64 {
	t.Helper()
	delivered := make(chan uint64, 1024)
	e.onEvent = func(_, _, _ string, block, _ uint64, _ map[string]any) {
		select {
		case delivered <- block:
		default:
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = e.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return delivered
}

func waitPendingZero(t *testing.T, e *Engine) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.BackfillsPending() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("BackfillsPending = %d, want 0", e.BackfillsPending())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitMarkerCleared(t *testing.T, e *Engine) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for persistedBackfillTo(t, e) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("BackfillTo still persisted after the backfill completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitDelivered(t *testing.T, ch <-chan uint64, want uint64) {
	t.Helper()
	select {
	case b := <-ch:
		if b != want {
			t.Fatalf("delivered block %d, want %d", b, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no event delivered")
	}
}

// manualLoop wires a per-contract subscriber into e and runs only its event
// loop, so a test can drive the engine methods itself.
func manualLoop(t *testing.T, e *Engine) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.runCtx = ctx
	e.subscriber = e.provider.NewSubscriber(nil, e.events, &provider.SubscriberConfig{})
	done := make(chan struct{})
	go func() { defer close(done); _ = e.eventLoop(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func persistedBackfillTo(t *testing.T, e *Engine) uint64 {
	t.Helper()
	got, err := e.store.GetDynamicContracts(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("GetDynamicContracts: %v (%d)", err, len(got))
	}
	return got[0].BackfillTo
}

func u64(v uint64) *uint64 { return &v }

// (a) A non-frozen contract restarted mid-backfill: a live event pushed its
// cursor past the unfetched gap, so the whole range is re-fetched from the
// start block, not from the cursor.
func TestRun_NonFrozenRestartMidBackfillRefetchesFullRange(t *testing.T) {
	e, _, srv := resumeHarness(t, config.ContractConfig{StartBlock: u64(900), BackfillTo: provider.ToTip}, 999, "http")
	delivered := runEngine(t, e)

	waitDelivered(t, delivered, 950)
	waitMarkerCleared(t, e)
	if !srv.spans(900, 1000) {
		t.Fatalf("getEvents ranges = %v, want the full [900, tip 1000]", srv.requests())
	}
}

// (b) Frozen at registration, restarted mid-backfill: full range, on every
// transport, backfill only (no live stream), marker cleared afterwards.
func TestRun_FrozenRestartRefetchesFullRange(t *testing.T) {
	for _, transport := range []string{"http", "firehose", "firehose-keys"} {
		t.Run(transport, func(t *testing.T) {
			e, cs, srv := resumeHarness(t,
				config.ContractConfig{Frozen: true, StartBlock: u64(900), BackfillTo: 990}, 999, transport)
			e.optionSelectors = []*felt.Felt{new(felt.Felt).SetUint64(0x999)} // keys firehose needs a non-empty set
			delivered := runEngine(t, e)

			waitDelivered(t, delivered, 950)
			waitMarkerCleared(t, e)
			waitPendingZero(t, e)
			if !srv.spans(900, 990) {
				t.Fatalf("getEvents ranges = %v, want [900, 990]", srv.requests())
			}
			if _, total, _ := e.TransportStatus(); transport == "http" && total != 0 {
				t.Fatalf("live streams = %d, want 0 (frozen: backfill only)", total)
			}
			if !cs.config.Frozen || cs.config.BackfillTo != 0 {
				t.Fatalf("frozen=%v backfillTo=%d, want frozen and cleared", cs.config.Frozen, cs.config.BackfillTo)
			}
		})
	}
}

// (c) Reconcile freezes a rehydrated child that was still behind the tip: it
// gets a marker and the next run fetches its history up to the tip.
func TestReconcileFrozen_RehydratedChildBehindTipResumes(t *testing.T) {
	e, cs, srv := resumeHarness(t, config.ContractConfig{StartBlock: u64(900)}, 940, "http")
	cs.config.Freeze = &config.FreezeConfig{Any: []config.FreezeRule{
		{Predicate: &config.FreezePredicate{MetaField: "expiry", Op: "lt", Value: "now() - 2d"}},
	}}
	cs.config.FactoryMeta = map[string]any{"expiry": int64(1000000000)}

	e.reconcileFrozenContracts(context.Background())
	if !cs.config.Frozen || persistedBackfillTo(t, e) != provider.ToTip {
		t.Fatalf("frozen=%v persisted BackfillTo=%d, want frozen with the to-tip marker", cs.config.Frozen, persistedBackfillTo(t, e))
	}

	delivered := runEngine(t, e)
	waitDelivered(t, delivered, 950)
	waitMarkerCleared(t, e)
	if !srv.spans(900, 1000) {
		t.Fatalf("getEvents ranges = %v, want [900, tip 1000]", srv.requests())
	}
}

// (d) A contract loaded at startup catches up in-stream, with no tracked
// backfill. Freezing it mid-catchup must not cut its history: it gets its own
// backfill, marked before it starts.
func TestFreezeContract_InStreamCatchupGetsBackfill(t *testing.T) {
	e, cs, srv := resumeHarness(t, config.ContractConfig{StartBlock: u64(900)}, 0, "http")
	srv.mu.Lock()
	srv.hold = make(chan struct{})
	srv.mu.Unlock()
	delivered := make(chan uint64, 8)
	e.onEvent = func(_, _, _ string, block, _ uint64, _ map[string]any) { delivered <- block }
	manualLoop(t, e)

	if err := e.FreezeContract(context.Background(), cs.config.Name); err != nil {
		t.Fatal(err)
	}
	if got := persistedBackfillTo(t, e); got != provider.ToTip {
		t.Fatalf("persisted BackfillTo = %d mid-backfill, want the to-tip marker", got)
	}
	close(srv.hold)
	waitDelivered(t, delivered, 950)
	waitMarkerCleared(t, e)
	waitPendingZero(t, e)
	if !srv.spans(900, 1000) {
		t.Fatalf("getEvents ranges = %v, want [900, 1000]", srv.requests())
	}
}

// (e) The marker clears only once the backfill's last event has been
// processed: the completion travels in-band behind the events.
func TestBackfillMarkerClearsAfterLastEventProcessed(t *testing.T) {
	e, _, srv := resumeHarness(t, config.ContractConfig{StartBlock: u64(900), BackfillTo: provider.ToTip}, 0, "http")
	inEvent, release := make(chan struct{}), make(chan struct{})
	e.onEvent = func(_, _, _ string, _, _ uint64, _ map[string]any) {
		inEvent <- struct{}{}
		<-release
	}
	manualLoop(t, e)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock) // runs before manualLoop's: a failure must not wedge the loop
	e.resumeBackfills(e.runCtx)

	<-inEvent             // the last event is being processed
	waitPendingZero(t, e) // backfill done: completion queued behind it
	if persistedBackfillTo(t, e) == 0 {
		t.Fatal("marker cleared before the last backfill event was processed")
	}
	unblock()
	waitMarkerCleared(t, e)
	_ = srv
}

// (f) A completion for a deregistered contract is ignored, and one for a
// re-added contract does not write back the old config.
func TestCompleteBackfill_DeregisterAndReAdd(t *testing.T) {
	ctx := context.Background()
	e, cs, _ := resumeHarness(t, config.ContractConfig{StartBlock: u64(900), BackfillTo: provider.ToTip}, 0, "http")
	if err := e.DeregisterContract(ctx, cs.config.Name, false); err != nil {
		t.Fatal(err)
	}
	e.completeBackfill(ctx, cs.address)
	e.persistContract(ctx, cs)
	if got, _ := e.store.GetDynamicContracts(ctx); len(got) != 0 {
		t.Fatalf("deregistered contract resurrected in the store: %+v", got)
	}

	// Re-added with a different config and no marker: the stale completion
	// must leave the fresh record alone.
	fresh := config.ContractConfig{Name: cs.config.Name, Address: cs.config.Address, Dynamic: true, StartBlock: u64(950)}
	cs2 := &contractState{config: fresh, address: cs.address, schemas: cs.schemas}
	e.contracts = []*contractState{cs2}
	if err := e.store.SaveDynamicContract(ctx, &fresh); err != nil {
		t.Fatal(err)
	}
	e.completeBackfill(ctx, cs.address)
	got, _ := e.store.GetDynamicContracts(ctx)
	if len(got) != 1 || got[0].StartBlock == nil || *got[0].StartBlock != 950 {
		t.Fatalf("fresh record clobbered: %+v", got)
	}
}

// (g) Complete history (BackfillTo == 0) is never re-fetched, frozen or not.
// The legacy case is a record persisted before the field existed.
func TestRun_DoesNotRefetchCompleteContract(t *testing.T) {
	var legacy config.ContractConfig
	if err := json.Unmarshal([]byte(`{"name":"x","frozen":true,"start_block":900}`), &legacy); err != nil {
		t.Fatal(err)
	}
	for name, saved := range map[string]config.ContractConfig{
		"complete frozen": {Frozen: true, StartBlock: u64(900)},
		"legacy frozen":   legacy,
		"live":            {StartBlock: u64(900)},
	} {
		t.Run(name, func(t *testing.T) {
			e, _, srv := resumeHarness(t, saved, 999, "http")
			runEngine(t, e)
			time.Sleep(300 * time.Millisecond)
			for _, r := range srv.requests() {
				if r[0] < 1000 {
					t.Fatalf("getEvents %v re-fetches history, want none below the cursor", r)
				}
			}
			if got := e.BackfillsPending(); got != 0 {
				t.Fatalf("BackfillsPending = %d, want 0", got)
			}
		})
	}
}

// Registration persists the marker before the backfill can deliver anything,
// and completion clears it, both when a backfill ran and when none was needed.
func TestRegisterChild_MarkerPersistedBeforeBackfillAndCleared(t *testing.T) {
	for name, start := range map[string]uint64{"backfill": 900, "nothing before tip": 2000} {
		t.Run(name, func(t *testing.T) {
			addr := new(felt.Felt).SetUint64(0xBEEF)
			e, _, srv := resumeHarness(t, config.ContractConfig{}, 0, "http")
			e.contracts = nil
			_ = e.store.DeleteDynamicContract(context.Background(), "Child_c0ffee")
			srv.mu.Lock()
			srv.hold = make(chan struct{})
			srv.mu.Unlock()
			manualLoop(t, e)

			cc := &config.ContractConfig{
				Name: "Child_beef", Address: addr.String(), StartBlock: u64(start),
				Events: []config.EventConfig{{Name: "Transfer"}},
			}
			childABI := &abi.ABI{Types: map[string]*abi.TypeDef{}, Events: []*abi.EventDef{testEventDef("Transfer")}}
			if err := e.registerWithABI(context.Background(), cc, childABI); err != nil {
				t.Fatal(err)
			}
			if name == "backfill" {
				if got := persistedBackfillTo(t, e); got != provider.ToTip {
					t.Fatalf("persisted BackfillTo = %d with the backfill held, want the marker", got)
				}
			}
			close(srv.hold)
			waitMarkerCleared(t, e)
		})
	}
}
