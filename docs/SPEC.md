# Ibis - Specification

## Project Overview

**Ibis** is a fast, easy-to-use Starknet indexer written in Go. It indexes events from Starknet using only an RPC WebSocket connection, generates typed database tables and REST APIs from contract ABIs, and launches with a single command from a YAML config file.

### Problem Statement

Setting up a production-grade Starknet indexer today requires either:
- **Apibara**: Powerful but demands custom TypeScript transform logic, no auto-generated APIs, stateless by default
- **Torii**: Zero-config but locked to the Dojo ECS framework, cannot index arbitrary contracts
- **Custom solutions**: High effort to build, maintain, and evolve

There is no general-purpose Starknet indexer that provide a modern, easy-to-use developer experience with the flexibility to index any contract.

### Target Users

- Starknet application developers who need indexed event data with typed APIs
- Me
- Teams wanting a drop-in indexer they can configure, deploy, and query without writing indexer code

### Design Principles

1. **One config, one command** -- `ibis.config.yaml` + `ibis run` is all you need
2. **ABI-driven** -- contract ABIs drive table schemas, REST endpoints, and type safety
3. **Production-ready** -- pending block support, reorg handling, multiple DB backends, Docker/K8s deployment
4. **AI era ready** -- natural language queries and AI-powered config generation via Claude Code agent skills

---

## Architecture

```
                        ┌──────────────────────────┐
                        │   Starknet RPC (WSS)     │
                        └────────────┬─────────────┘
                                     │
                  ┌──────────────────┼──────────────────┐
                  │                  │                   │
       ┌──────────▼───────────┐     │        ┌──────────▼───────────┐
       │  Event Subscriber    │     │        │  View Poller         │
       │  - Per-contract subs │     │        │  - starknet_call     │
       │  - Reconnection      │     │        │  - Interval polling  │
       │  - Polling fallback  │     │        │  - ABI decoding      │
       └──────────┬───────────┘     │        └──────────┬───────────┘
                  │                 │                    │
       ┌──────────▼───────────┐     │                   │
       │  Event Processor     │     │                   │
       │  - ABI decoding      │     │                   │
       │  - Selector matching │     │                   │
       │  - Pending tracking  │     │                   │
       │  - Factory detection │     │                   │
       │  - UDC discovery     │     │                   │
       └──────────┬───────────┘     │                   │
                  │                 │                   │
                  └─────────┬───────┘───────────────────┘
                            │
                ┌───────────┼───────────┐
                │           │           │
         ┌──────▼──────┐ ┌──▼───────┐ ┌─▼──────────┐
         │  BadgerDB   │ │PostgreSQL│ │ In-Memory   │
         │  (embedded) │ │(external)│ │ (dev/test)  │
         └─────────────┘ └──────────┘ └─────────────┘
                │           │           │
                └───────────┼───────────┘
                            │
                 ┌──────────▼───────────┐
                 │   API Server         │
                 │  - REST (generated)  │
                 │  - SSE (real-time)   │
                 │  - Admin API         │
                 │  - Factory endpoints │
                 │  - Discovery endpoints│
                 │  - CORS              │
                 └──────────────────────┘
```

### Data Flow

1. **Subscribe** -- Event Subscriber connects to Starknet RPC WSS and calls `starknet_subscribeEvents` per configured contract (with `from_address` and `block_id` params). Falls back to `starknet_getEvents` HTTP polling if WSS fails.
2. **Process** -- Event Processor matches incoming events by selector (`keys[0]`) against ABI event definitions, then decodes `keys[]` and `data[]` Felt arrays into typed data. Factory events trigger child contract registration. UDC events trigger class-hash-based contract discovery.
3. **Store** -- Decoded events are written to the configured database backend using revert/add operation pairs (for pending block safety)
4. **Serve** -- API Server exposes auto-generated REST endpoints, SSE streams, admin endpoints, and factory/discovery endpoints based on the ABI-derived table schemas
5. **Reorg** -- On reorg notification (delivered inline via event subscription), revert operations undo orphaned data. Factory children and discovered contracts deployed within reverted blocks are deregistered. If reorg notifications are not available, the engine uses linear forward progression with cursor-based resume.
6. **Backfill** -- On startup, if the cursor is behind chain head, uses `starknet_getEvents` HTTP RPC with continuation tokens to catch up before switching to WSS streaming
7. **Poll** -- View Poller periodically calls `starknet_call` on configured view functions, decodes the results via ABI, and stores them as indexed data with the same table/API semantics as events.

