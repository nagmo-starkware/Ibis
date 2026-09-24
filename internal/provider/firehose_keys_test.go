package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/client"
	"github.com/NethermindEth/starknet.go/rpc"
)

// newKeysFirehoseSub mirrors newFirehoseSub (firehose_test.go) but wires the
// firehose-keys (option D) transport instead of the shared single-stream
// firehose (option C).
func newKeysFirehoseSub(t *testing.T, handlers map[string]func(json.RawMessage) (interface{}, error)) (*EventSubscriber, chan RawEvent, func()) {
	t.Helper()
	if handlers == nil {
		handlers = map[string]func(json.RawMessage) (interface{}, error){}
	}
	server := mockRPCServer(t, handlers)
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		server.Close()
		t.Fatalf("New() error: %v", err)
	}
	events := make(chan RawEvent, 64)
	sub := p.NewSubscriber(nil, events, &SubscriberConfig{
		KeysFirehose:    true,
		OptionSelectors: []*felt.Felt{newTestFelt(0x999)},
	})
	return sub, events, func() { p.Close(); server.Close() }
}

// mockWSSDialerKeyed returns a wssDialer that inspects the subscription input
// to pick which event set to deliver: address-subs (input.FromAddress set)
// get addrEvents[input.FromAddress.String()]; the keys-sub (no from_address)
// gets keysSubEvents. mockWSSDialerFunc (provider_test.go) delivers the SAME
// events to every dial regardless of filter, which doesn't work once a test
// has multiple concurrently-running streams with different filters — hence
// this filter-aware variant, added here rather than changing the shared
// production dialer type or the existing single-stream mock helper.
func mockWSSDialerKeyed(keysSubEvents []*rpc.EmittedEventWithFinalityStatus, addrEvents map[string][]*rpc.EmittedEventWithFinalityStatus) wssDialer {
	return func(ctx context.Context, wsURL string, input *rpc.EventSubscriptionInput) (*wssSession, error) {
		var events []*rpc.EmittedEventWithFinalityStatus
		if input.FromAddress != nil {
			events = addrEvents[input.FromAddress.String()]
		} else {
			events = keysSubEvents
		}

		eventCh := make(chan *rpc.EmittedEventWithFinalityStatus, len(events)+1)
		errCh := make(chan error, 1)
		reorgCh := make(chan *client.ReorgEvent, 1)

		go func() {
			for _, e := range events {
				select {
				case eventCh <- e:
				case <-ctx.Done():
					return
				}
			}
			// Stay "connected" (no error, no further events) until canceled,
			// mirroring mockWSSDialerFailThenSucceed's steady-state behavior.
			<-ctx.Done()
		}()

		return &wssSession{
			events: eventCh,
			errs:   errCh,
			reorgs: reorgCh,
			close:  func() {},
		}, nil
	}
}

// TestSharedAddrWSDialedOnce verifies the multiplexing guarantee: every
// address-sub shares ONE websocket connection. Many concurrent sharedAddrWS
// callers must dial exactly once (one handshake, not N) and all receive the
// same provider instance — the property that removes the per-child 429
// handshake storm.
func TestSharedAddrWSDialedOnce(t *testing.T) {
	sub, _, cleanup := newKeysFirehoseSub(t, nil)
	defer cleanup()
	sub.wsCtx = context.Background()

	var dials int64
	sentinel := &rpc.WsProvider{}
	sub.dialSharedWS = func(_ context.Context, _ string) (*rpc.WsProvider, error) {
		atomic.AddInt64(&dials, 1)
		return sentinel, nil
	}

	const n = 64
	var wg sync.WaitGroup
	got := make([]*rpc.WsProvider, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = sub.sharedAddrWS(context.Background(), "ws://mock")
		}(i)
	}
	wg.Wait()

	if d := atomic.LoadInt64(&dials); d != 1 {
		t.Fatalf("shared address socket dialed %d times, want exactly 1", d)
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: unexpected error %v", i, errs[i])
		}
		if got[i] != sentinel {
			t.Fatalf("caller %d got a different provider; all address-subs must share one", i)
		}
	}
}

// TestSharedAddrWSDialErrorNotMemoized verifies a failed dial is not cached:
// the next caller retries rather than being stuck on the earlier failure.
func TestSharedAddrWSDialErrorNotMemoized(t *testing.T) {
	sub, _, cleanup := newKeysFirehoseSub(t, nil)
	defer cleanup()
	sub.wsCtx = context.Background()

	var dials int64
	sentinel := &rpc.WsProvider{}
	sub.dialSharedWS = func(_ context.Context, _ string) (*rpc.WsProvider, error) {
		if atomic.AddInt64(&dials, 1) == 1 {
			return nil, errors.New("boom")
		}
		return sentinel, nil
	}

	if _, err := sub.sharedAddrWS(context.Background(), "ws://mock"); err == nil {
		t.Fatal("expected first dial to error")
	}
	ws, err := sub.sharedAddrWS(context.Background(), "ws://mock")
	if err != nil {
		t.Fatalf("second dial should retry and succeed, got %v", err)
	}
	if ws != sentinel {
		t.Fatal("second dial should return the freshly dialed provider")
	}
	if d := atomic.LoadInt64(&dials); d != 2 {
		t.Fatalf("dials = %d, want 2 (a failed dial must not be memoized)", d)
	}
}

