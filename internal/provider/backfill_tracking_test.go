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
			if e.BackfillDone {
				continue
			}
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

// TestBackfillSurvivesStopLive: freezing a contract (StopLive) must not cancel
// its backfill — a child frozen at registration would otherwise never get its
// history, and the frozen flag keeps it from being re-subscribed later.
func TestBackfillSurvivesStopLive(t *testing.T) {
	for name, cfg := range map[string]*SubscriberConfig{
		"shared firehose": {SharedFirehose: true},
		"keys firehose":   {KeysFirehose: true, OptionSelectors: []*felt.Felt{newTestFelt(0x999)}},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			sub, events, cleanup := newBackfillSubWith(t, cfg, func(params json.RawMessage) (interface{}, error) {
				if calls.Add(1) <= 2 {
					return nil, errors.New("rpc down") // still retrying when StopLive lands
				}
				return chunkEvent(params), nil
			})
			defer cleanup()

			addr := newTestFelt(0xC0FFEE)
			sub.reserveBackfill()
			sub.launchReservedBackfill(context.Background(), ContractSubscription{Address: addr}, 0, 350)
			sub.StopLive(addr.String())

			select {
			case <-events:
			case <-time.After(10 * time.Second):
				t.Fatal("backfill delivered no events after StopLive")
			}
			waitPending(t, sub, 0, 10*time.Second)
		})
	}
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

// stopLiveFixture builds a subscriber for each firehose transport with a held
// backfill, plus a forward func that feeds it one live event. Keys firehose
// needs its keys-sub seeded the way startKeysFirehose would.
func stopLiveFixture(t *testing.T, cfg *SubscriberConfig, getEvents func(json.RawMessage) (interface{}, error)) (*EventSubscriber, chan RawEvent, func(block uint64), func()) {
	t.Helper()
	sub, events, cleanup := newBackfillSubWith(t, cfg, getEvents)
	forward := func(block uint64) { sub.forwardIfTracked(context.Background(), fhEvent(0xC0FFEE, block)) }
	if cfg.KeysFirehose {
		sub.streamsMu.Lock()
		sub.keysStream = newFirehoseKeysStream("keys-sub", nil, [][]*felt.Felt{sub.optionSelectors})
		sub.streamsMu.Unlock()
		forward = func(block uint64) {
			sub.forwardStream(context.Background(), sub.keysStream, fhEvent(0xC0FFEE, block))
		}
	}
	return sub, events, forward, cleanup
}

var stopLiveConfigs = map[string]*SubscriberConfig{
	"shared firehose": {SharedFirehose: true},
	"keys firehose":   {KeysFirehose: true, OptionSelectors: []*felt.Felt{newTestFelt(0x999)}},
}

// TestStopLiveIdempotentAndUnknownAddress: StopLive on an unknown address or a
// repeated call neither panics nor disturbs a backfill that is still pending.
func TestStopLiveIdempotentAndUnknownAddress(t *testing.T) {
	for name, cfg := range stopLiveConfigs {
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			sub, _, _, cleanup := stopLiveFixture(t, cfg, func(params json.RawMessage) (interface{}, error) {
				<-release
				return chunkEvent(params), nil
			})
			defer cleanup()
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()

			addr := newTestFelt(0xC0FFEE)
			sub.reserveBackfill()
			sub.launchReservedBackfill(context.Background(), ContractSubscription{Address: addr}, 900, 950)

			sub.StopLive(newTestFelt(0xDEAD).String()) // never added
			sub.StopLive(addr.String())
			sub.StopLive(addr.String()) // already stopped
			if got := sub.BackfillsPending(); got != 1 {
				t.Fatalf("BackfillsPending = %d, want 1 after no-op/repeated StopLive", got)
			}

			unblock()
			waitPending(t, sub, 0, 3*time.Second)
		})
	}
}

