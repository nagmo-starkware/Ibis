package provider

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/rpc"

	"github.com/b-j-roberts/ibis/internal/abi"
)

// firehose_keys.go implements the "firehose-keys" transport (option D): a
// cost-reduced alternative to firehose.go's single all-events subscription
// (option C). Where C opens ONE empty-filter WSS subscription and demuxes the
// WHOLE chain's events in-process (~18 ev/s chain-wide, of which only a sliver
// is ever tracked), D opens MULTIPLE key-filtered subscriptions so the node
// itself only ever delivers events this deployment cares about:
//
//  1. ONE keys-sub: EventSubscriptionInput{Keys: [][]*felt.Felt{optionSelectors}}
//     (no from_address). Streams every option-family event (managers,
//     factories, and every OptionToken/OrderBook/Exerciser child) chain-wide,
//     EXCLUDING Transfer/Approval — the router still drops any foreign
//     contract that happens to emit a same-named event (name-collision noise).
//  2. One address-sub per static, non-wildcard contract (ERC20 tokens, the UDC
//     discovery address): EventSubscriptionInput{FromAddress: addr, Keys: ...}.
//  3. One address-sub per active OptionToken child (a wildcard contract whose
//     ABI is also an ERC20 — has a Transfer event):
//     EventSubscriptionInput{FromAddress: child, Keys: {Transfer, Approval}}.
//     This captures exactly the two event classes the keys-sub excludes; the
//     child's other events (Written/Exercised/Settled/...) still arrive via
//     the keys-sub.
//
// Each (address, event-class) is delivered on EXACTLY ONE subscription — the
// filters partition cleanly — so there is never duplicate delivery BETWEEN
// streams. But an OptionToken child's cursor is touched by TWO streams (the
// keys-sub, for its non-Transfer events, and its own Transfer/Approval
// address-sub). If those two streams shared one per-address cursor (as C's
// single-stream firehoseSink does), whichever stream ran ahead would advance
// the shared cursor and the other stream's still-valid, still-unforwarded
// events would be silently dropped by the "< cursor" dedup guard.
//
// THEREFORE the core invariant of this file: every firehoseKeysStream owns
// its OWN per-address cursor map. The ONLY state shared across streams is the
// tracked *membership* set (EventSubscriber.tracked) — which addresses get
// forwarded at all — never the cursors. See forwardStream and
// firehoseKeysStream.cursors.

// transferSelector and approvalSelector are the standard ERC20 event
// selectors. An OptionToken child's Transfer/Approval events are routed
// through its own address-sub stream (case 3 above) rather than the shared
// keys-sub, so they need to be named explicitly here.
var (
	transferSelector = abi.ComputeSelector("Transfer")
	approvalSelector = abi.ComputeSelector("Approval")
)

// firehoseKeysStream is one independent WSS subscription stream. A stream
// owns:
//   - a label (for logging),
//   - a fixed WSS input shape: address is nil for the keys-sub (chain-wide,
//     no from_address filter) or the contract's address for an address-sub;
//     keys is the event-key filter used on every (re)connect,
//   - its OWN per-address cursor map — see the package doc comment above,
//   - its OWN gap-fill responsibility set: which ContractSubscriptions to
//     HTTP-catch-up (via the existing pollUntilCaughtUp) before each
//     (re)connect. For the keys-sub this is every tracked option-family
//     contract and is mutated dynamically as children are added/removed; for
//     an address-sub it holds exactly one entry (or, transiently, zero before
//     the stream's target contract has been seeded).
type firehoseKeysStream struct {
	label   string
	address *felt.Felt     // nil for the keys-sub
	keys    [][]*felt.Felt // event key filter for this stream's WSS input

	// runCtx is the context this stream's run-loop goroutine was launched
	// with. Only set for dynamically-created address-sub streams (the
	// keys-sub and the initial token streams are launched directly against
	// the ctx passed to Start/startKeysFirehose); recorded here so the stream
	// and its cancel func (EventSubscriber.addrCancels) always agree on
	// exactly which context to cancel.
	runCtx context.Context

	mu      sync.Mutex
	cursors map[string]uint64

	fillsMu sync.Mutex
	fills   map[string]ContractSubscription
}

