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
)

// chunkEvent is one event at the first block of a getEvents query's range, so
// every chunk a backfill fetches yields exactly one identifiable event.
func chunkEvent(params json.RawMessage) map[string]interface{} {
	// Positional JSON-RPC params: [{"from_block":{"block_number":N}, ...}].
	var p []struct {
		FromBlock struct {
			BlockNumber uint64 `json:"block_number"`
		} `json:"from_block"`
	}
	var from uint64
	if json.Unmarshal(params, &p) == nil && len(p) == 1 {
		from = p[0].FromBlock.BlockNumber
	}
	return map[string]interface{}{
		"events": []map[string]interface{}{{
			"block_number":     from,
			"block_hash":       "0x1",
			"transaction_hash": "0x2",
			"from_address":     "0xc0ffee",
			"keys":             []string{"0x4"},
			"data":             []string{"0x5"},
		}},
	}
}

func newBackfillSub(t *testing.T, getEvents func(json.RawMessage) (interface{}, error)) (*EventSubscriber, chan RawEvent, func()) {
	t.Helper()
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) { return uint64(1000), nil },
		"starknet_getEvents":   getEvents,
		// Timestamps are resolved per block during backfill; any value will do.
		"starknet_getBlockWithTxHashes": func(_ json.RawMessage) (interface{}, error) {
			return map[string]interface{}{"timestamp": 1, "block_number": 1, "block_hash": "0x1"}, nil
		},
	}
	server := mockRPCServer(t, handlers)
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		server.Close()
		t.Fatalf("New() error: %v", err)
	}
	events := make(chan RawEvent, 256)
	sub := p.NewSubscriber(nil, events, &SubscriberConfig{
		KeysFirehose: true, OptionSelectors: []*felt.Felt{newTestFelt(0x999)},
	})
	return sub, events, func() { p.Close(); server.Close() }
}

func waitPending(t *testing.T, sub *EventSubscriber, want int64, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if sub.BackfillsPending() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("BackfillsPending = %d, want %d", sub.BackfillsPending(), want)
}

// TestBackfillHoldsCatchupIncompleteUntilDone: a contract discovered after
// startup is seeded at tip+1 and backfilled separately. Until that backfill
// lands its history is missing, so catchup_complete must be false — otherwise a
// promote gate can flip onto a slot that silently lacks a new option token.
func TestBackfillHoldsCatchupIncompleteUntilDone(t *testing.T) {
	release := make(chan struct{})
	sub, _, cleanup := newBackfillSub(t, func(params json.RawMessage) (interface{}, error) {
		<-release // hold the backfill in flight
		return chunkEvent(params), nil
	})
	defer cleanup()

	// Pretend every stream is already live, so pending is the only variable.
	sub.streamsTotal.Store(1)
	sub.streamsLive.Store(1)
	if _, _, complete := sub.TransportStatus(); !complete {
		t.Fatal("precondition: expected complete with every stream live and nothing pending")
	}

	sub.reserveBackfill()
	sub.launchReservedBackfill(context.Background(), ContractSubscription{Address: newTestFelt(0xC0FFEE)}, 900, 950)

	if _, _, complete := sub.TransportStatus(); complete {
		t.Fatal("catchup_complete true while a dynamic backfill is in flight")
	}
	if got := sub.BackfillsPending(); got != 1 {
		t.Fatalf("BackfillsPending = %d, want 1", got)
	}

	close(release)
	waitPending(t, sub, 0, 3*time.Second)
	if _, _, complete := sub.TransportStatus(); !complete {
		t.Fatal("catchup_complete still false after the backfill finished")
	}
}

// TestBackfillRetryResumesFromFailedChunk: a failed chunk is retried, and the
// retry resumes AT that chunk. Earlier chunks are already in the engine;
// restarting from the first block would deliver them again, and the engine's
// dedupe cannot be relied on to drop them.
func TestBackfillRetryResumesFromFailedChunk(t *testing.T) {
	var failedOnce atomic.Bool
	sub, events, cleanup := newBackfillSub(t, func(params json.RawMessage) (interface{}, error) {
		// blocksPerQuery is 100, so [0,350] fetches chunks starting at 0, 100,
		// 200, 300. Fail the third chunk exactly once.
		if strings.Contains(string(params), `"block_number":200`) && failedOnce.CompareAndSwap(false, true) {
			return nil, errors.New("rpc hiccup")
		}
		return chunkEvent(params), nil
	})
	defer cleanup()

	sub.reserveBackfill()
	sub.launchReservedBackfill(context.Background(), ContractSubscription{Address: newTestFelt(0xC0FFEE)}, 0, 350)
	waitPending(t, sub, 0, 5*time.Second) // retry backoff is minBackoff (1s)

	if !failedOnce.Load() {
		t.Fatal("the failing chunk was never requested — test is not exercising a retry")
	}
	var mu sync.Mutex
	seen := map[uint64]int{}
	for drained := false; !drained; {
		select {
		case e := <-events:
			mu.Lock()
			seen[e.BlockNumber]++
			mu.Unlock()
		default:
			drained = true
		}
	}
	for _, b := range []uint64{0, 100, 200, 300} {
		if seen[b] != 1 {
			t.Errorf("chunk at block %d delivered %d times, want exactly 1 (all: %v)", b, seen[b], seen)
		}
	}
}

// TestBackfillCancelledOnRemoval: a contract removed while its backfill keeps
// failing must stop retrying and release its pending count. Leaking it would
// pin catchup_complete false for the life of the process.
func TestBackfillCancelledOnRemoval(t *testing.T) {
	sub, _, cleanup := newBackfillSub(t, func(_ json.RawMessage) (interface{}, error) {
		return nil, errors.New("rpc down") // never succeeds
	})
	defer cleanup()

	addr := newTestFelt(0xC0FFEE)
	sub.reserveBackfill()
	sub.launchReservedBackfill(context.Background(), ContractSubscription{Address: addr}, 0, 350)
	if got := sub.BackfillsPending(); got != 1 {
		t.Fatalf("BackfillsPending = %d, want 1 while retrying", got)
	}

	sub.RemoveContract(addr.String())
	waitPending(t, sub, 0, 3*time.Second)
}
