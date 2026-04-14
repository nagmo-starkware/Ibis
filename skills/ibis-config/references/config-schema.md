# Ibis Config Schema Reference

## Complete Config Structure

```yaml
# REQUIRED: Network identifier
network: mainnet|sepolia|custom

# REQUIRED: Starknet RPC endpoint
# WSS preferred (enables starknet_subscribeEvents real-time streaming)
# HTTP falls back to polling via starknet_getEvents
rpc: wss://... | https://...

# REQUIRED: Database configuration
database:
  backend: postgres|badger|memory    # Default: memory
  postgres:                          # Required if backend=postgres
    host: localhost                  # Default: "" (required)
    port: 5432                       # Default: 5432
    user: ibis                       # Required
    password: ${IBIS_DB_PASSWORD}    # Required (env var expansion supported)
    name: ibis                       # Required
  badger:                            # For embedded KV store
    path: ./data/ibis                # Default: ./data/ibis

# OPTIONAL: REST API configuration
api:
  host: 0.0.0.0                      # Default: 0.0.0.0
  port: 8080                         # Default: 8080
  cors_origins:                      # Optional: CORS allow list
    - "http://localhost:3000"
    - "https://myapp.example.com"
  admin_key: ${IBIS_ADMIN_KEY}       # Optional: protects /v1/admin/* endpoints

# OPTIONAL: Indexer settings
indexer:
  start_block: 0                     # 0 = genesis block. Omit to start from latest. Use specific number for historical backfill
  pending_blocks: true               # Default: true (index pre-confirmed blocks)
  batch_size: 10                     # Default: 10 (blocks per backfill query)
  udc_address: "0x04a64cd09a853868621571d..."  # Default: mainnet UDC. Override for devnet/appchains
  udc_event:                         # Optional: fine-grained UDC event parsing (advanced)
    version: auto                    # auto | v0 | v1. Default: auto
    address_key: 1                   # Index in keys[] for deployed address (mutually exclusive with address_data)
    address_data: 0                  # Index in data[] for deployed address (mutually exclusive with address_key)
    class_hash_key: 2                # Index in keys[] for class hash (mutually exclusive with class_hash_data)
    class_hash_data: 0               # Index in data[] for class hash (mutually exclusive with class_hash_key)

# REQUIRED: At least one contract OR one discover entry
contracts:
  - name: MyContract                 # Required: unique identifier (used in API paths, table names)
    address: "0x049d..."             # Required: Starknet contract address (0x + 1-64 hex chars)
    abi: fetch                       # Default: "fetch". Options: fetch | ./path/to.json | ContractName
    start_block: 850000              # Optional: per-contract start block override

    # REQUIRED: at least one event
    events:
      - name: Transfer               # Event name from ABI, or "*" for all events
        table:
          type: log                  # log | unique | aggregation

      - name: BalanceUpdate
        table:
          type: unique
          unique_key: account        # Required for unique: field for last-write-wins dedup

      - name: VolumeUpdate
        table:
          type: aggregation
          aggregate:                 # Required for aggregation: at least one spec
            - column: total_volume   # Output column name
              operation: sum         # sum | count | avg
              field: amount          # Source event field

    # OPTIONAL: View function polling
    views:
      - function: total_supply       # Required: view function name from ABI
        calldata: []                 # Optional: hex felt arguments (0x-prefixed)
        interval: 30s                # Required: polling interval (Go duration, minimum 1s)
        table:                       # Required: log or unique only (no aggregation)
          type: unique
          unique_key: _view_key      # Use _view_key for single-row "latest value" mode

      - function: get_price
        calldata:
          - "0x04554482d..."         # Token pair identifier
        interval: 5s                 # Faster polling for price-like data
        table:
          type: log                  # Append-only: preserves price history
        headers:                     # Optional: custom HTTP headers for RPC call
          X-Custom: value

    # OPTIONAL: Factory configuration (for contracts that deploy child contracts)
    factory:
      event: PairCreated             # Required: factory event that signals child deployment
      child_address_field: pair      # Required: event field containing deployed child address
      child_abi: fetch               # Default: "fetch". ABI for children (cached after first)
      shared_tables: true            # Default: false. All children write to same tables
      child_name_template: "{factory}_{token0}_{token1}"  # Template for child contract names
      child_events:                  # Required: event/table config template for each child
        - name: "*"
          table:
            type: log

# OPTIONAL: Class hash discovery (watch for new contract deployments)
discover:
  - class_hash: "0x07b3e05f48f..."   # Required: class hash to watch for (0x-prefixed hex)
    group: my-tokens                 # Optional: logical group name (lowercase alphanumeric + hyphens)
    abi: MyToken                     # Required: ABI source (must be named ABI when shared_tables=true)
    shared_tables: true              # Default: false. Recommended when many instances expected
    name_template: "{group}_{address_short}"  # Template for discovered contract names
    events:                          # Required: at least one event
      - name: "*"
        table:
          type: log
    views:                           # Optional: view functions for discovered contracts
      - function: balance_of
        calldata: []
        interval: 30s
        table:
          type: unique
          unique_key: _view_key
```