func newFirehoseKeysStream(label string, address *felt.Felt, keys [][]*felt.Felt) *firehoseKeysStream {
	return &firehoseKeysStream{
		label:   label,
		address: address,
		keys:    keys,
		cursors: make(map[string]uint64),
		fills:   make(map[string]ContractSubscription),
	}
}

// setCursor advances (or, if force, forcibly sets) this stream's cursor for
// addrHex.
func (st *firehoseKeysStream) setCursor(addrHex string, block uint64, force bool) {
	st.mu.Lock()
	if force || block > st.cursors[addrHex] {
		st.cursors[addrHex] = block
	}
	st.mu.Unlock()
}

// cursor returns this stream's current cursor for addrHex (0 if unset).
func (st *firehoseKeysStream) cursor(addrHex string) uint64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.cursors[addrHex]
}

// rollback moves every cursor on this stream that is at or past startBlock
// back to startBlock, for reorg recovery.
func (st *firehoseKeysStream) rollback(startBlock uint64) {
	st.mu.Lock()
	for addr, c := range st.cursors {
		if c > startBlock {
			st.cursors[addr] = startBlock
		}
	}
	st.mu.Unlock()
}

// setFill adds or replaces an entry in this stream's gap-fill responsibility
// set.
func (st *firehoseKeysStream) setFill(addrHex string, sub ContractSubscription) {
	st.fillsMu.Lock()
	st.fills[addrHex] = sub
	st.fillsMu.Unlock()
}

// removeFill drops an entry from this stream's gap-fill responsibility set.
func (st *firehoseKeysStream) removeFill(addrHex string) {
	st.fillsMu.Lock()
	delete(st.fills, addrHex)
	st.fillsMu.Unlock()
}

// snapshotFills returns a shallow copy of the current fill set for iteration
// without holding the lock across RPC calls.
func (st *firehoseKeysStream) snapshotFills() []ContractSubscription {
	st.fillsMu.Lock()
	out := make([]ContractSubscription, 0, len(st.fills))
	for _, sub := range st.fills {
		out = append(out, sub)
	}
	st.fillsMu.Unlock()
	return out
}

// --- EventSubscriber tracked-membership set -------------------------------

// trackContract adds sub to the shared tracked-membership set: from this
// point its events are eligible for forwarding on whichever stream(s) cover
// its address. Does NOT touch any stream's cursor.
func (s *EventSubscriber) trackContract(sub ContractSubscription) {
	s.trackedMu.Lock()
	s.tracked[sub.Address.String()] = sub
	s.trackedMu.Unlock()
}

// untrackContract removes addrHex from the shared tracked-membership set.
func (s *EventSubscriber) untrackContract(addrHex string) {
	s.trackedMu.Lock()
	delete(s.tracked, addrHex)
	s.trackedMu.Unlock()
}

// isTracked reports whether addrHex is currently in the tracked-membership set.
func (s *EventSubscriber) isTracked(addrHex string) bool {
	s.trackedMu.RLock()
	_, ok := s.tracked[addrHex]
	s.trackedMu.RUnlock()
	return ok
}

// --- Transport entry points ------------------------------------------------

