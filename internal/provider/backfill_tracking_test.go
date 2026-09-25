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
	return map[string]interface{}{
		"events": []map[string]interface{}{{
			"block_number":     fromBlockOf(params),
			"block_hash":       "0x1",
			"transaction_hash": "0x2",
			"from_address":     "0xc0ffee",
			"keys":             []string{"0x4"},
			"data":             []string{"0x5"},
		}},
	}
}

// fromBlockOf reads from_block out of a getEvents query's params.
func fromBlockOf(params json.RawMessage) uint64 {
	// Positional JSON-RPC params: [{"from_block":{"block_number":N}, ...}].
	var p []struct {
		FromBlock struct {
			BlockNumber uint64 `json:"block_number"`
		} `json:"from_block"`
	}
	if json.Unmarshal(params, &p) == nil && len(p) == 1 {
		return p[0].FromBlock.BlockNumber
	}
	return 0
}

func newBackfillSub(t *testing.T, getEvents func(json.RawMessage) (interface{}, error)) (*EventSubscriber, chan RawEvent, func()) {
	t.Helper()
	return newBackfillSubWith(t, &SubscriberConfig{
		KeysFirehose: true, OptionSelectors: []*felt.Felt{newTestFelt(0x999)},
	}, getEvents)
}

func newBackfillSubWith(t *testing.T, cfg *SubscriberConfig, getEvents func(json.RawMessage) (interface{}, error)) (*EventSubscriber, chan RawEvent, func()) {
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
	sub := p.NewSubscriber(nil, events, cfg)
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
	// Unblock the held handler even on failure, or cleanup's server.Close hangs.
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()

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

	unblock()
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

// TestBackfillReAddSupersedesInFlight: re-adding a contract cancels its earlier
// backfill, and that attempt's cleanup must not delete the newer entry — else
// RemoveContract could no longer cancel it. Each attempt releases exactly once.
func TestBackfillReAddSupersedesInFlight(t *testing.T) {
	release := make(chan struct{})
	sub, _, cleanup := newBackfillSub(t, func(params json.RawMessage) (interface{}, error) {
		if fromBlockOf(params) < 500 {
			return nil, errors.New("rpc down") // first attempt retries until cancelled
		}
		<-release // hold the second attempt in flight
		return chunkEvent(params), nil
	})
	defer cleanup()
	// Unblock the held handler even on failure, or cleanup's server.Close hangs.
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()

	addr := newTestFelt(0xC0FFEE)
	sub.reserveBackfill()
	sub.launchReservedBackfill(context.Background(), ContractSubscription{Address: addr}, 0, 350)
	sub.reserveBackfill()
	sub.launchReservedBackfill(context.Background(), ContractSubscription{Address: addr}, 500, 550)

	// The superseded attempt exits and releases; the second is still running.
	waitPending(t, sub, 1, 3*time.Second)
	sub.backfillMu.Lock()
	tracked := sub.backfills[addr.String()] != nil
	sub.backfillMu.Unlock()
	if !tracked {
		t.Fatal("superseded backfill's cleanup deleted the newer entry")
	}

	unblock()
	waitPending(t, sub, 0, 3*time.Second)
	sub.backfillMu.Lock()
	defer sub.backfillMu.Unlock()
	if n := len(sub.backfills); n != 0 {
		t.Errorf("%d backfill entries left after both attempts finished", n)
	}
}

// TestBackfillSharedFirehoseHoldsCatchupIncomplete: the shared-firehose
// transport adds contracts through its own path, which must reserve and
// release the pending count the same way.
func TestBackfillSharedFirehoseHoldsCatchupIncomplete(t *testing.T) {
	release := make(chan struct{})
	sub, _, cleanup := newBackfillSubWith(t, &SubscriberConfig{SharedFirehose: true},
		func(params json.RawMessage) (interface{}, error) {
			<-release // hold the backfill in flight
			return chunkEvent(params), nil
		})
	defer cleanup()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()

	// The one shared stream is live, so pending is the only variable.
	sub.streamsTotal.Store(1)
	sub.streamsLive.Store(1)

	// Tip is 1000, so [900, 1000] is backfilled.
	sub.AddContract(context.Background(), ContractSubscription{Address: newTestFelt(0xC0FFEE), StartBlock: 900})
	if got := sub.BackfillsPending(); got != 1 {
		t.Fatalf("BackfillsPending = %d, want 1 while the backfill is in flight", got)
	}
	if _, _, complete := sub.TransportStatus(); complete {
		t.Fatal("catchup_complete true while a dynamic backfill is in flight")
	}

	unblock()
	waitPending(t, sub, 0, 3*time.Second)
	if _, _, complete := sub.TransportStatus(); !complete {
		t.Fatal("catchup_complete still false after the backfill finished")
	}
}

// TestAddContractKeysFirehoseLaggingTipSeedsAtResumeFloor: a registration whose
// tip read hits a lagging replica must not seed below the block the keys-sub
// already resumed from; its backfill covers the difference instead.
func TestAddContractKeysFirehoseLaggingTipSeedsAtResumeFloor(t *testing.T) {
	var maxTo atomic.Uint64
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) { return uint64(970), nil },
		"starknet_getEvents": func(params json.RawMessage) (interface{}, error) {
			var p []struct {
				ToBlock struct {
					BlockNumber uint64 `json:"block_number"`
				} `json:"to_block"`
			}
			if json.Unmarshal(params, &p) == nil && len(p) == 1 && p[0].ToBlock.BlockNumber > maxTo.Load() {
				maxTo.Store(p[0].ToBlock.BlockNumber)
			}
			return chunkEvent(params), nil
		},
		"starknet_getBlockWithTxHashes": func(_ json.RawMessage) (interface{}, error) {
			return map[string]interface{}{"timestamp": 1, "block_number": 1, "block_hash": "0x1"}, nil
		},
	}
	server := mockRPCServer(t, handlers)
	defer server.Close()
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer p.Close()
	sub := p.NewSubscriber(nil, make(chan RawEvent, 256), &SubscriberConfig{
		KeysFirehose: true, OptionSelectors: []*felt.Felt{newTestFelt(0x999)},
	})
	st := newFirehoseKeysStream("keys-sub", nil, [][]*felt.Felt{sub.optionSelectors})
	st.fillBehind(1000) // the keys-sub resumed from 1000
	sub.streamsMu.Lock()
	sub.keysStream = st
	sub.streamsMu.Unlock()

	addr := newTestFelt(0x1A7E)
	sub.AddContract(context.Background(), ContractSubscription{Address: addr, StartBlock: 900, Wildcard: true})
	waitPending(t, sub, 0, 3*time.Second)

	if got := st.cursor(addr.String()); got != 1000 {
		t.Errorf("cursor = %d, want the resume floor 1000 (lagging tip 970)", got)
	}
	if got := maxTo.Load(); got != 999 {
		t.Errorf("backfill reached block %d, want 999: blocks up to the floor must be covered", got)
	}
}