---

## Tech Stack

| Component | Technology | Rationale |
|-----------|-----------|-----------|
| Language | Go 1.25+ | Performance, concurrency, single binary |
| Starknet SDK | `NethermindEth/starknet.go` v0.17+ | Official Go SDK, RPC + WS support, selectors, hashing |
| Juno types | `NethermindEth/juno` | Felt type, crypto primitives |
| WebSocket | `gorilla/websocket` (via starknet.go) | Industry standard, used internally by starknet.go |
| Embedded DB | BadgerDB v4 (`dgraph-io/badger/v4`) | Fast KV store, prefix scanning, used by major blockchains |
| Relational DB | PostgreSQL (via `pgx/v5`) | Production-grade, pgx is the fastest Go Postgres driver |
| Query scanning | `georgysavva/scany/v2` | Struct scanning for pgx result sets |
| In-Memory DB | Custom (Go maps + mutexes) | Zero-dependency dev/test mode |
| Config | `gopkg.in/yaml.v3` | YAML parsing with env var expansion |
| HTTP Router | `net/http` (stdlib) | Go 1.22+ has built-in routing, no dependency needed |
| SSE | Custom (stdlib) | Simple streaming over HTTP, no library needed |
| CLI | `spf13/cobra` | Standard Go CLI framework |
| ABI Parsing | Custom | starknet.go lacks ABI decoding; build on foc-engine's parser patterns |
| Testing | `testcontainers-go` | PostgreSQL integration tests with disposable containers |
| Containerization | Docker | Standard deployment |
| Task Runner | Makefile | Per user preferences |

---

## Project Structure