// startKeysFirehose runs the firehose-keys transport: seed the shared
// tracked-membership set and the keys-sub's gap-fill set from the initial
// contract list, classifying each by Wildcard/ERC20 (see ContractSubscription
// and the design doc comment above); launch one goroutine per stream (the
// keys-sub, plus one address-sub per non-wildcard contract and per ERC20
// wildcard child); then block until ctx is canceled.
func (s *EventSubscriber) startKeysFirehose(ctx context.Context) error {
	// Record the subscriber-scoped context for the shared address-sub socket
	// (multiplexKeysDialer/sharedAddrWS) so it outlives any single stream, and
	// tear that socket down once every stream goroutine has exited.
	s.wsCtx = ctx
	defer s.closeSharedAddrWS()

	// An empty selector union is a misconfiguration (e.g. option-family ABIs
	// failed to resolve at setup). The keys-sub filter would then be [[]],
	// which Starknet treats as "match any" at position 0 — silently degrading
	// this transport to option C's whole-chain cost. It stays correct (the
	// router still drops untracked addresses) but loses the entire CU saving,
	// so make the failure loud rather than a silent cost regression.
	if len(s.optionSelectors) == 0 {
		s.logger.Error("firehose-keys: option selector union is EMPTY — keys-sub will match all chain events (no CU saving); check ABI resolution")
	}

	s.streamsMu.Lock()
	s.keysStream = newFirehoseKeysStream("keys-sub", nil, [][]*felt.Felt{s.optionSelectors})
	keysStream := s.keysStream
	s.streamsMu.Unlock()
	// Counted now, although it is launched only after the per-contract loop
	// below — see reserveKeysStream for why that ordering matters.
	s.reserveKeysStream()

	var wg sync.WaitGroup
	optionFamilyCount, tokenStreamCount, childStreamCount := 0, 0, 0

	for _, c := range s.contracts {
		s.trackContract(c)
		addrHex := c.Address.String()

		switch {
		case c.Wildcard && !c.ERC20:
			// Option-family, non-ERC20 (e.g. an OptionManager/Factory or an
			// OrderBook/Exerciser child): keys-sub coverage only.
			s.seedKeysStreamFill(keysStream, addrHex, c, c.StartBlock)
			optionFamilyCount++

		case c.Wildcard && c.ERC20:
			// Option-family AND ERC20 (an OptionToken child): keys-sub
			// coverage for its non-Transfer events, PLUS its own
			// Transfer/Approval address-sub.
			s.seedKeysStreamFill(keysStream, addrHex, c, c.StartBlock)
			optionFamilyCount++

			st := s.newChildTransferStream(ctx, c, c.StartBlock)
			s.startKeysStream(st.runCtx, st, &wg)
			childStreamCount++

		default:
			// Non-wildcard (a static ERC20 token or the UDC discovery
			// contract): its own address-sub, using its already-configured Keys.
			st := newFirehoseKeysStream("token:"+addrHex, c.Address, c.Keys)
			st.setFill(addrHex, c)
			st.setCursor(addrHex, c.StartBlock, true)

			streamCtx, cancel := context.WithCancel(ctx)
			s.streamsMu.Lock()
			s.addrStreams[addrHex] = st
			s.addrCancels[addrHex] = cancel
			s.streamsMu.Unlock()

			s.startKeysStream(streamCtx, st, &wg)
			tokenStreamCount++
		}
	}

	s.logger.Info("firehose-keys transport starting",
		"initial_contracts", len(s.contracts),
		"option_selectors", len(s.optionSelectors),
		"option_family_contracts", optionFamilyCount,
		"token_streams", tokenStreamCount,
		"child_transfer_streams", childStreamCount,
	)

	go s.reportTransportStatus(ctx)

	// Reserved at creation, above — before any other stream existed.
	s.launchReservedKeysStream(ctx, keysStream, &wg)

	wg.Wait()
	return ctx.Err()
}

// seedKeysStreamFill adds an option-family contract to the keys-sub's gap-fill
// set and seeds its cursor. The fill's Keys is overridden to the option
// selector union (rather than the contract's own possibly-empty Keys) so
// HTTP catch-up matches exactly what the keys-sub's own WSS filter would
// deliver — critically EXCLUDING Transfer/Approval so an ERC20 child's
// catch-up here never overlaps with its own Transfer/Approval address-sub's
// catch-up.
func (s *EventSubscriber) seedKeysStreamFill(keysStream *firehoseKeysStream, addrHex string, sub ContractSubscription, wssFrom uint64) {
	fillSub := sub
	fillSub.Keys = [][]*felt.Felt{s.optionSelectors}
	keysStream.setFill(addrHex, fillSub)
	keysStream.setCursor(addrHex, wssFrom, true)
}

