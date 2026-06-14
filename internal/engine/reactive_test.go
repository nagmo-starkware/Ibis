package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/store/memory"
	"github.com/b-j-roberts/ibis/internal/types"
)

// reactiveViewContract builds a contractState exposing one view function under
// the given refresh policy. The view function is resolvable from the ABI.
func reactiveViewContract(addr *felt.Felt, name, viewFn string, refresh *config.ViewRefreshConfig) *contractState {
	funcDef := testFunctionDef(viewFn)
	return &contractState{
		config: config.ContractConfig{
			Name:    name,
			Address: addr.String(),
			Views: []config.ViewConfig{{
				Function: viewFn,
				Refresh:  refresh,
				Table:    config.TableConfig{Type: "unique", UniqueKey: "_view_key"},
			}},
		},
		address: addr,
		abi: &abi.ABI{
			Types:     make(map[string]*abi.TypeDef),
			Functions: map[string]*abi.FunctionDef{viewFn: funcDef},
		},
		schemas: make(map[string]*types.TableSchema),
	}
}

func startPoller(t *testing.T, vp *ViewPoller, cs *contractState, st store.Store) context.CancelFunc {
	t.Helper()
	schemas, err := vp.Setup([]*contractState{cs})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	for _, s := range schemas {
		if err := st.CreateTable(context.Background(), s); err != nil {
			t.Fatalf("CreateTable: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go vp.Run(ctx)
	return cancel
}

// A constant view is read exactly once (at registration) and never polled again.
func TestViewPoller_ConstantReadsOnce(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	cs := reactiveViewContract(addr, "OptToken", "get_strike",
		&config.ViewRefreshConfig{Mode: config.RefreshModeConstant})

	mp := &mockProvider{blockNumber: 100, callResult: []*felt.Felt{new(felt.Felt).SetUint64(7)}}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())
	cancel := startPoller(t, vp, cs, st)
	defer cancel()

	// Give the goroutine ample time; a constant view must NOT keep polling.
	time.Sleep(300 * time.Millisecond)

	if got := mp.callCount.Load(); got != 1 {
		t.Fatalf("constant view should poll exactly once, got %d", got)
	}
	events, _ := st.GetUniqueEvents(context.Background(), "opttoken_get_strike", store.Query{Limit: 10})
	if len(events) == 0 {
		t.Fatal("expected the single constant read to be stored")
	}
	if vp.Status()[0].RefreshMode != config.RefreshModeConstant {
		t.Fatalf("expected status mode constant, got %q", vp.Status()[0].RefreshMode)
	}
}

// A reactive view polls once at startup, ignores non-matching triggers, and
// re-reads when a matching (event, contract) trigger fires.
func TestViewPoller_ReactiveTriggerRePoll(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	cs := reactiveViewContract(addr, "OrderBook", "get_depth",
		&config.ViewRefreshConfig{On: []string{"OrderFilled"}, Debounce: "0"})

	mp := &mockProvider{blockNumber: 100, callResult: []*felt.Felt{new(felt.Felt).SetUint64(1)}}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())
	cancel := startPoller(t, vp, cs, st)
	defer cancel()

	time.Sleep(120 * time.Millisecond)
	initial := mp.callCount.Load()
	if initial != 1 {
		t.Fatalf("expected exactly 1 startup poll, got %d", initial)
	}

	// Non-matching event name: no re-read.
	vp.TriggerView("OrderBook", addr, "SomeOtherEvent")
	time.Sleep(80 * time.Millisecond)
	if got := mp.callCount.Load(); got != initial {
		t.Fatalf("non-matching event must not trigger; calls %d -> %d", initial, got)
	}

	// Matching event but different contract address: no re-read.
	vp.TriggerView("OrderBook", new(felt.Felt).SetUint64(0x999), "OrderFilled")
	time.Sleep(80 * time.Millisecond)
	if got := mp.callCount.Load(); got != initial {
		t.Fatalf("event from another contract must not trigger; calls %d -> %d", initial, got)
	}

	// Matching event on the right contract: re-read.
	vp.TriggerView("OrderBook", addr, "OrderFilled")
	time.Sleep(120 * time.Millisecond)
	if got := mp.callCount.Load(); got <= initial {
		t.Fatalf("matching event must trigger re-read; calls %d -> %d", initial, got)
	}
}

// A transient RPC failure on the trigger-driven re-read must NOT freeze the
// view: the re-read retries with bounded backoff and eventually lands the new
// value. This is the regression guard for the prod incident where an Alchemy
// 429 dropped a get_active_deployment re-poll and the view stayed stuck on the
// (expired) prior deployment until the next rotation.
func TestViewPoller_ReactiveRePollRetriesOnTransientError(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	cs := reactiveViewContract(addr, "Mgr", "get_active_deployment",
		&config.ViewRefreshConfig{On: []string{"ActiveDeploymentChanged"}, Debounce: "0"})

	mp := &mockProvider{blockNumber: 100, callResult: []*felt.Felt{new(felt.Felt).SetUint64(1)}}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())
	cancel := startPoller(t, vp, cs, st)
	defer cancel()

	// Startup read lands at block 100.
	time.Sleep(120 * time.Millisecond)
	if got := mp.callCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 startup poll, got %d", got)
	}

	// Arm a transient failure for the upcoming re-read, and advance the chain so
	// a successful retry is observable as a new block_number.
	mp.mu.Lock()
	mp.callErr = errors.New("429 Too Many Requests")
	mp.callResult = []*felt.Felt{new(felt.Felt).SetUint64(2)}
	mp.blockNumber = 200
	mp.mu.Unlock()

	// Matching trigger: first attempt fails (429), then pollUntilSuccess backs
	// off ~1s before retrying.
	vp.TriggerView("Mgr", addr, "ActiveDeploymentChanged")
	time.Sleep(300 * time.Millisecond)

	// Clear the failure mid-backoff so the retry succeeds.
	mp.mu.Lock()
	mp.callErr = nil
	mp.mu.Unlock()

	// Wait out the ~1s backoff plus margin for attempt 2 to land.
	deadline := time.Now().Add(3 * time.Second)
	var lastBlock uint64
	for time.Now().Before(deadline) {
		events, _ := st.GetUniqueEvents(context.Background(), "mgr_get_active_deployment", store.Query{Limit: 1})
		if len(events) > 0 {
			lastBlock = events[0].BlockNumber
			if lastBlock == 200 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	if lastBlock != 200 {
		t.Fatalf("reactive re-read did not recover from transient error: view stuck at block %d, want 200", lastBlock)
	}
	if got := mp.callCount.Load(); got < 3 {
		t.Fatalf("expected the failed re-read to be retried (>=3 total calls), got %d", got)
	}
}

// Debounce collapses a burst of triggers into far fewer reads than triggers.
func TestViewPoller_ReactiveDebounceCoalesces(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	cs := reactiveViewContract(addr, "OrderBook", "get_depth",
		&config.ViewRefreshConfig{On: []string{"OrderFilled"}, Debounce: "200ms"})

	mp := &mockProvider{blockNumber: 100, callResult: []*felt.Felt{new(felt.Felt).SetUint64(1)}}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())
	cancel := startPoller(t, vp, cs, st)
	defer cancel()

	// Let the startup read settle past the debounce window.
	time.Sleep(300 * time.Millisecond)
	initial := mp.callCount.Load()

	// Fire a burst of 20 triggers well within one debounce window.
	const burst = 20
	for i := 0; i < burst; i++ {
		vp.TriggerView("OrderBook", addr, "OrderFilled")
	}

	// Wait out the throttle window plus margin.
	time.Sleep(400 * time.Millisecond)

	added := mp.callCount.Load() - initial
	if added < 1 {
		t.Fatalf("burst of %d triggers produced no read", burst)
	}
	if added >= burst {
		t.Fatalf("debounce failed to coalesce: %d reads for %d triggers", added, burst)
	}
	if added > 4 {
		t.Fatalf("expected burst to coalesce into a handful of reads, got %d", added)
	}
}