```
ibis/
├── cmd/
│   └── ibis/
│       └── main.go                  # CLI entry point (cobra root)
├── internal/
│   ├── abi/                         # ABI parsing and event decoding
│   │   ├── parser.go                # Parse Cairo ABI JSON into Go types
│   │   ├── decoder.go               # Decode Felt arrays into typed event data
│   │   ├── encoder.go               # Encode function calldata from string params
│   │   ├── selector.go              # Event/function selector computation and matching
│   │   └── types.go                 # ABI type definitions (struct, enum, tuple, felt, etc.)
│   ├── api/                         # HTTP API server
│   │   ├── server.go                # Server setup, middleware, route registration, CORS
│   │   ├── handlers.go              # Generated endpoint handler logic
│   │   ├── query.go                 # Query parsing and execution
│   │   ├── sse.go                   # SSE streaming handler with Last-Event-ID replay
│   │   ├── eventbus.go              # In-memory pub/sub for SSE event distribution
│   │   ├── admin.go                 # Admin API: register/deregister/update contracts
│   │   ├── factory.go               # Factory children listing and metadata filtering
│   │   └── discover.go              # Discovery endpoint for class-hash contracts
│   ├── cli/                         # CLI commands
│   │   ├── root.go                  # Cobra root command setup
│   │   ├── init.go                  # `ibis init` -- scaffold config
│   │   ├── run.go                   # `ibis run` -- start indexer
│   │   ├── query.go                 # `ibis query` -- CLI queries
│   │   └── prompt.go                # Interactive prompt helpers
│   ├── config/                      # Configuration management
│   │   ├── config.go                # Config struct and loader
│   │   ├── validate.go              # Config validation
│   │   └── abi_resolve.go           # ABI resolution (chain/local/scarb)
│   ├── engine/                      # Core indexing engine
│   │   ├── engine.go                # Main indexing orchestrator, dynamic contract lifecycle
│   │   ├── processor.go             # Event processing pipeline
│   │   ├── pending.go               # Pending event/block handling
│   │   ├── reorg.go                 # Reorg handling and rollback
│   │   ├── factory.go               # Factory child detection, registration, shared tables
│   │   ├── discover.go              # UDC watching, class-hash-based contract discovery
│   │   └── poller.go                # View function polling loop
│   ├── provider/                    # Starknet RPC/WS provider
│   │   ├── provider.go              # RPC + WS provider wrapper, starknet_call
│   │   └── subscriber.go            # Event subscription manager, dynamic add/remove
│   ├── store/                       # Database abstraction
│   │   ├── store.go                 # Store interface definitions
│   │   ├── operations.go            # Revert/add operation pair types
│   │   ├── query.go                 # Query, Filter, AggResult types
│   │   ├── badger/                  # BadgerDB implementation
│   │   │   └── badger.go
│   │   ├── postgres/                # PostgreSQL implementation
│   │   │   └── postgres.go
│   │   └── memory/                  # In-memory implementation
│   │       └── memory.go
│   ├── schema/                      # ABI-derived table schema system
│   │   ├── generator.go             # Schema generation from config, view schemas
│   │   ├── postgres.go              # PostgreSQL DDL generation
│   │   └── badger.go                # BadgerDB key layout generation
│   ├── types/                       # Shared type definitions
│   │   └── types.go
│   └── deps.go                      # Build dependency anchors
├── configs/                         # Example configs
│   ├── ibis.config.yaml             # Example config
│   └── ibis.config.docker.yaml      # Docker example
├── docs/
│   ├── SPEC.md                      # This file
│   ├── ROADMAP.md                   # Development roadmap
│   ├── GETTING-STARTED.md           # Quick start guide
│   ├── CONFIGURATION.md             # Config reference
│   ├── API-REFERENCE.md             # REST API reference
│   ├── CLI-REFERENCE.md             # CLI commands reference
│   ├── TABLE-TYPES.md               # Table type guide
│   └── ADVANCED-FEATURES.md         # Factory, discovery, views guide
├── scripts/                         # Bash scripts
│   └── install.sh                   # Binary installer
├── Dockerfile
├── docker-compose.yaml
├── Makefile
├── go.mod
├── go.sum
└── README.md
```

---

## Core Modules

### ABI Parser (`internal/abi/`)

Parses Cairo contract ABIs (JSON) into Go type definitions. Supports all Starknet/Cairo types:
- Primitives: `felt252`, `u8`-`u256`, `i8`-`i128`, `bool`, `ContractAddress`, `ClassHash`, `ByteArray`
- Composites: `Array<T>`, `Span<T>`, structs, enums
- Tuples: `(T1, T2, ...)` -- flattened into named fields (`v0`, `v1`, etc.)
- Unit: `()` -- zero-felt type
- Snapshots: `@Array<T>`, `@Span<T>`

Computes event selectors via `starknet_keccak` and matches incoming event keys to ABI event definitions. Decodes the `keys[]` and `data[]` Felt arrays into typed Go maps based on the ABI member definitions.

Also parses **function definitions** (`FunctionDef`) for view function polling, including input parameters, output parameters, and state mutability. Provides `EncodeFunctionCalldata` for encoding calldata from string parameters and `DecodeFunctionOutputs` for decoding `starknet_call` results.

Reference: foc-engine's `internal/registry/parser.go` for the visitor pattern.

### Config Manager (`internal/config/`)

Loads `ibis.config.yaml` with the following structure:

```yaml
# ibis.config.yaml
network: mainnet                        # mainnet | sepolia | custom
rpc: wss://starknet-mainnet.example.com # RPC WSS endpoint

database:
  backend: postgres                     # postgres | badger | memory
  postgres:
    host: localhost
    port: 5432
    user: ibis
    password: ${IBIS_DB_PASSWORD}
    name: ibis
  badger:
    path: ./data/ibis

api:
  host: 0.0.0.0
  port: 8080
  cors_origins:                         # Allowed CORS origins (default: ["*"])
    - http://localhost:3000
    - https://myapp.com
  admin_key: ${IBIS_ADMIN_KEY}          # Optional API key for admin endpoints

indexer:
  start_block: 0                        # 0 = genesis block. Omit to start from latest
  pending_blocks: true                  # Index pre-confirmed blocks
  batch_size: 10                        # Blocks per batch for backfill
  udc_address: "0x04a64cd09..."         # Universal Deployer Contract address (auto-set for mainnet/sepolia)
  udc_event:                            # Optional UDC event format override
    version: auto                       # "auto", "v0" (Cairo 0), or "v1" (modern Cairo)
    # Fine-grained overrides (when version is "auto"):
    # address_key: 1                    # index in keys[] for deployed address
    # address_data: 0                   # index in data[] for deployed address
    # class_hash_key: 0                 # index in keys[] for class hash
    # class_hash_data: 0                # index in data[] for class hash

contracts:
  - name: StarknetOptions
    address: "0x049d36570d4e46..."
    abi: ./target/dev/stops_StarknetOptions.contract_class.json  # Local path
    # OR
    # abi: fetch                        # Fetch from chain (default)
    # OR
    # abi: StarknetOptions              # Smart local discovery + scarb build
    start_block: 100000                 # Optional per-contract start block
    events:
      - name: "*"                       # Wildcard: index ALL events as log tables
        table:
          type: log
      - name: LeaderboardUpdate         # Override: specific events get custom table types
        table:
          type: unique
          unique_key: trader_address    # Field to use as unique identifier
      - name: VolumeUpdate
        table:
          type: aggregation
          aggregate:
            - column: total_volume
              operation: sum
              field: volume
            - column: trade_count
              operation: count
    views:                              # Optional view function polling
      - function: get_price
        interval: 30s
        table:
          type: unique
          unique_key: _view_key

    # Factory configuration for contracts that deploy children.
    factory:
      event: PairCreated                # Factory event that signals a new child
      child_address_field: pair         # Field in factory event containing child address
      child_abi: fetch                  # ABI source for children
      shared_tables: true               # All children write to same tables
      child_name_template: "{factory}_{short_address}"  # Naming template
      child_events:                     # Event config applied to each child
        - name: "*"
          table:
            type: log

# Class-hash-based contract discovery via UDC watching.
discover:
  - class_hash: "0xabc123..."           # Watch for deployments of this class hash
    group: options                       # Logical namespace for discovered contracts
    abi: optiontoken                     # ABI source (named value for shared tables)
    shared_tables: true                  # All discovered instances share tables
    name_template: "{group}_{address_short}"  # Naming template
    events:
      - name: "*"
        table:
          type: log
    views:                              # Views applied to discovered contracts
      - function: get_balance
        interval: 1m
        table:
          type: unique
          unique_key: _view_key
```

**Event selection rules**:
- `"*"` -- wildcard, matches all events defined in the contract's ABI
- Specific event entries override the wildcard for that event (e.g., `LeaderboardUpdate` above overrides the `*` default of `log` with `unique`)
- If no `"*"` entry exists, only explicitly listed events are indexed
- The wildcard table type sets the default; specific entries always take precedence

ABI resolution priority:
1. Explicit file path (e.g., `./target/dev/...`)
2. Smart local discovery by contract name (searches `target/dev/`, builds with `scarb` if needed)
3. Fetch from chain via RPC `getClassAt`

Environment variable expansion via `${VAR_NAME}` syntax.

### Indexing Engine (`internal/engine/`)

The core orchestrator that coordinates event processing. Unlike block-scanning indexers, Ibis subscribes directly to contract events via `starknet_subscribeEvents` (like foc-engine):

