package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the top-level ibis configuration.
type Config struct {
	Network   string           `yaml:"network"`
	RPC       string           `yaml:"rpc"`
	Database  DatabaseConfig   `yaml:"database"`
	API       APIConfig        `yaml:"api"`
	Indexer   IndexerConfig    `yaml:"indexer"`
	Contracts []ContractConfig `yaml:"contracts"`
	Discover  []DiscoverConfig `yaml:"discover,omitempty"`
}

type DatabaseConfig struct {
	Backend  string         `yaml:"backend"`
	Postgres PostgresConfig `yaml:"postgres"`
	Badger   BadgerConfig   `yaml:"badger"`
}

type PostgresConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Name     string `yaml:"name"`
}

type BadgerConfig struct {
	Path string `yaml:"path"`
}

type APIConfig struct {
	Host        string   `yaml:"host"`
	Port        int      `yaml:"port"`
	CORSOrigins []string `yaml:"cors_origins"`
	AdminKey    string   `yaml:"admin_key"`
}

type IndexerConfig struct {
	StartBlock    *uint64         `yaml:"start_block"`
	PendingBlocks bool            `yaml:"pending_blocks"`
	BatchSize     int             `yaml:"batch_size"`
	Transport     string          `yaml:"transport,omitempty"`
	UDCAddress    string          `yaml:"udc_address,omitempty"`
	UDCEvent      *UDCEventFormat `yaml:"udc_event,omitempty"`

	// Polling cadence knobs. All optional; when empty/zero the built-in defaults
	// in internal/provider apply. Exposed so the chain-tip poll rate and catchup
	// aggressiveness can be tuned per deployment without recompiling the binary —
	// tip-polling RPC dominates Compute-Unit consumption on busy indexers.
	//
	//   TipPollInterval      — how often the shared chain-tip poller refreshes the
	//                          latest block number, and the at-tip re-check cadence
	//                          (duration string, e.g. "2s"; default 2s).
	//   CatchupPollInterval  — delay between catchup iterations while behind tip
	//                          (duration string, e.g. "100ms"; default 100ms).
	//   MaxConcurrentCatchup — bound on concurrent catchup/poll iterations across
	//                          all contract goroutines (default 16).
	TipPollInterval      string `yaml:"tip_poll_interval,omitempty"`
	CatchupPollInterval  string `yaml:"catchup_poll_interval,omitempty"`
	MaxConcurrentCatchup int    `yaml:"max_concurrent_catchup,omitempty"`
}

// UDCEventFormat configures how ibis parses UDC ContractDeployed events.
// The two known layouts differ by Cairo version:
//   - v1 (modern Cairo): keys[1]=address, data[0]=classHash
//   - v0 (Cairo 0): data[0]=address, data[3]=classHash
//
// When Version is "auto" (default), ibis auto-detects based on key count.
type UDCEventFormat struct {
	// Version selects the UDC event layout: "auto", "v0", or "v1". Default: "auto".
	Version string `yaml:"version,omitempty"`

	// Fine-grained overrides (only valid when version is "auto" or omitted):
	AddressKey    *int `yaml:"address_key,omitempty"`     // index in keys[] for deployed address
	AddressData   *int `yaml:"address_data,omitempty"`    // index in data[] for deployed address
	ClassHashKey  *int `yaml:"class_hash_key,omitempty"`  // index in keys[] for class hash
	ClassHashData *int `yaml:"class_hash_data,omitempty"` // index in data[] for class hash
}

