package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/NethermindEth/juno/core/felt"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/provider"
	"github.com/b-j-roberts/ibis/internal/schema"
	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/types"
)

// DefaultConfirmationDepth is the number of blocks after which pending
// operations are promoted to confirmed and their revert data is discarded.
const DefaultConfirmationDepth uint64 = 10

// contractState holds per-contract ABI, event registry, and table schemas.
type contractState struct {
	config   config.ContractConfig
	address  *felt.Felt
	abi      *abi.ABI
	registry *abi.EventRegistry
	schemas  map[string]*types.TableSchema // event name -> schema

	// childABIs caches resolved ABIs for factory children keyed by factory.ChildABI string.
	// Each factory entry may specify a different ChildABI, so the cache is per-type.
	childABIs map[string]*abi.ABI

	// sharedSchemas caches shared table schemas keyed by factory.ChildABI string.
	// Outer key = ChildABI name; inner map = event name -> schema.
	sharedSchemas map[string]map[string]*types.TableSchema
}

// ContractStatus represents the current state of an indexed contract.
type ContractStatus string

const (
	ContractStatusActive      ContractStatus = "active"
	ContractStatusSyncing     ContractStatus = "syncing"
	ContractStatusError       ContractStatus = "error"
	ContractStatusBackfilling ContractStatus = "backfilling"
)

// ContractInfo holds status information about a registered contract.
type ContractInfo struct {
	Name         string         `json:"name"`
	Address      string         `json:"address"`
	Events       int            `json:"events"`
	CurrentBlock uint64         `json:"current_block"`
	StartBlock   uint64         `json:"start_block,omitempty"`
	Status       ContractStatus `json:"status"`
	Dynamic      bool           `json:"dynamic"`
	FactoryName  string         `json:"factory_name,omitempty"`
	FactoryMeta  map[string]any `json:"factory_meta,omitempty"`
	IsFactory    bool           `json:"is_factory,omitempty"`
}

// Engine is the core indexing orchestrator. It receives events from the
// subscriber, decodes them via ABI, generates revert/add operation pairs,
// writes to the store, and handles chain reorganizations.
type Engine struct {
	cfg      *config.Config
	store    store.Store
	provider *provider.StarknetProvider
	logger   *slog.Logger

	// Per-contract state built during setup. Protected by mu.
	contracts []*contractState
	mu        sync.RWMutex

	// Pending block operation tracker for reorg support.
	pending *PendingTracker

	// Event channel from subscriber.
	events chan provider.RawEvent

	// Reorg notification channel from subscriber.
	reorgs chan provider.ReorgNotification

	// Log index counter per block.
	logIndices map[uint64]uint64

	// Confirmation depth: blocks past this depth are considered confirmed.
	confirmDepth uint64

	// onEvent is an optional callback invoked after an event is successfully
	// indexed. Used by the API server's EventBus for SSE streaming.
	onEvent func(contract, event, table string, blockNumber, logIndex uint64, data map[string]any)

	// setupDone tracks whether Setup has been called.
	setupDone bool

	// subscriber is the active event subscriber, set during Run() for dynamic contract support.
	subscriber *provider.EventSubscriber

	// runCtx is the context from Run(), used for dynamic contract subscriptions.
	runCtx context.Context

	// onContractRegistered is called after a contract is dynamically registered,
	// passing the new schemas. Used by the API server to add routes.
	onContractRegistered func(cc *config.ContractConfig, schemas []*types.TableSchema)

	// onContractDeregistered is called after a contract is deregistered.
	onContractDeregistered func(name string)

	// discovery holds state for class-hash-based contract discovery (3.9).
	discovery *discoveryState

	// poller handles periodic view function polling (3.20).
	poller *ViewPoller

	// lastLoggedBlock tracks the last block number logged at INFO level
	// to avoid spamming logs with per-event messages.
	lastLoggedBlock uint64
}

// New creates an Engine with the given dependencies.
func New(cfg *config.Config, st store.Store, prov *provider.StarknetProvider, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		cfg:          cfg,
		store:        st,
		provider:     prov,
		logger:       logger.With("component", "engine"),
		pending:      NewPendingTracker(),
		events:       make(chan provider.RawEvent, 4096),
		reorgs:       make(chan provider.ReorgNotification, 16),
		logIndices:   make(map[uint64]uint64),
		confirmDepth: DefaultConfirmationDepth,
	}
}

// SetConfirmationDepth overrides the default confirmation depth.
func (e *Engine) SetConfirmationDepth(depth uint64) {
	e.confirmDepth = depth
}

// SetOnEvent sets a callback that is invoked after each event is successfully
// indexed. The callback receives the contract name, event name, table name,
// block number, log index, and decoded event data.
func (e *Engine) SetOnEvent(fn func(contract, event, table string, blockNumber, logIndex uint64, data map[string]any)) {
	e.onEvent = fn
}

// Setup resolves ABIs, builds event registries and table schemas, and creates
// tables in the store. Call this before Run to access Schemas() for the API server.
func (e *Engine) Setup(ctx context.Context) error {
	if e.setupDone {
		return nil
	}
	if err := e.setup(ctx); err != nil {
		return fmt.Errorf("engine setup: %w", err)
	}
	e.setupDone = true
	return nil
}

// Schemas returns all table schemas built during Setup.
func (e *Engine) Schemas() []*types.TableSchema {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var schemas []*types.TableSchema
	for _, cs := range e.contracts {
		for _, s := range cs.schemas {
			schemas = append(schemas, s)
		}
	}
	return schemas
}