## Environment Variable Expansion

Pattern: `${VAR_NAME}` anywhere in string values.
Unset variables expand to empty string.
Common pattern for secrets:

```yaml
database:
  postgres:
    host: ${IBIS_DB_HOST}
    password: ${IBIS_DB_PASSWORD}
api:
  admin_key: ${IBIS_ADMIN_KEY}
```

## ABI Resolution Priority

1. **Explicit file path**: Value contains `./`, `/`, `../`, or ends with `.json`
   - Example: `abi: "./target/dev/myproject_MyContract.contract_class.json"`
2. **Smart local discovery**: Value is not "fetch" and not a file path
   - Searches `target/dev/*_{Name}.contract_class.json` (Scarb build artifacts)
   - Example: `abi: "ERC20"` finds `target/dev/myproject_ERC20.contract_class.json`
3. **Chain fetch**: Value is "fetch" or other strategies fail
   - Calls `starknet_getClassAt` via RPC
   - Handles both Sierra (modern) and deprecated (Cairo 0) contract formats
   - Note: proxy contracts may return proxy ABI; use file path for implementation ABI

## Default RPC Endpoints

| Network  | HTTP                                               | WSS                |
|----------|----------------------------------------------------|--------------------|
| mainnet  | https://starknet-rpc.publicnode.com         | (user-provided)    |
| sepolia  | https://starknet-sepolia-rpc.publicnode.com         | (user-provided)    |

## Table Types

### Log (append-only)
Every event creates a new row. Full history preserved.
- Use for: transfers, swaps, mints, burns, deposits, withdrawals, trades, approvals
- No additional config required beyond `type: log`

### Unique (last-write-wins)
Only the latest entry per unique key is stored. Previous entries overwritten.
- Use for: leaderboards, balances, positions, status tracking, configuration state
- Required: `unique_key` field name (must exist in event fields)
- With shared tables: unique key becomes composite `(contract_address, unique_key)`

### Aggregation (auto-computed)
Automatically computes running aggregates from events.
- Use for: volume tracking, counters, statistics, averages
- Required: `aggregate` array with at least one spec
- Each spec: `column` (output name), `operation` (sum|count|avg), `field` (source event field)
- Field must be numeric (u8-u256, i8-i128)

## View Function Polling

View functions are read-only contract calls polled on a schedule. They complement events by capturing state that doesn't emit events (e.g., `total_supply`, `get_price`, `balance_of`).

### View Table Behavior
- **Unique with `_view_key`**: Stores only the latest polled value (single-row mode). Recommended for simple getters like `total_supply`, `get_price`. The `_view_key` is a synthetic column — set `unique_key: "_view_key"` to enable this mode.
- **Log**: Every poll result is appended. Preserves full polling history. Use for tracking value changes over time.
- **Aggregation**: Not supported for view tables.

