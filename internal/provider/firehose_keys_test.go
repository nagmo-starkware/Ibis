package provider

import (
	"context"
	"encoding/json"
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