// SetOnContractRegistered sets a callback invoked after a contract is
// dynamically registered. Passes the contract config and its schemas.
func (e *Engine) SetOnContractRegistered(fn func(cc *config.ContractConfig, schemas []*types.TableSchema)) {
	e.onContractRegistered = fn
}

// SetOnContractDeregistered sets a callback invoked after a contract is deregistered.
func (e *Engine) SetOnContractDeregistered(fn func(name string)) {
	e.onContractDeregistered = fn
}

// Contracts returns status information for all registered contracts.
func (e *Engine) Contracts(ctx context.Context) []ContractInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()

	infos := make([]ContractInfo, 0, len(e.contracts))
	for _, cs := range e.contracts {
		cursor, _ := e.store.GetCursor(ctx, cs.config.Name)
		infos = append(infos, ContractInfo{
			Name:         cs.config.Name,
			Address:      cs.config.Address,
			Events:       len(cs.config.Events),
			CurrentBlock: cursor,
			Status:       ContractStatusActive,
			Dynamic:      cs.config.Dynamic,
			FactoryName:  cs.config.FactoryName,
			FactoryMeta:  cs.config.FactoryMeta,
			IsFactory:    len(cs.config.Factories) > 0,
		})
	}
	return infos
}

// RegisterContract dynamically registers a new contract for indexing.
// It resolves the ABI, builds schemas, creates tables, persists the config,
// and spawns a subscription goroutine.
func (e *Engine) RegisterContract(ctx context.Context, cc *config.ContractConfig) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Check for duplicate name.
	for _, cs := range e.contracts {
		if cs.config.Name == cc.Name {
			return fmt.Errorf("contract %q already registered", cc.Name)
		}
	}

	// Default ABI to fetch.
	if cc.ABI == "" {
		cc.ABI = "fetch"
	}
	cc.Dynamic = true

	// Resolve ABI.
	resolver := config.NewABIResolver(e.provider)
	abis, err := resolver.ResolveAll(ctx, []config.ContractConfig{*cc})
	if err != nil {
		return fmt.Errorf("resolve ABI for %s: %w", cc.Name, err)
	}

	contractABI := abis[cc.Address]
	if contractABI == nil {
		return fmt.Errorf("no ABI resolved for contract %s (%s)", cc.Name, cc.Address)
	}

	registry := abi.NewEventRegistry(contractABI)

	// For admin-registered contracts with shared tables, use BuildOptions
	// so schemas are named after the factory/ABI name instead of the contract.
	var buildOpts *schema.BuildOptions
	if cc.SharedTables && cc.FactoryName != "" {
		buildOpts = &schema.BuildOptions{
			SharedTable: true,
			FactoryName: cc.FactoryName,
		}
	}
	schemas := schema.BuildSchemas(cc, contractABI, registry, buildOpts)

	// Parse contract address.
	address, err := new(felt.Felt).SetString(cc.Address)
	if err != nil {
		return fmt.Errorf("parsing address for %s: %w", cc.Name, err)
	}

	// Create tables in store. For shared tables that already exist (created by a
	// prior registration with the same FactoryName), CreateTable is idempotent.
	var schemaList []*types.TableSchema
	for _, sch := range schemas {
		if err := e.store.CreateTable(ctx, sch); err != nil {
			return fmt.Errorf("create table %s: %w", sch.Name, err)
		}
		schemaList = append(schemaList, sch)
		e.logger.Info("created table for dynamic contract",
			"name", sch.Name,
			"type", sch.TableType,
		)
	}

	// Persist dynamic contract config.
	if err := e.store.SaveDynamicContract(ctx, cc); err != nil {
		return fmt.Errorf("persisting dynamic contract %s: %w", cc.Name, err)
	}

	cs := &contractState{
		config:   *cc,
		address:  address,
		abi:      contractABI,
		registry: registry,
		schemas:  schemas,
	}
	e.contracts = append(e.contracts, cs)

	// Spawn subscription if the engine is running.
	if e.subscriber != nil && e.runCtx != nil {
		resolvedStart := cc.StartBlock
		if resolvedStart == nil {
			resolvedStart = e.cfg.Indexer.StartBlock // global fallback
		}
		if resolvedStart == nil {
			latest, err := e.provider.BlockNumber(ctx)
			if err != nil {
				e.logger.Warn("failed to get latest block for dynamic contract, using 0",
					"contract", cc.Name, "error", err)
			} else {
				resolvedStart = &latest
			}
		}

		sub := provider.ContractSubscription{
			Address:    address,
			StartBlock: derefUint64(resolvedStart),
		}

		// Build key filters if no wildcard.
		if !hasWildcardEvent(cc) {
			var selectors []*felt.Felt
			for _, ec := range cc.Events {
				if ev := registry.MatchName(ec.Name); ev != nil {
					selectors = append(selectors, ev.Selector)
				}
			}
			if len(selectors) > 0 {
				sub.Keys = [][]*felt.Felt{selectors}
			}
		}

		e.subscriber.AddContract(e.runCtx, sub)
		e.logger.Info("started subscription for dynamic contract",
			"contract", cc.Name,
			"start_block", derefUint64(resolvedStart),
		)
	}

	// Start view polling for this contract if it has views configured.
	if e.runCtx != nil {
		viewSchemas, err := e.startViewsForContract(e.runCtx, cs)
		if err != nil {
			e.logger.Error("failed to start views for dynamic contract",
				"contract", cc.Name, "error", err)
		} else {
			schemaList = append(schemaList, viewSchemas...)
		}
	}

	// Notify API server.
	if e.onContractRegistered != nil {
		e.onContractRegistered(cc, schemaList)
	}

	// Freeze immediately if this contract registered already past a time-based
	// predicate threshold, so we never start polling a dead contract.
	e.evaluatePredicateContract(cc.Name)

	return nil
}