// TestStopLiveStopsLiveDeliveryKeepsBackfill: after StopLive the live path
// drops the address's events, while the backfill still delivers its history.
func TestStopLiveStopsLiveDeliveryKeepsBackfill(t *testing.T) {
	for name, cfg := range stopLiveConfigs {
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			sub, events, forward, cleanup := stopLiveFixture(t, cfg, func(params json.RawMessage) (interface{}, error) {
				<-release
				return chunkEvent(params), nil
			})
			defer cleanup()
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()

			// Tip is 1000: backfill [900, 1000], live from 1001.
			addr := newTestFelt(0xC0FFEE)
			sub.AddContract(context.Background(), ContractSubscription{Address: addr, StartBlock: 900})

			forward(1001)
			if got := drainBlocks(events); len(got) != 1 || got[0] != 1001 {
				t.Fatalf("live blocks before StopLive = %v, want [1001]", got)
			}

			sub.StopLive(addr.String())
			forward(1002)
			if got := drainBlocks(events); len(got) != 0 {
				t.Fatalf("live path delivered %v after StopLive, want nothing", got)
			}

			unblock()
			waitPending(t, sub, 0, 3*time.Second)
			got := drainBlocks(events)
			if len(got) == 0 || got[0] != 900 {
				t.Fatalf("backfill blocks after StopLive = %v, want history from 900", got)
			}
			for _, b := range got {
				if b > 1000 {
					t.Fatalf("backfill delivered live-range block %d: %v", b, got)
				}
			}
		})
	}
}

// TestStopLiveHoldsCatchupIncompleteUntilBackfillDone: a frozen contract's
// backfill still counts as pending, so catchup_complete stays false until it ends.
func TestStopLiveHoldsCatchupIncompleteUntilBackfillDone(t *testing.T) {
	for name, cfg := range stopLiveConfigs {
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			sub, _, _, cleanup := stopLiveFixture(t, cfg, func(params json.RawMessage) (interface{}, error) {
				<-release
				return chunkEvent(params), nil
			})
			defer cleanup()
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()

			sub.streamsTotal.Store(1)
			sub.streamsLive.Store(1)

			addr := newTestFelt(0xC0FFEE)
			sub.AddContract(context.Background(), ContractSubscription{Address: addr, StartBlock: 900})
			sub.StopLive(addr.String())

			if _, _, complete := sub.TransportStatus(); complete {
				t.Fatal("catchup_complete true while a frozen contract's backfill runs")
			}
			unblock()
			waitPending(t, sub, 0, 3*time.Second)
			if _, _, complete := sub.TransportStatus(); !complete {
				t.Fatal("catchup_complete still false after the frozen backfill finished")
			}
		})
	}
}

// TestPerContractBackfillSurvivesStopLive: in the default per-contract
// transport a contract's pre-tip history is a tracked backfill too, so a freeze
// (StopLive) mid-fetch keeps it, while RemoveContract still cancels it.
func TestPerContractBackfillSurvivesStopLive(t *testing.T) {
	for name, cfg := range map[string]*SubscriberConfig{
		"default":         {},
		"catchup polling": {CatchupWithPolling: true},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			sub, events, cleanup := newBackfillSubWith(t, cfg, func(params json.RawMessage) (interface{}, error) {
				if fromBlockOf(params) <= 1000 && calls.Add(1) <= 2 {
					return nil, errors.New("rpc down") // still retrying when StopLive lands
				}
				return chunkEvent(params), nil
			})
			defer cleanup()

			addr := newTestFelt(0xC0FFEE)
			sub.AddContract(context.Background(), ContractSubscription{Address: addr, StartBlock: 900})
			sub.StopLive(addr.String())

			select {
			case <-events:
			case <-time.After(10 * time.Second):
				t.Fatal("per-contract backfill delivered no events after StopLive")
			}
			waitPending(t, sub, 0, 10*time.Second)
		})
	}
}

func TestPerContractRemoveCancelsBackfill(t *testing.T) {
	sub, events, cleanup := newBackfillSubWith(t, &SubscriberConfig{}, func(params json.RawMessage) (interface{}, error) {
		return nil, errors.New("rpc down") // never succeeds
	})
	defer cleanup()

	addr := newTestFelt(0xC0FFEE)
	sub.AddContract(context.Background(), ContractSubscription{Address: addr, StartBlock: 900})
	waitPending(t, sub, 1, 3*time.Second)
	sub.RemoveContract(addr.String())
	waitPending(t, sub, 0, 3*time.Second)
	select {
	case <-events:
		t.Fatal("removed contract delivered an event")
	default:
	}
}

// getEventsRanges records every [from, to] a mock RPC receives via getEvents.
type getEventsRanges struct {
	mu  sync.Mutex
	got [][2]uint64
}

func (r *getEventsRanges) record(params json.RawMessage) {
	var p []struct {
		From struct {
			N uint64 `json:"block_number"`
		} `json:"from_block"`
		To struct {
			N uint64 `json:"block_number"`
		} `json:"to_block"`
	}
	if json.Unmarshal(params, &p) != nil || len(p) != 1 {
		return
	}
	r.mu.Lock()
	r.got = append(r.got, [2]uint64{p[0].From.N, p[0].To.N})
	r.mu.Unlock()
}