// When a reactive view's registration-time initial read gives up (a transient
// 429 storm outlasts its bounded retry), the fast recovery cadence must keep
// re-attempting and land the value within ~recoveryInterval of the RPC clearing —
// WITHOUT any matching event. This is the guard for a just-rotated order book
// whose seed orders shared the rotate block (so the event trigger can't recover
// them) and whose get_depth read was dropped during the rotation storm.
func TestViewPoller_ReactiveRecoversWhenInitialReadGivesUp(t *testing.T) {
	origAttempts, origRecovery := maxInitialPollAttempts, recoveryInterval
	maxInitialPollAttempts = 1 // give up immediately so the recovery path runs fast
	recoveryInterval = 40 * time.Millisecond
	t.Cleanup(func() { maxInitialPollAttempts = origAttempts; recoveryInterval = origRecovery })

	addr := new(felt.Felt).SetUint64(0xABC)
	cs := reactiveViewContract(addr, "OrderBook", "get_depth",
		&config.ViewRefreshConfig{On: []string{"SellOrderPlaced"}, Debounce: "0"})

	// Initial read FAILS, and no matching event is ever sent — recovery is the
	// only path to a value.
	mp := &mockProvider{
		blockNumber: 100,
		callErr:     errors.New("429 Too Many Requests"),
		callResult:  []*felt.Felt{new(felt.Felt).SetUint64(1)},
	}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())
	cancel := startPoller(t, vp, cs, st)
	defer cancel()

	// Initial read gives up; recovery ticks keep failing while the RPC is down.
	time.Sleep(150 * time.Millisecond)
	if events, _ := st.GetUniqueEvents(context.Background(), "orderbook_get_depth", store.Query{Limit: 1}); len(events) != 0 {
		t.Fatalf("view should still be empty while the RPC is failing, got %d rows", len(events))
	}

	// RPC recovers — the next recovery tick must land the value with no event.
	mp.mu.Lock()
	mp.callErr = nil
	mp.blockNumber = 200
	mp.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	var landed bool
	for time.Now().Before(deadline) {
		events, _ := st.GetUniqueEvents(context.Background(), "orderbook_get_depth", store.Query{Limit: 1})
		if len(events) > 0 && events[0].BlockNumber == 200 {
			landed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !landed {
		t.Fatal("reactive view did not recover via the fast recovery cadence after the RPC cleared")
	}

	// Recovery must stand down once populated: the call count stops climbing.
	settle := mp.callCount.Load()
	time.Sleep(200 * time.Millisecond) // several recoveryIntervals
	if got := mp.callCount.Load(); got != settle {
		t.Fatalf("recovery cadence did not stand down after first success: calls %d -> %d", settle, got)
	}
}

// processEvent must NOT trigger reactive re-reads for catchup events, but MUST
// trigger for live events.
func TestProcessEvent_ReactiveTriggerSkippedDuringCatchup(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	addr := new(felt.Felt).SetUint64(0xABC)

	eventDef := testEventDef("OrderFilled")
	// Reuse testContractState for the event side (registry + event schema),
	// then attach a reactive view for the same function/contract.
	cs := testContractState(addr, "OrderBook", []*abi.EventDef{eventDef}, types.TableTypeLog)
	funcDef := testFunctionDef("get_depth")
	cs.abi.Functions = map[string]*abi.FunctionDef{"get_depth": funcDef}
	cs.config.Views = []config.ViewConfig{{
		Function: "get_depth",
		Refresh:  &config.ViewRefreshConfig{On: []string{"OrderFilled"}, Debounce: "0"},
		Table:    config.TableConfig{Type: "unique", UniqueKey: "_view_key"},
	}}

	if err := st.CreateTable(ctx, cs.schemas["OrderFilled"]); err != nil {
		t.Fatal(err)
	}

	mp := &mockProvider{blockNumber: 100, callResult: []*felt.Felt{new(felt.Felt).SetUint64(5)}}
	vp := NewViewPoller(mp, st, noopLogger())
	viewSchemas, err := vp.Setup([]*contractState{cs})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range viewSchemas {
		if err := st.CreateTable(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	go vp.Run(pctx)

	time.Sleep(120 * time.Millisecond)
	initial := mp.callCount.Load()
	if initial != 1 {
		t.Fatalf("expected 1 startup poll, got %d", initial)
	}

	e := &Engine{
		store:        st,
		logger:       noopLogger(),
		pending:      NewPendingTracker(),
		logIndices:   make(map[uint64]uint64),
		contracts:    []*contractState{cs},
		confirmDepth: DefaultConfirmationDepth,
		poller:       vp,
	}

	sender := new(felt.Felt).SetUint64(0xDEAD)
	amount := new(felt.Felt).SetUint64(1000)

	// Catchup event → no re-read.
	catchup := makeRawEvent(eventDef.Selector, addr, 100, sender, amount)
	catchup.IsCatchup = true
	if err := e.processEvent(ctx, &catchup); err != nil {
		t.Fatalf("processEvent (catchup): %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	if got := mp.callCount.Load(); got != initial {
		t.Fatalf("catchup event must not trigger view re-read; calls %d -> %d", initial, got)
	}

	// Live event → re-read.
	live := makeRawEvent(eventDef.Selector, addr, 101, sender, amount)
	live.IsCatchup = false
	if err := e.processEvent(ctx, &live); err != nil {
		t.Fatalf("processEvent (live): %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := mp.callCount.Load(); got <= initial {
		t.Fatalf("live event must trigger view re-read; calls %d -> %d", initial, got)
	}
}