// TestForwardStreamTrackedUntrackedDedup: forwardStream forwards only tracked
// addresses at/after the STREAM's own cursor, drops untracked, and drops
// events below the cursor (dedup guard) — the per-stream analog of
// TestFirehoseForwardIfTracked in firehose_test.go.
func TestForwardStreamTrackedUntrackedDedup(t *testing.T) {
	sub, events, cleanup := newKeysFirehoseSub(t, nil)
	defer cleanup()

	sub.trackContract(ContractSubscription{Address: newTestFelt(0xA), StartBlock: 100})
	st := newFirehoseKeysStream("test-stream", nil, nil)
	ctx := context.Background()

	sub.forwardStream(ctx, st, fhEvent(0xA, 100)) // tracked, >= cursor(0) -> forward
	sub.forwardStream(ctx, st, fhEvent(0xB, 101)) // untracked -> drop
	sub.forwardStream(ctx, st, fhEvent(0xA, 105)) // tracked -> forward, cursor->105
	sub.forwardStream(ctx, st, fhEvent(0xA, 103)) // tracked but < cursor(105) -> drop

	got := drainBlocks(events)
	want := []uint64{100, 105}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("forwarded blocks = %v, want %v", got, want)
	}
	if c := st.cursor(newTestFelt(0xA).String()); c != 105 {
		t.Errorf("stream cursor = %d, want 105", c)
	}
}

// TestForwardStreamIntraBlockMultiEvents: multiple events in the SAME block
// for one address are all forwarded (the guard is <, not <=).
func TestForwardStreamIntraBlockMultiEvents(t *testing.T) {
	sub, events, cleanup := newKeysFirehoseSub(t, nil)
	defer cleanup()

	sub.trackContract(ContractSubscription{Address: newTestFelt(0xA), StartBlock: 100})
	st := newFirehoseKeysStream("test-stream", nil, nil)
	ctx := context.Background()

	sub.forwardStream(ctx, st, fhEvent(0xA, 100))
	sub.forwardStream(ctx, st, fhEvent(0xA, 100))
	sub.forwardStream(ctx, st, fhEvent(0xA, 100))

	if got := drainBlocks(events); len(got) != 3 {
		t.Fatalf("intra-block events forwarded = %d, want 3", len(got))
	}
}

// TestForwardStreamPerStreamCursorIsolation is the regression test for the
// core correctness fix option D requires over a naive port of option C's
// single shared cursor: an OptionToken child's events split across TWO
// streams (the keys-sub, for its non-Transfer events; its own
// Transfer/Approval address-sub, for the rest). If the streams shared ONE
// per-address cursor (as firehoseSink does for option C), whichever stream
// ran ahead would advance the shared cursor and the OTHER stream's
// still-valid, still-unforwarded events would be silently dropped by the "<
// cursor" dedup guard.
//
// Here, streamA (simulating the child's faster address-sub) races ahead to
// block 500 for address 0xA. streamB (simulating the slower keys-sub,
// covering the SAME address) must still accept and forward its own event at
// block 50, because its OWN cursor for 0xA is untouched by streamA's advance
// — proving the two streams do not share cursor state.
func TestForwardStreamPerStreamCursorIsolation(t *testing.T) {
	sub, events, cleanup := newKeysFirehoseSub(t, nil)
	defer cleanup()

	addr := newTestFelt(0xA)
	sub.trackContract(ContractSubscription{Address: addr, StartBlock: 0})

	streamA := newFirehoseKeysStream("stream-A (fast)", addr, nil)
	streamB := newFirehoseKeysStream("stream-B (slow, same address)", nil, nil)
	ctx := context.Background()

	// Stream A races ahead on address 0xA.
	sub.forwardStream(ctx, streamA, fhEvent(0xA, 500))
	if c := streamA.cursor(addr.String()); c != 500 {
		t.Fatalf("stream A cursor = %d, want 500", c)
	}

	// Stream B's OWN cursor for the same address must be untouched.
	if c := streamB.cursor(addr.String()); c != 0 {
		t.Fatalf("stream B cursor = %d, want 0 (must not be affected by stream A)", c)
	}

	// Stream B forwards a LOWER block for the same address. If the streams
	// shared one cursor, this would be wrongly dropped as "< 500".
	sub.forwardStream(ctx, streamB, fhEvent(0xA, 50))

	got := drainBlocks(events)
	want := []uint64{500, 50}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("forwarded blocks = %v, want %v (per-stream cursor isolation broken: "+
			"stream B's block-50 event must NOT be dropped because of stream A's advance)", got, want)
	}
}

