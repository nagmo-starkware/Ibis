package engine

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/rpc"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/provider"
	"github.com/b-j-roberts/ibis/internal/schema"
	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/types"
)

// viewPollerProvider is the subset of provider functionality needed by the view poller.
// Extracted as an interface to enable testing with mock RPC.
type viewPollerProvider interface {
	BlockNumber(ctx context.Context) (uint64, error)
	Call(ctx context.Context, contractAddress *felt.Felt, entryPointSelector *felt.Felt, calldata []*felt.Felt, blockID rpc.BlockID) ([]*felt.Felt, error)
}

// Compile-time check that StarknetProvider satisfies viewPollerProvider.
var _ viewPollerProvider = (*provider.StarknetProvider)(nil)

// ViewStatus holds status information about a polled view function.
type ViewStatus struct {
	FunctionName      string    `json:"function_name"`
	Contract          string    `json:"contract"`
	Interval          string    `json:"interval"`
	LastPollBlock     uint64    `json:"last_poll_block"`
	LastPollTime      time.Time `json:"last_poll_time,omitempty"`
	ConsecutiveErrors int       `json:"consecutive_errors"`
}

// viewEntry holds the resolved state for a single view function to poll.
type viewEntry struct {
	contractName    string
	contractAddress *felt.Felt
	functionDef     *abi.FunctionDef
	calldata        []*felt.Felt
	interval        time.Duration
	schema          *types.TableSchema
	uniqueKey       string

	// Skip-if-busy: prevents overlapping polls for the same function.
	busy sync.Mutex

	// Status tracking (protected by statusMu).
	statusMu        sync.Mutex
	lastPollBlock   uint64
	lastPollTime    time.Time
	consecutiveErrs int
	pollIndex       uint64
}

// ViewPoller manages periodic starknet_call polling for view functions.
type ViewPoller struct {
	mu       sync.Mutex // protects entries for concurrent AddContract calls
	entries  []*viewEntry
	provider viewPollerProvider
	store    store.Store
	logger   *slog.Logger
	onEvent  func(contract, event, table string, blockNumber, logIndex uint64, data map[string]any)

	// Reorg notification: close-and-recreate pattern to broadcast to all goroutines.
	reorgMu sync.Mutex
	reorgCh chan struct{}
}

// NewViewPoller creates a ViewPoller. Call Setup() to initialize entries.
func NewViewPoller(prov viewPollerProvider, st store.Store, logger *slog.Logger) *ViewPoller {
	return &ViewPoller{
		provider: prov,
		store:    st,
		logger:   logger.With("component", "view_poller"),
		reorgCh:  make(chan struct{}),
	}
}

// SetOnEvent sets a callback invoked after each successful poll result is stored.
func (vp *ViewPoller) SetOnEvent(fn func(contract, event, table string, blockNumber, logIndex uint64, data map[string]any)) {
	vp.onEvent = fn
}

// Setup resolves view function definitions and builds schemas for all contracts.
// Returns the view schemas so they can be registered with the store and API.
func (vp *ViewPoller) Setup(contracts []*contractState) ([]*types.TableSchema, error) {
	var schemas []*types.TableSchema

	for _, cs := range contracts {
		if len(cs.config.Views) == 0 {
			continue
		}

		for _, viewCfg := range cs.config.Views {
			entry, viewSchema, err := vp.buildEntry(cs, &viewCfg)
			if err != nil {
				return nil, err
			}
			vp.entries = append(vp.entries, entry)
			schemas = append(schemas, viewSchema)

			vp.logger.Info("registered view function",
				"contract", cs.config.Name,
				"function", entry.functionDef.Name,
				"interval", entry.interval,
				"table", viewSchema.Name,
			)
		}
	}

	return schemas, nil
}

// buildEntry resolves a single ViewConfig into a viewEntry and its schema.
func (vp *ViewPoller) buildEntry(cs *contractState, viewCfg *config.ViewConfig) (*viewEntry, *types.TableSchema, error) {
	// Resolve function definition from parsed ABI.
	funcDef, ok := cs.abi.Functions[viewCfg.Function]
	if !ok {
		return nil, nil, fmt.Errorf("contract %s: view function %q not found in ABI", cs.config.Name, viewCfg.Function)
	}

	// Parse calldata.
	var calldata []*felt.Felt
	if len(viewCfg.Calldata) > 0 {
		var err error
		calldata, err = abi.EncodeFunctionCalldata(viewCfg.Calldata)
		if err != nil {
			return nil, nil, fmt.Errorf("contract %s, function %s: encoding calldata: %w", cs.config.Name, viewCfg.Function, err)
		}
	}

	// Parse interval.
	interval, err := time.ParseDuration(viewCfg.Interval)
	if err != nil {
		return nil, nil, fmt.Errorf("contract %s, function %s: parsing interval: %w", cs.config.Name, viewCfg.Function, err)
	}

	// Build table schema, using shared naming when the contract has shared tables.
	var buildOpts *schema.BuildOptions
	if cs.config.SharedTables && cs.config.FactoryName != "" {
		buildOpts = &schema.BuildOptions{
			SharedTable: true,
			FactoryName: cs.config.FactoryName,
		}
	}
	viewSchema := schema.BuildViewSchema(cs.config.Name, funcDef, viewCfg, buildOpts)

	entry := &viewEntry{
		contractName:    cs.config.Name,
		contractAddress: cs.address,
		functionDef:     funcDef,
		calldata:        calldata,
		interval:        interval,
		schema:          viewSchema,
		uniqueKey:       viewCfg.Table.UniqueKey,
	}

	return entry, viewSchema, nil
}

