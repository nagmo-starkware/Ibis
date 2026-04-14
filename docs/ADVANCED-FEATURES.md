# Advanced Features

Ibis goes beyond simple event indexing. This guide covers the features that let you index entire ecosystems of contracts -- factory-deployed children, class-hash-discovered deployments, shared tables for scale, dynamic runtime management, and periodic view function polling.

---

## Factory Contracts

A factory contract is a contract that deploys other contracts. On Starknet, this is a common pattern for AMMs (each trading pair is a child deployed by the factory), NFT collections, and lending pools. The challenge: you can't list all the children in advance because new ones are created on-chain continuously.

Ibis solves this by watching the factory's creation event, extracting the child contract address, and automatically subscribing to the child's events -- all without restarting.

### Configuration

Add a `factory` block to any contract in your config:

```yaml
contracts:
  - name: JediSwapFactory
    address: "0x0..."
    abi: fetch
    events:
      - name: PairCreated
        table:
          type: log
    factory:
      event: PairCreated
      child_address_field: pair_address
      child_abi: fetch
      child_events:
        - name: Swap
          table:
            type: log
        - name: Mint
          table:
            type: log
        - name: Burn
          table:
            type: log
```

| Field | Description |
|-------|-------------|
| `event` | The factory event that signals a new child deployment (e.g., `PairCreated`) |
| `child_address_field` | The field in that event containing the deployed child's address |
| `child_abi` | ABI source for child contracts: `fetch` (from chain), a file path, or a contract name for smart discovery |
| `child_events` | Event/table configuration template applied to every child |

When Ibis processes a `PairCreated` event, it:

1. Extracts the child address from the `pair_address` field
2. Fetches the child's ABI (and caches it -- all children share the same class hash)
3. Creates tables for `Swap`, `Mint`, and `Burn`
4. Subscribes to the child's events in real time
5. Collects non-system fields from the factory event as metadata (e.g., `token0`, `token1`)

### Child Naming

By default, children are named `{factory}_{short_address}` (last 8 hex chars of the address). You can customize this with `child_name_template`:

```yaml
factory:
  event: PairCreated
  child_address_field: pair_address
  child_abi: fetch
  child_name_template: "jedi_{token0}_{token1}"
  child_events:
    - name: Swap
      table:
        type: log
```

Available template variables:
- `{factory}` -- the factory contract's name
- `{short_address}` -- last 8 hex characters of the child's address
- `{address}` -- full child address
- Any field from the factory event (e.g., `{token0}`, `{token1}`)

### Factory API

Ibis auto-generates API endpoints for querying factory children:

**List children:**

```bash
curl "http://localhost:8080/v1/JediSwapFactory/children"
```

```json
{
  "data": [
    {
      "name": "jedi_abc12345_def67890",
      "address": "0x0abc...",
      "deployment_block": 850000,
      "current_block": 950123,
      "status": "active",
      "events": 3,
      "token0": "0x049d3...",
      "token1": "0x0534..."
    }
  ],
  "count": 1
}
```

Factory event metadata (like `token0` and `token1`) is promoted to top-level fields and can be used for filtering:

```bash
# Find the pair for a specific token
curl "http://localhost:8080/v1/JediSwapFactory/children?token0=eq.0x049d3..."
```

**Count children:**

```bash
curl "http://localhost:8080/v1/JediSwapFactory/children/count"
```

```json
{"count": 42}
```

---

## Shared Tables

Without shared tables, a factory with 1,000 children creates 1,000 sets of tables. For a factory tracking 3 events, that's 3,000 tables. This doesn't scale -- it bloats the database, slows queries, and makes cross-child analysis difficult.

Shared tables solve this by writing all children's events to a single set of tables, distinguished by a `contract_address` column.

### Configuration

Add `shared_tables: true` to your factory config:

```yaml
contracts:
  - name: JediSwapFactory
    address: "0x0..."
    abi: fetch
    events:
      - name: PairCreated
        table:
          type: log
    factory:
      event: PairCreated
      child_address_field: pair_address
      child_abi: fetch
      shared_tables: true
      child_events:
        - name: Swap
          table:
            type: log
        - name: Burn
          table:
            type: unique
            unique_key: owner
```

With `shared_tables: true`:
- Table names use the factory name: `jediswapfactory_swap`, `jediswapfactory_burn`
- A `contract_address` column identifies which child each row belongs to
- A `contract_name` column provides the human-readable child name
- For `unique` tables, uniqueness is scoped per child: `(contract_address, unique_key)`

### Querying Shared Tables

**All children's Swap events:**

```bash
curl "http://localhost:8080/v1/JediSwapFactory/Swap"
```

**A specific child's Swap events:**

```bash
curl "http://localhost:8080/v1/JediSwapFactory/Swap?contract_address=eq.0xabc123..."
```

**Streaming events from a specific child:**

```bash
curl -N "http://localhost:8080/v1/JediSwapFactory/Swap/stream?contract_address=eq.0xabc123..."
```

Shared tables are the recommended approach for any factory that may produce more than a handful of children.

---

## Class Hash Discovery

Factory contracts produce children through a known creation event. But what about contracts deployed directly via the Universal Deployer Contract (UDC) that share a known class hash?

Example: an ERC-20 token standard. Hundreds of contracts are deployed with the same class hash, but they don't come from a single factory. Class hash discovery watches for UDC `ContractDeployed` events and automatically indexes any contract matching a target class hash.

### Configuration

Add a top-level `discover` block:

```yaml
discover:
  - class_hash: "0xabc123..."
    abi: MyToken
    shared_tables: true
    events:
      - name: Transfer
        table:
          type: log
      - name: Approval
        table:
          type: log
```

| Field | Description |
|-------|-------------|
| `class_hash` | The class hash to watch for in UDC deploy events |
| `abi` | ABI source. When `shared_tables: true`, must be a named value (not `fetch` or a file path) |
| `events` | Event/table configuration applied to every discovered contract |
| `shared_tables` | Write all discovered contracts to the same tables (recommended) |
| `group` | Optional namespace for organization |
| `name_template` | Custom naming template (default: `{class_hash_short}_{address_short}`) |
| `views` | Optional view functions to poll for discovered contracts |

When Ibis sees a UDC `ContractDeployed` event with a matching class hash, it:

1. Extracts the deployed contract's address
2. Resolves the ABI (cached after first discovery)
3. Creates tables (or reuses shared tables)
4. Subscribes to the contract's events
5. Starts view polling if configured

### Naming

Default names use the last 8 hex characters of the class hash and address:

```
abc12345_def67890
```

Customize with `name_template`:

```yaml
discover:
  - class_hash: "0xabc123..."
    abi: MyToken
    name_template: "token_{address_short}"
    events:
      - name: "*"
        table:
          type: log
```

Available variables: `{class_hash}`, `{class_hash_short}`, `{address}`, `{address_short}`, `{group}`

### UDC Configuration

Ibis watches the standard Starknet UDC by default. For devnets or appchains with custom deployers, override the UDC address and event format:

```yaml
indexer:
  udc_address: "0x041a78e..."
  udc_event:
    version: v1  # "auto" (default), "v0", or "v1"
```

The UDC event layout differs between Cairo versions:
- **v1 (modern Cairo)**: address in `keys[1]`, class hash in `data[0]`
- **v0 (Cairo 0)**: address in `data[0]`, class hash in `data[3]`
- **auto** (default): Ibis detects the layout from the event shape

For non-standard UDC events, fine-grained overrides are available:

```yaml
indexer:
  udc_event:
    # version: auto (implied)
    address_key: 1       # index in keys[] for deployed address
    class_hash_data: 0   # index in data[] for class hash
```

### Discovery API

Query all contracts discovered for a given class hash:

```bash
curl "http://localhost:8080/v1/discover/0xabc123.../contracts"
```