type ContractConfig struct {
	Name       string        `yaml:"name" json:"name"`
	Address    string        `yaml:"address" json:"address"`
	ABI        string        `yaml:"abi" json:"abi"`
	Events     []EventConfig `yaml:"events" json:"events"`
	Views      []ViewConfig  `yaml:"views,omitempty" json:"views,omitempty"`
	StartBlock *uint64       `yaml:"start_block,omitempty" json:"start_block,omitempty"`
	Dynamic    bool          `yaml:"-" json:"dynamic,omitempty"`

	// Factories lists factory configurations for contracts that deploy child contracts.
	// Multiple entries allow a single parent event to register different child types
	// (e.g., one DeploymentCreated event that carries option_token, order_book, exerciser).
	Factories []FactoryConfig `yaml:"factories,omitempty" json:"factories,omitempty"`

	// FactoryName is set on factory-spawned child contracts to track their parent.
	FactoryName string `yaml:"-" json:"factory_name,omitempty"`

	// FactoryMeta stores additional fields from the factory event (e.g., token0, token1).
	FactoryMeta map[string]any `yaml:"-" json:"factory_meta,omitempty"`

	// SharedTables indicates this child contract writes to shared factory tables.
	SharedTables bool `yaml:"-" json:"shared_tables,omitempty"`

	// DiscoverClassHash is set on contracts discovered via class hash watching (3.9).
	DiscoverClassHash string `yaml:"-" json:"discover_class_hash,omitempty"`

	// Freeze declares an event-driven lifecycle freeze for this contract. When a
	// configured trigger event is observed, the contract is frozen: its event
	// subscription and view polling are torn down, but all indexed data is
	// retained and stays queryable. See FreezeConfig.
	Freeze *FreezeConfig `yaml:"freeze,omitempty" json:"freeze,omitempty"`

	// Frozen is runtime state (not user-authored): true once a freeze trigger
	// has fired. Persisted with the dynamic-contract record so a frozen contract
	// stays frozen across restarts and is never re-subscribed on rehydration.
	Frozen bool `yaml:"-" json:"frozen,omitempty"`
}

// FreezeConfig declares an event-driven lifecycle "freeze". When one of the
// named trigger events is observed, the owning contract is frozen: its event
// subscription (WSS or polling) and its view polling are torn down, but all
// previously indexed data is retained and remains queryable. The frozen state
// is persisted, so a frozen dynamic contract (e.g. a factory child) stays
// frozen across restarts and is never re-subscribed during rehydration.
//
// The trigger event must be terminal for the contract — no further events of
// interest should occur after it — because freezing stops fetching the
// contract's subsequent events.
type FreezeConfig struct {
	// On lists event names on THIS contract that trigger the freeze
	// (e.g. an option token's Settled/Expired event).
	On []string `yaml:"on,omitempty" json:"on,omitempty"`

	// OnForeign lists (contract, event) pairs on OTHER tracked contracts that
	// trigger the freeze. A foreign trigger freezes every contract that declares
	// it, so it is best used for 1:1 relationships or static targets; for
	// per-instance freezing prefer a local event the instance itself emits.
	OnForeign []ForeignTrigger `yaml:"on_foreign,omitempty" json:"on_foreign,omitempty"`

	// OnSibling triggers a PER-INSTANCE freeze off an event emitted by a sibling
	// contract identified through this contract's factory_meta. Unlike OnForeign
	// (which matches by contract name and so fires for every instance), a sibling
	// trigger freezes only the contract whose factory_meta[MetaField] equals the
	// emitting address — so e.g. an OptionToken's Settled freezes exactly the
	// OrderBook/Exerciser deployed alongside it (they each carry the option
	// token's address in factory_meta), not every order book.
	OnSibling []SiblingTrigger `yaml:"on_sibling,omitempty" json:"on_sibling,omitempty"`

	// Any is the declarative, OR-combined rule list: the contract freezes when
	// ANY rule matches. A rule is either an observed local event (rule.Event,
	// equivalent to adding to On) or a predicate over a captured factory_meta
	// field (rule.Predicate). This is additive — On/OnForeign/OnSibling keep
	// working unchanged, and an event rule here is merged with On via
	// LocalEvents(). Predicates exist to express "should stop indexing" for
	// contracts whose terminal condition is NOT an event (e.g. an option that
	// expired without ever being sold emits no Settled, but expiry < now()-grace
	// still means it is dead).
	Any []FreezeRule `yaml:"any,omitempty" json:"any,omitempty"`
}

// FreezeRule is one entry in FreezeConfig.Any. Exactly one of Event or Predicate
// should be set. Event is a local trigger event on THIS contract (same semantics
// as a FreezeConfig.On entry); Predicate is a comparison over a captured
// factory_meta field, re-evaluated against the current time.
type FreezeRule struct {
	// Event names a local event whose observation triggers the freeze.
	Event string `yaml:"event,omitempty" json:"event,omitempty"`
	// Predicate compares a stored factory_meta field against a value.
	Predicate *FreezePredicate `yaml:"predicate,omitempty" json:"predicate,omitempty"`
}