// TestRollbackAllStreams: a reorg observed on any one stream rolls back
// cursors across EVERY registered stream (keys-sub + every address-sub),
// leaving cursors before the reorg start untouched.
func TestRollbackAllStreams(t *testing.T) {
	sub, _, cleanup := newKeysFirehoseSub(t, nil)
	defer cleanup()

	addrA := newTestFelt(0xA).String()
	addrB := newTestFelt(0xB).String()

	sub.streamsMu.Lock()
	sub.keysStream = newFirehoseKeysStream("keys-sub", nil, nil)
	sub.keysStream.setCursor(addrA, 200, true) // ahead of reorg
	tokenStream := newFirehoseKeysStream("token", newTestFelt(0xB), nil)
	tokenStream.setCursor(addrB, 120, true) // behind reorg
	sub.addrStreams[addrB] = tokenStream
	sub.streamsMu.Unlock()

	sub.rollbackAllStreams(150)

	if c := sub.keysStream.cursor(addrA); c != 150 {
		t.Errorf("keys-sub cursor = %d, want rolled back to 150", c)
	}
	if c := tokenStream.cursor(addrB); c != 120 {
		t.Errorf("token stream cursor = %d, want untouched 120", c)
	}
}

// TestAddRemoveContractKeysFirehoseChild: AddContract on an ERC20 wildcard
// child (an OptionToken) seeds the keys-sub's fill cursor at tip+1 AND
// creates its own Transfer/Approval address-sub stream; RemoveContract tears
// both down (untracks it and cancels/removes the child stream).
func TestAddRemoveContractKeysFirehoseChild(t *testing.T) {
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) { return 500, nil },
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	}
	sub, _, cleanup := newKeysFirehoseSub(t, handlers)
	defer cleanup()

	// Seed the keys-sub the way startKeysFirehose would (Start isn't called
	// in this test — only AddContract's behavior is under test).
	sub.streamsMu.Lock()
	sub.keysStream = newFirehoseKeysStream("keys-sub", nil, [][]*felt.Felt{sub.optionSelectors})
	sub.streamsMu.Unlock()

	childAddr := newTestFelt(0xD)
	ctx := context.Background()
	sub.AddContract(ctx, ContractSubscription{
		Address: childAddr, StartBlock: 490, Wildcard: true, ERC20: true,
	})

	if c := sub.keysStream.cursor(childAddr.String()); c != 501 { // tip(500)+1
		t.Errorf("keys-sub fill cursor for child = %d, want 501 (tip+1)", c)
	}
	if !sub.isTracked(childAddr.String()) {
		t.Error("child not tracked after AddContract")
	}
	sub.streamsMu.Lock()
	_, hasStream := sub.addrStreams[childAddr.String()]
	sub.streamsMu.Unlock()
	if !hasStream {
		t.Fatal("expected a child Transfer/Approval address-sub stream to be created")
	}

	sub.RemoveContract(childAddr.String())

	if sub.isTracked(childAddr.String()) {
		t.Error("child still tracked after RemoveContract")
	}
	sub.streamsMu.Lock()
	_, stillHasStream := sub.addrStreams[childAddr.String()]
	sub.streamsMu.Unlock()
	if stillHasStream {
		t.Error("child Transfer/Approval stream still registered after RemoveContract")
	}
}

