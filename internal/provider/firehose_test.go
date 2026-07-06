package provider

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/rpc"
)

// fhEvent builds a firehose event from address `addr` at `block`.
func fhEvent(addr, block uint64) *rpc.EmittedEventWithFinalityStatus {
	return &rpc.EmittedEventWithFinalityStatus{
		EmittedEvent: rpc.EmittedEvent{
			Event: rpc.Event{
				FromAddress: newTestFelt(addr),
				EventContent: rpc.EventContent{
					Keys: []*felt.Felt{newTestFelt(0x1)},
					Data: []*felt.Felt{newTestFelt(0x2)},
				},
			},
			BlockNumber:     block,
			BlockHash:       newTestFelt(block * 10),
			TransactionHash: newTestFelt(block * 100),
		},
		FinalityStatus: rpc.TxnFinalityStatusAcceptedOnL2,
	}
}

func drainBlocks(ch chan RawEvent) []uint64 {
	var out []uint64
	for {
		select {
		case e := <-ch:
			out = append(out, e.BlockNumber)
		default:
			return out
		}
	}
}

func newFirehoseSub(t *testing.T, handlers map[string]func(json.RawMessage) (interface{}, error)) (*EventSubscriber, chan RawEvent, func()) {
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
	sub := p.NewSubscriber(nil, events, &SubscriberConfig{SharedFirehose: true})
	return sub, events, func() { p.Close(); server.Close() }
}

func (s *EventSubscriber) sinkLast(addr uint64) (uint64, bool) {
	s.routerMu.RLock()
	defer s.routerMu.RUnlock()
	sk := s.router[newTestFelt(addr).String()]
	if sk == nil {
		return 0, false
	}
	return sk.lastBlock, true
}

// TestFirehoseForwardIfTracked: the router forwards only tracked addresses at/after
// the sink cursor, drops untracked, and drops events below the cursor (dedup guard).
func TestFirehoseForwardIfTracked(t *testing.T) {
	sub, events, cleanup := newFirehoseSub(t, nil)
	defer cleanup()

	sub.addSink(ContractSubscription{Address: newTestFelt(0xA), StartBlock: 100}, 100)
	ctx := context.Background()

	sub.forwardIfTracked(ctx, fhEvent(0xA, 100)) // tracked, >= cursor -> forward
	sub.forwardIfTracked(ctx, fhEvent(0xB, 101)) // untracked -> drop
	sub.forwardIfTracked(ctx, fhEvent(0xA, 105)) // tracked -> forward, cursor->105
	sub.forwardIfTracked(ctx, fhEvent(0xA, 103)) // tracked but < cursor(105) -> drop

	got := drainBlocks(events)
	want := []uint64{100, 105}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("forwarded blocks = %v, want %v", got, want)
	}
	if last, ok := sub.sinkLast(0xA); !ok || last != 105 {
		t.Errorf("sink cursor = %d (ok=%v), want 105", last, ok)
	}
}

// TestFirehoseIntraBlockMultiEvents: multiple events in the SAME block for one
// contract are all forwarded (the guard is >=, not >).
func TestFirehoseIntraBlockMultiEvents(t *testing.T) {
	sub, events, cleanup := newFirehoseSub(t, nil)
	defer cleanup()

	sub.addSink(ContractSubscription{Address: newTestFelt(0xA), StartBlock: 100}, 100)
	ctx := context.Background()
	sub.forwardIfTracked(ctx, fhEvent(0xA, 100))
	sub.forwardIfTracked(ctx, fhEvent(0xA, 100))
	sub.forwardIfTracked(ctx, fhEvent(0xA, 100))

	if got := drainBlocks(events); len(got) != 3 {
		t.Fatalf("intra-block events forwarded = %d, want 3", len(got))
	}
}

// TestFirehoseRollbackSinks: reorg rollback moves cursors at/after the reorg start
// back, and leaves earlier cursors untouched.
func TestFirehoseRollbackSinks(t *testing.T) {
	sub, _, cleanup := newFirehoseSub(t, nil)
	defer cleanup()

	sub.addSink(ContractSubscription{Address: newTestFelt(0xA)}, 200) // ahead of reorg
	sub.addSink(ContractSubscription{Address: newTestFelt(0xB)}, 120) // behind reorg

	sub.rollbackSinks(150)

	if last, _ := sub.sinkLast(0xA); last != 150 {
		t.Errorf("0xA cursor = %d, want rolled back to 150", last)
	}
	if last, _ := sub.sinkLast(0xB); last != 120 {
		t.Errorf("0xB cursor = %d, want untouched 120", last)
	}
}

// TestFirehoseAddRemoveContract: AddContract seeds a sink forwarding from tip+1 and
// kicks off history backfill; RemoveContract drops it.
func TestFirehoseAddRemoveContract(t *testing.T) {
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) { return 500, nil },
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	}
	sub, _, cleanup := newFirehoseSub(t, handlers)
	defer cleanup()

	sub.AddContract(context.Background(), ContractSubscription{Address: newTestFelt(0xD), StartBlock: 490})

	last, ok := sub.sinkLast(0xD)
	if !ok {
		t.Fatal("sink 0xD not registered")
	}
	if last != 501 { // tip(500)+1
		t.Errorf("0xD cursor = %d, want 501 (tip+1)", last)
	}

	sub.RemoveContract(newTestFelt(0xD).String())
	if _, ok := sub.sinkLast(0xD); ok {
		t.Error("sink 0xD still present after RemoveContract")
	}
}

// TestFirehoseStartEndToEnd: full Start with a mock all-events WSS delivering a
// mixed firehose — only tracked-address events reach the engine channel.
func TestFirehoseStartEndToEnd(t *testing.T) {
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		// Tip within catchupThreshold of StartBlock so HTTP catchup returns
		// immediately and hands straight to the shared WSS.
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
			{Address: newTestFelt(0xA), StartBlock: 100},
			{Address: newTestFelt(0xB), StartBlock: 100},
		},
		events,
		&SubscriberConfig{SharedFirehose: true},
	)

	// One shared subscription delivering events from tracked (0xA, 0xB) and
	// untracked (0xC) addresses.
	sub.dialWSS = mockWSSDialerFunc([]*rpc.EmittedEventWithFinalityStatus{
		fhEvent(0xA, 100),
		fhEvent(0xB, 101),
		fhEvent(0xC, 102), // untracked -> must be dropped
		fhEvent(0xA, 103),
	}, nil)

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

	// Verify the three are the tracked ones and none is from 0xC.
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