### View Metadata Columns
View tables have different metadata than event tables:
- `block_number` — block at time of poll
- `timestamp` — block timestamp
- `contract_address` — contract that was called
- `_view_key` — synthetic dedup key (for unique tables)

Note: View tables do NOT have `transaction_hash`, `log_index`, `event_name`, or `status` columns.

### View Table Naming
Tables are named `{contract}_{function}` (e.g., `MyToken_total_supply`).

### Interval Recommendations
| Data Type | Interval | Rationale |
|-----------|----------|-----------|
| Price feeds, exchange rates | 5s | Fast-changing, time-sensitive |
| Token balances, supplies | 30s | Changes on every transfer |
| Governance state, config | 5m | Slow-changing parameters |
| Static metadata | 30m+ | Rarely or never changes |

## Discover Configuration (Class Hash Watching)

Discover mode watches the blockchain for new contract deployments matching a specific class hash. When a new contract with the matching class hash is deployed (detected via UDC events), ibis automatically registers and begins indexing it.

### When to Use Discover
- New contracts of a known class will be deployed in the future
- You don't know all contract addresses up front
- Examples: custom token deployments, game instances, protocol upgrades

### When NOT to Use Discover
- You know all contract addresses — use `contracts[]` instead
- You're watching a factory's children — use `factory` config instead

### Name Template Placeholders
- `{class_hash_short}` — first 8 hex chars of class hash
- `{address_short}` — first 8 hex chars of deployed address
- `{class_hash}` — full class hash
- `{address}` — full deployed address
- `{group}` — group name (if set)

Default template: `"{class_hash_short}_{address_short}"`

### Shared Tables with Discover
When `shared_tables: true`, all discovered contracts write to the same set of tables, with a `contract_name` column to disambiguate. Requires a **named ABI** (not `"fetch"` or a file path) so the ABI name can be used as the shared table prefix.

## UDC Configuration (Advanced)

The Universal Deployer Contract (UDC) is how ibis detects new contract deployments for factory and discover modes. The default UDC address handles mainnet and sepolia automatically.

Override `indexer.udc_address` only for:
- Devnet / Katana
- Appchains with custom UDC
- Custom deployer contracts

The `udc_event` field provides fine-grained control over how UDC event data is parsed:
- **v1 (modern Cairo)**: `keys[1]` = deployed address, `data[0]` = class hash
- **v0 (Cairo 0)**: `data[0]` = deployed address, `data[3]` = class hash
- **auto** (default): Tries v1 first, falls back to v0

Use `address_key`/`address_data` and `class_hash_key`/`class_hash_data` only when dealing with non-standard UDC event layouts.

## Factory Pattern Details

### When to Use Factory Config
- Contract deploys other contracts (e.g., AMM factories deploying pair contracts)
- Want to automatically discover and index child contracts
- All children share the same ABI/event structure

### Shared Tables vs Per-Contract Tables
- `shared_tables: true` (**recommended default for factories**): One table per event type. All children write to same tables with `contract_name` discriminator. Prevents table explosion.
- `shared_tables: false`: Separate tables per child. Table names: `{child_name}_{event_name}`. Only suitable for small numbers of children (<10).

### Child Name Template Placeholders
- `{factory}` — Factory contract name
- `{short_address}` — First 8 hex chars of child address
- Any field from the factory event (e.g., `{token0}`, `{token1}`, `{pair}`)

Default template: `"{factory}_{short_address}"`

### Factory Event Detection Heuristics
Events likely to be factory events:
- Name contains: "Created", "Deployed", "Registered", "Spawned", "New", "Launched"
- Has a data field of type ContractAddress (the deployed child address)
- Has additional data fields that describe the child (e.g., token addresses, pool parameters)

## Wildcard Events

```yaml
events:
  - name: "*"
    table:
      type: log
```

