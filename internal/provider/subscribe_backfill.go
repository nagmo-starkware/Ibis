package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NethermindEth/starknet.go/rpc"
)

// subscribe_backfill.go closes the gap a WSS (re)subscribe leaves.
//
// starknet_subscribeEvents is NOT trusted to replay from the requested
// block_id: measured on Alchemy (RPC v0.10) it replies with only the
// subscription id and delivers events from roughly the latest accepted block
// at subscribe time, so everything between our resume block and that point
// (up to catchupThreshold blocks of gap-fill slack, plus whatever lands while
// we connect) would be lost. So after the subscribe reply every site reads the
// pre-confirmed block number P and HTTP-backfills [resume, P] for the
// subscription's own scope. Duplicates with the live stream are fine; gaps are
// not. The stream counts as live only once that backfill has finished.

const (
	// preConfirmedAttempts is how often the pre-confirmed block is read after a
	// subscribe before the session is abandoned.
	preConfirmedAttempts = 3

	// acceptWaitTimeout bounds the wait for block P to be accepted (getEvents
	// with a numeric upper bound P only works once P is).
	acceptWaitTimeout = 2 * time.Minute
)

// dialWithGap dials a subscription and, once the node has replied, reads the
// pre-confirmed block number P. A failed read closes the session: it is
// reported like a failed dial, never used without a known P.
func (s *EventSubscriber) dialWithGap(ctx context.Context, input *rpc.EventSubscriptionInput) (*wssSession, uint64, error) {
	session, err := s.dialWSS(ctx, s.provider.wsURL, input)
	if err != nil {
		return nil, 0, err
	}
	p, err := s.readPreConfirmed(ctx)
	if err != nil {
		session.close()
		return nil, 0, fmt.Errorf("reading pre_confirmed block after subscribe: %w", err)
	}
	return session, p, nil
}

func (s *EventSubscriber) readPreConfirmed(ctx context.Context) (uint64, error) {
	var err error
	for attempt := 1; attempt <= preConfirmedAttempts; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, rpcCallTimeout)
		var p uint64
		p, err = s.provider.PreConfirmedBlockNumber(rctx)
		cancel()
		if err == nil {
			return p, nil
		}
		if attempt < preConfirmedAttempts {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(s.catchupPollInterval):
			}
		}
	}
	return 0, err
}

// waitAccepted blocks until the latest accepted block is >= p.
func (s *EventSubscriber) waitAccepted(ctx context.Context, p uint64) error {
	timeout := time.NewTimer(acceptWaitTimeout)
	defer timeout.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, rpcCallTimeout)
		tip, err := s.provider.BlockNumber(rctx) // uncached: this decides what we fetch
		cancel()
		if err == nil && tip >= p {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("block %d not accepted within %s (latest %d, err %v)", p, acceptWaitTimeout, tip, err)
		case <-time.After(s.tipPollInterval):
		}
	}
}

// errBackfillFailed marks a session ended by its own post-subscribe backfill
// failing (as opposed to the session dropping first), so the caller escalates
// its reconnect backoff.
var errBackfillFailed = errors.New("post-subscribe backfill failed")

// backoffAfterSession is the reconnect backoff after a session: reset unless
// the session ended because its backfill failed.
func backoffAfterSession(cur time.Duration, err error) time.Duration {
	if errors.Is(err, errBackfillFailed) {
		return cur
	}
	return minBackoff
}

// serveSession runs process (the session's event loop) while concurrently
// waiting for P to be accepted and running backfill(p), so live events keep
// draining and the socket never stalls. The stream goes live only after the
// backfill succeeds. A failed backfill ends the session; backfilled reports
// whether it completed, so the caller can pull cursors back for an incomplete
// one (live events may have advanced them past the unfilled gap).
//
// reconcile (nil = off) runs once the stream is live, until the session ends;
// the caller starts its reconciler at p inside backfill (see reconcile.go).
func (s *EventSubscriber) serveSession(ctx context.Context, live *streamLiveness, p uint64,
	backfill func(ctx context.Context, p uint64) error, process func(ctx context.Context) error,
	reconcile func(ctx context.Context),
) (backfilled bool, err error) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var ended atomic.Bool // process returned: a later backfill error is just the cancel
	var bfErr error
	failed := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		bfErr = s.waitAccepted(sctx, p)
		if bfErr == nil {
			bfErr = backfill(sctx, p)
		}
		if bfErr != nil {
			failed = !ended.Load()
			cancel()
			return
		}
		live.set(true)
		if reconcile != nil {
			reconcile(sctx)
		}
	}()

	err = process(sctx)
	ended.Store(true)
	cancel()
	<-done
	if failed && ctx.Err() == nil {
		err = fmt.Errorf("%w [..%d]: %w", errBackfillFailed, p, bfErr)
	}
	return bfErr == nil, err
}