// FreezePredicate freezes a contract when meta_field <op> value holds. MetaField
// names a key in the contract's captured factory_meta (e.g. "expiry", already
// captured from the DeploymentCreated event — no extra RPC). Op is one of
// lt|gt|lte|gte|eq. Value is either a numeric literal or a time expression
// "now() [+|-] <duration>" with a duration of the form <int><unit>, unit in
// s|m|h|d. now() resolves to the current unix time in seconds, so a meta_field
// holding a unix timestamp (like expiry) compares directly. Example: freeze two
// days after expiry => {meta_field: expiry, op: lt, value: "now() - 2d"}.
type FreezePredicate struct {
	MetaField string `yaml:"meta_field" json:"meta_field"`
	Op        string `yaml:"op" json:"op"`
	Value     string `yaml:"value" json:"value"`
}

// LocalEvents returns the union of the legacy On events and the event-kind rules
// in Any — the THIS-contract events that trigger a freeze. The no-Any fast path
// returns On directly so the common case stays allocation-free.
func (f *FreezeConfig) LocalEvents() []string {
	if f == nil {
		return nil
	}
	if len(f.Any) == 0 {
		return f.On
	}
	out := make([]string, 0, len(f.On)+len(f.Any))
	out = append(out, f.On...)
	for _, r := range f.Any {
		if r.Event != "" {
			out = append(out, r.Event)
		}
	}
	return out
}

// Predicates returns the predicate-kind rules declared in Any.
func (f *FreezeConfig) Predicates() []FreezePredicate {
	if f == nil || len(f.Any) == 0 {
		return nil
	}
	out := make([]FreezePredicate, 0, len(f.Any))
	for _, r := range f.Any {
		if r.Predicate != nil {
			out = append(out, *r.Predicate)
		}
	}
	return out
}

// SiblingTrigger freezes a contract when Event fires on the contract whose
// address is stored in this contract's factory_meta[MetaField]. Used for
// per-deployment cross-child freezes (the children of one DeploymentCreated
// each hold their siblings' addresses in factory_meta).
type SiblingTrigger struct {
	// Event is the event name on the sibling that triggers the freeze.
	Event string `yaml:"event" json:"event"`
	// MetaField is the factory_meta key holding the sibling's address.
	MetaField string `yaml:"meta_field" json:"meta_field"`
}

// FactoryConfig defines factory contract indexing settings.
type FactoryConfig struct {
	// Event is the factory event name that signals a new child contract (e.g., "PairCreated").
	Event string `yaml:"event" json:"event"`

	// ChildAddressField is the field in the factory event containing the child's address.
	ChildAddressField string `yaml:"child_address_field" json:"child_address_field"`

	// ChildABI is the ABI source for child contracts ("fetch", file path, or contract name).
	ChildABI string `yaml:"child_abi" json:"child_abi"`

	// ChildEvents defines the event/table config template applied to each child contract.
	ChildEvents []EventConfig `yaml:"child_events" json:"child_events"`

	// ChildViews defines view functions to poll on each auto-discovered child contract.
	// Mirrors the semantics of the static contracts:.views field.
	ChildViews []ViewConfig `yaml:"child_views,omitempty" json:"child_views,omitempty"`

	// ChildFreeze is the freeze policy applied to each factory child. Mirrors
	// ContractConfig.Freeze — e.g. freeze an option child once it emits Settled,
	// so expired options stop consuming RPC while their data stays queryable.
	ChildFreeze *FreezeConfig `yaml:"child_freeze,omitempty" json:"child_freeze,omitempty"`

	// SharedTables enables shared tables for all factory children (see task 3.11).
	SharedTables bool `yaml:"shared_tables" json:"shared_tables"`

	// ChildNameTemplate is an optional Go template for child naming.
	// Supports {factory}, {short_address}, and factory event field names.
	// Default: "{factory}_{short_address}"
	ChildNameTemplate string `yaml:"child_name_template,omitempty" json:"child_name_template,omitempty"`
}

