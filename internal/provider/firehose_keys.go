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
			wg.Add(1)
			go func(st *firehoseKeysStream) {
				defer wg.Done()
				_ = s.runFirehoseKeysStream(st.runCtx, st)
			}(st)
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

			wg.Add(1)
			go func(st *firehoseKeysStream, sc context.Context) {
				defer wg.Done()
				_ = s.runFirehoseKeysStream(sc, st)
			}(st, streamCtx)
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

	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.runFirehoseKeysStream(ctx, keysStream)
	}()

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

		fromBlock := s.keysStreamGapFill(ctx, st)
		if ctx.Err() != nil {
			return ctx.Err()
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

		err = s.processKeysStream(ctx, st, session)
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
// them to resume st's subscription from. Mirrors firehoseGapFill, scoped to
// one stream's own fills and own cursors.
func (s *EventSubscriber) keysStreamGapFill(ctx context.Context, st *firehoseKeysStream) uint64 {
	fills := st.snapshotFills()
	if len(fills) == 0 {
		bn, err := s.tipBlockNumber(ctx)
		if err != nil {
			return 0
		}
		return bn
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
			return 0
		}
		return bn
	}
	return minLast
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
			go func() { _ = s.runFirehoseKeysStream(st.runCtx, st) }()
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
		go func() { _ = s.runFirehoseKeysStream(st.runCtx, st) }()
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