// TestKeysFirehoseStartEndToEnd: a full Start() with mock sessions — the
// keys-sub delivers option-family events from a tracked address and an
// untracked/foreign address (only the tracked one is forwarded), while a
// separate token address-sub delivers events for a static ERC20 contract.
func TestKeysFirehoseStartEndToEnd(t *testing.T) {
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		// Tip within catchupThreshold of StartBlock so HTTP catchup returns
		// immediately and hands straight to WSS.
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) { return 120, nil },
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	}
	server := mockRPCServer(t, handlers)
	defer server.Close()
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer p.Close()

	events := make(chan RawEvent, 64)
	optionSel := newTestFelt(0x999)
	sub := p.NewSubscriber(
		[]ContractSubscription{
			{Address: newTestFelt(0xA), StartBlock: 100, Wildcard: true},  // option-family: keys-sub only
			{Address: newTestFelt(0xB), StartBlock: 100, Wildcard: false}, // static token: own address-sub
		},
		events,
		&SubscriberConfig{KeysFirehose: true, OptionSelectors: []*felt.Felt{optionSel}},
	)

	sub.dialWSS = mockWSSDialerKeyed(
		// keys-sub (chain-wide): events from tracked 0xA and untracked/foreign 0xC.
		[]*rpc.EmittedEventWithFinalityStatus{
			fhEvent(0xA, 100),
			fhEvent(0xC, 101), // untracked -> must be dropped
			fhEvent(0xA, 103),
		},
		// address-sub for 0xB: its own event stream.
		map[string][]*rpc.EmittedEventWithFinalityStatus{
			newTestFelt(0xB).String(): {
				fhEvent(0xB, 100),
			},
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go sub.Start(ctx)

	got := make([]RawEvent, 0, 3)
	for len(got) < 3 {
		select {
		case e := <-events:
			got = append(got, e)
		case <-ctx.Done():
			t.Fatalf("timed out; got %d tracked events, want 3", len(got))
		}
	}

	untracked := newTestFelt(0xC).String()
	for _, e := range got {
		if e.ContractAddress.String() == untracked {
			t.Errorf("untracked address 0xC leaked through the router")
		}
	}
	// No 4th (untracked) event should arrive shortly after.
	select {
	case e := <-events:
		t.Errorf("unexpected extra event from %s @ block %d", e.ContractAddress, e.BlockNumber)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestKeysStreamGapFillConvergesOnTip: a gap-fill pass returns only when its
// SLOWEST fill finishes, and with a large fill set that wait outlasts the
// node's WSS replay window. Fills that finished early are stale by then, so
// resuming from the min cursor asks the node to replay blocks it no longer
// offers -- every event in between is dropped silently and permanently. The
// fan-out must repeat until the whole set is within catchupThreshold of the
// CURRENT tip.
func TestKeysStreamGapFillConvergesOnTip(t *testing.T) {
	var tip atomic.Uint64
	tip.Store(1000)
	var getEventsCalls atomic.Int64

	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			return tip.Load(), nil
		},
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			// The chain advances while the slow fill grinds through its backlog,
			// then settles so catchup can converge. The first call is exempt so
			// the fast fill deterministically observes the starting tip and
			// finishes before the chain moves.
			if n := getEventsCalls.Add(1); n >= 2 && n <= 13 {
				tip.Add(50)
			}
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	}
	sub, _, cleanup := newKeysFirehoseSub(t, handlers)
	defer cleanup()

	// KeysFirehose implies sharedTipPoller, whose cache would mask the chain
	// moving for 4x the tip interval. Expire it so every read is fresh.
	sub.provider.tipIntervalNanos.Store(1)

	st := newFirehoseKeysStream("keys-sub", nil, [][]*felt.Felt{{newTestFelt(0x999)}})

	// fast: already within catchupThreshold of the tip, so it finishes at once
	// and then sits idle inside the barrier while the chain moves out from under
	// its cursor.
	fast := newTestFelt(0xFA51)
	st.setFill(fast.String(), ContractSubscription{Address: fast})
	st.setCursor(fast.String(), 950, true)

	// slow: a deep backlog, so it is what holds the barrier open.
	slow := newTestFelt(0x5104)
	st.setFill(slow.String(), ContractSubscription{Address: slow})
	st.setCursor(slow.String(), 0, true)

	resume, err := sub.keysStreamGapFill(context.Background(), st)
	if err != nil {
		t.Fatalf("keysStreamGapFill returned an error on a converging chain: %v", err)
	}

	final := tip.Load()
	if resume+catchupThreshold < final {
		t.Fatalf("resume block %d is %d behind tip %d; want within %d "+
			"(a resume this old falls outside the WSS replay window and loses events)",
			resume, final-resume, final, catchupThreshold)
	}
}

// TestKeysStreamGapFillNeverReturnsAStaleResume: on any failure the caller must
// get an error and a zero block, never a usable-looking resume. Handing back
// the block reached so far is what the old code did on an unreadable tip, and
// resuming from it subscribes past the node's replay window -- a permanent hole
// in the store, written silently.
func TestKeysStreamGapFillNeverReturnsAStaleResume(t *testing.T) {
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			return uint64(500_000), nil
		},
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	}
	sub, _, cleanup := newKeysFirehoseSub(t, handlers)
	defer cleanup()

	st := newFirehoseKeysStream("keys-sub", nil, [][]*felt.Felt{{newTestFelt(0x999)}})
	addr := newTestFelt(0xBEEF)
	st.setFill(addr.String(), ContractSubscription{Address: addr})
	st.setCursor(addr.String(), 1, true) // a long way behind: the pass will be mid-flight

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var resume uint64
	var err error
	go func() {
		defer close(done)
		resume, err = sub.keysStreamGapFill(ctx, st)
	}()

	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("keysStreamGapFill did not return after cancellation")
	}

	if err == nil {
		t.Fatalf("expected an error after cancellation, got resume=%d and nil", resume)
	}
	if resume != 0 {
		t.Errorf("resume = %d on failure; must be 0 so no caller can subscribe with it", resume)
	}
}

// TestKeysStreamGapFillToleratesFillSetGrowth: addContractKeysFirehose registers
// children mid-flight, each seeded at its deploy block. A pass that picks one up
// does more work, runs longer, and leaves the early finishers staler — so its
// gap GROWS for an entirely healthy reason. The divergence guard must not read
// that as "catchup is losing to the chain", or an indexer that merely discovered
// a new option token would tear down its own keys-sub.
func TestKeysStreamGapFillToleratesFillSetGrowth(t *testing.T) {
	var tip atomic.Uint64
	tip.Store(1000)
	var calls atomic.Int64

	st := newFirehoseKeysStream("keys-sub", nil, [][]*felt.Felt{{newTestFelt(0x999)}})
	late := newTestFelt(0x1A7E)

	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			return tip.Load(), nil
		},
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			n := calls.Add(1)
			// A child is discovered while pass 1 is running. Pass 1 already took
			// its snapshot, so the child first appears in pass 2 — at block 0.
			if n == 3 {
				st.setFill(late.String(), ContractSubscription{Address: late})
				st.setCursor(late.String(), 0, true)
			}
			// The chain moves through passes 1 and 2, then settles so pass 3 can
			// converge. The first call is exempt so the fast fill sees the start.
			if n >= 2 && n <= 80 {
				tip.Add(50)
			}
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	}
	sub, _, cleanup := newKeysFirehoseSub(t, handlers)
	defer cleanup()
	sub.provider.tipIntervalNanos.Store(1) // serve every tip read fresh

	fast := newTestFelt(0xFA51)
	st.setFill(fast.String(), ContractSubscription{Address: fast})
	st.setCursor(fast.String(), 950, true)
	slow := newTestFelt(0x5104)
	st.setFill(slow.String(), ContractSubscription{Address: slow})
	st.setCursor(slow.String(), 0, true)

	resume, err := sub.keysStreamGapFill(context.Background(), st)
	if err != nil {
		t.Fatalf("gap-fill failed after a child joined mid-flight — the set grew, so the "+
			"larger gap is expected and must not count as divergence: %v", err)
	}
	if final := tip.Load(); resume+catchupThreshold < final {
		t.Fatalf("resume block %d is %d behind tip %d; want within %d",
			resume, final-resume, final, catchupThreshold)
	}
}