Indexes ALL events found in the contract ABI with the specified table type.
Specific event entries override the wildcard for that event:

```yaml
events:
  - name: "*"              # Default: all events as log
    table:
      type: log
  - name: BalanceUpdate    # Override: this specific event as unique
    table:
      type: unique
      unique_key: account
```

## Metadata Columns (auto-added to ALL event tables)

| Column             | Type   | Description                                       |
|--------------------|--------|---------------------------------------------------|
| block_number       | uint64 | Block containing the event                        |
| transaction_hash   | string | Transaction that emitted the event                |
| log_index          | uint64 | Event index within the transaction                |
| timestamp          | uint64 | Block timestamp                                   |
| contract_address   | string | Address of the emitting contract                  |
| event_name         | string | Name of the event                                 |
| status             | string | PRE_CONFIRMED, ACCEPTED_L2, or ACCEPTED_L1       |

With shared tables (factory or discover), an additional `contract_name` column identifies the source contract.

## Validation Rules

### Top-Level
- `network`: required, must be `mainnet|sepolia|custom`
- `rpc`: required, must start with `wss://|ws://|https://|http://`
- `database.backend`: must be `postgres|badger|memory`
- At least one of `contracts[]` or `discover[]` must be present

### Contract Validation
- `name`: required, non-empty, unique across all contracts
- `address`: required, `0x` + 1-64 hex characters
- `events`: at least one required
- `start_block`: optional, overrides `indexer.start_block` for this contract

### Event/Table Validation
- `table.type`: must be `log|unique|aggregation`
- Unique table: `unique_key` required, must reference a valid event field
- Aggregation table: `aggregate` array required, each with `column`, `operation` (`sum|count|avg`), `field`

### View Validation
- `function`: required
- `interval`: required, valid Go duration (e.g., `5s`, `30s`, `5m`), minimum `1s`
- `table.type`: must be `log` or `unique` only (aggregation not supported for views)
- `calldata`: if present, all entries must be valid hex felts (`0x`-prefixed)

### Factory Validation
- `event`: required
- `child_address_field`: required
- `child_events`: at least one required

### Discover Validation
- `class_hash`: required, `0x`-prefixed, 1-64 hex characters
- **No duplicate class hashes** across all discover entries
- `abi`: required
- `events`: at least one required
- `group`: if present, must be **lowercase alphanumeric + hyphens** only (no special characters)
- `shared_tables: true` requires a **named ABI** (not `"fetch"` or file path) — the ABI name is used as the shared table prefix
- Views (if present): follow same validation as contract views

### UDC Event Validation
- `version`: must be `auto|v0|v1`
- `address_key` and `address_data` are **mutually exclusive** (cannot set both)
- `class_hash_key` and `class_hash_data` are **mutually exclusive** (cannot set both)
- Fine-grained overrides (`address_key`, `address_data`, `class_hash_key`, `class_hash_data`) are rejected when `version` is explicitly `v0` or `v1`
- All index values must be non-negative

## Cairo Type to SQL Column Mapping

| Cairo Type              | SQL Column Type      | Notes                         |
|-------------------------|----------------------|-------------------------------|
| felt252                 | string               | Hex-encoded                   |
| u8, u16, u32, u64       | int64                | Fits uint64                   |
| u128, u256              | string               | Big number as string          |
| i8, i16, i32, i64       | int64                | Two's complement              |
| i128                    | string               | Big number as string          |
| bool                    | bool                 | Native boolean                |
| ContractAddress         | string               | Hex-encoded                   |
| ClassHash               | string               | Hex-encoded                   |
| ByteArray               | string               | UTF-8 text                    |
| Array\<T\>, Span\<T\>  | string               | JSON-encoded array            |
| struct                  | string               | JSON-encoded object           |
| enum                    | string               | JSON-encoded variant          |
| (T1, T2, ...)           | string               | CairoTuple — JSON-encoded array |
| ()                      | (omitted)            | CairoUnit — zero-size, not stored |