1. **Startup**: Resolve ABIs, compute event selectors, load persisted dynamic contracts from store, determine per-contract starting block from cursor, set up view poller and discovery state
2. **Backfill** (if behind): Use `starknet_getEvents` HTTP RPC with continuation tokens to catch up in chunks
3. **Stream**: Subscribe to `starknet_subscribeEvents` per contract via WSS, starting from each contract's cursor
4. **Process**: For each received event, match selector, decode via ABI, generate operation pairs, write to store. Factory events are routed to child registration. UDC events are routed to discovery handling.
5. **Fallback**: If WSS fails, fall back to `starknet_getEvents` polling loop (adaptive timing: 100ms during catchup, 2s at chain tip)
6. **Reorg**: If the WSS subscription delivers reorg notifications, execute revert operations for orphaned data. Deregister factory children and discovered contracts deployed within reverted block range.
7. **Dynamic contracts**: Contracts can be registered/deregistered at runtime via the admin API. The engine resolves ABIs, builds schemas, creates tables, persists the config, and spawns subscriptions on the fly.

**Pending Block Strategy**: Every database write is recorded as an `(add, revert)` operation pair. The `add` operation writes the new data. The `revert` operation undoes it (delete for inserts, restore previous value for updates). When a pending block is replaced or reverted, the revert operations are executed in reverse order. After a configurable confirmation depth (default: 10 blocks), pending operations are promoted to confirmed and revert data is discarded.

**Per-contract cursors**: Each contract tracks its own last-processed block number independently. On startup, the engine computes `max(persisted_cursor + 1, config_start_block)` per contract. If neither is set, indexing starts from the chain tip.

### Factory & Discovery (`internal/engine/factory.go`, `internal/engine/discover.go`)

Ibis supports two mechanisms for dynamically registering contracts at runtime:

**Factory contracts** -- A factory contract emits an event (e.g., `PairCreated`) when it deploys a child. The engine detects these events, extracts the child address from a configurable field, and auto-registers the child for indexing:

1. The factory event is matched by name in the event processor
2. The child address is extracted from the configured `child_address_field`
3. A child `ContractConfig` is built from the factory template (`child_events`, `child_abi`)
4. The child's ABI is resolved (cached after the first child to avoid repeated RPC calls)
5. The child is registered: tables created, config persisted, subscription spawned
6. Additional factory event fields (e.g., `token0`, `token1`) are stored as `factory_meta`

**Shared tables** -- When `shared_tables: true`, all factory children (or all discovered instances of a class hash) write to the same set of tables. Table names use the factory/ABI name instead of the individual contract name (e.g., `myfactory_transfer` instead of `myfactory_abc123_transfer`). A `contract_name` column distinguishes rows from different contracts. Schemas are built once on the first child and cached; subsequent children reuse the same table references.

**Class-hash-based discovery** -- The `discover[]` config watches for UDC `ContractDeployed` events matching specific class hashes. When a match is found, the deployed contract is auto-registered:

1. The engine subscribes to the UDC address for `ContractDeployed` events
2. Each event is parsed using auto-detected or configured UDC layout (v0/v1)
3. The class hash is matched against `discover[]` entries
4. The contract is registered using the discover template (events, views, ABI)
5. ABI is cached per class hash (same class hash = same code = same ABI)

**UDC event formats** -- Two known layouts:
- **v1 (modern Cairo)**: `keys[1]=address`, `data[0]=classHash`
- **v0 (Cairo 0)**: `data[0]=address`, `data[3]=classHash`

Auto-detection chooses based on key/data array sizes. Fine-grained overrides allow custom layouts.

### View Function Polling (`internal/engine/poller.go`)

The `ViewPoller` periodically calls `starknet_call` on configured view functions and stores the results as indexed data with the same table/API/SSE semantics as events.

**Lifecycle**:
1. During engine setup, `ViewPoller.Setup()` resolves function definitions from ABIs, parses calldata, and builds view table schemas
2. On `Run()`, per-function goroutines poll at the configured interval with startup jitter to spread RPC load
3. Each poll: get current block number, execute `starknet_call`, decode output via ABI, store as operation
4. On reorg, `NotifyReorg()` broadcasts to all goroutines to re-poll immediately