// TestGapFillGuardDecisions pins the two comparisons that decide whether a
// finished gap-fill pass is diverging (fail), regrew (excuse and log), or is
// merely still catching up (repeat). A flipped comparison here would either let
// a stream spin forever or take down a healthy one, and no end-to-end test can
// catch it: when catchup genuinely loses to the chain it loses INSIDE a pass, so
// this guard only ever sees the finished-but-regressed case.
func TestGapFillGuardDecisions(t *testing.T) {
	cases := []struct {
		name              string
		pass              int
		fills, prevFills  int
		behind, prevBehnd uint64
		wantDiverging     bool
		wantRegrew        bool
	}{
		{"first pass never diverges", 1, 5, 0, 10_000, 0, false, false},
		{"stable set, gap shrank: catching up", 2, 5, 5, 400, 900, false, false},
		{"stable set, gap grew: diverging", 2, 5, 5, 3000, 900, true, false},
		{"stable set, gap equal: diverging (no progress)", 2, 5, 5, 900, 900, true, false},
		{"set grew, gap grew: excused, logged", 2, 6, 5, 3000, 900, false, true},
		{"set grew, gap shrank: plain catch-up", 2, 6, 5, 400, 900, false, false},
		{"set shrank, gap grew: diverging (less work, still losing)", 3, 4, 5, 3000, 900, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := gapFillDiverging(c.pass, c.fills, c.prevFills, c.behind, c.prevBehnd); got != c.wantDiverging {
				t.Errorf("gapFillDiverging = %v, want %v", got, c.wantDiverging)
			}
			if got := gapFillRegrew(c.pass, c.fills, c.prevFills, c.behind, c.prevBehnd); got != c.wantRegrew {
				t.Errorf("gapFillRegrew = %v, want %v", got, c.wantRegrew)
			}
		})
	}
}

// TestKeysStreamGapFillErrorsOnUnreadableTip: the convergence check is the one
// read that decides it is safe to subscribe. If the tip cannot be read it must
// fail — not fall back to a cached tip, which CachedBlockNumber hands back with
// a nil error however stale it is, and a stale low tip makes a lagging cursor
// look converged.
func TestKeysStreamGapFillErrorsOnUnreadableTip(t *testing.T) {
	var blockNumberCalls atomic.Int64
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			// First read (inside the pass, via the cache) succeeds and primes it.
			// Every later read — including the convergence check — fails.
			if blockNumberCalls.Add(1) == 1 {
				return uint64(1000), nil
			}
			return nil, errors.New("rpc unavailable")
		},
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	}
	sub, _, cleanup := newKeysFirehoseSub(t, handlers)
	defer cleanup()

	st := newFirehoseKeysStream("keys-sub", nil, [][]*felt.Felt{{newTestFelt(0x999)}})
	addr := newTestFelt(0xBEEF)
	st.setFill(addr.String(), ContractSubscription{Address: addr})
	st.setCursor(addr.String(), 980, true) // within catchupThreshold of 1000

	resume, err := sub.keysStreamGapFill(context.Background(), st)
	if err == nil {
		t.Fatalf("expected an error when the tip cannot be read, got resume=%d — "+
			"a cached tip must not stand in for the convergence check", resume)
	}
	if !strings.Contains(err.Error(), "reading chain tip") {
		t.Errorf("error = %q; want the tip-read error", err)
	}
	if resume != 0 {
		t.Errorf("resume = %d on failure; must be 0", resume)
	}
}

// TestKeysStreamGapFillEmptySetConverges: with nothing to fill, the pass's
// cursor is just the cached tip. A stale cache must not read as divergence —
// resume from the cached tip, the one new contracts are seeded from.
func TestKeysStreamGapFillEmptySetConverges(t *testing.T) {
	var calls atomic.Int64
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			// The first read primes the cache at 1000; the chain is really at 5000.
			if calls.Add(1) == 1 {
				return uint64(1000), nil
			}
			return uint64(5000), nil
		},
	}
	sub, _, cleanup := newKeysFirehoseSub(t, handlers)
	defer cleanup()

	st := newFirehoseKeysStream("keys-sub", nil, [][]*felt.Felt{{newTestFelt(0x999)}})
	resume, err := sub.keysStreamGapFill(context.Background(), st)
	if err != nil {
		t.Fatalf("empty fill set errored: %v", err)
	}
	if resume != 1000 {
		t.Errorf("resume = %d, want the cached tip 1000", resume)
	}
}