### Aggregation-Compatible Types (numeric)
Only these types support sum/avg operations:
- u8, u16, u32, u64, u128, u256
- i8, i16, i32, i64, i128
- count operation works on any field type

## Example Configurations

### Views-Only Config (polling token `total_supply`)

```yaml
network: mainnet
rpc: https://starknet-rpc.publicnode.com

database:
  backend: memory

api:
  host: 0.0.0.0
  port: 8080

contracts:
  - name: STRK
    address: "0x04718f5a0fc34cc1af16a1cdee98ffb20c31f5cd61d6ab07201858f4287c938d"
    abi: fetch
    events:
      - name: Transfer
        table:
          type: log
    views:
      - function: total_supply
        interval: 30s
        table:
          type: unique
          unique_key: _view_key      # Single-row: always shows latest value
      - function: name
        interval: 30m                # Rarely changes
        table:
          type: unique
          unique_key: _view_key
```

### Discover Config (watching a class hash with shared tables)

```yaml
network: mainnet
rpc: wss://my-rpc.example.com

database:
  backend: postgres
  postgres:
    host: ${IBIS_DB_HOST}
    port: 5432
    user: ${IBIS_DB_USER}
    password: ${IBIS_DB_PASSWORD}
    name: ibis

api:
  host: 0.0.0.0
  port: 8080

discover:
  - class_hash: "0x07b3e05f48f0a44e5a1e750e84b78a57a34b0c4b6c9d2f7a8e3f1d0c6b9a2e4"
    group: game-instances
    abi: GameContract                # Named ABI required for shared_tables
    shared_tables: true
    name_template: "{group}_{address_short}"
    events:
      - name: "*"
        table:
          type: log
      - name: ScoreUpdated
        table:
          type: unique
          unique_key: player
    views:
      - function: get_leaderboard
        interval: 30s
        table:
          type: unique
          unique_key: _view_key
```

### Full Factory + Views Combo Config

```yaml
network: mainnet
rpc: wss://my-rpc.example.com

database:
  backend: postgres
  postgres:
    host: ${IBIS_DB_HOST}
    port: 5432
    user: ${IBIS_DB_USER}
    password: ${IBIS_DB_PASSWORD}
    name: ibis

api:
  host: 0.0.0.0
  port: 8080
  cors_origins:
    - "http://localhost:3000"
    - "https://myapp.example.com"
  admin_key: ${IBIS_ADMIN_KEY}

indexer:
  start_block: 800000
  pending_blocks: true
  batch_size: 20

contracts:
  - name: JediSwapFactory
    address: "0x01aa950c..."
    abi: fetch
    start_block: 850000              # Per-contract override
    events:
      - name: PairCreated
        table:
          type: log
    views:
      - function: all_pairs_length
        interval: 5m
        table:
          type: unique
          unique_key: _view_key
    factory:
      event: PairCreated
      child_address_field: pair
      child_abi: fetch
      shared_tables: true            # Recommended for factories
      child_name_template: "{factory}_{token0}_{token1}"
      child_events:
        - name: "*"
          table:
            type: log
        - name: Swap
          table:
            type: aggregation
            aggregate:
              - column: total_volume
                operation: sum
                field: amount_in
              - column: swap_count
                operation: count
                field: amount_in
        - name: Sync
          table:
            type: unique
            unique_key: pair_address

  - name: Oracle
    address: "0x0346c57f..."
    abi: fetch
    events:
      - name: PriceUpdated
        table:
          type: log
    views:
      - function: get_price
        calldata:
          - "0x04554482d..."         # ETH/USD pair ID
        interval: 5s                 # Fast polling for prices
        table:
          type: unique
          unique_key: _view_key
      - function: get_price
        calldata:
          - "0x0535448..."           # BTC/USD pair ID
        interval: 5s
        table:
          type: log                  # Keep price history
```
