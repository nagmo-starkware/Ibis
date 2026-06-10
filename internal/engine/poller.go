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
	RefreshMode       string    `json:"refresh_mode"`
	Interval          string    `json:"interval"`
	LastPollBlock     uint64    `json:"last_poll_block"`
	LastPollTime      time.Time `json:"last_poll_time,omitempty"`
	ConsecutiveErrors int       `json:"consecutive_errors"`
}

// defaultReactiveDebounce throttles reactive reads when no debounce is set in
// config, collapsing multiple events in quick succession (e.g. several fills
// in one block) into a single view read.
const defaultReactiveDebounce = time.Second

// viewEntry holds the resolved state for a single view function to poll.
type viewEntry struct {
	contractName    string
	contractAddress *felt.Felt
	functionDef     *abi.FunctionDef
	calldata        []*felt.Felt
	interval        time.Duration
	schema          *types.TableSchema
	uniqueKey       string

	// Refresh policy. refreshMode is one of config.RefreshMode{Interval,Constant,Reactive}.
	refreshMode     string
	onEvents        map[string]bool // reactive: event names on THIS contract
	foreignTriggers map[string]bool // reactive: "contract/event" keys on OTHER contracts
	debounce        time.Duration   // reactive: throttle window (0 = read on every event)
	maxInterval     time.Duration   // reactive: optional staleness ceiling (0 = none)
	trigger         chan struct{}   // reactive: buffered(1) re-read signal

	// cancel stops this entry's polling goroutine. Assigned when the goroutine
	// is spawned (Run/AddContract) and invoked by RemoveContract when the
	// contract is frozen or deregistered. Guarded by ViewPoller.mu.
	cancel context.CancelFunc

	// Skip-if-busy: prevents overlapping polls for the same function.
	busy sync.Mutex

	// Status tracking (protected by statusMu).
	statusMu        sync.Mutex
	lastPollBlock   uint64
	lastPollTime    time.Time
	consecutiveErrs int
	pollIndex       uint64
}

// maxConcurrentPolls bounds how many view starknet_calls run at once across ALL
// view goroutines. On a large deployment (thousands of factory children) every
// view fires an initial poll at startup; without a cap they hit the RPC provider
// simultaneously and trip its concurrent-request limit (HTTP 429). The semaphore
// turns that thundering herd into a steady, bounded stream.
const maxConcurrentPolls = 16

// maxInitialPollAttempts bounds the retry of a view's FIRST read. Reactive/constant
// views are read once (then event-driven / never again), so a transient RPC failure
// on that one read would otherwise leave the view empty until the next restart. We
// retry with capped backoff up to this many attempts, then give up (NOT an infinite
// loop) — the next event or restart re-attempts.
const maxInitialPollAttempts = 12

// ViewPoller manages periodic starknet_call polling for view functions.
type ViewPoller struct {
	mu       sync.Mutex // protects entries for concurrent AddContract calls
	entries  []*viewEntry
	provider viewPollerProvider
	store    store.Store
	logger   *slog.Logger
	onEvent  func(contract, event, table string, blockNumber, logIndex uint64, data map[string]any)

	// sem bounds concurrent starknet_calls across all view goroutines.
	sem chan struct{}

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
		sem:      make(chan struct{}, maxConcurrentPolls),
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
		// Frozen contracts retain their data but do no further polling.
		if cs.config.Frozen {
			continue
		}
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

	// Resolve refresh policy (interval | constant | reactive).
	refreshMode := config.RefreshModeInterval
	if viewCfg.Refresh != nil {
		refreshMode = viewCfg.Refresh.ResolvedMode()
	}

	var (
		interval        time.Duration
		debounce        time.Duration
		maxInterval     time.Duration
		onEvents        map[string]bool
		foreignTriggers map[string]bool
		trigger         chan struct{}
	)

	switch refreshMode {
	case config.RefreshModeConstant:
		// No interval, no trigger: read once at registration.

	case config.RefreshModeReactive:
		onEvents = make(map[string]bool, len(viewCfg.Refresh.On))
		for _, ev := range viewCfg.Refresh.On {
			onEvents[ev] = true
		}
		foreignTriggers = make(map[string]bool, len(viewCfg.Refresh.OnForeign))
		for _, f := range viewCfg.Refresh.OnForeign {
			foreignTriggers[foreignKey(f.Contract, f.Event)] = true
		}
		debounce = defaultReactiveDebounce
		if viewCfg.Refresh.Debounce != "" {
			d, err := time.ParseDuration(viewCfg.Refresh.Debounce)
			if err != nil {
				return nil, nil, fmt.Errorf("contract %s, function %s: parsing refresh.debounce: %w", cs.config.Name, viewCfg.Function, err)
			}
			debounce = d
		}
		if viewCfg.Refresh.MaxInterval != "" {
			d, err := time.ParseDuration(viewCfg.Refresh.MaxInterval)
			if err != nil {
				return nil, nil, fmt.Errorf("contract %s, function %s: parsing refresh.max_interval: %w", cs.config.Name, viewCfg.Function, err)
			}
			maxInterval = d
		}
		trigger = make(chan struct{}, 1)

	default:
		refreshMode = config.RefreshModeInterval
		d, err := time.ParseDuration(viewCfg.Interval)
		if err != nil {
			return nil, nil, fmt.Errorf("contract %s, function %s: parsing interval: %w", cs.config.Name, viewCfg.Function, err)
		}
		interval = d
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
		refreshMode:     refreshMode,
		onEvents:        onEvents,
		foreignTriggers: foreignTriggers,
		debounce:        debounce,
		maxInterval:     maxInterval,
		trigger:         trigger,
	}

	return entry, viewSchema, nil
}

