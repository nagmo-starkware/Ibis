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

	// Factory configuration for contracts that deploy child contracts.
	Factory *FactoryConfig `yaml:"factory,omitempty" json:"factory,omitempty"`

	// FactoryName is set on factory-spawned child contracts to track their parent.
	FactoryName string `yaml:"-" json:"factory_name,omitempty"`

	// FactoryMeta stores additional fields from the factory event (e.g., token0, token1).
	FactoryMeta map[string]any `yaml:"-" json:"factory_meta,omitempty"`

	// SharedTables indicates this child contract writes to shared factory tables.
	SharedTables bool `yaml:"-" json:"shared_tables,omitempty"`

	// DiscoverClassHash is set on contracts discovered via class hash watching (3.9).
	DiscoverClassHash string `yaml:"-" json:"discover_class_hash,omitempty"`
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

// ViewConfig defines a view function to poll at a configurable interval.
type ViewConfig struct {
	Function string            `yaml:"function" json:"function"`
	Calldata []string          `yaml:"calldata,omitempty" json:"calldata,omitempty"`
	Interval string            `yaml:"interval" json:"interval"`
	Table    TableConfig       `yaml:"table" json:"table"`
	Headers  map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
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