**Table semantics**:
- View results get a `_view_key` column for key management
- `unique` tables with `unique_key: _view_key` maintain a single "latest" row per contract
- `log` tables append each poll result as a time series
- Shared view tables include a `contract_name` column

**Dynamic views**: When a contract is registered at runtime (via admin API, factory, or discovery), `ViewPoller.AddContract()` builds entries, creates tables, and spawns polling goroutines on the fly.

### Store Interface (`internal/store/`)

Database-agnostic interface supporting three backends:

```go
type Store interface {
    // Event operations (with revert support)
    ApplyOperations(ctx context.Context, ops []Operation) error
    RevertOperations(ctx context.Context, ops []Operation) error

    // Query operations
    GetEvents(ctx context.Context, table string, query Query) ([]types.IndexedEvent, error)
    GetUniqueEvents(ctx context.Context, table string, query Query) ([]types.IndexedEvent, error)
    GetAggregation(ctx context.Context, table string, query Query) (AggResult, error)
    CountEvents(ctx context.Context, table string, filters []Filter) (int64, error)

    // Cursor tracking (per-contract)
    GetCursor(ctx context.Context, contract string) (uint64, error)
    SetCursor(ctx context.Context, contract string, blockNumber uint64) error
    GetAllCursors(ctx context.Context) (map[string]uint64, error)
    DeleteCursor(ctx context.Context, contract string) error

    // Schema management
    CreateTable(ctx context.Context, schema *types.TableSchema) error
    MigrateTable(ctx context.Context, schema *types.TableSchema) error
    DropTable(ctx context.Context, tableName string) error

    // Dynamic contract persistence
    SaveDynamicContract(ctx context.Context, cc *config.ContractConfig) error
    GetDynamicContracts(ctx context.Context) ([]config.ContractConfig, error)
    DeleteDynamicContract(ctx context.Context, name string) error

    Close() error
}
```

Key differences from the original simplified interface:
- **Per-contract cursors**: `GetCursor`/`SetCursor` take a `contract` parameter; `GetAllCursors` returns all cursors; `DeleteCursor` removes a contract's cursor
- **CountEvents**: Separate count query with filters (used by the `/count` endpoint)
- **DropTable**: Removes a table entirely (used when deregistering contracts)
- **Dynamic contract persistence**: `SaveDynamicContract`/`GetDynamicContracts`/`DeleteDynamicContract` persist dynamically registered contract configs so they survive restarts

### API Server (`internal/api/`)

Auto-generates REST endpoints from the ABI-derived table schemas:

```
# For each configured event table:
GET  /v1/{contract}/{event}                  # List events (paginated)
GET  /v1/{contract}/{event}/latest           # Get latest event
GET  /v1/{contract}/{event}/count            # Count events
GET  /v1/{contract}/{event}/unique           # List unique entries (unique tables)
GET  /v1/{contract}/{event}/aggregate        # Get aggregated values (aggregation tables)

# SSE streaming:
GET  /v1/{contract}/{event}/stream           # SSE stream of new events

# Factory endpoints:
GET  /v1/{factory}/children                  # List factory children with metadata
GET  /v1/{factory}/children/count            # Count factory children

# Discovery endpoints:
GET  /v1/discover/{classHash}/contracts      # List contracts discovered by class hash

# Admin endpoints (protected by X-Admin-Key header):
POST   /v1/admin/contracts                   # Register a new contract at runtime
DELETE /v1/admin/contracts/{name}             # Deregister a contract (?drop_tables=true)
GET    /v1/admin/contracts                   # List all registered contracts
PUT    /v1/admin/contracts/{name}            # Update a contract's config

# System:
GET  /v1/health                              # Health check
GET  /v1/status                              # Indexer status (block heights, contracts, views)
```