// newChildTransferStream builds and registers (under s.streamsMu) a new
// Transfer/Approval address-sub stream for an ERC20 wildcard child, seeded to
// start forwarding from wssFrom. The stream's own context (derived from ctx)
// is stored on the stream so its cancel func, and the run-loop invocation,
// agree on the same context.
func (s *EventSubscriber) newChildTransferStream(ctx context.Context, sub ContractSubscription, wssFrom uint64) *firehoseKeysStream {
	addrHex := sub.Address.String()
	st := newFirehoseKeysStream("child-transfer:"+addrHex, sub.Address, [][]*felt.Felt{{transferSelector, approvalSelector}})

	fillSub := sub
	fillSub.Keys = st.keys
	st.setFill(addrHex, fillSub)
	st.setCursor(addrHex, wssFrom, true)

	streamCtx, cancel := context.WithCancel(ctx)
	st.runCtx = streamCtx

	s.streamsMu.Lock()
	s.addrStreams[addrHex] = st
	s.addrCancels[addrHex] = cancel
	s.streamsMu.Unlock()

	return st
}

// allKeysFirehoseStreams returns the keys-sub plus every currently registered
// address-sub stream, for operations that must touch every stream (namely
// rollbackAllStreams).
func (s *EventSubscriber) allKeysFirehoseStreams() []*firehoseKeysStream {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	out := make([]*firehoseKeysStream, 0, len(s.addrStreams)+1)
	if s.keysStream != nil {
		out = append(out, s.keysStream)
	}
	for _, st := range s.addrStreams {
		out = append(out, st)
	}
	return out
}

// rollbackAllStreams rolls every stream's cursors back for a reorg observed on
// ANY one of them — a reorg is a chain-wide event, so every stream (even ones
// that haven't seen it yet on their own WSS session) must reprocess the
// orphaned range.
func (s *EventSubscriber) rollbackAllStreams(startBlock uint64) {
	for _, st := range s.allKeysFirehoseStreams() {
		st.rollback(startBlock)
	}
}

// reserveKeysStream counts a stream in streamsTotal BEFORE it is launched. Pair
// every call with exactly one launchReservedKeysStream.
//
// Counting must precede launching. Counting inside the goroutine left a window
// where a launched-but-unscheduled stream contributed to neither counter, so a
// few fast streams going live could read as live == total > 0 —
// catchup_complete true — while slower streams had not counted themselves yet.
// The keys-sub makes that window wide: it is launched only after the per-
// contract loop (thousands of contracts in prod), so it is reserved as soon as
// it is created, before any other stream exists. It also keeps total >= live as
// an invariant: a stream is always counted before it can go live.
func (s *EventSubscriber) reserveKeysStream() {
	s.streamsTotal.Add(1)
}

// launchReservedKeysStream runs st's lifecycle in a goroutine and releases its
// reservation when that lifecycle ends. wg may be nil for dynamically added
// streams nobody waits on.
func (s *EventSubscriber) launchReservedKeysStream(ctx context.Context, st *firehoseKeysStream, wg *sync.WaitGroup) {
	if wg != nil {
		wg.Add(1)
	}
	go func() {
		defer s.streamsTotal.Add(-1)
		if wg != nil {
			defer wg.Done()
		}
		_ = s.runFirehoseKeysStream(ctx, st)
	}()
}

// startKeysStream reserves and launches in one step, for streams that are
// launched as soon as they are created.
func (s *EventSubscriber) startKeysStream(ctx context.Context, st *firehoseKeysStream, wg *sync.WaitGroup) {
	s.reserveKeysStream()
	s.launchReservedKeysStream(ctx, st, wg)
}