// TestKeysStreamGapFillEmptySetFillsLateContract: a contract registered after
// an empty pass took its snapshot must still be covered from its own cursor,
// not skipped by resuming at a tip past it.
func TestKeysStreamGapFillEmptySetFillsLateContract(t *testing.T) {
	st := newFirehoseKeysStream("keys-sub", nil, [][]*felt.Felt{{newTestFelt(0x999)}})
	late := newTestFelt(0x1A7E)
	var tipCalls atomic.Int64
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			// Registered during the first pass's tip read, after its snapshot,
			// seeded below that tip.
			if tipCalls.Add(1) == 1 {
				st.setFill(late.String(), ContractSubscription{Address: late})
				st.setCursor(late.String(), 990, true)
			}
			return uint64(1000), nil
		},
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	}
	sub, _, cleanup := newKeysFirehoseSub(t, handlers)
	defer cleanup()

	resume, err := sub.keysStreamGapFill(context.Background(), st)
	if err != nil {
		t.Fatalf("gap-fill errored: %v", err)
	}
	if resume > 990 {
		t.Fatalf("resume = %d, past the late contract's cursor 990: its blocks would be skipped", resume)
	}
}

// TestKeysStreamGapFillConvergedCoversLateContract: the same for a non-empty
// set that converges on its first pass — a contract that joined after the
// snapshot must not be left behind the resume block.
func TestKeysStreamGapFillConvergedCoversLateContract(t *testing.T) {
	st := newFirehoseKeysStream("keys-sub", nil, [][]*felt.Felt{{newTestFelt(0x999)}})
	late := newTestFelt(0x1A7E)
	var tipCalls atomic.Int64
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			// The first read is inside the pass, after its snapshot.
			if tipCalls.Add(1) == 1 {
				st.setFill(late.String(), ContractSubscription{Address: late})
				st.setCursor(late.String(), 980, true)
			}
			return uint64(1000), nil
		},
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	}
	sub, _, cleanup := newKeysFirehoseSub(t, handlers)
	defer cleanup()

	known := newTestFelt(0xFA51)
	st.setFill(known.String(), ContractSubscription{Address: known})
	st.setCursor(known.String(), 995, true)

	resume, err := sub.keysStreamGapFill(context.Background(), st)
	if err != nil {
		t.Fatalf("gap-fill errored: %v", err)
	}
	if resume > 980 {
		t.Fatalf("resume = %d, past the late contract's cursor 980: its blocks would be skipped", resume)
	}
}

// TestKeysStreamGapFillWaitsForInFlightSeed: a contract mid-registration
// (tip read, not yet seeded) holds seedMu, so the resume decision waits for it
// instead of resuming past its cursor.
func TestKeysStreamGapFillWaitsForInFlightSeed(t *testing.T) {
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) { return uint64(1000), nil },
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	}
	sub, _, cleanup := newKeysFirehoseSub(t, handlers)
	defer cleanup()

	st := newFirehoseKeysStream("keys-sub", nil, [][]*felt.Felt{{newTestFelt(0x999)}})
	known := newTestFelt(0xFA51)
	st.setFill(known.String(), ContractSubscription{Address: known})
	st.setCursor(known.String(), 995, true)

	st.seedMu.RLock() // a registration is between its tip read and its seed
	type result struct {
		resume uint64
		err    error
	}
	done := make(chan result, 1)
	go func() {
		r, err := sub.keysStreamGapFill(context.Background(), st)
		done <- result{r, err}
	}()
	select {
	case r := <-done:
		st.seedMu.RUnlock()
		t.Fatalf("resume %d decided while a seed was in flight", r.resume)
	case <-time.After(100 * time.Millisecond):
	}
	late := newTestFelt(0x1A7E)
	st.setFill(late.String(), ContractSubscription{Address: late})
	st.setCursor(late.String(), 980, true)
	st.seedMu.RUnlock()

	r := <-done
	if r.err != nil {
		t.Fatalf("gap-fill errored: %v", r.err)
	}
	if r.resume > 980 {
		t.Fatalf("resume = %d, past the just-seeded cursor 980", r.resume)
	}
}

// TestReadTipAndSeedHoldsSeedMu: the add path's tip read must happen under
// seedMu, or a resume decision can slip in between it and the seed.
func TestReadTipAndSeedHoldsSeedMu(t *testing.T) {
	st := newFirehoseKeysStream("keys-sub", nil, [][]*felt.Felt{{newTestFelt(0x999)}})
	var unguarded atomic.Bool
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			if st.seedMu.TryLock() {
				unguarded.Store(true)
				st.seedMu.Unlock()
			}
			return uint64(1000), nil
		},
	}
	sub, _, cleanup := newKeysFirehoseSub(t, handlers)
	defer cleanup()
	sub.streamsMu.Lock()
	sub.keysStream = st
	sub.streamsMu.Unlock()

	addr := newTestFelt(0x1A7E)
	if _, _, err := sub.readTipAndSeed(context.Background(), ContractSubscription{Address: addr}); err != nil {
		t.Fatalf("readTipAndSeed: %v", err)
	}
	if unguarded.Load() {
		t.Fatal("tip read ran without seedMu held")
	}
	if got := st.cursor(addr.String()); got != 1001 {
		t.Errorf("seeded cursor = %d, want 1001", got)
	}
}