// DeregisterContract removes a contract from indexing. If dropTables is true,
// the contract's tables are also dropped from the store.
func (e *Engine) DeregisterContract(ctx context.Context, name string, dropTables bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	var found *contractState
	var idx int
	for i, cs := range e.contracts {
		if cs.config.Name == name {
			found = cs
			idx = i
			break
		}
	}
	if found == nil {
		return fmt.Errorf("contract %q not found", name)
	}

	// Stop subscription.
	if e.subscriber != nil {
		e.subscriber.RemoveContract(found.address.String())
	}

	// Stop view polling for this contract.
	if e.poller != nil {
		e.poller.RemoveContract(name)
	}

	// Drop tables if requested (skip shared tables — other children still use them).
	if dropTables {
		for _, sch := range found.schemas {
			if sch.SharedTable {
				continue
			}
			if err := e.store.DropTable(ctx, sch.Name); err != nil {
				e.logger.Error("failed to drop table", "table", sch.Name, "error", err)
			}
		}
	}

	// Delete cursor.
	if err := e.store.DeleteCursor(ctx, name); err != nil {
		e.logger.Error("failed to delete cursor", "contract", name, "error", err)
	}

	// Delete dynamic contract metadata.
	if err := e.store.DeleteDynamicContract(ctx, name); err != nil {
		e.logger.Error("failed to delete dynamic contract", "contract", name, "error", err)
	}

	// Remove from contracts slice.
	e.contracts = append(e.contracts[:idx], e.contracts[idx+1:]...)

	// Notify API server.
	if e.onContractDeregistered != nil {
		e.onContractDeregistered(name)
	}

	e.logger.Info("deregistered contract", "name", name, "drop_tables", dropTables)
	return nil
}

// FreezeContract tears down a contract's event subscription and view polling
// while retaining all indexed data: the contract stays in e.contracts so its
// tables remain queryable. For dynamic (factory/discovered) contracts the
// frozen flag is persisted, so the contract is not re-subscribed on the next
// restart. Idempotent — a no-op if the contract is already frozen or unknown.
func (e *Engine) FreezeContract(ctx context.Context, name string) error {
	e.mu.Lock()
	var found *contractState
	for _, cs := range e.contracts {
		if cs.config.Name == name {
			found = cs
			break
		}
	}
	if found == nil {
		e.mu.Unlock()
		return fmt.Errorf("contract %q not found", name)
	}
	if found.config.Frozen {
		e.mu.Unlock()
		return nil
	}
	found.config.Frozen = true
	addr := found.address.String()
	dynamic := found.config.Dynamic
	ccCopy := found.config
	e.mu.Unlock()

	// Stop the event subscription (closes WSS / stops the polling goroutine).
	if e.subscriber != nil {
		e.subscriber.RemoveContract(addr)
	}
	// Stop view polling for this contract.
	if e.poller != nil {
		e.poller.RemoveContract(name)
	}

	// Persist the frozen flag so rehydration skips re-subscribing. Only dynamic
	// contracts live in the store; a frozen static contract reverts to its
	// (unfrozen) config on restart.
	if dynamic {
		if err := e.store.SaveDynamicContract(ctx, &ccCopy); err != nil {
			e.logger.Error("failed to persist frozen state", "contract", name, "error", err)
		}
	} else {
		e.logger.Warn("froze a static (config) contract; freeze will not persist across restart", "contract", name)
	}

	e.logger.Info("froze contract (lifecycle)", "contract", name, "address", addr)
	return nil
}

// evaluateFreeze checks whether an observed event triggers a lifecycle freeze
// on any tracked contract, and freezes every match. A freeze fires when either
// (a) the event is in the emitting contract's own Freeze.On set (per-instance),
// or (b) another contract declares this (emitter, event) pair in
// Freeze.OnForeign. Safe to call on both catchup and live events: freezing a
// contract whose terminal event is replayed during catchup (e.g. an option that
// settled while the indexer was down) stops it from re-subscribing.
func (e *Engine) evaluateFreeze(emitterName string, emitterAddr *felt.Felt, eventName string) {
	// fk is computed lazily — only when a contract actually declares a foreign
	// trigger — so the common (no freeze configured) path stays allocation-free
	// even on the high-volume catchup hot path.
	var fk string
	var fkComputed bool

	var targets []string
	e.mu.RLock()
	for _, cs := range e.contracts {
		if cs.config.Frozen || cs.config.Freeze == nil {
			continue
		}
		local := emitterAddr != nil && cs.address != nil &&
			cs.address.Equal(emitterAddr) && containsString(cs.config.Freeze.LocalEvents(), eventName)
		foreign := false
		if len(cs.config.Freeze.OnForeign) > 0 {
			if !fkComputed {
				fk = foreignKey(emitterName, eventName)
				fkComputed = true
			}
			for _, f := range cs.config.Freeze.OnForeign {
				if foreignKey(f.Contract, f.Event) == fk {
					foreign = true
					break
				}
			}
		}
		// Sibling: per-instance trigger off a deployment sibling. Freeze only if
		// the emitter is the address this contract records in factory_meta — so
		// e.g. an OptionToken's Settled freezes just its own OrderBook/Exerciser.
		sibling := false
		for _, st := range cs.config.Freeze.OnSibling {
			if st.Event == eventName && metaAddrEquals(cs.config.FactoryMeta[st.MetaField], emitterAddr) {
				sibling = true
				break
			}
		}
		if local || foreign || sibling {
			targets = append(targets, cs.config.Name)
		}
	}
	e.mu.RUnlock()

	for _, name := range targets {
		if err := e.FreezeContract(e.runCtx, name); err != nil {
			e.logger.Error("failed to freeze contract", "contract", name, "error", err)
		}
	}
}

