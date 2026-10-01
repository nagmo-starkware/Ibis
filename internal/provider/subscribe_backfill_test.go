package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/client"
	"github.com/NethermindEth/starknet.go/rpc"
)

// A node that IGNORES block_id, as Alchemy does: a subscription delivers only
// what the test pushes after the dial returns. Events the chain holds below
// the subscribe point can therefore only arrive via the post-subscribe HTTP
// backfill.

// gapChain is the HTTP side of the fixture.
type gapChain struct {
	tip       atomic.Uint64 // latest accepted block
	pre       atomic.Uint64 // pre_confirmed block number
	preFails  atomic.Int64  // pre_confirmed reads that still fail
	preReads  atomic.Int64
	fetches   atomic.Int64 // getEvents calls
	failFetch atomic.Bool  // getEvents returns an error

	gate    chan struct{} // if set, getEvents blocks until closed
	entered chan struct{} // signalled (non-blocking) on each getEvents entry

	mu  sync.Mutex
	evs map[uint64][]uint64 // block -> emitting addresses
}

func newGapChain(tip, pre uint64) *gapChain {
	c := &gapChain{evs: map[uint64][]uint64{}, entered: make(chan struct{}, 64)}
	c.tip.Store(tip)
	c.pre.Store(pre)
	return c
}

func (c *gapChain) add(block, addr uint64) {
	c.mu.Lock()
	c.evs[block] = append(c.evs[block], addr)
	c.mu.Unlock()
}

func (c *gapChain) handlers() map[string]func(json.RawMessage) (interface{}, error) {
	return map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) { return c.tip.Load(), nil },
		"starknet_getBlockWithTxHashes": func(params json.RawMessage) (interface{}, error) {
			if string(params) == `["pre_confirmed"]` || string(params) == `{"block_id":"pre_confirmed"}` {
				c.preReads.Add(1)
				if c.preFails.Add(-1) >= 0 {
					return nil, fmt.Errorf("pre_confirmed unavailable")
				}
				return map[string]interface{}{"block_number": c.pre.Load(), "timestamp": 1}, nil
			}
			return map[string]interface{}{"block_number": 1, "timestamp": 1, "block_hash": "0x1"}, nil
		},
		"starknet_getEvents": func(params json.RawMessage) (interface{}, error) {
			c.fetches.Add(1)
			select {
			case c.entered <- struct{}{}:
			default:
			}
			if c.gate != nil {
				<-c.gate
			}
			if c.failFetch.Load() {
				return nil, fmt.Errorf("getEvents unavailable")
			}
			var q []struct {
				From struct {
					N uint64 `json:"block_number"`
				} `json:"from_block"`
				To struct {
					N uint64 `json:"block_number"`
				} `json:"to_block"`
				Address string `json:"address"`
			}
			if err := json.Unmarshal(params, &q); err != nil || len(q) != 1 {
				return nil, fmt.Errorf("bad params %s", params)
			}
			out := []map[string]interface{}{}
			c.mu.Lock()
			for b := q[0].From.N; b <= q[0].To.N; b++ {
				for _, a := range c.evs[b] {
					addr := newTestFelt(a).String()
					if q[0].Address != "" && q[0].Address != addr {
						continue
					}
					out = append(out, map[string]interface{}{
						"block_number": b, "block_hash": "0x1", "transaction_hash": fmt.Sprintf("0x%x", b),
						"from_address": addr, "keys": []string{"0x1"}, "data": []string{"0x2"},
					})
				}
			}
			c.mu.Unlock()
			return map[string]interface{}{"events": out}, nil
		},
	}
}

// gapNode is the WSS side: it records the requested resume block and hands the
// test each session; nothing is ever replayed.
type gapNode struct {
	sessions  chan *gapSess
	dials     atomic.Int32
	failDials atomic.Int32
	closed    atomic.Int32
	mu        sync.Mutex
	resumes   []uint64
}

type gapSess struct {
	in     *rpc.EventSubscriptionInput
	events chan *rpc.EmittedEventWithFinalityStatus
	errs   chan error
}

func newGapNode() *gapNode { return &gapNode{sessions: make(chan *gapSess, 32)} }

func (n *gapNode) dialer() wssDialer {
	return func(_ context.Context, _ string, in *rpc.EventSubscriptionInput) (*wssSession, error) {
		n.dials.Add(1)
		if n.failDials.Add(-1) >= 0 {
			return nil, fmt.Errorf("subscribe reply timeout")
		}
		n.mu.Lock()
		if in.SubBlockID.Number != nil {
			n.resumes = append(n.resumes, *in.SubBlockID.Number)
		}
		n.mu.Unlock()
		s := &gapSess{in: in, events: make(chan *rpc.EmittedEventWithFinalityStatus, 64), errs: make(chan error, 1)}
		n.sessions <- s
		return &wssSession{
			events: s.events, errs: s.errs, reorgs: make(chan *client.ReorgEvent, 1),
			close: func() { n.closed.Add(1) },
		}, nil
	}
}

func (n *gapNode) next(t *testing.T) *gapSess {
	t.Helper()
	select {
	case s := <-n.sessions:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no WSS session was opened")
		return nil
	}
}