// Status returns the current status of all view function pollers.
func (vp *ViewPoller) Status() []ViewStatus {
	vp.mu.Lock()
	entries := make([]*viewEntry, len(vp.entries))
	copy(entries, vp.entries)
	vp.mu.Unlock()

	statuses := make([]ViewStatus, len(entries))
	for i, entry := range entries {
		entry.statusMu.Lock()
		statuses[i] = ViewStatus{
			FunctionName:      entry.functionDef.Name,
			Contract:          entry.contractName,
			Interval:          entry.interval.String(),
			LastPollBlock:     entry.lastPollBlock,
			LastPollTime:      entry.lastPollTime,
			ConsecutiveErrors: entry.consecutiveErrs,
		}
		entry.statusMu.Unlock()
	}
	return statuses
}

// Run starts polling goroutines for all registered view functions.
// Blocks until ctx is canceled.
func (vp *ViewPoller) Run(ctx context.Context) {
	if len(vp.entries) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, entry := range vp.entries {
		wg.Add(1)
		go func(e *viewEntry) {
			defer wg.Done()
			vp.runView(ctx, e)
		}(entry)
	}
	vp.logger.Info("view poller started", "views", len(vp.entries))
	wg.Wait()
	vp.logger.Info("view poller stopped")
}

// NotifyReorg signals all view goroutines to re-poll immediately.
func (vp *ViewPoller) NotifyReorg() {
	vp.reorgMu.Lock()
	close(vp.reorgCh)
	vp.reorgCh = make(chan struct{})
	vp.reorgMu.Unlock()
}

// reorgChan returns the current reorg notification channel.
func (vp *ViewPoller) reorgChan() <-chan struct{} {
	vp.reorgMu.Lock()
	defer vp.reorgMu.Unlock()
	return vp.reorgCh
}

// runView is the per-function polling loop.
func (vp *ViewPoller) runView(ctx context.Context, entry *viewEntry) {
	// Add startup jitter: up to 10% of interval to spread RPC load.
	jitter := time.Duration(rand.Int63n(int64(entry.interval / 10)))
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	// Initial poll on startup.
	vp.poll(ctx, entry)

	ticker := time.NewTicker(entry.interval)
	defer ticker.Stop()

	for {
		reorgCh := vp.reorgChan()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			vp.poll(ctx, entry)
		case <-reorgCh:
			vp.poll(ctx, entry)
			ticker.Reset(entry.interval)
		}
	}
}

