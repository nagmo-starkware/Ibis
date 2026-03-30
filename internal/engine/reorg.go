package engine

import (
	"context"
	"fmt"

	"github.com/b-j-roberts/ibis/internal/provider"
)

// handleReorg reverts all stored operations for blocks in the orphaned range
// [startBlock, endBlock], then resets the cursor.
func (e *Engine) handleReorg(ctx context.Context, reorg provider.ReorgNotification) error {
	e.logger.Warn("handling reorg",
		"start_block", reorg.StartBlock,
		"end_block", reorg.EndBlock,
	)

	// Revert blocks in reverse order (highest first).
	pendingBlocks := e.pending.BlockRange()
	for i := len(pendingBlocks) - 1; i >= 0; i-- {
		block := pendingBlocks[i]
		if block < reorg.StartBlock || block > reorg.EndBlock {
			continue
		}

		ops := e.pending.GetBlock(block)
		if len(ops) == 0 {
			continue
		}

		if err := e.store.RevertOperations(ctx, ops); err != nil {
			return fmt.Errorf("reverting block %d: %w", block, err)
		}

		e.pending.RemoveBlock(block)

		// Clean up log index tracking for reverted block.
		delete(e.logIndices, block)

		e.logger.Info("reverted block", "block", block, "ops", len(ops))
	}

	// Deregister factory children whose deploy block is in the reverted range.
	e.reorgFactoryChildren(ctx, reorg.StartBlock, reorg.EndBlock)

	// Deregister discovered contracts whose deploy block is in the reverted range.
	e.reorgDiscoveredContracts(ctx, reorg.StartBlock, reorg.EndBlock)

	// Reset per-contract cursors to just before the reorg start.
	newCursor := reorg.StartBlock
	if newCursor > 0 {
		newCursor--
	}

	// Reset discovery cursor if it was past the reorg point.
	if e.discovery != nil {
		discCursor, err := e.store.GetCursor(ctx, discoveryCursorName)
		if err == nil && discCursor >= reorg.StartBlock {
			if err := e.store.SetCursor(ctx, discoveryCursorName, newCursor); err != nil {
				e.logger.Error("failed to reset discovery cursor", "error", err)
			}
		}
	}

	for _, cs := range e.contracts {
		cursor, err := e.store.GetCursor(ctx, cs.config.Name)
		if err != nil {
			return fmt.Errorf("getting cursor for %s: %w", cs.config.Name, err)
		}
		// Only reset if contract's cursor was at or past the reorg point.
		if cursor >= reorg.StartBlock {
			if err := e.store.SetCursor(ctx, cs.config.Name, newCursor); err != nil {
				return fmt.Errorf("resetting cursor for %s to %d: %w", cs.config.Name, newCursor, err)
			}
		}
	}

	e.logger.Info("reorg handled", "new_cursor", newCursor)
	return nil
}

// confirmBlocks promotes blocks that are past the confirmation depth.
// Blocks at (currentBlock - confirmDepth) or earlier are considered confirmed
// and their revert data is discarded.
func (e *Engine) confirmBlocks(currentBlock uint64) {
	if currentBlock < e.confirmDepth {
		return
	}
	confirmUpTo := currentBlock - e.confirmDepth
	e.pending.ConfirmUpTo(confirmUpTo)

	// Clean up log index tracking for confirmed blocks.
	for block := range e.logIndices {
		if block <= confirmUpTo {
			delete(e.logIndices, block)
		}
	}
}
