package provider

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/NethermindEth/starknet.go/rpc"
)

// firehoseSink holds per-address forwarding state for the SharedFirehose
// transport. lastBlock is the highest block whose events have been forwarded for
// this contract; the router forwards a firehose event only if its block is >=
// lastBlock (the dedup guard — see forwardIfTracked). Mirrors the per-contract
// lastBlock cursor of the WSS/polling transports, so it is no stronger against
// boundary re-delivery than they are: the block-granular guard can re-forward the
// resume block's events on reconnect (log tables have no unique constraint). A
// true unique index would close that, but is a data migration (see the plan doc).
type firehoseSink struct {
	sub       ContractSubscription
	lastBlock uint64
}

func (s *EventSubscriber) addSink(sub ContractSubscription, lastBlock uint64) {
	s.routerMu.Lock()
	s.router[sub.Address.String()] = &firehoseSink{sub: sub, lastBlock: lastBlock}
	s.routerMu.Unlock()
}

func (s *EventSubscriber) removeSink(addrHex string) {
	s.routerMu.Lock()
	delete(s.router, addrHex)
	s.routerMu.Unlock()
}

// setSinkLast advances a sink's lastBlock, never moving it backwards unless
// force is set (reorg rollback needs to move it back).
func (s *EventSubscriber) setSinkLast(addrHex string, block uint64, force bool) {
	s.routerMu.Lock()
	if sk := s.router[addrHex]; sk != nil {
		if force || block > sk.lastBlock {
			sk.lastBlock = block
		}
	}
	s.routerMu.Unlock()
}

// snapshotSinks returns a shallow copy of the current sink set for iteration
// without holding the lock across RPC calls.
func (s *EventSubscriber) snapshotSinks() []*firehoseSink {
	s.routerMu.RLock()
	out := make([]*firehoseSink, 0, len(s.router))
	for _, sk := range s.router {
		out = append(out, sk)
	}
	s.routerMu.RUnlock()
	return out
}

// startFirehose runs the single-subscription firehose transport: seed a sink per
// initial contract, then loop forever opening ONE all-events WSS subscription and
// demultiplexing by from_address. Blocks until ctx is canceled.
func (s *EventSubscriber) startFirehose(ctx context.Context) error {
	for _, c := range s.contracts {
		s.addSink(c, c.StartBlock)
	}
	s.logger.Info("firehose transport starting", "initial_contracts", len(s.contracts))
	return s.runFirehoseWSS(ctx)
}

// runFirehoseWSS is the connection lifecycle: before each (re)connect it HTTP-fills
// every sink up to near tip (gap-fill — also keeps indexing progressing if WSS is
// down), then opens one empty-filter subscription resumed from the min sink
// cursor and forwards matching events until the session drops.
func (s *EventSubscriber) runFirehoseWSS(ctx context.Context) error {
	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		fromBlock := s.firehoseGapFill(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		subInput := &rpc.EventSubscriptionInput{}
		if fromBlock > 0 {
			bn := fromBlock
			subInput.SubBlockID = rpc.SubscriptionBlockID{Number: &bn}
		}

		session, err := s.dialWSS(ctx, s.provider.wsURL, subInput)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.logger.Warn("firehose WSS dial failed, retrying", "error", err, "backoff", backoff, "from_block", fromBlock)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = time.Duration(math.Min(float64(backoff)*2, float64(maxBackoff)))
			continue
		}
		backoff = minBackoff
		s.logger.Info("firehose WSS active", "from_block", fromBlock, "tracked", len(s.router))

		err = s.processFirehose(ctx, session)
		session.close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.logger.Warn("firehose WSS session ended, reconnecting", "error", err, "backoff", backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = time.Duration(math.Min(float64(backoff)*2, float64(maxBackoff)))
	}
}