```json
[
  {
    "name": "abc12345_def67890",
    "address": "0x0def...",
    "events": 2,
    "current_block": 950123,
    "start_block": 840000,
    "status": "active",
    "dynamic": true
  }
]
```

---

## Dynamic Contract Management

The admin API lets you register and deregister contracts at runtime -- no config file changes, no restarts. This is useful for adding contracts discovered through external systems, user requests, or custom logic.

### Authentication

If an admin key is configured, all admin endpoints require the `X-Admin-Key` header:

```yaml
api:
  admin_key: "your_secret_key"
```

### Register a Contract

```bash
curl -X POST http://localhost:8080/v1/admin/contracts \
  -H "X-Admin-Key: your_secret_key" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "NewToken",
    "address": "0x0abc...",
    "abi": "fetch",
    "events": [
      {
        "name": "Transfer",
        "table": {"type": "log"}
      }
    ]
  }'
```

```json
{"status": "registered", "name": "NewToken", "address": "0x0abc..."}
```

Ibis immediately resolves the ABI, creates tables, and begins indexing events.

### Deregister a Contract

```bash
curl -X DELETE "http://localhost:8080/v1/admin/contracts/NewToken?drop_tables=true" \
  -H "X-Admin-Key: your_secret_key"
```

```json
{"status": "deregistered", "name": "NewToken", "drop_tables": true}
```

The `drop_tables=true` query parameter drops the contract's database tables. Omit it to keep the data but stop indexing. Shared tables are never dropped during deregistration (other contracts may still be writing to them).

### List Registered Contracts

```bash
curl http://localhost:8080/v1/admin/contracts \
  -H "X-Admin-Key: your_secret_key"
```

### Update a Contract

```bash
curl -X PUT http://localhost:8080/v1/admin/contracts/NewToken \
  -H "X-Admin-Key: your_secret_key" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "NewToken",
    "address": "0x0abc...",
    "abi": "fetch",
    "events": [
      {"name": "Transfer", "table": {"type": "log"}},
      {"name": "Approval", "table": {"type": "log"}}
    ]
  }'
```

Dynamically registered contracts are persisted to the database and automatically restored on restart.

For full endpoint documentation, see the [API Reference](API-REFERENCE.md).

---

## View Function Polling

Events capture state changes, but sometimes you need current state -- token balances, pool reserves, oracle prices. View function polling calls read-only contract functions at a configurable interval and indexes the results into queryable tables.

### Configuration

Add a `views` block to any contract:

```yaml
contracts:
  - name: LendingPool
    address: "0x0..."
    abi: fetch
    events:
      - name: Deposit
        table:
          type: log
    views:
      - function: get_reserve_data
        calldata: ["0x049d3..."]  # asset address
        interval: 30s
        table:
          type: unique
          unique_key: _view_key
      - function: get_user_account_data
        calldata: ["0x0123..."]  # user address
        interval: 1m
        table:
          type: log
```

| Field | Description |
|-------|-------------|
| `function` | The view function name to call |
| `calldata` | Static arguments as hex felts (empty array `[]` for no arguments) |
| `interval` | How often to poll (Go duration: `5s`, `1m`, `30s`) |
| `table.type` | `log` for historical tracking, `unique` for latest-value-only |
| `table.unique_key` | Required for `unique` tables. Use `_view_key` for a single-row table |

### How It Works

At each interval, Ibis:

1. Calls the view function via `starknet_call`
2. Decodes the return value using the contract's ABI
3. Adds metadata: `block_number`, `timestamp`, `contract_address`
4. Stores the result in the configured table

**Log tables** append every poll result, giving you a time series of the view function's return value. Use this for tracking how values change over time.

**Unique tables** with `unique_key: _view_key` keep a single row that's overwritten on each poll -- always reflecting the latest value. Use this for dashboards showing current state.

### Querying View Data

View function results are queryable through the same REST API as events:

```bash
# Latest reserve data
curl "http://localhost:8080/v1/LendingPool/get_reserve_data/latest"

# Historical reserve data (log table)
curl "http://localhost:8080/v1/LendingPool/get_user_account_data?order=block_number.desc&limit=100"
```

### Views with Discovery

View functions work with class hash discovery. All discovered contracts will have their views polled:

```yaml
discover:
  - class_hash: "0xdef456..."
    abi: OptionToken
    shared_tables: true
    events:
      - name: Transfer
        table:
          type: log
    views:
      - function: totalSupply
        calldata: []
        interval: 10s
        table:
          type: unique
          unique_key: _view_key
```

---

## End-to-End Example: DEX Factory

Here's a complete configuration for indexing a JediSwap-style AMM factory with shared tables, child events, and view function polling:

```yaml
network: mainnet
rpc: ${IBIS_RPC_URL}

database:
  backend: postgres
  postgres:
    host: localhost
    port: 5432
    user: ibis
    password: ${IBIS_DB_PASSWORD}
    name: ibis

api:
  host: 0.0.0.0
  port: 8080
  admin_key: ${IBIS_ADMIN_KEY}

indexer:
  start_block: 800000
  pending_blocks: true
  batch_size: 50

contracts:
  - name: JediSwapFactory
    address: "0x0..."
    abi: fetch
    events:
      - name: PairCreated
        table:
          type: log
    factory:
      event: PairCreated
      child_address_field: pair_address
      child_abi: fetch
      shared_tables: true
      child_name_template: "jedi_{short_address}"
      child_events:
        - name: Swap
          table:
            type: log
        - name: Sync
          table:
            type: unique
            unique_key: _view_key
      # Poll reserves for each pair
    views:
      - function: get_reserves
        calldata: []
        interval: 30s
        table:
          type: unique
          unique_key: _view_key
```

This configuration:

1. **Indexes the factory** -- logs every `PairCreated` event
2. **Auto-discovers pairs** -- when `PairCreated` fires, Ibis starts indexing the child
3. **Uses shared tables** -- all pairs write to `jediswapfactory_swap` and `jediswapfactory_sync`
4. **Polls reserves** -- calls `get_reserves()` every 30 seconds for each pair

**Query all swaps across all pairs:**

```bash
curl "http://localhost:8080/v1/JediSwapFactory/Swap?order=block_number.desc&limit=20"
```

**Query swaps for a specific pair:**

```bash
curl "http://localhost:8080/v1/JediSwapFactory/Swap?contract_address=eq.0xabc..."
```

**List all discovered pairs:**

```bash
curl "http://localhost:8080/v1/JediSwapFactory/children"
```

**Find a pair by token:**

```bash
curl "http://localhost:8080/v1/JediSwapFactory/children?token0=eq.0x049d3..."
```

**Stream swaps in real time:**

```bash
curl -N "http://localhost:8080/v1/JediSwapFactory/Swap/stream"
```

**Register a new contract at runtime:**

```bash
curl -X POST http://localhost:8080/v1/admin/contracts \
  -H "X-Admin-Key: ${IBIS_ADMIN_KEY}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "ManualPair",
    "address": "0x0new...",
    "abi": "fetch",
    "events": [{"name": "Swap", "table": {"type": "log"}}]
  }'
```

---

## Agent Skills

If you use [Claude Code](https://claude.com/claude-code) or another AI coding assistant, two agent skills can help with the features in this guide:

- **`ibis-config`** generates factory, discovery, and view function configs from natural language. Example: *"index all pairs from the JediSwap factory with shared tables and reserve polling"*.
- **`ibis-admin`** manages a running indexer -- registering contracts, checking status, and listing children -- without writing curl commands.

Install with:

```bash
npx skills add b-j-roberts/ibis --skill ibis-config
npx skills add b-j-roberts/ibis --skill ibis-admin
```

See the [Agent Skills Guide](AGENT-SKILLS.md) for details.