// runFirehoseKeysStream is one stream's connection lifecycle, mirroring
// runFirehoseWSS but scoped to a single stream: before each (re)connect it
// HTTP-fills every contract in the stream's own responsibility set up to near
// tip, then opens THIS stream's WSS subscription (chain-wide keys filter for
// the keys-sub; from_address+keys for an address-sub) resumed from the MIN
// cursor across its fills, and forwards matching events until the session
// drops.
func (s *EventSubscriber) runFirehoseKeysStream(ctx context.Context, st *firehoseKeysStream) error {
	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		fromBlock, err := s.keysStreamGapFill(ctx, st)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// Subscribing anyway would resume from a block the node can no
			// longer replay and write a permanent hole into the store. Back off
			// and re-run the whole gap-fill instead. The stream stays
			// not-live meanwhile, which /v1/catchup_status reports and a
			// promote gate blocks on, rather than looking healthy while losing
			// events.
			s.logger.Error("firehose-keys gap-fill failed; not subscribing",
				"stream", st.label, "error", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = time.Duration(math.Min(float64(backoff)*2, float64(maxBackoff)))
			continue
		}

		subInput := &rpc.EventSubscriptionInput{
			FromAddress: st.address,
			Keys:        st.keys,
		}
		if fromBlock > 0 {
			bn := fromBlock
			subInput.SubBlockID = rpc.SubscriptionBlockID{Number: &bn}
		}

		session, err := s.dialWSS(ctx, s.provider.wsURL, subInput)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.logger.Warn("firehose-keys WSS dial failed, retrying",
				"stream", st.label, "error", err, "backoff", backoff, "from_block", fromBlock)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = time.Duration(math.Min(float64(backoff)*2, float64(maxBackoff)))
			continue
		}
		backoff = minBackoff
		s.logger.Info("firehose-keys WSS active", "stream", st.label, "from_block", fromBlock)

		// Live only for the duration of the session: gap-fill above and the
		// reconnect backoff below both count as not-live, which is exactly
		// what a promote gate needs to distinguish from "serving fine".
		s.streamsLive.Add(1)
		err = s.processKeysStream(ctx, st, session)
		s.streamsLive.Add(-1)
		session.close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.logger.Warn("firehose-keys WSS session ended, reconnecting",
			"stream", st.label, "error", err, "backoff", backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = time.Duration(math.Min(float64(backoff)*2, float64(maxBackoff)))
	}
}