Query parameters follow Supabase conventions:
- `?limit=50&offset=0` -- pagination
- `?order=block_number.desc` -- ordering
- `?trader_address=eq.0x123` -- field filtering
- `?block_number=gte.100000` -- comparison operators (eq, neq, gt, gte, lt, lte)

**SSE streaming details** (`internal/api/sse.go`, `internal/api/eventbus.go`):
- The `EventBus` is an in-memory pub/sub that distributes events from the engine to SSE handlers
- The engine publishes events via `onEvent` callback after each successful store write
- SSE handlers subscribe to a specific table with optional field filters
- Format: `id: {block}:{logIndex}\ndata: {json}\n\n`
- Reconnection: `Last-Event-ID` header replays missed events from the store
- Non-blocking: slow subscribers have events dropped rather than blocking the bus

**CORS** -- Configurable via `api.cors_origins`. Defaults to `["*"]`. Preflight (`OPTIONS`) is handled automatically.

**Admin authentication** -- When `api.admin_key` is set, admin endpoints require the `X-Admin-Key` header. When unset, admin endpoints are open.

**Dynamic schemas** -- When a contract is registered at runtime, `AddSchemas` adds its table schemas to the server's route map. `RemoveSchemas` cleans up when a contract is deregistered.

### Schema System (`internal/schema/`)

Translates ABI event definitions + config into table schemas:

| Table Type | Behavior | Use Case |
|-----------|----------|----------|
| `log` | Append-only event log | Transaction history, audit trail |
| `unique` | Last-write-wins by unique key | Leaderboards, current state |
| `aggregation` | Auto-computed aggregates | Volume tracking, counters |

For PostgreSQL, schemas are translated into `CREATE TABLE` statements with appropriate indices. For BadgerDB, schemas define key prefix patterns and secondary index strategies. For in-memory, schemas define map structures.

**Shared table naming** -- When `BuildOptions.SharedTable` is true, table names use the factory/ABI name: `{factory}_{event}` instead of `{contract}_{event}`. A `contract_name` column is added to distinguish rows.

**View schemas** -- `BuildViewSchema` generates table schemas for view function results. View tables include `block_number`, `timestamp`, `contract_address`, and `_view_key` metadata columns, plus columns for each function output parameter.

---

## Data Models

### Core Entities

**IndexedEvent** -- A decoded, stored event:
```go
type IndexedEvent struct {
    ID              string
    ContractAddress string
    EventName       string
    BlockNumber     uint64
    BlockHash       string
    TransactionHash string
    LogIndex        uint64
    Data            map[string]any
    Timestamp       uint64
    Status          BlockStatus       // PreConfirmed | AcceptedL2 | AcceptedL1
}
```

**Operation** -- A reversible database operation:
```go
type Operation struct {
    Type        OpType           // Insert | Update | Delete
    Table       string
    Key         string
    Data        map[string]any   // For Insert/Update
    Prev        map[string]any   // Previous data (for revert on Update/Delete)
    BlockNumber uint64
    LogIndex    uint64
}
```

**TableSchema** -- An ABI-derived table definition:
```go
type TableSchema struct {
    Name        string
    Contract    string
    Event       string
    TableType   TableType        // Log | Unique | Aggregation
    Columns     []Column
    UniqueKey   string           // For unique tables
    Aggregates  []AggregateSpec  // For aggregation tables
    SharedTable bool             // Whether multiple contracts write to same table
}
```

**ContractConfig** -- A contract's indexing configuration:
```go
type ContractConfig struct {
    Name             string
    Address          string
    ABI              string
    Events           []EventConfig
    Views            []ViewConfig           // View function polling
    StartBlock       *uint64                // Per-contract start block
    Dynamic          bool                   // Registered at runtime
    Factory          *FactoryConfig         // Factory settings (if this is a factory)
    FactoryName      string                 // Parent factory name (if this is a child)
    FactoryMeta      map[string]any         // Additional fields from factory event
    SharedTables     bool                   // Writes to shared tables
    DiscoverClassHash string                // Set on UDC-discovered contracts
}
```