type gapSite struct {
	name      string
	contracts []ContractSubscription
	cfg       SubscriberConfig
	addr      uint64 // the tracked contract whose events are asserted
	resume    uint64 // block_id the site subscribes with
	fromAddr  uint64 // from_address of the site's subscription (0: none)
	streams   int    // subscriptions the transport opens (keys: + the keys-sub)
}

// nextFor returns the site's own subscription, discarding other streams' ones.
func (n *gapNode) nextFor(t *testing.T, site gapSite) *gapSess {
	t.Helper()
	for {
		s := n.next(t)
		fa := s.in.FromAddress
		if (fa == nil && site.fromAddr == 0) || (fa != nil && fa.String() == newTestFelt(site.fromAddr).String()) {
			return s
		}
	}
}

func gapSites() []gapSite {
	keysCfg := SubscriberConfig{KeysFirehose: true, OptionSelectors: []*felt.Felt{newTestFelt(0x999)}}
	return []gapSite{
		{"keys-sub", []ContractSubscription{{Address: newTestFelt(0xA), StartBlock: 100, Wildcard: true}}, keysCfg, 0xA, 100, 0, 1},
		{"keys-address-sub", []ContractSubscription{{Address: newTestFelt(0xB), StartBlock: 100}}, keysCfg, 0xB, 100, 0xB, 2},
		{"shared-firehose", []ContractSubscription{{Address: newTestFelt(0xA), StartBlock: 100}}, SubscriberConfig{SharedFirehose: true}, 0xA, 100, 0, 1},
		{"per-contract", []ContractSubscription{{Address: newTestFelt(0xA), StartBlock: 100}}, SubscriberConfig{}, 0xA, 100, 0xA, 1},
	}
}

// startGapSite wires a subscriber for site against chain/node and starts it.
func startGapSite(t *testing.T, site gapSite, chain *gapChain, node *gapNode) (*EventSubscriber, chan RawEvent) {
	t.Helper()
	server := mockRPCServer(t, chain.handlers())
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		server.Close()
		t.Fatalf("New() error: %v", err)
	}
	cfg := site.cfg
	cfg.TipPollInterval = 5 * time.Millisecond
	cfg.CatchupPollInterval = 5 * time.Millisecond
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
	return sub, events
}