// keysStreamGapFill brings every contract in st's gap-fill set within
// catchupThreshold of chain tip over HTTP, then returns the min cursor across
// them to resume st's subscription from.
//
// The fan-out is REPEATED until the whole set sits within catchupThreshold of
// the tip at the same time. One pass is not enough: the pass returns only when
// its slowest contract finishes, and with thousands of fills sharing
// maxConcurrentCatchup that wait can run for over an hour. Contracts that
// finished early are stale by then, and the resume block handed to WSS is as
// old as the slowest cursor -- far outside the node's replay window, so every
// event in between is dropped silently and permanently. Repeating costs little:
// each pass only covers the blocks produced during the previous one, so cursors
// converge on the tip geometrically, and the resume lands inside the replay
// window where the node can actually redeliver.
// Returns an error rather than a usable resume block whenever it cannot get
// the whole set to the tip. Every failure here would mean subscribing from a
// block the node can no longer replay, i.e. writing a permanent hole into the
// store -- so the caller must retry, never proceed.
func (s *EventSubscriber) keysStreamGapFill(ctx context.Context, st *firehoseKeysStream) (uint64, error) {
	var prev uint64
	var prevFills int
	var regrowths int // consecutive passes excused from the guard by growth
	for pass := 1; ; pass++ {
		minLast, fills := s.keysStreamGapFillPass(ctx, st)
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}

		// A tip we cannot read is not evidence of convergence. Treating it as
		// such is how a resume block from an hour ago looks acceptable.
		//
		// Deliberately UNCACHED. This is the one read that decides whether it is
		// safe to subscribe, and s.tipBlockNumber goes through CachedBlockNumber
		// on this transport — which, when the RPC fails, returns the last cached
		// tip with a nil error, however stale. A stale, low tip makes a lagging
		// cursor look converged and hands WSS a resume block outside the replay
		// window. One direct RPC per pass is negligible next to the pass itself.
		tip, err := s.provider.BlockNumber(ctx)
		if err != nil {
			return 0, fmt.Errorf("reading chain tip after gap-fill pass %d: %w", pass, err)
		}

		// Converged once the laggard is within the same threshold the pass
		// itself stops at.
		if minLast+catchupThreshold >= tip {
			return minLast, nil
		}

		// No fixed pass cap: a pass only has to cover the blocks the previous
		// one took, so the count needed is not knowable up front -- a first
		// pass measured at 77 minutes in prod would blow any constant worth
		// picking. Terminate on the property that actually matters instead:
		// the GAP must shrink. A cursor that advances while the gap grows is
		// catchup losing the race with the chain, and more passes cannot win
		// it -- checking only that the cursor moved would spin forever there.
		behind := tip - minLast
		//
		// ...but only when both passes covered the same set. addContractKeysFirehose
		// registers children mid-flight, each seeded at its deploy block —
		// potentially millions of blocks back. A pass that picks one up does more
		// work, runs longer, and lets the early finishers go staler, so its gap
		// can grow for an entirely healthy reason. Treating that as divergence
		// would take down the keys-sub of an indexer that merely discovered a new
		// option token. When the set grows, re-baseline instead of comparing.
		if gapFillDiverging(pass, fills, prevFills, behind, prev) {
			return 0, fmt.Errorf(
				"gap-fill is not converging: %d blocks behind after pass %d (was %d, %d fills): "+
					"catchup is not outrunning the chain", behind, pass, prev, fills)
		}
		// Growth is excused, but not silently. get_or_deploy is permissionless by
		// design, so anyone can grow the set, and set growth that outpaces the
		// passes keeps this loop re-baselining forever: the stream never goes
		// live, catchup_complete stays false, and a promote gate blocks. That is
		// the safe failure — nothing is reported current that is not — but it
		// must be distinguishable from ordinary catch-up, hence a WARN with a
		// consecutive count an operator can alert on.
		if gapFillRegrew(pass, fills, prevFills, behind, prev) {
			regrowths++
			s.logger.Warn("firehose-keys gap-fill set grew mid-pass; re-baselining instead of failing",
				"stream", st.label, "pass", pass, "fills", fills, "prev_fills", prevFills,
				"behind", behind, "prev_behind", prev, "consecutive_regrowths", regrowths)
		} else {
			regrowths = 0
		}
		prev, prevFills = behind, fills

		s.logger.Info("firehose-keys gap-fill still behind tip; repeating",
			"stream", st.label, "pass", pass, "resume_block", minLast,
			"tip", tip, "behind", tip-minLast)
	}
}

// gapFillDiverging reports whether a COMPLETED gap-fill pass shows catchup
// losing to the chain: the gap failed to shrink across two passes over a set
// that did not grow. A set that shrank (a contract removed) does less work, so a
// non-shrinking gap there is divergence too.
//
// Kept as a pure function so the comparison is table-tested directly. An
// end-to-end test cannot reach this reliably: when catchup genuinely loses, it
// loses INSIDE a pass (pollUntilCaughtUp is unbounded), so this guard only ever
// sees the narrower finished-but-regressed case.
func gapFillDiverging(pass, fills, prevFills int, behind, prevBehind uint64) bool {
	return pass > 1 && fills <= prevFills && behind >= prevBehind
}

// gapFillRegrew reports the case gapFillDiverging deliberately excuses: the gap
// did not shrink, but the set grew, which explains the extra work. The loop
// re-baselines instead of failing, and logs it.
func gapFillRegrew(pass, fills, prevFills int, behind, prevBehind uint64) bool {
	return pass > 1 && fills > prevFills && behind >= prevBehind
}