// matchesFreezePredicate reports whether any predicate rule on cs.Freeze holds at
// now. Pure read of the contract's captured factory_meta — no RPC, no store I/O.
// A per-predicate eval error is logged and skipped (treated as no-match) so a
// single bad field never blocks the others. Caller must hold at least e.mu.RLock.
func (e *Engine) matchesFreezePredicate(cs *contractState, now time.Time) bool {
	if cs.config.Frozen || cs.config.Freeze == nil {
		return false
	}
	for _, p := range cs.config.Freeze.Predicates() {
		match, err := config.EvalPredicate(cs.config.FactoryMeta, p, now)
		if err != nil {
			e.logger.Warn("freeze predicate eval failed",
				"contract", cs.config.Name, "meta_field", p.MetaField, "error", err)
			continue
		}
		if match {
			return true
		}
	}
	return false
}

// evaluatePredicateFreezes scans every tracked contract and freezes any whose
// freeze predicate now holds. Unlike event triggers (which fire on ingestion),
// predicates depend on the wall clock, so this is the periodic re-check hook —
// driven by the engine tick — that catches a contract crossing its threshold
// (e.g. expiry+grace elapsing) while the indexer is live.
func (e *Engine) evaluatePredicateFreezes(now time.Time) {
	var targets []string
	e.mu.RLock()
	for _, cs := range e.contracts {
		if e.matchesFreezePredicate(cs, now) {
			targets = append(targets, cs.config.Name)
		}
	}
	e.mu.RUnlock()
	for _, name := range targets {
		if err := e.FreezeContract(e.runCtx, name); err != nil {
			e.logger.Error("failed to freeze contract (predicate)", "contract", name, "error", err)
		}
	}
}

// evaluatePredicateContract is the on-registration predicate hook: it freezes a
// single newly-registered contract if it is already past its predicate threshold
// (e.g. a factory child discovered after it had already expired), so the engine
// never even starts polling it. Targeted to one contract to stay O(1) on the
// per-child registration path during catchup.
func (e *Engine) evaluatePredicateContract(name string) {
	now := time.Now()
	freeze := false
	e.mu.RLock()
	for _, cs := range e.contracts {
		if cs.config.Name == name {
			freeze = e.matchesFreezePredicate(cs, now)
			break
		}
	}
	e.mu.RUnlock()
	if freeze {
		if err := e.FreezeContract(e.runCtx, name); err != nil {
			e.logger.Error("failed to freeze contract (predicate)", "contract", name, "error", err)
		}
	}
}

// reconcileFrozenContracts freezes, at startup, any contract whose local freeze
// trigger event was already indexed in a previous run. Those events sit below
// the contract's resume cursor and would never be replayed, so the live/catchup
// freeze path can't catch them — this one-time scan over the already-indexed
// event tables drains the existing backlog (e.g. options that expired before the
// freeze feature existed).
//
// Only local triggers (Freeze.On) are reconciled: a local terminal event is an
// unambiguous per-instance lifecycle signal. Foreign triggers (Freeze.OnForeign)
// are deliberately skipped — a foreign event isn't tied to one instance's
// lifecycle, so "it already happened" can't be interpreted per-contract here.
// Must be called from setup() before the subscriber and view poller start, so it
// only sets the persisted Frozen flag (no live teardown is needed).
func (e *Engine) reconcileFrozenContracts(ctx context.Context) {
	frozen := 0
	for _, cs := range e.contracts {
		if cs.config.Frozen || cs.config.Freeze == nil {
			continue
		}
		if len(cs.config.Freeze.LocalEvents()) == 0 &&
			len(cs.config.Freeze.OnSibling) == 0 &&
			len(cs.config.Freeze.Predicates()) == 0 {
			continue
		}

		triggered := false
		for _, evName := range cs.config.Freeze.LocalEvents() {
			sch, ok := cs.schemas[evName]
			if !ok || sch == nil {
				// Event not in this contract's ABI / not indexed — can't reconcile it.
				continue
			}

			var filters []store.Filter
			if sch.SharedTable {
				// Shared event table holds rows for many children; scope to this one.
				filters = []store.Filter{{Field: "contract_address", Operator: "eq", Value: cs.address.String()}}
			}

			count, err := e.store.CountEvents(ctx, sch.Name, filters)
			if err != nil {
				e.logger.Warn("freeze reconcile: count failed",
					"contract", cs.config.Name, "event", evName, "table", sch.Name, "error", err)
				continue
			}
			if count > 0 {
				triggered = true
				break
			}
		}
		// Sibling triggers: freeze if the deployment sibling identified via
		// factory_meta already emitted its terminal event in a prior run (e.g.
		// an OrderBook/Exerciser whose OptionToken settled before this restart).
		if !triggered {
			for _, st := range cs.config.Freeze.OnSibling {
				if e.siblingEventAlreadyIndexed(ctx, cs, st) {
					triggered = true
					break
				}
			}
		}
		// Predicate triggers: a captured meta field already satisfies its
		// condition (e.g. expiry < now()-grace). This is how a child that never
		// emitted a terminal event — an option that expired without ever being
		// sold, so settle() never ran — freezes on the next restart with no
		// manual DB work. Evaluated against the current time; no RPC.
		if !triggered {
			now := time.Now()
			for _, p := range cs.config.Freeze.Predicates() {
				match, err := config.EvalPredicate(cs.config.FactoryMeta, p, now)
				if err != nil {
					e.logger.Warn("freeze reconcile: predicate eval failed",
						"contract", cs.config.Name, "meta_field", p.MetaField, "error", err)
					continue
				}
				if match {
					triggered = true
					break
				}
			}
		}
		if !triggered {
			continue
		}

		cs.config.Frozen = true
		frozen++
		if cs.config.Dynamic {
			if err := e.store.SaveDynamicContract(ctx, &cs.config); err != nil {
				e.logger.Error("freeze reconcile: persist failed", "contract", cs.config.Name, "error", err)
			}
		}
		e.logger.Info("froze contract on startup reconcile (terminal event already indexed)",
			"contract", cs.config.Name, "address", cs.config.Address)
	}
	if frozen > 0 {
		e.logger.Info("freeze reconcile complete", "frozen", frozen, "contracts", len(e.contracts))
	}
}