func (r *getEventsRanges) snapshot() [][2]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][2]uint64(nil), r.got...)
}

// waitRanges polls until cond holds over the recorded ranges.
func waitRanges(t *testing.T, r *getEventsRanges, what string, cond func([][2]uint64) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond(r.snapshot()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; getEvents ranges = %v", what, r.snapshot())
}

// newRangeSub builds a per-contract subscriber whose starknet_blockNumber
// answers tip(n) on its n-th call (1-based, counted from AddContract), so a
// test controls the tip AddContract sees and the tip the live stream sees.
func newRangeSub(t *testing.T, cfg *SubscriberConfig, tip func(n int32) (interface{}, error)) (*EventSubscriber, *getEventsRanges, context.Context, func()) {
	t.Helper()
	ranges := &getEventsRanges{}
	var tipCalls atomic.Int32
	c := *cfg
	c.TipPollInterval, c.CatchupPollInterval = 10*time.Millisecond, 10*time.Millisecond
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) { return tip(tipCalls.Add(1)) },
		"starknet_getEvents": func(params json.RawMessage) (interface{}, error) {
			ranges.record(params)
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	}
	server := mockRPCServer(t, handlers)
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		server.Close()
		t.Fatalf("New() error: %v", err)
	}
	sub := p.NewSubscriber(nil, make(chan RawEvent, 256), &c)
	tipCalls.Store(0)
	ctx, cancel := context.WithCancel(context.Background())
	return sub, ranges, ctx, func() { cancel(); p.Close(); server.Close() }
}

var perContractPollingConfigs = map[string]*SubscriberConfig{
	"force polling":   {ForcePolling: true},
	"catchup polling": {CatchupWithPolling: true},
}

// TestPerContractHandoffAtTipPlusOne: a normal contract's backfill covers
// exactly [StartBlock, tip] and its live poll starts at tip+1 — no gap, no overlap.
func TestPerContractHandoffAtTipPlusOne(t *testing.T) {
	for name, cfg := range perContractPollingConfigs {
		t.Run(name, func(t *testing.T) {
			// AddContract sees tip 1000; the live stream then sees 1200.
			sub, ranges, ctx, cleanup := newRangeSub(t, cfg, func(n int32) (interface{}, error) {
				if n == 1 {
					return uint64(1000), nil
				}
				return uint64(1200), nil
			})
			defer cleanup()

			sub.AddContract(ctx, ContractSubscription{Address: newTestFelt(0xC0FFEE), StartBlock: 900})
			waitPending(t, sub, 0, 5*time.Second)
			waitRanges(t, ranges, "a live poll past the tip", func(rs [][2]uint64) bool {
				for _, r := range rs {
					if r[0] > 1000 {
						return true
					}
				}
				return false
			})

			// Backfill chunks (blocksPerQuery 100) tile [900, 1000]; the first
			// live poll starts at 1001. Nothing straddles the tip.
			var backfill [][2]uint64
			var firstLive uint64
			for _, r := range ranges.snapshot() {
				switch {
				case r[1] <= 1000:
					backfill = append(backfill, r)
				case r[0] > 1000:
					if firstLive == 0 {
						firstLive = r[0]
					}
				default:
					t.Fatalf("range %v straddles the tip 1000", r)
				}
			}
			want := [][2]uint64{{900, 999}, {1000, 1000}}
			if len(backfill) != len(want) || backfill[0] != want[0] || backfill[1] != want[1] {
				t.Fatalf("backfill ranges = %v, want %v", backfill, want)
			}
			if firstLive != 1001 {
				t.Fatalf("live poll starts at %d, want 1001", firstLive)
			}
		})
	}
}

