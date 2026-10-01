package provider

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/NethermindEth/juno/core/felt"
)

// reconcile.go: periodic HTTP reconciliation of every live stream.
//
// A live WSS subscription can silently miss events while connected (measured
// upstream of ibis: a chain-wide keys subscription delivered nothing from three
// transactions over ~4h with no error and no reconnect), so ibis must not
// assume a live stream is complete. While a stream is live, the events its live
// path delivered are recorded in a bounded "seen" set; every reconcileInterval
// the stream's own scope is re-read over HTTP for the blocks the live stream has
// had time to deliver (up to tip - reconcileLag) and any event not in the seen
// set is delivered with IsCatchup=true and logged at WARN.
//
// Hand-off: the reconciler starts at P, the end of the post-subscribe backfill
// (see subscribe_backfill.go), so backfill covers [resume, P] and reconcile
// covers everything after, with no gap. A reconciler lives for one session;
// after a reconnect the gap-fill + backfill cover from the cursors and a fresh
// reconciler starts at the new P.

const (
	// DefaultReconcileInterval is the engine's default when
	// indexer.reconcile_interval is unset. 0 disables reconciliation.
	DefaultReconcileInterval = 60 * time.Second

	// defaultReconcileLag is how many blocks behind the accepted tip are left to
	// the live stream before they are checked.
	defaultReconcileLag uint64 = 2

	// reconcileWindow hard-bounds the seen set: blocks older than this many
	// blocks behind the newest seen one are dropped even if reconcile has not
	// caught up (a stall longer than this may re-deliver, never grows memory).
	reconcileWindow uint64 = 1024
)

// eventID identifies an event's content within a block: a hash of tx hash,
// emitter, all keys and all data. Identical events in one tx (same content) are
// told apart by a per-id occurrence count, see reconciler.missing.
type eventID [sha256.Size]byte

func idOf(tx, from *felt.Felt, keys, data []*felt.Felt) eventID {
	h := sha256.New()
	put := func(f *felt.Felt) {
		if f == nil {
			h.Write([]byte{0})
			return
		}
		b := f.Bytes()
		h.Write([]byte{1})
		h.Write(b[:])
	}
	put(tx)
	put(from)
	for _, l := range [][]*felt.Felt{keys, data} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(l)))
		h.Write(n[:])
		for _, f := range l {
			put(f)
		}
	}
	var id eventID
	h.Sum(id[:0])
	return id
}

func rawID(e RawEvent) eventID {
	return idOf(e.TransactionHash, e.ContractAddress, e.Keys, e.Data)
}

// reconciler is one live session's reconcile state. All methods are safe for
// concurrent use; observe/rollback are no-ops on a nil receiver (reconcile off).
type reconciler struct {
	mu    sync.Mutex
	seen  map[uint64]map[eventID]int // block -> id -> times the live path delivered it
	hi    uint64                     // newest block seen live
	last  uint64                     // last reconciled block (valid once ready)
	ready bool                       // start() ran: the post-subscribe backfill is done
}

func newReconciler() *reconciler {
	return &reconciler{seen: make(map[uint64]map[eventID]int)}
}

// addLocked records one delivery and prunes blocks outside the window.
func (r *reconciler) addLocked(block uint64, id eventID) {
	m := r.seen[block]
	if m == nil {
		m = make(map[eventID]int)
		r.seen[block] = m
	}
	m[id]++
	if block > r.hi {
		r.hi = block
		if r.hi > reconcileWindow {
			r.pruneLocked(r.hi - reconcileWindow)
		}
	}
}

// pruneLocked drops every block below floor.
func (r *reconciler) pruneLocked(floor uint64) {
	for b := range r.seen {
		if b < floor {
			delete(r.seen, b)
		}
	}
}

// observe records an event the live path delivered.
func (r *reconciler) observe(block uint64, id eventID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ready && block <= r.last {
		return // already reconciled
	}
	r.addLocked(block, id)
}

// start marks the post-subscribe backfill done through p: reconcile begins at p+1.
func (r *reconciler) start(p uint64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last, r.ready = p, true
	r.pruneLocked(p + 1)
}