// siblingEventAlreadyIndexed reports whether the deployment sibling that cs
// references via factory_meta[st.MetaField] has already emitted st.Event in the
// indexed data. Used by startup reconcile to freeze a child (e.g. OrderBook /
// Exerciser) whose sibling OptionToken settled in a prior run.
func (e *Engine) siblingEventAlreadyIndexed(ctx context.Context, cs *contractState, st config.SiblingTrigger) bool {
	metaVal := cs.config.FactoryMeta[st.MetaField]
	if metaVal == nil {
		return false
	}
	siblingAddr, err := new(felt.Felt).SetString(fmt.Sprintf("%v", metaVal))
	if err != nil {
		return false
	}
	var sib *contractState
	for _, c := range e.contracts {
		if c.address != nil && c.address.Equal(siblingAddr) {
			sib = c
			break
		}
	}
	if sib == nil {
		return false
	}
	sch, ok := sib.schemas[st.Event]
	if !ok || sch == nil {
		return false
	}
	var filters []store.Filter
	if sch.SharedTable {
		filters = []store.Filter{{Field: "contract_address", Operator: "eq", Value: sib.address.String()}}
	}
	count, err := e.store.CountEvents(ctx, sch.Name, filters)
	if err != nil {
		e.logger.Warn("freeze reconcile: sibling count failed",
			"contract", cs.config.Name, "sibling_event", st.Event, "table", sch.Name, "error", err)
		return false
	}
	return count > 0
}

// containsString reports whether v is present in s.
func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// metaAddrEquals reports whether a factory_meta value (a decoded address, held
// as a string or *felt.Felt) equals addr. Used to match a sibling-freeze
// trigger's recorded address against an emitting contract's address.
func metaAddrEquals(metaVal any, addr *felt.Felt) bool {
	if metaVal == nil || addr == nil {
		return false
	}
	f, err := new(felt.Felt).SetString(fmt.Sprintf("%v", metaVal))
	if err != nil {
		return false
	}
	return f.Equal(addr)
}

// UpdateContract updates a registered contract's config (e.g., add new events).
func (e *Engine) UpdateContract(ctx context.Context, name string, cc *config.ContractConfig) error {
	// Deregister the old one (without dropping tables).
	if err := e.DeregisterContract(ctx, name, false); err != nil {
		return fmt.Errorf("deregistering old contract: %w", err)
	}
	// Register the new one.
	cc.Name = name
	return e.RegisterContract(ctx, cc)
}

// FindContract returns the contract config for a registered contract, or nil.
func (e *Engine) FindContract(name string) *config.ContractConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, cs := range e.contracts {
		if cs.config.Name == name {
			cc := cs.config
			return &cc
		}
	}
	return nil
}

// Run starts the indexing engine. It resolves ABIs, creates table schemas,
// determines the starting block, starts the event subscriber, and processes
// events until the context is canceled.
func (e *Engine) Run(ctx context.Context) error {
	// Step 1: Resolve ABIs and build per-contract state.
	if !e.setupDone {
		if err := e.setup(ctx); err != nil {
			return fmt.Errorf("engine setup: %w", err)
		}
	}

	// Step 2: Determine per-contract starting blocks.
	startBlocks, err := e.determineStartBlocks(ctx)
	if err != nil {
		return fmt.Errorf("determine start blocks: %w", err)
	}
	for _, cs := range e.contracts {
		e.logger.Info("contract start block",
			"contract", cs.config.Name,
			"start_block", startBlocks[cs.config.Name],
		)
	}

	// Step 3: Build subscriptions and start the subscriber.
	subs := e.buildSubscriptions(startBlocks)
	subscriber := e.provider.NewSubscriber(subs, e.events, &provider.SubscriberConfig{
		BlocksPerQuery:     uint64(e.cfg.Indexer.BatchSize) * 10,
		ForcePolling:       e.cfg.Indexer.Transport == "http",
		CatchupWithPolling: e.cfg.Indexer.Transport == "catchup",
	})
	subscriber.SetReorgChan(e.reorgs)

	// Store references for dynamic contract management.
	e.subscriber = subscriber

	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()

	e.runCtx = subCtx

	var subErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := subscriber.Start(subCtx); err != nil && ctx.Err() == nil {
			subErr = err
			subCancel()
		}
	}()

	// Always set the onEvent callback on the poller so dynamically added views
	// (via discovery or admin API) inherit it even if no views exist at startup.
	if e.poller != nil && e.onEvent != nil {
		e.poller.SetOnEvent(e.onEvent)
	}

	// Start view poller alongside event subscriber.
	if e.poller != nil && e.poller.HasEntries() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.poller.Run(subCtx)
		}()
	}

	// Step 4: Main event loop.
	err = e.eventLoop(ctx)

	// Step 5: Graceful shutdown.
	subCancel()
	wg.Wait()

	if err != nil && ctx.Err() != nil {
		e.logger.Info("engine stopped")
		return nil
	}
	if subErr != nil {
		return fmt.Errorf("subscriber: %w", subErr)
	}
	return err
}