// waitEvent returns the first event satisfying pred; fails on timeout.
func waitEvent(t *testing.T, ch <-chan RawEvent, what string, pred func(RawEvent) bool) RawEvent {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-ch:
			if pred(e) {
				return e
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func isLive(sub *EventSubscriber) bool {
	live, total, _ := sub.TransportStatus()
	return total > 0 && live == total
}

func waitLive(t *testing.T, sub *EventSubscriber) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !isLive(sub) {
		select {
		case <-deadline:
			live, total, _ := sub.TransportStatus()
			t.Fatalf("never went live: live=%d total=%d", live, total)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func liveEvent(addr, block uint64) *rpc.EmittedEventWithFinalityStatus { return fhEvent(addr, block) }

// An event in a block between the resume block and the subscribe point is
// delivered by the post-subscribe backfill, though the node never replays it.
func TestSubscribeBackfillDeliversGapEvent(t *testing.T) {
	for _, site := range gapSites() {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(120, 121) // P=121 not yet accepted
			chain.add(105, site.addr)      // in the gap [resume, P]
			chain.add(106, 0xC)            // untracked: must not leak
			node := newGapNode()
			sub, events := startGapSite(t, site, chain, node)

			sess := node.nextFor(t, site)
			if n := sess.in.SubBlockID.Number; n == nil || *n != site.resume {
				t.Errorf("subscribed with block_id %v, want %d", n, site.resume)
			}

			if isLive(sub) {
				t.Fatal("live before P was accepted and the backfill ran")
			}
			chain.tip.Store(121) // P becomes accepted -> backfill may fetch up to it

			got := waitEvent(t, events, "gap event @105", func(e RawEvent) bool { return e.BlockNumber == 105 })
			if got.ContractAddress.String() != newTestFelt(site.addr).String() {
				t.Errorf("gap event from %s", got.ContractAddress)
			}
			if !got.IsCatchup {
				t.Error("backfilled event must be flagged IsCatchup")
			}
			waitLive(t, sub)
			select {
			case e := <-events:
				if e.ContractAddress.String() == newTestFelt(0xC).String() {
					t.Errorf("untracked address leaked via backfill (block %d)", e.BlockNumber)
				}
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

// Not live until the backfill has finished; live events still flow meanwhile.
func TestSubscribeBackfillGatesLiveness(t *testing.T) {
	for _, site := range gapSites() {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(121, 121)
			chain.gate = make(chan struct{})
			chain.add(105, site.addr)
			node := newGapNode()
			sub, events := startGapSite(t, site, chain, node)

			sess := node.nextFor(t, site)
			select {
			case <-chain.entered: // backfill is in flight (blocked on the gate)
			case <-time.After(5 * time.Second):
				t.Fatal("backfill never started")
			}
			if isLive(sub) {
				t.Fatal("stream reported live while its post-subscribe backfill is running")
			}

			// The session keeps draining while the backfill is stuck.
			sess.events <- liveEvent(site.addr, 122)
			waitEvent(t, events, "live event during backfill", func(e RawEvent) bool { return e.BlockNumber == 122 && !e.IsCatchup })
			if isLive(sub) {
				t.Fatal("live before the backfill finished")
			}

			close(chain.gate)
			waitEvent(t, events, "gap event @105", func(e RawEvent) bool { return e.BlockNumber == 105 })
			waitLive(t, sub)
		})
	}
}

// A pre_confirmed read that keeps failing closes the session unused (never
// live, nothing fetched) and the next attempt succeeds.
func TestSubscribePreConfirmedFailureIsFailedDial(t *testing.T) {
	for _, site := range gapSites() {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(121, 121)
			chain.preFails.Store(int64(preConfirmedAttempts * site.streams)) // each stream's first dial
			chain.add(105, site.addr)
			node := newGapNode()
			sub, events := startGapSite(t, site, chain, node)

			first := node.nextFor(t, site)
			deadline := time.After(5 * time.Second)
			for node.closed.Load() < int32(site.streams) {
				select {
				case <-deadline:
					t.Fatal("session with unknown P was not closed")
				case <-time.After(2 * time.Millisecond):
				}
			}
			first.events <- liveEvent(site.addr, 130) // must never be consumed
			if isLive(sub) || chain.fetches.Load() != 0 {
				t.Fatalf("session used without a known P: live=%v fetches=%d", isLive(sub), chain.fetches.Load())
			}
			if n := chain.preReads.Load(); n != int64(preConfirmedAttempts*site.streams) {
				t.Errorf("pre_confirmed reads = %d, want %d", n, preConfirmedAttempts*site.streams)
			}

			node.nextFor(t, site) // retried after backoff
			waitEvent(t, events, "gap event @105", func(e RawEvent) bool { return e.BlockNumber == 105 })
			waitLive(t, sub)
		})
	}
}

// A subscribe that fails (error or reply timeout from the dialer) is retried
// and never counts as live.
func TestSubscribeDialFailureRetried(t *testing.T) {
	for _, site := range gapSites() {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(121, 121)
			node := newGapNode()
			node.failDials.Store(int32(site.streams)) // each stream's first dial
			sub, _ := startGapSite(t, site, chain, node)

			node.nextFor(t, site)
			waitLive(t, sub) // every stream, after its retry
			if node.dials.Load() < int32(2*site.streams) {
				t.Errorf("dials = %d, want a retry per stream after the failed subscribe", node.dials.Load())
			}
		})
	}
}

// A session that drops before its backfill finishes is not live and does not
// let live events push the resume point past the unfilled gap: the next
// (re)subscribe starts at or below it (the cursors were capped back).
func TestSubscribeBackfillIncompleteResumesBelowGap(t *testing.T) {
	for _, site := range gapSites() {
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(121, 121)
			chain.gate = make(chan struct{})
			chain.add(105, site.addr)
			node := newGapNode()
			sub, events := startGapSite(t, site, chain, node)

			sess := node.nextFor(t, site)
			<-chain.entered
			sess.events <- liveEvent(site.addr, 125) // past the unfilled gap
			waitEvent(t, events, "live @125", func(e RawEvent) bool { return e.BlockNumber == 125 })
			sess.errs <- fmt.Errorf("socket dropped")
			close(chain.gate)

			second := node.nextFor(t, site)
			n := second.in.SubBlockID.Number
			if n == nil {
				t.Fatal("second subscribe has no block_id")
			}
			if *n > 105 {
				t.Errorf("second subscribe block_id %d, want <= 105 (below the unfilled gap)", *n)
			}
			_ = sub
		})
	}
}

// A backfill that fails ends the session unused-as-live, escalates the
// reconnect backoff (1s, 2s, ...), and a later successful session delivers the
// gap event and goes live.
func TestSubscribeBackfillFailureEscalatesBackoff(t *testing.T) {
	for _, site := range gapSites() {
		t.Run(site.name, func(t *testing.T) {
			t.Parallel()
			chain := newGapChain(121, 121)
			chain.failFetch.Store(true)
			chain.add(105, site.addr)
			node := newGapNode()
			sub, events := startGapSite(t, site, chain, node)

			node.nextFor(t, site)
			node.nextFor(t, site)
			t2 := time.Now()
			node.nextFor(t, site)
			if gap := time.Since(t2); gap < 1800*time.Millisecond {
				t.Errorf("third subscribe %v after the second, want >= ~2s (backoff must escalate after a failed backfill)", gap)
			}
			if isLive(sub) || node.closed.Load() < int32(2*site.streams) {
				t.Errorf("failed backfill: live=%v closed=%d, want not live and failed sessions closed", isLive(sub), node.closed.Load())
			}

			chain.failFetch.Store(false)
			waitEvent(t, events, "gap event @105", func(e RawEvent) bool { return e.BlockNumber == 105 })
			waitLive(t, sub)
		})
	}
}