// floorFixture is a keys-firehose subscriber whose keys-sub has resumed from
// 1000, with getEvents recording the highest to_block a backfill asks for.
func floorFixture(t *testing.T, blockNumber func() (interface{}, error)) (*EventSubscriber, *firehoseKeysStream, *atomic.Uint64, func()) {
	t.Helper()
	var maxTo atomic.Uint64
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) { return blockNumber() },
		"starknet_getEvents": func(params json.RawMessage) (interface{}, error) {
			var p []struct {
				ToBlock struct {
					BlockNumber uint64 `json:"block_number"`
				} `json:"to_block"`
			}
			if json.Unmarshal(params, &p) == nil && len(p) == 1 && p[0].ToBlock.BlockNumber > maxTo.Load() {
				maxTo.Store(p[0].ToBlock.BlockNumber)
			}
			return chunkEvent(params), nil
		},
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
	sub := p.NewSubscriber(nil, make(chan RawEvent, 256), &SubscriberConfig{
		KeysFirehose: true, OptionSelectors: []*felt.Felt{newTestFelt(0x999)},
	})
	st := newFirehoseKeysStream("keys-sub", nil, [][]*felt.Felt{sub.optionSelectors})
	st.fillBehind(1000) // the keys-sub resumed from 1000
	sub.streamsMu.Lock()
	sub.keysStream = st
	sub.streamsMu.Unlock()
	return sub, st, &maxTo, func() { p.Close(); server.Close() }
}