// firehoseGapFill brings every sink within catchupThreshold of the chain tip over
// HTTP (bounded by the shared semaphore inside pollUntilCaughtUp), then returns
// the min sink cursor to resume the subscription from. This both closes any gap
// larger than the 1024-block WSS resume cap after a long disconnect and, on the
// first call, performs the initial catchup. Runs only between sessions, so no WSS
// forward mutates sinks concurrently.
func (s *EventSubscriber) firehoseGapFill(ctx context.Context) uint64 {
	sinks := s.snapshotSinks()
	if len(sinks) == 0 {
		// Dynamic-only: no contracts yet. Resume from the current tip.
		bn, err := s.provider.CachedBlockNumber(ctx)
		if err != nil {
			return 0
		}
		return bn
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	minLast := uint64(math.MaxUint64)
	for _, sk := range sinks {
		wg.Add(1)
		go func(sk *firehoseSink) {
			defer wg.Done()
			last := sk.lastBlock
			logger := s.logger.With("contract", sk.sub.Address)
			if err := s.pollUntilCaughtUp(ctx, sk.sub, &last, logger); err != nil {
				return // ctx canceled
			}
			s.setSinkLast(sk.sub.Address.String(), last, false)
			mu.Lock()
			if last < minLast {
				minLast = last
			}
			mu.Unlock()
		}(sk)
	}
	wg.Wait()

	if minLast == math.MaxUint64 {
		bn, err := s.provider.CachedBlockNumber(ctx)
		if err != nil {
			return 0
		}
		return bn
	}
	return minLast
}

// processFirehose reads the shared subscription until it errors or ctx is
// canceled, forwarding tracked events and propagating reorgs.
func (s *EventSubscriber) processFirehose(ctx context.Context, session *wssSession) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-session.errs:
			return fmt.Errorf("firehose subscription error: %w", err)

		case reorg := <-session.reorgs:
			if reorg != nil {
				s.logger.Warn("firehose reorg", "start_block", reorg.StartBlockNum, "end_block", reorg.EndBlockNum)
				if s.reorgs != nil {
					select {
					case s.reorgs <- ReorgNotification{StartBlock: reorg.StartBlockNum, EndBlock: reorg.EndBlockNum}:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				// Roll every affected sink back to the reorg start so the orphaned
				// blocks are re-fetched/re-delivered.
				s.rollbackSinks(reorg.StartBlockNum)
			}

		case evt := <-session.events:
			if evt == nil {
				continue
			}
			s.forwardIfTracked(ctx, evt)
		}
	}
}

// rollbackSinks moves every sink whose cursor is at or past startBlock back to
// startBlock so a reorg's orphaned range is reprocessed.
func (s *EventSubscriber) rollbackSinks(startBlock uint64) {
	s.routerMu.Lock()
	for _, sk := range s.router {
		if sk.lastBlock > startBlock {
			sk.lastBlock = startBlock
		}
	}
	s.routerMu.Unlock()
}

// forwardIfTracked routes one firehose event: drop it unless its from_address is
// tracked and its block is at/after the sink cursor, then enrich with a timestamp
// and hand it to the engine. Advances the sink cursor to the event's block.
func (s *EventSubscriber) forwardIfTracked(ctx context.Context, evt *rpc.EmittedEventWithFinalityStatus) {
	if evt.FromAddress == nil {
		return
	}
	addr := evt.FromAddress.String()

	s.routerMu.RLock()
	sk := s.router[addr]
	var last uint64
	if sk != nil {
		last = sk.lastBlock
	}
	s.routerMu.RUnlock()

	if sk == nil {
		return // untracked contract — the whole point of demux
	}
	if evt.BlockNumber < last {
		return // already forwarded (dedup guard, block-granular)
	}

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
		s.setSinkLast(addr, evt.BlockNumber, false)
	case <-ctx.Done():
	}
}

// addContractFirehose registers a contract with the running firehose: backfill
// its history [StartBlock, tip] over HTTP, and route its future events (> tip)
// through the shared subscription. The two ranges partition cleanly at the tip
// captured now, so there is no overlap (dup) and no gap.
func (s *EventSubscriber) addContractFirehose(ctx context.Context, sub ContractSubscription) {
	tip, err := s.provider.CachedBlockNumber(ctx)
	if err != nil {
		// No tip available: fall back to forwarding from StartBlock and skip
		// backfill; gap-fill on the next reconnect will reconcile.
		s.addSink(sub, sub.StartBlock)
		return
	}

	// The subscription forwards blocks >= wssFrom; backfill covers [StartBlock, tip].
	wssFrom := tip + 1
	if sub.StartBlock > wssFrom {
		wssFrom = sub.StartBlock
	}
	s.addSink(sub, wssFrom)

	if sub.StartBlock <= tip {
		go func() {
			if err := s.Backfill(ctx, sub, sub.StartBlock, tip); err != nil && ctx.Err() == nil {
				s.logger.Error("firehose backfill failed", "contract", sub.Address, "error", err)
			}
		}()
	}
}