// --- shared firehose (option C) --------------------------------------------

// sinkBase snapshots each sink's cursor at session start. The backfill uses it
// instead of the live cursor, which live events advance concurrently.
type sinkBase struct {
	sub  ContractSubscription
	last uint64
}

func (s *EventSubscriber) snapshotSinkBase() map[string]sinkBase {
	s.routerMu.RLock()
	defer s.routerMu.RUnlock()
	out := make(map[string]sinkBase, len(s.router))
	for addr, sk := range s.router {
		out[addr] = sinkBase{sub: sk.sub, last: sk.lastBlock}
	}
	return out
}

// firehoseBackfill fetches [last, p] for every sink in base over HTTP, bounded
// by the shared catchup semaphore. Returns the first error.
func (s *EventSubscriber) firehoseBackfill(ctx context.Context, base map[string]sinkBase, p uint64) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(base))
	for _, b := range base {
		if b.last > p {
			continue
		}
		wg.Add(1)
		go func(b sinkBase) {
			defer wg.Done()
			select {
			case s.sem <- struct{}{}:
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			}
			defer func() { <-s.sem }()
			if _, err := s.backfillFrom(ctx, b.sub, b.last, p); err != nil {
				errs <- err
			}
		}(b)
	}
	wg.Wait()
	close(errs)
	return <-errs // nil when empty
}

// capSinks pulls sink cursors back to base (never forward), for an incomplete
// backfill: a reconnect's gap-fill must start below the unfilled gap.
func (s *EventSubscriber) capSinks(base map[string]sinkBase) {
	s.routerMu.Lock()
	defer s.routerMu.Unlock()
	for addr, b := range base {
		if sk := s.router[addr]; sk != nil && sk.lastBlock > b.last {
			sk.lastBlock = b.last
		}
	}
}

// --- firehose-keys (option D) ----------------------------------------------

// streamBase snapshots the cursors of st's fill set at session start (see
// sinkBase). Its keys are exactly the contracts the backfill may deliver.
func (st *firehoseKeysStream) streamBase() map[string]uint64 {
	out := make(map[string]uint64)
	for _, sub := range st.snapshotFills() {
		addr := sub.Address.String()
		out[addr] = st.cursor(addr)
	}
	return out
}

// capCursors pulls st's cursors back to base (never forward); see capSinks.
func (st *firehoseKeysStream) capCursors(base map[string]uint64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for addr, c := range base {
		if st.cursors[addr] > c {
			st.cursors[addr] = c
		}
	}
}

// capCursorsTo pulls st's cursors back to at most block (never forward): after
// a session that went live, a reconnect must resume at/below the first
// unreconciled block even if live events advanced the cursors past it.
func (st *firehoseKeysStream) capCursorsTo(block uint64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for addr, c := range st.cursors {
		if c > block {
			st.cursors[addr] = block
		}
	}
}

// keysStreamBackfill fetches [from, p] with st's own subscription filter
// (address and keys) and routes the events like forwardStream: only tracked
// contracts in base, and only at/after their base cursor (earlier blocks were
// already delivered by the gap-fill, or by a late joiner's own backfill).
func (s *EventSubscriber) keysStreamBackfill(ctx context.Context, st *firehoseKeysStream, base map[string]uint64, from, p uint64) error {
	logger := s.logger.With("stream", st.label, "action", "post-subscribe-backfill")
	logger.Info("post-subscribe backfill", "from", from, "to", p)
	for cur := from; cur <= p; {
		end := min(cur+s.blocksPerQuery-1, p)

		rctx, cancel := context.WithTimeout(ctx, rpcCallTimeout)
		events, err := s.provider.GetEvents(rctx, GetEventsOptions{
			FromBlock: cur,
			ToBlock:   end,
			Address:   st.address,
			Keys:      st.keys,
			ChunkSize: 1000,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("events [%d, %d]: %w", cur, end, err)
		}

		kept := events[:0]
		for _, e := range events {
			if e.ContractAddress == nil || !s.isTracked(e.ContractAddress.String()) {
				continue
			}
			if c, ok := base[e.ContractAddress.String()]; ok && e.BlockNumber >= c {
				kept = append(kept, e)
			}
		}
		s.resolveTimestamps(ctx, kept, logger)
		for _, e := range kept {
			e.IsCatchup = true
			select {
			case s.events <- e:
				st.setCursor(e.ContractAddress.String(), e.BlockNumber, false)
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		cur = end + 1
	}
	return nil
}