// TestReadTipAndSeedRegistrationsRunConcurrently: registrations share seedMu,
// so one slow tip read does not queue every other registration behind it.
func TestReadTipAndSeedRegistrationsRunConcurrently(t *testing.T) {
	var inside, peak atomic.Int64
	bothIn := make(chan struct{})
	var once sync.Once
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			n := inside.Add(1)
			defer inside.Add(-1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			if n == 2 {
				once.Do(func() { close(bothIn) })
			}
			select { // hold each read until both are inside at once
			case <-bothIn:
			case <-time.After(time.Second):
			}
			return uint64(1000), nil
		},
	}
	sub, _, cleanup := newKeysFirehoseSub(t, handlers)
	defer cleanup()
	sub.provider.tipIntervalNanos.Store(1) // every read goes to the RPC
	sub.streamsMu.Lock()
	sub.keysStream = newFirehoseKeysStream("keys-sub", nil, [][]*felt.Felt{{newTestFelt(0x999)}})
	sub.streamsMu.Unlock()

	var wg sync.WaitGroup
	for _, a := range []uint64{0xA1, 0xA2} {
		wg.Add(1)
		go func(a uint64) {
			defer wg.Done()
			_, _, _ = sub.readTipAndSeed(context.Background(), ContractSubscription{Address: newTestFelt(a)})
		}(a)
	}
	wg.Wait()
	if peak.Load() < 2 {
		t.Fatal("the two registrations' tip reads never overlapped: they are serialized")
	}
}

// TestTransportStatusTracksStreamLiveness: readiness and cursor numbers both
// report healthy while a stream is still gap-filling, which is how a standby
// gets promoted before it is current. TransportStatus must distinguish the two:
// a stream counts as live only once it holds a WSS subscription.
func TestTransportStatusTracksStreamLiveness(t *testing.T) {
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) { return 120, nil },
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	}
	server := mockRPCServer(t, handlers)
	defer server.Close()
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer p.Close()

	events := make(chan RawEvent, 64)
	sub := p.NewSubscriber(
		[]ContractSubscription{
			{Address: newTestFelt(0xA), StartBlock: 100, Wildcard: true},  // keys-sub only
			{Address: newTestFelt(0xB), StartBlock: 100, Wildcard: false}, // its own address-sub
		},
		events,
		&SubscriberConfig{KeysFirehose: true, OptionSelectors: []*felt.Felt{newTestFelt(0x999)}},
	)
	sub.dialWSS = mockWSSDialerKeyed(nil, nil)

	if live, total, complete := sub.TransportStatus(); live != 0 || total != 0 || complete {
		t.Fatalf("before start: live=%d total=%d complete=%v, want 0/0/false", live, total, complete)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go sub.Start(ctx)

	// keys-sub + one address-sub for the static token.
	const wantStreams = 2
	deadline := time.After(2 * time.Second)
	for {
		live, total, complete := sub.TransportStatus()
		if total == wantStreams && live == wantStreams && complete {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("streams never went live: live=%d total=%d, want %d/%d",
				live, total, wantStreams, wantStreams)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// newLivenessSub builds the two-stream setup used by the counter tests: one
// option-family contract (keys-sub only) and one static token (its own
// address-sub), with the tip within catchupThreshold so gap-fill is immediate.
func newLivenessSub(t *testing.T, blockNumber func() (interface{}, error)) (*EventSubscriber, func()) {
	t.Helper()
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) { return blockNumber() },
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	}
	server := mockRPCServer(t, handlers)
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		server.Close()
		t.Fatalf("New() error: %v", err)
	}
	sub := p.NewSubscriber(
		[]ContractSubscription{
			{Address: newTestFelt(0xA), StartBlock: 100, Wildcard: true},
			{Address: newTestFelt(0xB), StartBlock: 100, Wildcard: false},
		},
		make(chan RawEvent, 64),
		&SubscriberConfig{KeysFirehose: true, OptionSelectors: []*felt.Felt{newTestFelt(0x999)}},
	)
	return sub, func() { p.Close(); server.Close() }
}

// TestTransportStatusNeverCompleteBeforeAllStreamsCounted: a stream must be
// counted before it can go live. When the count was taken inside each
// goroutine, the keys-sub — launched only after the per-contract loop — could
// be uncounted while token streams were already live, reading as
// live == total > 0 with the most important stream missing.
func TestTransportStatusNeverCompleteBeforeAllStreamsCounted(t *testing.T) {
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) { return uint64(120), nil },
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	}
	server := mockRPCServer(t, handlers)
	defer server.Close()
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer p.Close()

	// Token stream FIRST, then a wide set of option-family contracts — the shape
	// of prod, where the per-contract loop covers thousands of contracts and the
	// keys-sub is launched only after it. That loop is the window: the token
	// stream can go live long before the keys-sub would have counted itself.
	contracts := []ContractSubscription{{Address: newTestFelt(0xB), StartBlock: 100, Wildcard: false}}
	for i := uint64(0); i < 5000; i++ {
		contracts = append(contracts, ContractSubscription{Address: newTestFelt(0x100000 + i), StartBlock: 100, Wildcard: true})
	}
	sub := p.NewSubscriber(contracts, make(chan RawEvent, 64),
		&SubscriberConfig{KeysFirehose: true, OptionSelectors: []*felt.Felt{newTestFelt(0x999)}})
	sub.dialWSS = mockWSSDialerKeyed(nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go sub.Start(ctx)

	const want = 2
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		live, total, complete := sub.TransportStatus()
		if live > total {
			t.Fatalf("live=%d > total=%d: a stream went live before it was counted", live, total)
		}
		if complete && total != want {
			t.Fatalf("catchup_complete with total=%d, want %d: reported complete while a stream was still uncounted", total, want)
		}
		if complete {
			return
		}
		time.Sleep(50 * time.Microsecond)
	}
	t.Fatal("streams never went live")
}