// foreignKey builds the lookup key for a foreign (cross-contract) trigger.
func foreignKey(contract, event string) string {
	return contract + "/" + event
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
			RefreshMode:       entry.refreshMode,
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
	vp.mu.Lock()
	if len(vp.entries) == 0 {
		vp.mu.Unlock()
		return
	}

	var wg sync.WaitGroup
	for _, entry := range vp.entries {
		// Per-entry context so RemoveContract can stop one contract's views
		// (on freeze/deregister) without tearing down the whole poller.
		entryCtx, entryCancel := context.WithCancel(ctx)
		entry.cancel = entryCancel
		wg.Add(1)
		go func(e *viewEntry, c context.Context) {
			defer wg.Done()
			vp.runView(c, e)
		}(entry, entryCtx)
	}
	n := len(vp.entries)
	vp.mu.Unlock()

	vp.logger.Info("view poller started", "views", n)
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

// runView dispatches a view entry to the loop matching its refresh mode.
func (vp *ViewPoller) runView(ctx context.Context, entry *viewEntry) {
	switch entry.refreshMode {
	case config.RefreshModeConstant:
		vp.runConstant(ctx, entry)
	case config.RefreshModeReactive:
		vp.runReactive(ctx, entry)
	default:
		vp.runInterval(ctx, entry)
	}
}

// runInterval is the classic fixed-interval polling loop.
func (vp *ViewPoller) runInterval(ctx context.Context, entry *viewEntry) {
	// Add startup jitter: up to min(10s, interval) to spread RPC load.
	maxJitter := 10 * time.Second
	if entry.interval < maxJitter {
		maxJitter = entry.interval
	}
	jitter := time.Duration(rand.Int63n(int64(maxJitter)))
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

// pollUntilSuccess performs a view's initial read, retrying with capped
// exponential backoff up to maxInitialPollAttempts. Returns true once a poll
// succeeds. This is BOUNDED — it never loops forever: on exhaustion it logs and
// returns false, leaving the view to be (re)populated by a later event or the
// next restart. Used for the one-shot initial read of constant/reactive views,
// so a transient RPC failure (e.g. a 429 during the startup wave) doesn't leave
// the view empty with no recovery path.
func (vp *ViewPoller) pollUntilSuccess(ctx context.Context, entry *viewEntry) bool {
	backoff := time.Second
	for attempt := 1; attempt <= maxInitialPollAttempts; attempt++ {
		vp.poll(ctx, entry)

		entry.statusMu.Lock()
		ok := entry.consecutiveErrs == 0 && !entry.lastPollTime.IsZero()
		entry.statusMu.Unlock()
		if ok {
			return true
		}
		if attempt == maxInitialPollAttempts {
			break
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff):
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
	vp.logger.Warn("initial view poll did not succeed after retries; relying on events / next restart",
		"function", entry.functionDef.Name,
		"contract", entry.contractName,
		"attempts", maxInitialPollAttempts,
	)
	return false
}

// runConstant reads a deploy-time-immutable view exactly once, at registration
// (with bounded retry), then exits — the value can never change on-chain, so no
// further polling is scheduled and the contract costs zero ongoing RPC.
func (vp *ViewPoller) runConstant(ctx context.Context, entry *viewEntry) {
	vp.pollUntilSuccess(ctx, entry)
}

// runReactive reads a view once at registration (capturing current chain state
// regardless of catchup progress), then re-reads only when a trigger fires —
// an event in entry.onEvents/foreignTriggers, a reorg, or the optional
// max-interval ceiling. Triggers within entry.debounce of the last read are
// throttled into a single trailing read so a burst of events in one block
// produces at most one extra RPC call.
func (vp *ViewPoller) runReactive(ctx context.Context, entry *viewEntry) {
	// Initial read at registration: current state via BlockTagLatest, so the
	// view is populated immediately without waiting for any event. Bounded retry
	// so a transient RPC failure (e.g. a 429 in the startup wave) doesn't leave
	// the view empty until the next matching event — important for views like
	// get_active_deployment that drive market discovery and rarely re-fire.
	vp.pollUntilSuccess(ctx, entry)
	lastPoll := time.Now()

	var (
		throttle  *time.Timer
		throttleC <-chan time.Time
		pending   bool
	)
	stopThrottle := func() {
		if throttle != nil {
			throttle.Stop()
			throttle = nil
			throttleC = nil
		}
	}
	defer stopThrottle()

	doPoll := func() {
		stopThrottle()
		vp.poll(ctx, entry)
		lastPoll = time.Now()
		pending = false
	}

	// Optional staleness ceiling.
	var maxC <-chan time.Time
	if entry.maxInterval > 0 {
		ticker := time.NewTicker(entry.maxInterval)
		defer ticker.Stop()
		maxC = ticker.C
	}

	for {
		reorgCh := vp.reorgChan()
		select {
		case <-ctx.Done():
			return
		case <-entry.trigger:
			if entry.debounce <= 0 {
				doPoll()
				continue
			}
			if elapsed := time.Since(lastPoll); elapsed >= entry.debounce {
				doPoll()
			} else if throttle == nil {
				// Schedule a trailing read at the end of the throttle window.
				pending = true
				throttle = time.NewTimer(entry.debounce - elapsed)
				throttleC = throttle.C
			} else {
				pending = true
			}
		case <-throttleC:
			stopThrottle()
			if pending {
				doPoll()
			}
		case <-maxC:
			doPoll()
		case <-reorgCh:
			doPoll()
		}
	}
}

// TriggerView signals reactive view entries to re-read after an event was
// indexed. emitterName/emitterAddr identify the contract that emitted the
// event. An entry matches when either (a) the event fired on its own contract
// and is in its onEvents set, or (b) a foreign (contract/event) trigger
// matches. Sends are non-blocking: the trigger channel is buffered(1) so a
// pending signal coalesces repeated triggers between reads.
func (vp *ViewPoller) TriggerView(emitterName string, emitterAddr *felt.Felt, eventName string) {
	fk := foreignKey(emitterName, eventName)

	vp.mu.Lock()
	entries := make([]*viewEntry, len(vp.entries))
	copy(entries, vp.entries)
	vp.mu.Unlock()

	for _, entry := range entries {
		if entry.refreshMode != config.RefreshModeReactive {
			continue
		}
		local := entry.onEvents[eventName] &&
			entry.contractAddress != nil && emitterAddr != nil &&
			entry.contractAddress.Equal(emitterAddr)
		if !local && !entry.foreignTriggers[fk] {
			continue
		}
		select {
		case entry.trigger <- struct{}{}:
		default: // already pending — coalesce
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

	// Bound global concurrency: at most maxConcurrentPolls starknet_calls run at
	// once across all view goroutines, so a startup wave of initial polls doesn't
	// trip the RPC provider's concurrent-request limit (429). Respect ctx so a
	// shutdown doesn't block on a full semaphore.
	select {
	case vp.sem <- struct{}{}:
		defer func() { <-vp.sem }()
	case <-ctx.Done():
		return
	}

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
	decoded, err := abi.DecodeFunctionOutputs(entry.functionDef.Name, entry.functionDef.Outputs, result)
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

// RemoveContract stops all view polling for the named contract and drops its
// entries. Used when a contract is frozen (lifecycle) or deregistered (reorg).
// Each removed entry's goroutine is canceled via its per-entry context; the
// underlying tables and data are left untouched. Returns the number of view
// entries removed.
func (vp *ViewPoller) RemoveContract(contractName string) int {
	vp.mu.Lock()
	defer vp.mu.Unlock()

	kept := vp.entries[:0]
	removed := 0
	for _, e := range vp.entries {
		if e.contractName == contractName {
			if e.cancel != nil {
				e.cancel()
			}
			removed++
			continue
		}
		kept = append(kept, e)
	}
	vp.entries = kept

	if removed > 0 {
		vp.logger.Info("removed view entries", "contract", contractName, "count", removed)
	}
	return removed
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

	// Append entries and assign per-entry cancel contexts under lock so a later
	// RemoveContract (freeze/deregister) can stop them individually.
	entryCtxs := make([]context.Context, len(newEntries))
	vp.mu.Lock()
	for i, entry := range newEntries {
		entryCtx, entryCancel := context.WithCancel(ctx)
		entry.cancel = entryCancel
		entryCtxs[i] = entryCtx
	}
	vp.entries = append(vp.entries, newEntries...)
	vp.mu.Unlock()

	// Spawn per-function polling goroutines using each entry's context.
	for i, entry := range newEntries {
		go func(e *viewEntry, c context.Context) {
			vp.runView(c, e)
		}(entry, entryCtxs[i])
	}

	return schemas, nil
}