// setup resolves ABIs, builds event registries and table schemas, and creates
// tables in the store. Also loads persisted dynamic contracts.
func (e *Engine) setup(ctx context.Context) error {
	// Initialize contract discovery if configured.
	if err := e.setupDiscovery(); err != nil {
		return fmt.Errorf("setup discovery: %w", err)
	}

	// Load static contracts from config.
	allContracts := make([]config.ContractConfig, len(e.cfg.Contracts))
	copy(allContracts, e.cfg.Contracts)

	// Load dynamic contracts from store.
	dynamicContracts, err := e.store.GetDynamicContracts(ctx)
	if err != nil {
		e.logger.Warn("failed to load dynamic contracts", "error", err)
	} else {
		// Merge: static config takes precedence on name conflicts.
		staticNames := make(map[string]bool, len(e.cfg.Contracts))
		for i := range e.cfg.Contracts {
			staticNames[e.cfg.Contracts[i].Name] = true
		}
		for i := range dynamicContracts {
			dc := &dynamicContracts[i]
			if !staticNames[dc.Name] {
				dc.Dynamic = true
				// Re-sync the child's view/event config from the CURRENT parent
				// factory config in the YAML. The store holds the config snapshot
				// captured at first registration; without this, config edits
				// (e.g. pruned or reactive views) never reach already-registered
				// children — they keep polling the view set from when they were
				// first seen, forever.
				e.resyncDynamicChildConfig(dc)
				allContracts = append(allContracts, *dc)
				e.logger.Info("loaded dynamic contract from store", "name", dc.Name, "address", dc.Address)
			} else {
				e.logger.Info("skipping dynamic contract (overridden by static config)", "name", dc.Name)
			}
		}
	}

	resolver := config.NewABIResolver(e.provider)
	abis, err := resolver.ResolveAll(ctx, allContracts)
	if err != nil {
		return fmt.Errorf("resolve ABIs: %w", err)
	}

	for i := range allContracts {
		cc := &allContracts[i]
		contractABI := abis[cc.Address]
		if contractABI == nil {
			return fmt.Errorf("no ABI resolved for contract %s (%s)", cc.Name, cc.Address)
		}

		registry := abi.NewEventRegistry(contractABI)

		// For factory children with shared tables, use factory name for table naming.
		var buildOpts *schema.BuildOptions
		if cc.SharedTables && cc.FactoryName != "" {
			buildOpts = &schema.BuildOptions{
				SharedTable: true,
				FactoryName: cc.FactoryName,
			}
		}
		schemas := schema.BuildSchemas(cc, contractABI, registry, buildOpts)

		// Parse contract address.
		address, err := new(felt.Felt).SetString(cc.Address)
		if err != nil {
			return fmt.Errorf("parsing address for %s: %w", cc.Name, err)
		}

		cs := &contractState{
			config:   *cc,
			address:  address,
			abi:      contractABI,
			registry: registry,
			schemas:  schemas,
		}
		e.contracts = append(e.contracts, cs)

		// Create tables in store.
		for _, schema := range schemas {
			if err := e.store.CreateTable(ctx, schema); err != nil {
				return fmt.Errorf("create table %s: %w", schema.Name, err)
			}
			e.logger.Info("created table",
				"name", schema.Name,
				"type", schema.TableType,
				"columns", len(schema.Columns),
			)
		}
	}

	// Reconcile contracts whose terminal freeze event was already indexed in a
	// previous run (cursor now past it, so it will never be replayed). Must run
	// before the view poller and subscriptions are built so frozen contracts are
	// skipped by both. Event tables exist by now (created in the loop above).
	e.reconcileFrozenContracts(ctx)

	// Set up view function poller for contracts with views configured.
	vp := NewViewPoller(e.provider, e.store, e.logger)
	viewSchemas, err := vp.Setup(e.contracts)
	if err != nil {
		return fmt.Errorf("view poller setup: %w", err)
	}
	for _, vs := range viewSchemas {
		// Add view schemas to the contract's schema map so they're
		// included in Schemas() and accessible by the API server.
		// For shared-table contracts, the view schema's Contract field
		// carries the factory/ABI name (e.g. "OrderBook") rather than
		// the instance name, so fall back to a FactoryName match so the
		// schema still gets attached and surfaces in the API.
		for _, cs := range e.contracts {
			if cs.config.Name == vs.Contract ||
				(cs.config.SharedTables && cs.config.FactoryName == vs.Contract) {
				cs.schemas[vs.Event] = vs
				break
			}
		}
		// Create view table in store.
		if err := e.store.CreateTable(ctx, vs); err != nil {
			return fmt.Errorf("create view table %s: %w", vs.Name, err)
		}
		e.logger.Info("created view table",
			"name", vs.Name,
			"type", vs.TableType,
			"columns", len(vs.Columns),
		)
	}
	e.poller = vp

	return nil
}