// TestResumeFloorFollowsRollback: after a reorg rolls the keys-sub back, a new
// contract is seeded from the post-reorg tip, not the pre-reorg floor.
func TestResumeFloorFollowsRollback(t *testing.T) {
	sub, st, maxTo, cleanup := floorFixture(t, func() (interface{}, error) { return uint64(820), nil })
	defer cleanup()
	sub.rollbackAllStreams(800)

	addr := newTestFelt(0x1A7E)
	sub.AddContract(context.Background(), ContractSubscription{Address: addr, StartBlock: 700, Wildcard: true})
	waitPending(t, sub, 0, 3*time.Second)

	if got := st.cursor(addr.String()); got != 821 {
		t.Errorf("cursor = %d, want 821 (post-reorg tip 820), not the stale floor 1000", got)
	}
	if got := maxTo.Load(); got != 820 {
		t.Errorf("backfill reached %d, want 820", got)
	}
}

// TestResumeFloorAppliesWithoutTip: with the tip unreadable, a contract whose
// StartBlock is below the floor is still seeded at the floor and backfilled up
// to it — WSS already resumed past StartBlock.
func TestResumeFloorAppliesWithoutTip(t *testing.T) {
	sub, st, maxTo, cleanup := floorFixture(t, func() (interface{}, error) { return nil, errors.New("rpc unavailable") })
	defer cleanup()

	addr := newTestFelt(0x1A7E)
	sub.AddContract(context.Background(), ContractSubscription{Address: addr, StartBlock: 900, Wildcard: true})
	waitPending(t, sub, 0, 3*time.Second)

	if got := st.cursor(addr.String()); got != 1000 {
		t.Errorf("cursor = %d, want the resume floor 1000", got)
	}
	if got := maxTo.Load(); got != 999 {
		t.Errorf("backfill reached %d, want 999", got)
	}
}