// poll executes a single view function call, decodes the result, and stores it.
func (vp *ViewPoller) poll(ctx context.Context, entry *viewEntry) {
	// Skip if previous poll is still in flight.
	if !entry.busy.TryLock() {
		vp.logger.Debug("skipping poll: previous call in flight",
			"function", entry.functionDef.Name,
			"contract", entry.contractName,
		)
		return
	}
	defer entry.busy.Unlock()

	// Get current block number to anchor the poll.
	blockNumber, err := vp.provider.BlockNumber(ctx)
	if err != nil {
		vp.handlePollError(entry, fmt.Errorf("getting block number: %w", err))
		return
	}

	// Execute starknet_call.
	result, err := vp.provider.Call(
		ctx,
		entry.contractAddress,
		entry.functionDef.Selector,
		entry.calldata,
		rpc.BlockID{Tag: rpc.BlockTagLatest},
	)
	if err != nil {
		vp.handlePollError(entry, fmt.Errorf("starknet_call %s: %w", entry.functionDef.Name, err))
		return
	}

	// Decode results using ABI.
	decoded, err := abi.DecodeFunctionOutputs(entry.functionDef.Outputs, result)
	if err != nil {
		vp.handlePollError(entry, fmt.Errorf("decoding %s output: %w", entry.functionDef.Name, err))
		return
	}

	// Add metadata.
	now := time.Now()
	decoded["block_number"] = blockNumber
	decoded["timestamp"] = uint64(now.Unix())
	decoded["contract_address"] = entry.contractAddress.String()

	// For shared view tables, include contract_name to distinguish rows from different contracts.
	if entry.schema.SharedTable {
		decoded["contract_name"] = entry.contractName
	}

	// Generate poll index for key generation.
	entry.statusMu.Lock()
	pollIdx := entry.pollIndex
	entry.pollIndex++
	entry.statusMu.Unlock()

	// Build operation key and _view_key based on table type.
	var opKey string
	if entry.schema.TableType == types.TableTypeUnique {
		if entry.uniqueKey == "_view_key" {
			// Single-row mode: constant key so there's always one row.
			decoded["_view_key"] = "latest"
			opKey = "latest"
		} else {
			// Use the decoded value of the unique_key field.
			if keyVal, ok := decoded[entry.uniqueKey]; ok {
				opKey = fmt.Sprintf("%v", keyVal)
			} else {
				opKey = fmt.Sprintf("%d:%d", blockNumber, pollIdx)
			}
			decoded["_view_key"] = opKey
		}
	} else {
		// Log table: append with block_number:poll_index key.
		decoded["_view_key"] = fmt.Sprintf("%d:%d", blockNumber, pollIdx)
		opKey = fmt.Sprintf("%d:%d", blockNumber, pollIdx)
	}

	// Create and apply store operation.
	op := store.Operation{
		Type:        store.OpInsert,
		Table:       entry.schema.Name,
		Key:         opKey,
		Data:        decoded,
		BlockNumber: blockNumber,
		LogIndex:    pollIdx,
	}

	if err := vp.store.ApplyOperations(ctx, []store.Operation{op}); err != nil {
		vp.handlePollError(entry, fmt.Errorf("storing %s result: %w", entry.functionDef.Name, err))
		return
	}

	// Update status on success.
	entry.statusMu.Lock()
	entry.lastPollBlock = blockNumber
	entry.lastPollTime = now
	entry.consecutiveErrs = 0
	entry.statusMu.Unlock()

	// Notify SSE subscribers.
	if vp.onEvent != nil {
		vp.onEvent(entry.contractName, entry.functionDef.Name, entry.schema.Name, blockNumber, 0, decoded)
	}

	vp.logger.Debug("polled view function",
		"function", entry.functionDef.Name,
		"contract", entry.contractName,
		"block", blockNumber,
	)
}

// handlePollError logs the error and tracks consecutive failures.
// After 10 consecutive failures, escalates to error level.
func (vp *ViewPoller) handlePollError(entry *viewEntry, err error) {
	entry.statusMu.Lock()
	entry.consecutiveErrs++
	consecutiveErrs := entry.consecutiveErrs
	entry.statusMu.Unlock()

	level := slog.LevelWarn
	if consecutiveErrs >= 10 {
		level = slog.LevelError
	}

	vp.logger.Log(context.Background(), level, "view poll failed",
		"function", entry.functionDef.Name,
		"contract", entry.contractName,
		"error", err,
		"consecutive_errors", consecutiveErrs,
	)
}

// HasEntries returns true if there are any view functions to poll.
func (vp *ViewPoller) HasEntries() bool {
	vp.mu.Lock()
	defer vp.mu.Unlock()
	return len(vp.entries) > 0
}

// AddContract dynamically adds view functions for a contract that was registered
// after engine startup (e.g., via discovery or admin API). It builds view entries,
// creates tables in the store, spawns per-function polling goroutines, and returns
// the view schemas for API registration.
func (vp *ViewPoller) AddContract(ctx context.Context, cs *contractState) ([]*types.TableSchema, error) {
	if len(cs.config.Views) == 0 {
		return nil, nil
	}

	var newEntries []*viewEntry
	var schemas []*types.TableSchema

	for _, viewCfg := range cs.config.Views {
		entry, viewSchema, err := vp.buildEntry(cs, &viewCfg)
		if err != nil {
			return nil, err
		}
		newEntries = append(newEntries, entry)
		schemas = append(schemas, viewSchema)

		vp.logger.Info("registered view function (dynamic)",
			"contract", cs.config.Name,
			"function", entry.functionDef.Name,
			"interval", entry.interval,
			"table", viewSchema.Name,
		)
	}

	// Append entries under lock.
	vp.mu.Lock()
	vp.entries = append(vp.entries, newEntries...)
	vp.mu.Unlock()

	// Spawn per-function polling goroutines using the provided context.
	for _, entry := range newEntries {
		go func(e *viewEntry) {
			vp.runView(ctx, e)
		}(entry)
	}

	return schemas, nil
}