// keysStreamGapFillPass runs one gap-fill fan-out over st's current fill set and
// returns the min cursor across them, plus the size of the set it fanned out
// over. Mirrors firehoseGapFill, scoped to one stream's own fills and own
// cursors.
//
// The count is returned rather than re-read by the caller because the set is
// mutable — addContractKeysFirehose registers children mid-flight — so a
// separate snapshot could disagree with the one this pass actually used.
func (s *EventSubscriber) keysStreamGapFillPass(ctx context.Context, st *firehoseKeysStream) (uint64, int) {
	fills := st.snapshotFills()
	if len(fills) == 0 {
		bn, err := s.tipBlockNumber(ctx)
		if err != nil {
			return 0, 0
		}
		return bn, 0
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	minLast := uint64(math.MaxUint64)
	for _, sub := range fills {
		wg.Add(1)
		go func(sub ContractSubscription) {
			defer wg.Done()
			addrHex := sub.Address.String()
			last := st.cursor(addrHex)
			logger := s.logger.With("contract", sub.Address, "stream", st.label)
			if err := s.pollUntilCaughtUp(ctx, sub, &last, logger); err != nil {
				return // ctx canceled
			}
			st.setCursor(addrHex, last, false)
			mu.Lock()
			if last < minLast {
				minLast = last
			}
			mu.Unlock()
		}(sub)
	}
	wg.Wait()

	if minLast == math.MaxUint64 {
		bn, err := s.tipBlockNumber(ctx)
		if err != nil {
			return 0, len(fills)
		}
		return bn, len(fills)
	}
	return minLast, len(fills)
}

// processKeysStream reads one stream's session until it errors or ctx is
// canceled, forwarding tracked events and propagating reorgs. Mirrors
// processFirehose, scoped to a single stream.
func (s *EventSubscriber) processKeysStream(ctx context.Context, st *firehoseKeysStream, session *wssSession) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-session.errs:
			return fmt.Errorf("firehose-keys subscription error (%s): %w", st.label, err)

		case reorg := <-session.reorgs:
			if reorg != nil {
				s.logger.Warn("firehose-keys reorg",
					"stream", st.label, "start_block", reorg.StartBlockNum, "end_block", reorg.EndBlockNum)
				if s.reorgs != nil {
					select {
					case s.reorgs <- ReorgNotification{StartBlock: reorg.StartBlockNum, EndBlock: reorg.EndBlockNum}:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				// A reorg is chain-wide: roll EVERY stream back, not just this
				// one, so addresses covered by other streams are reprocessed too.
				s.rollbackAllStreams(reorg.StartBlockNum)
			}

		case evt := <-session.events:
			if evt == nil {
				continue
			}
			s.forwardStream(ctx, st, evt)
		}
	}
}

// forwardStream routes one event observed on stream st: drop it unless its
// from_address is in the shared tracked-membership set and its block is
// at/after THIS STREAM's own cursor for that address (the per-stream dedup
// guard — see the package doc comment on cursor isolation), then enrich with
// a timestamp and hand it to the engine. Advances st's cursor to the event's
// block only after a successful send, so a blocked/canceled send never
// silently loses the event by advancing past it first.
func (s *EventSubscriber) forwardStream(ctx context.Context, st *firehoseKeysStream, evt *rpc.EmittedEventWithFinalityStatus) {
	if evt.FromAddress == nil {
		return
	}
	addr := evt.FromAddress.String()

	if !s.isTracked(addr) {
		return // untracked, or a foreign contract's same-named event — dropped
	}

	st.mu.Lock()
	last := st.cursors[addr]
	if evt.BlockNumber < last {
		st.mu.Unlock()
		return // already forwarded on THIS stream (per-stream dedup guard)
	}
	st.mu.Unlock()

	var ts uint64
	if t, err := s.provider.GetBlockTimestamp(ctx, evt.BlockNumber); err == nil {
		ts = t
	}

	raw := RawEvent{
		BlockNumber:     evt.BlockNumber,
		BlockHash:       evt.BlockHash,
		TransactionHash: evt.TransactionHash,
		ContractAddress: evt.FromAddress,
		Keys:            evt.Keys,
		Data:            evt.Data,
		FinalityStatus:  string(evt.FinalityStatus),
		Timestamp:       ts,
	}

	select {
	case s.events <- raw:
		st.mu.Lock()
		if evt.BlockNumber > st.cursors[addr] {
			st.cursors[addr] = evt.BlockNumber
		}
		st.mu.Unlock()
	case <-ctx.Done():
	}
}