// TestResumeFloorSeedsERC20ChildAtFloor: an ERC20 child's own Transfer/Approval
// stream starts where its keys-sub fill does, so the one shared backfill up to
// floor-1 covers both event classes without a gap.
func TestResumeFloorSeedsERC20ChildAtFloor(t *testing.T) {
	sub, st, maxTo, cleanup := floorFixture(t, func() (interface{}, error) { return uint64(970), nil })
	defer cleanup()
	sub.dialWSS = mockWSSDialerKeyed(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := newTestFelt(0x1A7E)
	sub.AddContract(ctx, ContractSubscription{Address: addr, StartBlock: 900, Wildcard: true, ERC20: true})
	waitPending(t, sub, 0, 3*time.Second)

	sub.streamsMu.Lock()
	child := sub.addrStreams[addr.String()]
	sub.streamsMu.Unlock()
	if child == nil {
		t.Fatal("no child Transfer/Approval stream was started")
	}
	if c, k := child.cursor(addr.String()), st.cursor(addr.String()); c != 1000 || k != 1000 {
		t.Errorf("child cursor %d, keys-sub cursor %d; want both at the floor 1000", c, k)
	}
	if got := maxTo.Load(); got != 999 {
		t.Errorf("backfill reached %d, want 999", got)
	}
}

// TestBackfillRetriesPastLaggingReplicaHead: a backfill up to the floor can hit
// a replica that has not produced those blocks yet. It must keep retrying and
// finish once the replica catches up, not give up or skip the tail.
func TestBackfillRetriesPastLaggingReplicaHead(t *testing.T) {
	var head atomic.Uint64
	head.Store(950)
	var refused atomic.Bool
	sub, events, cleanup := newBackfillSub(t, func(params json.RawMessage) (interface{}, error) {
		var p []struct {
			ToBlock struct {
				BlockNumber uint64 `json:"block_number"`
			} `json:"to_block"`
		}
		if json.Unmarshal(params, &p) == nil && len(p) == 1 && p[0].ToBlock.BlockNumber > head.Load() {
			refused.Store(true)
			head.Store(1000) // caught up by the retry
			return nil, errors.New("block not found")
		}
		return chunkEvent(params), nil
	})
	defer cleanup()

	sub.reserveBackfill()
	sub.launchReservedBackfill(context.Background(), ContractSubscription{Address: newTestFelt(0xC0FFEE)}, 900, 999)
	waitPending(t, sub, 0, 5*time.Second) // retry backoff is minBackoff (1s)
	if !refused.Load() {
		t.Fatal("the replica never refused a block past its head; the test is not exercising a retry")
	}
	// [900, 999] is one chunk, so exactly one event, at 900, once it lands.
	select {
	case e := <-events:
		if e.BlockNumber != 900 {
			t.Errorf("delivered block %d, want 900", e.BlockNumber)
		}
	default:
		t.Fatal("the refused range was never delivered: the backfill gave up instead of retrying")
	}
}

// TestResumeFloorAfterRollbackBindsLaggingTip: once a reorg lowers the floor,
// the lowered floor still applies to a registration whose tip read lags it.
func TestResumeFloorAfterRollbackBindsLaggingTip(t *testing.T) {
	sub, st, maxTo, cleanup := floorFixture(t, func() (interface{}, error) { return uint64(750), nil })
	defer cleanup()
	sub.rollbackAllStreams(800)

	addr := newTestFelt(0x1A7E)
	sub.AddContract(context.Background(), ContractSubscription{Address: addr, StartBlock: 700, Wildcard: true})
	waitPending(t, sub, 0, 3*time.Second)

	if got := st.cursor(addr.String()); got != 800 {
		t.Errorf("cursor = %d, want the lowered floor 800 (lagging tip 750)", got)
	}
	if got := maxTo.Load(); got != 799 {
		t.Errorf("backfill reached %d, want 799", got)
	}
}

// TestResumeFloorSeedsERC20ChildWithoutTip: with the tip unreadable, an ERC20
// child's own stream still starts at the floor alongside its keys-sub fill.
func TestResumeFloorSeedsERC20ChildWithoutTip(t *testing.T) {
	sub, st, maxTo, cleanup := floorFixture(t, func() (interface{}, error) { return nil, errors.New("rpc unavailable") })
	defer cleanup()
	sub.dialWSS = mockWSSDialerKeyed(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := newTestFelt(0x1A7E)
	sub.AddContract(ctx, ContractSubscription{Address: addr, StartBlock: 900, Wildcard: true, ERC20: true})
	waitPending(t, sub, 0, 3*time.Second)

	sub.streamsMu.Lock()
	child := sub.addrStreams[addr.String()]
	sub.streamsMu.Unlock()
	if child == nil {
		t.Fatal("no child Transfer/Approval stream was started")
	}
	if c, k := child.cursor(addr.String()), st.cursor(addr.String()); c != 1000 || k != 1000 {
		t.Errorf("child cursor %d, keys-sub cursor %d; want both at the floor 1000", c, k)
	}
	if got := maxTo.Load(); got != 999 {
		t.Errorf("backfill reached %d, want 999", got)
	}
}

// TestResumeFloorAfterRollbackSeedsERC20ChildAtFloor: all three together — a
// rollback-lowered floor, a readable tip lagging below it, and an ERC20 child.
// The child's stream and its keys-sub fill both start at the lowered floor.
func TestResumeFloorAfterRollbackSeedsERC20ChildAtFloor(t *testing.T) {
	sub, st, maxTo, cleanup := floorFixture(t, func() (interface{}, error) { return uint64(750), nil })
	defer cleanup()
	sub.dialWSS = mockWSSDialerKeyed(nil, nil)
	sub.rollbackAllStreams(800)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := newTestFelt(0x1A7E)
	sub.AddContract(ctx, ContractSubscription{Address: addr, StartBlock: 700, Wildcard: true, ERC20: true})
	waitPending(t, sub, 0, 3*time.Second)

	sub.streamsMu.Lock()
	child := sub.addrStreams[addr.String()]
	sub.streamsMu.Unlock()
	if child == nil {
		t.Fatal("no child Transfer/Approval stream was started")
	}
	if c, k := child.cursor(addr.String()), st.cursor(addr.String()); c != 800 || k != 800 {
		t.Errorf("child cursor %d, keys-sub cursor %d; want both at the lowered floor 800", c, k)
	}
	if got := maxTo.Load(); got != 799 {
		t.Errorf("backfill reached %d, want 799", got)
	}
}