// state returns the last reconciled block and whether start has run.
func (r *reconciler) state() (last uint64, ready bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last, r.ready
}

// advance records [.., to] as reconciled and forgets those blocks.
func (r *reconciler) advance(to uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if to > r.last {
		r.last = to
		r.pruneLocked(to + 1)
	}
}

// resumeFloor is the lowest block a reconnect must resume at so the unreconciled
// range is not skipped; 0 = no constraint (never started or reconcile off).
func (r *reconciler) resumeFloor() uint64 {
	if r == nil {
		return 0
	}
	last, ready := r.state()
	if !ready {
		return 0
	}
	return last + 1
}

// rollback handles a reorg from startBlock: blocks >= it were orphaned, the
// live stream will redeliver them, and they must be reconciled again.
func (r *reconciler) rollback(startBlock uint64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for b := range r.seen {
		if b >= startBlock {
			delete(r.seen, b)
		}
	}
	if r.ready && startBlock > 0 && r.last >= startBlock {
		r.last = startBlock - 1
	}
}

// missing returns, in order, the events of an HTTP fetch that the live path did
// not deliver, and records them as delivered. Per (block, id) the live count is
// subtracted from the fetched count, so a dropped event among identical ones in
// one tx is still found.
func (r *reconciler) missing(events []RawEvent) []RawEvent {
	type key struct {
		block uint64
		id    eventID
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	local := make(map[key]int)
	var out []RawEvent
	for _, e := range events {
		id := rawID(e)
		k := key{e.BlockNumber, id}
		n := local[k]
		local[k] = n + 1
		if n >= r.seen[e.BlockNumber][id] {
			r.addLocked(e.BlockNumber, id)
			out = append(out, e)
		}
	}
	return out
}

// newSessionReconciler returns a fresh reconciler, or nil when reconcile is off.
func (s *EventSubscriber) newSessionReconciler() *reconciler {
	if s.reconcileInterval <= 0 {
		return nil
	}
	return newReconciler()
}

// reconcileScope is what one stream (or sink, or contract) reconciles.
type reconcileScope struct {
	label     string
	fetch     func(ctx context.Context, from, to uint64) ([]RawEvent, error) // the stream's own filter
	keep      func(RawEvent) bool                                            // routing, as the live path
	delivered func(RawEvent)                                                 // cursor bookkeeping; may be nil
}

// reconcileLoop runs ticks until ctx ends. A failed tick leaves lastReconciled
// where it was and is retried sooner (1s, 2s, ... capped at the interval).
func (s *EventSubscriber) reconcileLoop(ctx context.Context, rec *reconciler, sc reconcileScope) {
	logger := s.logger.With("stream", sc.label, "action", "reconcile")
	wait, retry := s.reconcileInterval, minBackoff
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		err := s.reconcileTick(ctx, rec, sc, logger)
		if err == nil || ctx.Err() != nil {
			wait, retry = s.reconcileInterval, minBackoff
			continue
		}
		logger.Warn("reconcile failed; will retry", "error", err, "retry_in", min(retry, s.reconcileInterval))
		wait = min(retry, s.reconcileInterval)
		retry *= 2
	}
}

// reconcileTarget returns the newest block to check: the accepted tip minus lag.
func (s *EventSubscriber) reconcileTarget(ctx context.Context) (uint64, error) {
	rctx, cancel := context.WithTimeout(ctx, rpcCallTimeout)
	defer cancel()
	tip, err := s.provider.BlockNumber(rctx) // uncached: decides what we fetch
	if err != nil {
		return 0, fmt.Errorf("reading tip: %w", err)
	}
	if tip < s.reconcileLag {
		return 0, nil
	}
	return tip - s.reconcileLag, nil
}

func (s *EventSubscriber) reconcileTick(ctx context.Context, rec *reconciler, sc reconcileScope, logger *slog.Logger) error {
	to, err := s.reconcileTarget(ctx)
	if err != nil {
		return err
	}
	last, _ := rec.state()
	if to <= last {
		return nil
	}
	return s.reconcileRange(ctx, rec, sc, last+1, to, logger)
}