// TestPerContractNoTipStreamsFromStartBlock: when AddContract cannot read the
// tip, no backfill is reserved and the live stream starts at StartBlock.
func TestPerContractNoTipStreamsFromStartBlock(t *testing.T) {
	for name, cfg := range perContractPollingConfigs {
		t.Run(name, func(t *testing.T) {
			// Tip read fails in AddContract only; the live stream then sees 1000.
			sub, ranges, ctx, cleanup := newRangeSub(t, cfg, func(n int32) (interface{}, error) {
				if n == 1 {
					return nil, errors.New("rpc down")
				}
				return uint64(1000), nil
			})
			defer cleanup()

			sub.AddContract(ctx, ContractSubscription{Address: newTestFelt(0xC0FFEE), StartBlock: 900})
			if got := sub.BackfillsPending(); got != 0 {
				t.Fatalf("BackfillsPending = %d, want 0 with no tip", got)
			}
			waitRanges(t, ranges, "the first live poll", func(rs [][2]uint64) bool { return len(rs) > 0 })
			if first := ranges.snapshot()[0]; first[0] != 900 {
				t.Fatalf("live poll starts at %d, want StartBlock 900", first[0])
			}
			if got := sub.BackfillsPending(); got != 0 {
				t.Fatalf("BackfillsPending = %d, want 0", got)
			}
		})
	}
}

// TestPerContractStartAfterTipNoBackfill: a StartBlock beyond the tip has no
// history to fetch — no backfill, and the live stream starts at StartBlock.
func TestPerContractStartAfterTipNoBackfill(t *testing.T) {
	for name, cfg := range perContractPollingConfigs {
		t.Run(name, func(t *testing.T) {
			// AddContract sees tip 1000; the live stream then sees 2000.
			sub, ranges, ctx, cleanup := newRangeSub(t, cfg, func(n int32) (interface{}, error) {
				if n == 1 {
					return uint64(1000), nil
				}
				return uint64(2000), nil
			})
			defer cleanup()

			sub.AddContract(ctx, ContractSubscription{Address: newTestFelt(0xC0FFEE), StartBlock: 1500})
			if got := sub.BackfillsPending(); got != 0 {
				t.Fatalf("BackfillsPending = %d, want 0 for StartBlock > tip", got)
			}
			waitRanges(t, ranges, "the first live poll", func(rs [][2]uint64) bool { return len(rs) > 0 })
			for _, r := range ranges.snapshot() {
				if r[0] < 1500 {
					t.Fatalf("range %v starts below StartBlock 1500", r)
				}
			}
			if got := sub.BackfillsPending(); got != 0 {
				t.Fatalf("BackfillsPending = %d, want 0", got)
			}
		})
	}
}

// TestBackfillSignalsDoneAfterLastEvent: a successful backfill ends with one
// in-band BackfillDone, behind every event it delivered, so the consumer sees
// it only once those are processed.
func TestBackfillSignalsDoneAfterLastEvent(t *testing.T) {
	sub, events, cleanup := newBackfillSub(t, func(params json.RawMessage) (interface{}, error) {
		return chunkEvent(params), nil
	})
	defer cleanup()

	sub.BackfillBackground(context.Background(), ContractSubscription{Address: newTestFelt(0xC0FFEE)}, 0, 350)
	waitPending(t, sub, 0, 5*time.Second)

	var got []RawEvent
	for len(events) > 0 {
		got = append(got, <-events)
	}
	if len(got) != 5 {
		t.Fatalf("got %d events, want 4 chunk events + 1 sentinel", len(got))
	}
	for i, e := range got[:4] {
		if e.BackfillDone {
			t.Fatalf("event %d is the sentinel, want it last", i)
		}
	}
	if last := got[4]; !last.BackfillDone || last.ContractAddress.String() != newTestFelt(0xC0FFEE).String() {
		t.Fatalf("last event = %+v, want the BackfillDone sentinel for the contract", last)
	}
}

// TestBackfillToTip: ToTip is resolved by the backfill itself.
func TestBackfillToTip(t *testing.T) {
	var maxFrom atomic.Uint64
	sub, events, cleanup := newBackfillSub(t, func(params json.RawMessage) (interface{}, error) {
		if f := fromBlockOf(params); f > maxFrom.Load() {
			maxFrom.Store(f)
		}
		return chunkEvent(params), nil
	})
	defer cleanup()

	sub.BackfillBackground(context.Background(), ContractSubscription{Address: newTestFelt(0xC0FFEE)}, 900, ToTip)
	waitPending(t, sub, 0, 5*time.Second)
	// Tip 1000, 100 blocks per query: [900,999] then [1000,1000].
	if got := maxFrom.Load(); got != 1000 {
		t.Fatalf("last chunk started at %d, want 1000 (the tip)", got)
	}
	var done bool
	for len(events) > 0 {
		done = (<-events).BackfillDone
	}
	if !done {
		t.Fatal("no BackfillDone after a ToTip backfill")
	}
}