// resyncDynamicChildConfig refreshes a reloaded factory child's Views/Events
// from the CURRENT factory config in the YAML. The child's identity (name,
// address, factory metadata, shared-table flag) is preserved — only the
// polling/indexing config is re-applied, so config edits (pruned views, reactive
// refresh, added/removed events) take effect on the next restart instead of
// being pinned to the snapshot persisted when the child was first registered.
//
// Matching is two-tier:
//  1. Primary: the child's recorded FactoryName resolves to a parent contract
//     whose factory entry has the child's ABI.
//  2. Fallback: if that lookup fails — e.g. the factory config was relocated to
//     a shared OptionFactory contract during a migration, so the child's stored
//     FactoryName no longer carries a matching entry — match by child ABI across
//     ALL factory entries. Children of a given ABI share identical view config,
//     so the first ABI match is correct.
//
// No match at all leaves the persisted config untouched (back-compat).
func (e *Engine) resyncDynamicChildConfig(dc *config.ContractConfig) {
	if dc.FactoryName == "" {
		return
	}

	// Primary: match by FactoryName, then child ABI.
	for i := range e.cfg.Contracts {
		if e.cfg.Contracts[i].Name != dc.FactoryName {
			continue
		}
		if f := factoryEntryForABI(&e.cfg.Contracts[i], dc.ABI); f != nil {
			e.applyChildConfig(dc, f, dc.FactoryName)
			return
		}
		// Named parent found but no entry for this ABI — fall through to the ABI
		// fallback rather than giving up.
		break
	}

	// Fallback: match by child ABI across all factory entries.
	for i := range e.cfg.Contracts {
		if f := factoryEntryForABI(&e.cfg.Contracts[i], dc.ABI); f != nil {
			e.applyChildConfig(dc, f, e.cfg.Contracts[i].Name+" (abi fallback)")
			return
		}
	}
	e.logger.Debug("no factory config found to resync dynamic child",
		"name", dc.Name, "factory", dc.FactoryName, "child_abi", dc.ABI)
}

// factoryEntryForABI returns the contract's factory entry whose ChildABI matches,
// or nil.
func factoryEntryForABI(c *config.ContractConfig, childABI string) *config.FactoryConfig {
	for j := range c.Factories {
		if c.Factories[j].ChildABI == childABI {
			return &c.Factories[j]
		}
	}
	return nil
}

// applyChildConfig copies a factory entry's view/event config onto a dynamic child.
func (e *Engine) applyChildConfig(dc *config.ContractConfig, f *config.FactoryConfig, via string) {
	dc.Views = f.ChildViews
	dc.Events = f.ChildEvents
	// Propagate the current freeze policy too, so freeze config added after a
	// child was first registered takes effect on the next restart (the live
	// freeze path and startup reconcile both key off dc.Freeze). A child already
	// Frozen stays frozen regardless.
	dc.Freeze = f.ChildFreeze
	e.logger.Info("resynced dynamic child config from factory",
		"name", dc.Name,
		"factory", dc.FactoryName,
		"child_abi", dc.ABI,
		"matched_via", via,
		"views", len(f.ChildViews),
		"events", len(f.ChildEvents),
		"freeze", f.ChildFreeze != nil,
	)
}

// AllContracts returns a copy of all registered contract configs (for use by API server).
func (e *Engine) AllContracts() []config.ContractConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()
	result := make([]config.ContractConfig, len(e.contracts))
	for i, cs := range e.contracts {
		result[i] = cs.config
	}
	return result
}

// determineStartBlocks computes the starting block for each contract independently.
// Logic per contract: max(persisted_cursor + 1, config_start_block).
// If no start block is configured and cursor is 0, starts from the latest chain block.
func (e *Engine) determineStartBlocks(ctx context.Context) (map[string]uint64, error) {
	result := make(map[string]uint64, len(e.contracts))

	// Lazily fetch latest block only if needed.
	var latestBlock uint64
	var latestFetched bool

	for _, cs := range e.contracts {
		cursor, err := e.store.GetCursor(ctx, cs.config.Name)
		if err != nil {
			return nil, fmt.Errorf("get cursor for %s: %w", cs.config.Name, err)
		}

		// Use per-contract start block if set (e.g., factory children use their deploy block),
		// otherwise fall back to global indexer start block. nil means "not configured".
		configStart := cs.config.StartBlock
		if configStart == nil {
			configStart = e.cfg.Indexer.StartBlock
		}

		if configStart == nil && cursor == 0 {
			// No start block configured and no cursor — use chain tip.
			if !latestFetched {
				latest, err := e.provider.BlockNumber(ctx)
				if err != nil {
					return nil, fmt.Errorf("get latest block: %w", err)
				}
				latestBlock = latest
				latestFetched = true
			}
			result[cs.config.Name] = latestBlock
			e.logger.Info("start block resolved",
				"contract", cs.config.Name,
				"source", "chain_tip",
				"value", latestBlock,
			)
			continue
		}

		var startBlock uint64
		if configStart != nil {
			startBlock = *configStart
		}
		if cursor > 0 && cursor+1 > startBlock {
			startBlock = cursor + 1
		}

		source := "config"
		if cursor > 0 && cursor+1 > derefUint64(configStart) {
			source = "cursor"
		}
		e.logger.Info("start block resolved",
			"contract", cs.config.Name,
			"source", source,
			"value", startBlock,
		)

		result[cs.config.Name] = startBlock
	}

	return result, nil
}

// derefUint64 safely dereferences a *uint64, returning 0 if nil.
func derefUint64(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}