**FactoryConfig** -- Factory contract settings:
```go
type FactoryConfig struct {
    Event             string       // Factory event name (e.g., "PairCreated")
    ChildAddressField string       // Event field containing child address
    ChildABI          string       // ABI source for children
    ChildEvents       []EventConfig // Event config template for children
    SharedTables      bool         // All children share tables
    ChildNameTemplate string       // Go template for child naming
}
```

**DiscoverConfig** -- Class-hash-based discovery settings:
```go
type DiscoverConfig struct {
    ClassHash    string
    Group        string         // Logical namespace
    ABI          string         // ABI source
    Events       []EventConfig
    Views        []ViewConfig
    SharedTables bool           // Discovered instances share tables
    NameTemplate string         // Template for naming discovered contracts
}
```

**FunctionDef** -- A parsed view function definition:
```go
type FunctionDef struct {
    Name            string
    FullName        string
    Selector        *felt.Felt
    Inputs          []FieldDef
    Outputs         []FieldDef
    StateMutability string     // "view" or "external"
}
```

---

## External Integrations

| Integration | Purpose | Protocol |
|------------|---------|----------|
| Starknet RPC Node | Block/event data, ABI fetching, view function calls | WSS + HTTP JSON-RPC |
| PostgreSQL | Production database backend | TCP (pgx driver) |
| Scarb | Local ABI building from Cairo source | CLI subprocess |

---

## Key Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Event subscription, not block scanning | `starknet_subscribeEvents` per contract, not `newHeads` | Direct event delivery is simpler, lower overhead, and matches foc-engine's proven pattern. HTTP `starknet_getEvents` for backfill and polling fallback. |
| Custom ABI decoder | Build on foc-engine patterns | starknet.go lacks ABI decoding; need full Cairo type system support |
| Operation pairs for reorgs | Every write has a paired revert | Clean pending block handling without full state replay |
| Supabase-style REST | Filter syntax inspired by PostgREST/Supabase | Familiar to web developers, powerful query semantics |
| Cobra for CLI | `ibis init`, `ibis run`, `ibis query` | Standard Go CLI framework, subcommand pattern |
| No GraphQL for MVP | REST + SSE is simpler | GraphQL adds complexity; REST covers most use cases; add later if needed |
| Store interface pattern | Repository pattern from zindex | Clean separation, easy to add backends, testable |
| YAML config over code | Declarative config file | Aligns with "one config, one command" philosophy; no indexer code to write |
| Shared tables for factory/discovery | Single table per event type across all children | Avoids table proliferation (hundreds of identical tables); `contract_name` column enables per-contract filtering; schemas built once and cached |
| UDC watching for discovery | Subscribe to UDC `ContractDeployed` events | Catches all deployments of a class hash without scanning blocks; auto-detect v0/v1 event format handles both Cairo versions |
| Admin API over config reload | REST endpoints for runtime contract management | Enables adding contracts without restart; integrates with Claude Code agent skills for natural-language contract management |
| SSE over WebSocket for streaming | Server-Sent Events with `Last-Event-ID` replay | Simpler than WebSocket for server→client push; native reconnection support; standard HTTP — no upgrade needed |
| Per-contract cursors | Each contract tracks its own block independently | Factory children and discovered contracts start from their deploy block, not block 0; enables mixed start blocks |

---

## Documentation

Additional documentation is available in the `docs/` directory:

- [Getting Started](GETTING-STARTED.md) -- Quick start guide for installing and running ibis
- [Configuration](CONFIGURATION.md) -- Full config reference with examples
- [API Reference](API-REFERENCE.md) -- REST API endpoints, query syntax, SSE streaming
- [CLI Reference](CLI-REFERENCE.md) -- CLI commands and flags
- [Table Types](TABLE-TYPES.md) -- Guide to log, unique, and aggregation tables
- [Advanced Features](ADVANCED-FEATURES.md) -- Factory contracts, discovery, view function polling