// DiscoverConfig defines class-hash-based contract discovery settings.
// When a ContractDeployed event from the UDC matches a watched class hash,
// ibis auto-registers the new contract for indexing.
type DiscoverConfig struct {
	// ClassHash is the class hash to watch for in UDC deploy events.
	ClassHash string `yaml:"class_hash" json:"class_hash"`

	// Group is an optional logical namespace for discovered contracts.
	Group string `yaml:"group,omitempty" json:"group,omitempty"`

	// ABI is the ABI source for discovered contracts ("fetch" or file path).
	ABI string `yaml:"abi" json:"abi"`

	// Events defines the event/table config template applied to discovered contracts.
	Events []EventConfig `yaml:"events" json:"events"`

	// SharedTables enables shared tables for all discovered instances of this class hash.
	// When true, all discovered contracts write to the same set of tables named
	// {abi}_{event_name} (e.g., optiontoken_transfer). Requires ABI to be a named
	// value (not "fetch" or a file path).
	SharedTables bool `yaml:"shared_tables" json:"shared_tables"`

	// Views defines view functions to poll for discovered contracts.
	Views []ViewConfig `yaml:"views,omitempty" json:"views,omitempty"`

	// NameTemplate is an optional template for naming discovered contracts.
	// Supports {class_hash_short}, {address_short}, {class_hash}, {address}, {group}.
	// Default: "{class_hash_short}_{address_short}"
	NameTemplate string `yaml:"name_template,omitempty" json:"name_template,omitempty"`
}

type EventConfig struct {
	Name  string      `yaml:"name" json:"name"`
	Table TableConfig `yaml:"table" json:"table"`
}

// UnmarshalJSON supports both nested and flat JSON formats for EventConfig.
// Nested (matches YAML structure): {"name": "Transfer", "table": {"type": "log"}}
// Flat (API shorthand):            {"name": "Transfer", "table_type": "log"}
// When both are present, the nested "table" field takes precedence.
func (e *EventConfig) UnmarshalJSON(data []byte) error {
	type alias EventConfig
	var nested alias
	if err := json.Unmarshal(data, &nested); err != nil {
		return err
	}
	*e = EventConfig(nested)

	// If the nested table.type is already set, we're done.
	if e.Table.Type != "" {
		return nil
	}

	// Check for flat shorthand fields.
	var flat struct {
		TableType string `json:"table_type"`
		UniqueKey string `json:"unique_key"`
	}
	if err := json.Unmarshal(data, &flat); err == nil {
		if flat.TableType != "" {
			e.Table.Type = flat.TableType
		}
		if flat.UniqueKey != "" && e.Table.UniqueKey == "" {
			e.Table.UniqueKey = flat.UniqueKey
		}
	}

	return nil
}

type TableConfig struct {
	Type       string            `yaml:"type" json:"type"`
	UniqueKey  string            `yaml:"unique_key" json:"unique_key,omitempty"`
	Aggregates []AggregateConfig `yaml:"aggregate" json:"aggregate,omitempty"`
}

type AggregateConfig struct {
	Column    string `yaml:"column" json:"column"`
	Operation string `yaml:"operation" json:"operation"`
	Field     string `yaml:"field" json:"field"`
}

// ViewConfig defines a view function and how often to read it.
//
// Refresh modes (mutually exclusive):
//   - interval (default): set Interval, the view is polled every Interval.
//   - constant: set Refresh: {mode: constant}, the view is read exactly once
//     when the contract is registered and never again (deploy-time immutables).
//   - reactive: set Refresh: {on: [Event, ...]}, the view is re-read only when
//     one of the named events is observed on this contract (or on a foreign
//     contract via OnForeign). Idle contracts emit no events and cost no RPC.
type ViewConfig struct {
	Function string             `yaml:"function" json:"function"`
	Calldata []string           `yaml:"calldata,omitempty" json:"calldata,omitempty"`
	Interval string             `yaml:"interval,omitempty" json:"interval,omitempty"`
	Refresh  *ViewRefreshConfig `yaml:"refresh,omitempty" json:"refresh,omitempty"`
	Table    TableConfig        `yaml:"table" json:"table"`
	Headers  map[string]string  `yaml:"headers,omitempty" json:"headers,omitempty"`
}

// Refresh mode constants.
const (
	RefreshModeInterval = "interval"
	RefreshModeConstant = "constant"
	RefreshModeReactive = "reactive"
)