// buildSubscriptions creates ContractSubscription entries for the subscriber.
// Each contract gets its own start block from its persisted cursor.
// If a contract only configures specific events (no wildcard "*"), the
// subscription includes a Keys filter so the node only sends matching events.
func (e *Engine) buildSubscriptions(startBlocks map[string]uint64) []provider.ContractSubscription {
	subs := make([]provider.ContractSubscription, 0, len(e.contracts))
	for _, cs := range e.contracts {
		// Frozen contracts keep their indexed data but are never re-subscribed:
		// no event subscription, no view polling. This is what makes a freeze
		// survive restarts (the Frozen flag is persisted with the contract).
		if cs.config.Frozen {
			e.logger.Info("skipping subscription for frozen contract", "contract", cs.config.Name)
			continue
		}
		sub := provider.ContractSubscription{
			Address:    cs.address,
			StartBlock: startBlocks[cs.config.Name],
		}

		// Only set key filters when there is no wildcard event configured.
		if !hasWildcardEvent(&cs.config) {
			var selectors []*felt.Felt
			for _, ec := range cs.config.Events {
				if ev := cs.registry.MatchName(ec.Name); ev != nil {
					selectors = append(selectors, ev.Selector)
				}
			}
			if len(selectors) > 0 {
				sub.Keys = [][]*felt.Felt{selectors}
			}
		}

		subs = append(subs, sub)
	}

	// Add UDC subscription for contract discovery by class hash.
	if e.discovery != nil {
		udcStart := e.discoveryStartBlock(context.Background())
		subs = append(subs, provider.ContractSubscription{
			Address:    e.discovery.udcAddress,
			StartBlock: udcStart,
			Keys:       [][]*felt.Felt{{e.discovery.udcSelector}},
		})
		e.logger.Info("UDC subscription added for contract discovery",
			"start_block", udcStart,
		)
	}

	return subs
}

// hasWildcardEvent returns true if the contract config includes a "*" event entry.
func hasWildcardEvent(cc *config.ContractConfig) bool {
	for _, ec := range cc.Events {
		if ec.Name == "*" {
			return true
		}
	}
	return false
}

// freezePredicateInterval is how often the engine re-checks time-based freeze
// predicates (e.g. expiry+grace). Freeze conditions are day-scale, so an hourly
// tick is ample; tighten here if sub-hour granularity is ever needed.
const freezePredicateInterval = time.Hour

// eventLoop processes events and reorg notifications until the context is canceled.
func (e *Engine) eventLoop(ctx context.Context) error {
	freezeTicker := time.NewTicker(freezePredicateInterval)
	defer freezeTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-freezeTicker.C:
			// Periodic re-check: freeze contracts whose time-based predicate has
			// crossed its threshold since the last tick (event triggers fire on
			// ingestion and don't need this).
			e.evaluatePredicateFreezes(time.Now())

		case reorg := <-e.reorgs:
			if err := e.handleReorg(ctx, reorg); err != nil {
				e.logger.Error("reorg handling failed", "error", err)
				return fmt.Errorf("handle reorg: %w", err)
			}

		case event, ok := <-e.events:
			if !ok {
				return nil
			}

			// Route UDC events to the discovery handler instead of normal processing.
			if e.isDiscoveryEvent(&event) {
				e.handleDiscoveryEvent(ctx, &event)
				continue
			}

			if err := e.processEvent(ctx, &event); err != nil {
				e.logger.Error("event processing failed",
					"block", event.BlockNumber,
					"error", err,
				)
				continue
			}
		}
	}
}

// EventChan returns the engine's event channel for direct injection (testing).
func (e *Engine) EventChan() chan<- provider.RawEvent {
	return e.events
}

// ReorgChan returns the engine's reorg channel for direct injection (testing).
func (e *Engine) ReorgChan() chan<- provider.ReorgNotification {
	return e.reorgs
}

// IsFactory returns true if a contract with the given name is registered
// and has a factory configuration.
func (e *Engine) IsFactory(name string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, cs := range e.contracts {
		if cs.config.Name == name && len(cs.config.Factories) > 0 {
			return true
		}
	}
	return false
}

// InjectContractForTest adds a contract directly to the engine's internal state
// without ABI resolution. This is a test helper — do not use in production.
func (e *Engine) InjectContractForTest(cc *config.ContractConfig, schemas map[string]*types.TableSchema) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var addr *felt.Felt
	if cc.Address != "" {
		addr, _ = new(felt.Felt).SetString(cc.Address)
	}
	e.contracts = append(e.contracts, &contractState{
		config:  *cc,
		address: addr,
		schemas: schemas,
	})
}

// ViewStatuses returns status info for all polled view functions.
func (e *Engine) ViewStatuses() []ViewStatus {
	if e.poller == nil {
		return nil
	}
	return e.poller.Status()
}

// startViewsForContract sets up view function polling for a dynamically registered
// contract. It calls ViewPoller.AddContract to build entries and spawn goroutines,
// creates view tables in the store, and adds schemas to the contract state.
// Returns the view schemas so the caller can notify the API server.
func (e *Engine) startViewsForContract(ctx context.Context, cs *contractState) ([]*types.TableSchema, error) {
	if e.poller == nil || len(cs.config.Views) == 0 {
		return nil, nil
	}

	viewSchemas, err := e.poller.AddContract(ctx, cs)
	if err != nil {
		return nil, fmt.Errorf("add views for %s: %w", cs.config.Name, err)
	}

	for _, vs := range viewSchemas {
		if err := e.store.CreateTable(ctx, vs); err != nil {
			return nil, fmt.Errorf("create view table %s: %w", vs.Name, err)
		}
		cs.schemas[vs.Event] = vs
		e.logger.Info("created view table (dynamic)",
			"name", vs.Name,
			"contract", cs.config.Name,
		)
	}

	return viewSchemas, nil
}

// Store returns the engine's store (for testing/inspection).
func (e *Engine) Store() store.Store {
	return e.store
}
