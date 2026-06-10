package engine

import (
	"context"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"

	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/store/memory"
)

// Composition of the two features: freezing a contract (RemoveContract) must
// tear down its REACTIVE views — cancel the per-entry goroutine and drop the
// entry — so a later trigger event no longer drives a poll.
func TestViewPoller_RemoveContract_StopsReactiveView(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	cs := reactiveViewContract(addr, "OrderBook", "get_depth",
		&config.ViewRefreshConfig{On: []string{"OrderFilled"}, Debounce: "0"})

	mp := &mockProvider{blockNumber: 100, callResult: []*felt.Felt{new(felt.Felt).SetUint64(1)}}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())
	schemas, err := vp.Setup([]*contractState{cs})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range schemas {
		if err := st.CreateTable(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go vp.Run(ctx)

	time.Sleep(120 * time.Millisecond)
	initial := mp.callCount.Load() // startup read
	if initial != 1 {
		t.Fatalf("expected 1 startup poll, got %d", initial)
	}

	// Freeze → tear down the contract's view polling.
	if n := vp.RemoveContract("OrderBook"); n != 1 {
		t.Fatalf("RemoveContract removed %d entries, want 1", n)
	}
	if vp.HasEntries() {
		t.Fatal("poller still has entries after RemoveContract")
	}
	time.Sleep(50 * time.Millisecond)

	// A matching trigger after freeze must not produce a poll — the reactive
	// goroutine was canceled and the entry dropped.
	vp.TriggerView("OrderBook", addr, "OrderFilled")
	time.Sleep(120 * time.Millisecond)
	if got := mp.callCount.Load(); got != initial {
		t.Fatalf("reactive view polled after freeze/RemoveContract: %d -> %d", initial, got)
	}
}