// addContractKeysFirehose registers a contract with the running firehose-keys
// transport. Mirrors addContractFirehose's backfill/live partition at a
// captured tip, generalized to option D's multi-stream routing (see the
// design doc comment above):
//   - always added to the shared tracked set;
//   - added to the (already-running) keys-sub's fill set with its cursor
//     seeded at tip+1 — non-ERC20 children need nothing more, their events
//     were always going to arrive on the shared keys-sub;
//   - if ERC20 (an OptionToken child), ALSO gets its own new Transfer/Approval
//     address-sub stream, launched now;
//   - history [StartBlock, tip] is backfilled once over HTTP with NO key
//     filter — covering BOTH event classes in a single fetch — so future
//     events split cleanly at tip between the keys-sub and (if ERC20) the new
//     address-sub, with no gap and no overlap.
func (s *EventSubscriber) addContractKeysFirehose(ctx context.Context, sub ContractSubscription) {
	s.trackContract(sub)
	addrHex := sub.Address.String()

	tip, err := s.tipBlockNumber(ctx)
	if err != nil {
		// No tip available: forward from StartBlock and skip backfill; the
		// next gap-fill cycle (on stream reconnect) will reconcile.
		s.streamsMu.Lock()
		keysStream := s.keysStream
		s.streamsMu.Unlock()
		if keysStream != nil {
			s.seedKeysStreamFill(keysStream, addrHex, sub, sub.StartBlock)
		}
		if sub.ERC20 {
			st := s.newChildTransferStream(ctx, sub, sub.StartBlock)
			s.startKeysStream(st.runCtx, st, nil)
		}
		return
	}

	wssFrom := tip + 1
	if sub.StartBlock > wssFrom {
		wssFrom = sub.StartBlock
	}

	s.streamsMu.Lock()
	keysStream := s.keysStream
	s.streamsMu.Unlock()
	if keysStream != nil {
		s.seedKeysStreamFill(keysStream, addrHex, sub, wssFrom)
	}

	if sub.ERC20 {
		st := s.newChildTransferStream(ctx, sub, wssFrom)
		s.startKeysStream(st.runCtx, st, nil)
	}

	if sub.StartBlock <= tip {
		go func() {
			backfillSub := sub
			backfillSub.Keys = nil // no filter: one fetch covers both event classes
			if err := s.Backfill(ctx, backfillSub, sub.StartBlock, tip); err != nil && ctx.Err() == nil {
				s.logger.Error("firehose-keys backfill failed", "contract", sub.Address, "error", err)
			}
		}()
	}
}

// removeContractKeysFirehose freezes a contract on the firehose-keys
// transport: it stops being forwarded (tracked set), its keys-sub fill is
// dropped, and if it owns its own address-sub stream (a static token/UDC sub,
// or a child's Transfer/Approval sub), that stream is canceled and removed.
func (s *EventSubscriber) removeContractKeysFirehose(addrHex string) {
	s.untrackContract(addrHex)

	s.streamsMu.Lock()
	keysStream := s.keysStream
	cancel, hasOwnStream := s.addrCancels[addrHex]
	if hasOwnStream {
		delete(s.addrCancels, addrHex)
		delete(s.addrStreams, addrHex)
	}
	s.streamsMu.Unlock()

	if keysStream != nil {
		keysStream.removeFill(addrHex)
	}
	if hasOwnStream {
		cancel()
	}
}