// reconcileRange re-reads [from, to] in chunks and delivers what the live path
// missed. lastReconciled advances per fully handled chunk only, so an error
// leaves the rest to the next tick.
func (s *EventSubscriber) reconcileRange(ctx context.Context, rec *reconciler, sc reconcileScope, from, to uint64, logger *slog.Logger) error {
	recovered := 0
	for cur := from; cur <= to; {
		end := min(cur+s.blocksPerQuery-1, to)
		events, err := s.reconcileFetch(ctx, sc.fetch, cur, end)
		if err != nil {
			return fmt.Errorf("events [%d, %d]: %w", cur, end, err)
		}
		kept := events[:0]
		for _, e := range events {
			if e.ContractAddress != nil && sc.keep(e) {
				kept = append(kept, e)
			}
		}
		n, err := s.deliverMissed(ctx, rec, sc, kept, logger)
		recovered += n
		if err != nil {
			return err
		}
		rec.advance(end)
		cur = end + 1
	}
	if recovered > 0 {
		logger.Warn("reconcile recovered events the live stream missed", "from", from, "to", to, "recovered", recovered)
	}
	return nil
}

// reconcileFetch runs one fetch under the shared RPC semaphore.
func (s *EventSubscriber) reconcileFetch(ctx context.Context, fetch func(context.Context, uint64, uint64) ([]RawEvent, error), from, to uint64) ([]RawEvent, error) {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.sem }()
	rctx, cancel := context.WithTimeout(ctx, rpcCallTimeout)
	defer cancel()
	return fetch(rctx, from, to)
}

// deliverMissed sends the events of kept that rec has not seen.
func (s *EventSubscriber) deliverMissed(ctx context.Context, rec *reconciler, sc reconcileScope, kept []RawEvent, logger *slog.Logger) (int, error) {
	miss := rec.missing(kept)
	if len(miss) == 0 {
		return 0, nil
	}
	s.resolveTimestamps(ctx, miss, logger)
	for i, e := range miss {
		e.IsCatchup = true
		logger.Warn("live stream missed event; recovered by reconcile",
			"stream", sc.label, "block", e.BlockNumber, "tx", e.TransactionHash, "address", e.ContractAddress)
		select {
		case s.events <- e:
			if sc.delivered != nil {
				sc.delivered(e)
			}
		case <-ctx.Done():
			return i, ctx.Err()
		}
	}
	return len(miss), nil
}

// --- firehose-keys streams ---------------------------------------------------

// streamReconciler returns a fresh reconciler for st's session, or nil when st
// is not reconciled.
func (s *EventSubscriber) streamReconciler(st *firehoseKeysStream) *reconciler {
	if st.address != nil {
		return nil // address-subs: later commit
	}
	return s.newSessionReconciler()
}

// streamReconcileRun returns the loop to run while st's session is live (nil = none).
func (s *EventSubscriber) streamReconcileRun(st *firehoseKeysStream, rec *reconciler) func(context.Context) {
	if rec == nil {
		return nil
	}
	return func(ctx context.Context) { s.reconcileLoop(ctx, rec, s.keysStreamScope(st)) }
}

// keysStreamScope is st's own subscription filter, routed like keysStreamBackfill:
// tracked contracts only.
func (s *EventSubscriber) keysStreamScope(st *firehoseKeysStream) reconcileScope {
	return reconcileScope{
		label: st.label,
		fetch: func(ctx context.Context, from, to uint64) ([]RawEvent, error) {
			return s.provider.GetEvents(ctx, GetEventsOptions{
				FromBlock: from, ToBlock: to, Address: st.address, Keys: st.keys, ChunkSize: 1000,
			})
		},
		keep: func(e RawEvent) bool { return s.isTracked(e.ContractAddress.String()) },
		delivered: func(e RawEvent) {
			st.setCursor(e.ContractAddress.String(), e.BlockNumber, false)
		},
	}
}