// TestTransportStatusDropsOnReconnect: a stream is live only while it holds a
// session. When the session ends, catchup_complete must drop until the stream
// has re-run its gap-fill and resubscribed — otherwise a reconnect loop would
// look healthy to a promote gate.
func TestTransportStatusDropsOnReconnect(t *testing.T) {
	sub, cleanup := newLivenessSub(t, func() (interface{}, error) { return uint64(120), nil })
	defer cleanup()

	var keysDials atomic.Int64
	steady := mockWSSDialerKeyed(nil, nil)
	sub.dialWSS = func(ctx context.Context, wsURL string, in *rpc.EventSubscriptionInput) (*wssSession, error) {
		if in.FromAddress == nil && keysDials.Add(1) == 1 {
			// First keys-sub session drops shortly after connecting.
			errCh := make(chan error, 1)
			go func() { time.Sleep(200 * time.Millisecond); errCh <- errors.New("session dropped") }()
			return &wssSession{
				events: make(chan *rpc.EmittedEventWithFinalityStatus),
				errs:   errCh,
				reorgs: make(chan *client.ReorgEvent),
				close:  func() {},
			}, nil
		}
		return steady(ctx, wsURL, in)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	go sub.Start(ctx)

	// live, then not-live with total unchanged, then live again.
	stage := 0
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) && stage < 3 {
		live, total, complete := sub.TransportStatus()
		switch {
		case stage == 0 && complete:
			stage = 1
		case stage == 1 && !complete:
			if total != 2 {
				t.Fatalf("total=%d during reconnect, want 2: a reconnecting stream is still a stream", total)
			}
			if live >= total {
				t.Fatalf("live=%d total=%d during reconnect; the dropped stream must not count as live", live, total)
			}
			stage = 2
		case stage == 2 && complete:
			stage = 3
		}
		time.Sleep(time.Millisecond)
	}
	if stage != 3 {
		t.Fatalf("reached stage %d of 3 (0 live, 1 dropped, 2 recovered)", stage)
	}
}

// TestTransportStatusNotLiveWhileGapFillFails: a stream whose gap-fill keeps
// failing backs off and retries without subscribing. It must stay counted but
// never read as live — this is the state that used to be invisible, and the one
// a promote gate exists to block on.
func TestTransportStatusNotLiveWhileGapFillFails(t *testing.T) {
	var calls atomic.Int64
	sub, cleanup := newLivenessSub(t, func() (interface{}, error) {
		if calls.Add(1) == 1 {
			return uint64(120), nil // primes the cache the passes read from
		}
		return nil, errors.New("rpc unavailable") // every convergence check fails
	})
	defer cleanup()
	sub.dialWSS = mockWSSDialerKeyed(nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go sub.Start(ctx)

	sawBoth := false
	for end := time.Now().Add(1500 * time.Millisecond); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		live, total, complete := sub.TransportStatus()
		if complete || live > 0 {
			t.Fatalf("live=%d total=%d complete=%v while every gap-fill is failing; nothing may read as live", live, total, complete)
		}
		if total == 2 {
			sawBoth = true
		}
	}
	if !sawBoth {
		t.Error("never saw both retrying streams counted in total")
	}
}

// TestTransportStatusReleasesRemovedStream: removing a contract tears its stream
// down, and its reservation must go with it — a leaked count would hold
// catchup_complete false for good.
func TestTransportStatusReleasesRemovedStream(t *testing.T) {
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) { return uint64(500), nil },
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	}
	sub, _, cleanup := newKeysFirehoseSub(t, handlers)
	defer cleanup()
	sub.dialWSS = mockWSSDialerKeyed(nil, nil)
	sub.streamsMu.Lock()
	sub.keysStream = newFirehoseKeysStream("keys-sub", nil, [][]*felt.Felt{sub.optionSelectors})
	sub.streamsMu.Unlock()

	child := newTestFelt(0xD)
	sub.AddContract(context.Background(), ContractSubscription{
		Address: child, StartBlock: 490, Wildcard: true, ERC20: true,
	})
	if _, total, _ := sub.TransportStatus(); total != 1 {
		t.Fatalf("total=%d after adding an ERC20 child, want 1 (its Transfer stream)", total)
	}

	sub.RemoveContract(child.String())
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, total, _ := sub.TransportStatus(); total == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, total, _ := sub.TransportStatus()
	t.Fatalf("total=%d after RemoveContract, want 0: the removed stream's count leaked", total)
}