// ViewRefreshConfig declares a non-interval refresh policy for a view.
//
// Accepts two YAML forms:
//
//	refresh: constant                       # scalar shorthand
//	refresh: { on: [OrderFilled], debounce: 1s, max_interval: 6h }
type ViewRefreshConfig struct {
	// Mode is "constant" or "reactive". May be left empty when On is set
	// (inferred as reactive) or when the scalar shorthand "constant" is used.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// On lists event names on THIS contract that invalidate the view.
	On []string `yaml:"on,omitempty" json:"on,omitempty"`
	// OnForeign lists (contract, event) pairs on OTHER contracts that
	// invalidate the view (e.g. a view that reads an oracle in another
	// contract). Reserved for future use; matched by foreign contract name.
	OnForeign []ForeignTrigger `yaml:"on_foreign,omitempty" json:"on_foreign,omitempty"`
	// Debounce throttles reactive reads to at most one per window, collapsing
	// bursts (e.g. several fills in one block) into a single read. Empty => a
	// small default is applied; "0" disables throttling (read on every event).
	Debounce string `yaml:"debounce,omitempty" json:"debounce,omitempty"`
	// MaxInterval is an optional staleness ceiling: force a read at least this
	// often even with no events. Empty => no ceiling (pure event-driven).
	MaxInterval string `yaml:"max_interval,omitempty" json:"max_interval,omitempty"`
}

// ForeignTrigger names an event on another contract that invalidates a view.
type ForeignTrigger struct {
	Contract string `yaml:"contract" json:"contract"`
	Event    string `yaml:"event" json:"event"`
}

// UnmarshalYAML accepts either a scalar string (e.g. `refresh: constant`) or a
// full mapping (`refresh: { on: [...] }`).
func (r *ViewRefreshConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		r.Mode = value.Value
		return nil
	}
	// Decode the mapping without recursing back into this method.
	type rawRefresh ViewRefreshConfig
	var raw rawRefresh
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*r = ViewRefreshConfig(raw)
	return nil
}

// ResolvedMode returns the effective refresh mode, inferring "reactive" when
// On/OnForeign are set but Mode was omitted.
func (r *ViewRefreshConfig) ResolvedMode() string {
	if r.Mode != "" {
		return r.Mode
	}
	if len(r.On) > 0 || len(r.OnForeign) > 0 {
		return RefreshModeReactive
	}
	return ""
}

// envVarPattern matches ${VAR_NAME} for environment variable expansion.
var envVarPattern = regexp.MustCompile(`\$\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// expandEnvVars replaces all ${VAR_NAME} occurrences with their environment
// variable values. Unset variables expand to empty string.
func expandEnvVars(data []byte) []byte {
	return envVarPattern.ReplaceAllFunc(data, func(match []byte) []byte {
		varName := envVarPattern.FindSubmatch(match)[1]
		return []byte(os.Getenv(string(varName)))
	})
}

// Load reads the YAML config file at path, expands environment variables,
// parses it into a Config, and validates it.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	expanded := expandEnvVars(data)

	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	applyDefaults(&cfg)

	if err := Validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Database.Backend == "" {
		cfg.Database.Backend = "memory"
	}
	if cfg.Database.Postgres.Port == 0 {
		cfg.Database.Postgres.Port = 5432
	}
	if cfg.Database.Badger.Path == "" {
		cfg.Database.Badger.Path = "./data/ibis"
	}
	if cfg.API.Host == "" {
		cfg.API.Host = "0.0.0.0"
	}
	if cfg.API.Port == 0 {
		cfg.API.Port = 8080
	}
	if cfg.Indexer.BatchSize == 0 {
		cfg.Indexer.BatchSize = 10
	}
	if cfg.Indexer.UDCAddress == "" {
		cfg.Indexer.UDCAddress = "0x04a64cd09a853868621d94cae9952b106f2c36a3f81260f85de6696c6b050221"
	}

	for i := range cfg.Contracts {
		if cfg.Contracts[i].ABI == "" {
			cfg.Contracts[i].ABI = "fetch"
		}
	}
}

// RPCScheme returns the scheme of the RPC URL (wss, ws, https, http).
func (c *Config) RPCScheme() string {
	if idx := strings.Index(c.RPC, "://"); idx != -1 {
		return c.RPC[:idx]
	}
	return ""
}

// IsWSS returns true if the RPC URL uses a WebSocket scheme.
func (c *Config) IsWSS() bool {
	scheme := c.RPCScheme()
	return scheme == "wss" || scheme == "ws"
}

// Uint64Ptr returns a pointer to the given uint64 value.
func Uint64Ptr(v uint64) *uint64 { return &v }
