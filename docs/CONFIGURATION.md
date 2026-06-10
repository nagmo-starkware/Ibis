# Configuration Reference

Complete reference for `ibis.config.yaml`. This document covers every field, its type, default value, validation rules, and usage examples.

> **Tip:** The `ibis-config` agent skill can generate complete configs from natural language or a contract address — useful for bootstrapping a config that you then customize manually. See the [Agent Skills](../README.md#agent-skills) section in the README.

---

## Table of Contents

- [Top-Level Fields](#top-level-fields)
  - [network](#network)
  - [rpc](#rpc)
- [Database](#database)
  - [database.backend](#databasebackend)
  - [database.postgres](#databasepostgres)
  - [database.badger](#databasebadger)
- [API](#api)
  - [api.host](#apihost)
  - [api.port](#apiport)
  - [api.cors_origins](#apicors_origins)
  - [api.admin_key](#apiadmin_key)
- [Indexer](#indexer)
  - [indexer.start_block](#indexerstart_block)
  - [indexer.pending_blocks](#indexerpending_blocks)
  - [indexer.batch_size](#indexerbatch_size)
  - [indexer.udc_address](#indexerudc_address)
  - [indexer.udc_event](#indexerudc_event)
- [Contracts](#contracts)
  - [contracts[].name](#contractsname)
  - [contracts[].address](#contractsaddress)
  - [contracts[].abi](#contractsabi)
  - [contracts[].start_block](#contractsstart_block)
  - [contracts[].events](#contractsevents)
  - [contracts[].views](#contractsviews)
  - [contracts[].factory](#contractsfactory)
- [Events](#events)
  - [events[].name](#eventsname)
  - [events[].table.type](#eventstabletype)
  - [events[].table.unique_key](#eventstableunique_key)
  - [events[].table.aggregate](#eventstableaggregate)
- [Factory](#factory)
  - [factory.event](#factoryevent)
  - [factory.child_address_field](#factorychild_address_field)
  - [factory.child_abi](#factorychild_abi)
  - [factory.child_events](#factorychild_events)
  - [factory.child_freeze](#factorychild_freeze)
  - [factory.shared_tables](#factoryshared_tables)
  - [factory.child_name_template](#factorychild_name_template)
- [Lifecycle Freeze](#lifecycle-freeze)
  - [freeze.on](#freezeon)
  - [freeze.on_foreign](#freezeon_foreign)
- [Views](#views)
  - [views[].function](#viewsfunction)
  - [views[].calldata](#viewscalldata)
  - [views[].interval](#viewsinterval)
  - [views[].table](#viewstable)
  - [views[].headers](#viewsheaders)
- [Discover](#discover)
  - [discover[].class_hash](#discoverclass_hash)
  - [discover[].group](#discovergroup)
  - [discover[].abi](#discoverabi)
  - [discover[].events](#discoverevents)
  - [discover[].shared_tables](#discovershared_tables)
  - [discover[].views](#discoverviews)
  - [discover[].name_template](#discovername_template)
- [Environment Variable Expansion](#environment-variable-expansion)
- [Validation Rules Summary](#validation-rules-summary)
- [Examples](#examples)
  - [Full Annotated Example](#full-annotated-example)
  - [Minimal Config](#minimal-config)
  - [Memory-Only Dev Config](#memory-only-dev-config)
  - [PostgreSQL Production Config](#postgresql-production-config)

---

## Top-Level Fields

### `network`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | Yes |
| Values | `mainnet`, `sepolia`, `custom` |

The Starknet network to connect to. Determines default UDC auto-detection behavior. Use `custom` for devnets, appchains, or networks other than mainnet/Sepolia.

```yaml
network: mainnet
```

### `rpc`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | Yes |
| Scheme | `wss://`, `ws://`, `https://`, `http://` |

Starknet RPC endpoint URL. WebSocket (`wss://`) is preferred for real-time event subscriptions via `starknet_subscribeEvents`. HTTP endpoints (`https://`) fall back to polling with `starknet_getEvents`.

```yaml
rpc: wss://starknet-mainnet.example.com
```

Supports [environment variable expansion](#environment-variable-expansion):

```yaml
rpc: ${IBIS_RPC_URL}
```

---

## Database

### `database.backend`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | No |
| Default | `memory` |
| Values | `postgres`, `badger`, `memory` |

The storage backend. Choose based on your use case:

- **`postgres`** — Production deployments. Requires a running PostgreSQL instance.
- **`badger`** — Embedded key-value store. No external dependencies, good for single-machine deployments.
- **`memory`** — In-memory store. Fast, zero-dependency, but data is lost on restart. Ideal for development and testing.

```yaml
database:
  backend: postgres
```

### `database.postgres`

PostgreSQL connection settings. Required when `backend` is `postgres`.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `host` | `string` | Yes (when postgres) | — | Database host |
| `port` | `int` | No | `5432` | Database port |
| `user` | `string` | Yes (when postgres) | — | Database user |
| `password` | `string` | No | — | Database password |
| `name` | `string` | Yes (when postgres) | — | Database name |

```yaml
database:
  backend: postgres
  postgres:
    host: ${IBIS_DB_HOST}
    port: 5432
    user: ${IBIS_DB_USER}
    password: ${IBIS_DB_PASSWORD}
    name: ibis
```

### `database.badger`

BadgerDB embedded store settings. Used when `backend` is `badger`.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `path` | `string` | No | `./data/ibis` | Directory for BadgerDB data files |

```yaml
database:
  backend: badger
  badger:
    path: ./data/ibis
```

---

## API

### `api.host`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | No |
| Default | `0.0.0.0` |

Bind address for the REST API server.

### `api.port`

| Property | Value |
|----------|-------|
| Type | `int` |
| Required | No |
| Default | `8080` |

Port for the REST API server.

### `api.cors_origins`

| Property | Value |
|----------|-------|
| Type | `[]string` |
| Required | No |
| Default | — |

Allowed CORS origins. Set to `["*"]` to allow all origins (convenient for development). In production, restrict to your frontend domains.

```yaml
api:
  cors_origins:
    - "https://myapp.com"
    - "https://staging.myapp.com"
```

### `api.admin_key`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | No |
| Default | — |

Secret key that protects the admin API endpoints (`/v1/admin/*`). When set, admin requests must include the `X-Admin-Key` header with this value. When unset, admin endpoints are unprotected.

**Security:** Always set this in production deployments. Use an environment variable to avoid committing secrets:

```yaml
api:
  admin_key: ${IBIS_ADMIN_KEY}
```

---

## Indexer

### `indexer.start_block`

| Property | Value |
|----------|-------|
| Type | `uint64` (pointer) |
| Required | No |
| Default | — |

Global starting block for indexing. When set to `0`, ibis starts from block 0 (genesis). When set to a specific block number, ibis backfills from that block on startup.

> **Note:** Omitting `start_block` entirely and setting it to `0` are different behaviors. Omitting starts from the chain tip (latest block); setting to `0` starts from genesis.

Individual contracts can override this with their own `start_block`.

```yaml
indexer:
  start_block: 500000
```

### `indexer.pending_blocks`

| Property | Value |
|----------|-------|
| Type | `bool` |
| Required | No |
| Default | `false` |

Enable indexing of pending (pre-confirmed) blocks. When enabled, ibis indexes events from pending blocks and automatically reverts them if they are reorganized.

```yaml
indexer:
  pending_blocks: true
```

### `indexer.batch_size`

| Property | Value |
|----------|-------|
| Type | `int` |
| Required | No |
| Default | `10` |

Number of blocks per batch during historical backfill. Larger values reduce RPC round-trips but increase memory usage per batch.

```yaml
indexer:
  batch_size: 50
```

### `indexer.udc_address`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | No |
| Default | `0x04a64cd09a853868621d94cae9952b106f2c36a3f81260f85de6696c6b050221` |
| Format | `0x`-prefixed hex, 1-64 hex characters |

The Universal Deployer Contract (UDC) address. Used by factory and discover features to detect `ContractDeployed` events. The default is the standard mainnet/Sepolia UDC.

**When to customize:** Only if you are indexing a devnet, appchain, or custom deployer contract that uses a different UDC address.

```yaml
indexer:
  udc_address: "0x041a78e741e5af2fec34b695679bc6891742439f7afb8484ecd7766661ad02bf"
```

### `indexer.udc_event`

Advanced configuration for how ibis parses UDC `ContractDeployed` events. Most users will never need this — auto-detection handles mainnet and Sepolia correctly.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `version` | `string` | No | `auto` | UDC event layout: `auto`, `v0`, or `v1` |
| `address_key` | `int` (pointer) | No | — | Index in `keys[]` for the deployed address |
| `address_data` | `int` (pointer) | No | — | Index in `data[]` for the deployed address |
| `class_hash_key` | `int` (pointer) | No | — | Index in `keys[]` for the class hash |
| `class_hash_data` | `int` (pointer) | No | — | Index in `data[]` for the class hash |

The two known UDC event layouts:

- **v1 (modern Cairo):** `keys[1]` = deployed address, `data[0]` = class hash
- **v0 (Cairo 0):** `data[0]` = deployed address, `data[3]` = class hash

When `version` is `auto` (default), ibis auto-detects the layout based on the key count. Fine-grained overrides (`address_key`, `address_data`, `class_hash_key`, `class_hash_data`) are only valid when `version` is `auto` or omitted.

**Validation rules:**
- `address_key` and `address_data` are mutually exclusive
- `class_hash_key` and `class_hash_data` are mutually exclusive
- All index values must be non-negative
- Fine-grained overrides are not allowed when `version` is explicitly `v0` or `v1`

```yaml
indexer:
  udc_event:
    version: auto
    address_key: 1
    class_hash_data: 0
```

---

## Contracts

At least one `contracts[]` entry or one `discover[]` entry is required.

### `contracts[].name`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | Yes |

Identifier for this contract. Should be unique — used in API routes (`/v1/{name}/{event}`) and table names.

### `contracts[].address`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | Yes |
| Format | `0x`-prefixed hex, 1-64 hex characters |

The Starknet contract address to index.

### `contracts[].abi`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | No |
| Default | `fetch` |

How to resolve the contract's ABI. Three modes:

- **`fetch`** — Fetch the ABI from the deployed contract via RPC at startup. Simplest option.
- **File path** — Local path to a Sierra contract class JSON file (e.g., `./target/dev/MyContract.contract_class.json`). Detected by `./`, `/`, `../` prefixes or `.json` suffix.
- **Contract name** — A Scarb package contract name (e.g., `MyContract`). Ibis searches `target/dev/` for a matching file and runs `scarb build` if needed.

```yaml
contracts:
  - name: ETH
    address: "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7"
    abi: fetch
```

### `contracts[].start_block`

| Property | Value |
|----------|-------|
| Type | `uint64` (pointer) |
| Required | No |
| Default | Inherits from `indexer.start_block` |

Per-contract override for the starting block. Useful when contracts were deployed at different times.

```yaml
contracts:
  - name: NewContract
    address: "0x0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" # Replace with your contract address
    abi: fetch
    start_block: 800000
    events:
      - name: "*"
        table:
          type: log
```

### `contracts[].events`

| Property | Value |
|----------|-------|
| Type | `[]EventConfig` |
| Required | Yes (at least one) |

Event indexing configuration. See [Events](#events) for field details.

### `contracts[].views`

| Property | Value |
|----------|-------|
| Type | `[]ViewConfig` |
| Required | No |

View function polling configuration. See [Views](#views) for field details.

### `contracts[].freeze`

| Property | Value |
|----------|-------|
| Type | `FreezeConfig` (pointer) |
| Required | No |

Event-driven lifecycle freeze. When a configured trigger event is observed, the
contract is **frozen**: its event subscription (WSS/polling) and view polling are
torn down so it consumes no further RPC, while all previously indexed data is
retained and stays queryable. For dynamic contracts (factory children, discovered
contracts) the frozen state is persisted, so a frozen contract is not re-subscribed
after a restart. See [Lifecycle Freeze](#lifecycle-freeze) for fields and semantics.

### `contracts[].factory`

| Property | Value |
|----------|-------|
| Type | `FactoryConfig` (pointer) |
| Required | No |

Factory contract configuration for automatic child contract discovery. See [Factory](#factory) for field details.

---

## Events

### `events[].name`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | Yes |

The event name to index. Must match an event defined in the contract's ABI.

**Wildcard:** Use `"*"` to index all events from the contract as log tables. You can then override specific events by name — named entries take precedence over the wildcard.

```yaml
events:
  # Index all events as log tables
  - name: "*"
    table:
      type: log
  # Override Transfer with a unique table
  - name: Transfer
    table:
      type: unique
      unique_key: account
```

### `events[].table.type`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | Yes |
| Values | `log`, `unique`, `aggregation` |

The table type determines how events are stored:

- **`log`** — Append-only. Every event is a new row. Best for event histories.
- **`unique`** — Last-write-wins by key. Only the most recent event per `unique_key` is kept. Best for current-state views (balances, leaderboards).
- **`aggregation`** — Auto-computed aggregates (sum, count, avg). No individual events stored, only running totals.

### `events[].table.unique_key`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | Yes (when `type` is `unique`) |

The event field used as the deduplication key. Only the most recent event with each key value is retained.

```yaml
events:
  - name: BalanceUpdate
    table:
      type: unique
      unique_key: account_address
```

### `events[].table.aggregate`

| Property | Value |
|----------|-------|
| Type | `[]AggregateConfig` |
| Required | Yes (when `type` is `aggregation`, at least one) |

Defines the aggregation columns to compute. Each entry creates a column in the aggregation table.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `column` | `string` | Yes | Output column name in the aggregation table |
| `operation` | `string` | Yes | Aggregation operation: `sum`, `count`, or `avg` |
| `field` | `string` | Yes | Source event field to aggregate |

```yaml
events:
  - name: Trade
    table:
      type: aggregation
      aggregate:
        - column: total_volume
          operation: sum
          field: amount
        - column: trade_count
          operation: count
          field: amount
        - column: avg_price
          operation: avg
          field: price
```

---

## Factory

Factory configuration enables automatic child contract discovery. When the factory contract emits the configured event, ibis registers the child contract and begins indexing it.

### `factory.event`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | Yes |

The factory event name that signals a new child contract deployment (e.g., `PairCreated`).

### `factory.child_address_field`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | Yes |

The field in the factory event that contains the deployed child contract's address.

### `factory.child_abi`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | No |
| Default | `fetch` |

ABI source for child contracts. Same resolution modes as `contracts[].abi`: `fetch`, file path, or contract name.

### `factory.child_events`

| Property | Value |
|----------|-------|
| Type | `[]EventConfig` |
| Required | Yes (at least one) |

Event/table configuration template applied to each child contract. Uses the same format as `contracts[].events`.

### `factory.child_freeze`

| Property | Value |
|----------|-------|
| Type | `FreezeConfig` (pointer) |
| Required | No |

Lifecycle freeze policy applied to **each** child contract. Mirrors
`contracts[].freeze`. Typical use: freeze an option child once it emits its
terminal `Settled`/`Expired` event, so long-expired children stop consuming RPC
forever while their indexed data stays queryable. See [Lifecycle Freeze](#lifecycle-freeze).

### `factory.shared_tables`

| Property | Value |
|----------|-------|
| Type | `bool` |
| Required | No |
| Default | `false` |

When `true`, all child contracts write to the same set of shared event tables rather than each child having its own tables. Shared tables include a `contract_address` column to distinguish which child emitted each event.

### `factory.child_name_template`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | No |
| Default | `{factory}_{short_address}` |

Go template for naming child contracts. Available template variables:

| Variable | Description |
|----------|-------------|
| `{factory}` | Parent factory contract name |
| `{short_address}` | Truncated child contract address |
| Any factory event field name | Value from the factory event (e.g., `{token0}`, `{pair}`) |

```yaml
factory:
  event: PairCreated
  child_address_field: pair
  child_abi: fetch
  shared_tables: true
  child_name_template: "{factory}_{short_address}"
  child_events:
    - name: Swap
      table:
        type: log
    - name: Sync
      table:
        type: unique
        unique_key: contract_address
```

---

## Lifecycle Freeze

A **freeze** stops all ongoing RPC for a contract once it reaches a terminal
lifecycle state, while keeping its indexed data. This matters for factory
patterns that mint short-lived children (e.g. one option deployment per
rotation): without a freeze, every historical child keeps a live event
subscription and view pollers forever, so RPC cost grows without bound and is
dominated by long-dead contracts.

When a configured trigger event is observed, ibis:

1. removes the contract's event subscription (closes its WSS session / stops its
   polling loop),
2. cancels its view-poller goroutines, and
3. for dynamic contracts (factory children, discovered contracts), persists a
   `frozen` flag so the contract is **not re-subscribed** on the next restart.

The contract's tables and rows are left untouched and remain queryable. Freezing
is evaluated on both live and catch-up events, so a terminal event that fired
while the indexer was down still freezes the contract on the next start.

**Startup reconciliation.** On boot, ibis also scans already-indexed event data
and freezes any contract whose local trigger event (`on`) was recorded in a
previous run — i.e. it fired below the contract's resume cursor and would never
be replayed. This drains the existing backlog the first time the feature is
enabled (e.g. options that expired before `freeze` was configured). Only local
`on` triggers are reconciled; `on_foreign` is evaluated on live/catch-up events
only, since a foreign event isn't tied to a single instance's lifecycle.

> The trigger event should be **terminal** — no further events of interest
> should occur after it — because freezing stops fetching the contract's
> subsequent events.

### `freeze.on`

| Property | Value |
|----------|-------|
| Type | `[]string` |
| Required | No |

Event names on **this** contract that trigger the freeze (e.g. `[Settled]`). The
freeze applies to the specific instance that emitted the event — the natural
choice for per-instance lifecycles like option settlement.

### `freeze.on_foreign`

| Property | Value |
|----------|-------|
| Type | `[]ForeignTrigger` (`{contract, event}`) |
| Required | No |

Events on **other** tracked contracts that trigger the freeze. A foreign trigger
freezes every contract that declares it, so it is best for 1:1 relationships or
static targets; for per-instance freezing prefer a local `on` event the instance
emits.

```yaml
# Static contract: freeze when it emits its own terminal event.
contracts:
  - name: OptionToken
    address: "0x…"
    abi: OptionToken
    events:
      - name: "*"
        table: { type: log }
    freeze:
      on: [Settled]

# Factory children: freeze each child once it settles.
  - name: OptionFactory
    address: "0x…"
    abi: OptionFactory
    events:
      - name: "*"
        table: { type: log }
    factories:
      - event: DeploymentCreated
        child_address_field: option_token
        child_abi: OptionToken
        shared_tables: true
        child_events:
          - name: "*"
            table: { type: log }
        child_freeze:
          on: [Settled]
```

---

## Views

View functions are Starknet contract read calls (`starknet_call`) polled at a configurable interval. Results are stored in tables just like events.

### `views[].function`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | Yes |

The view function name to call on the contract (e.g., `get_reserves`, `total_supply`).

### `views[].calldata`

| Property | Value |
|----------|-------|
| Type | `[]string` |
| Required | No |
| Format | Each element: `0x`-prefixed hex, 1-64 hex characters |

Arguments to pass to the view function. Each element is a felt value.

```yaml
views:
  - function: balance_of
    calldata:
      - "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7"
    interval: 30s
    table:
      type: log
```

### `views[].interval`

| Property | Value |
|----------|-------|
| Type | `string` (Go duration) |
| Required | Yes |
| Minimum | `1s` |

How often to poll this view function. Uses Go duration syntax: `1s`, `30s`, `5m`, `1h`, etc.

### `views[].table`

| Property | Value |
|----------|-------|
| Type | `TableConfig` |
| Required | Yes |
| Allowed types | `log`, `unique` |

Table configuration for storing view results. Note: `aggregation` type is not supported for views.

When using `unique` table type, set `unique_key` to `_view_key` — a special column ibis creates automatically to identify each poll result, ensuring only the latest result is retained.

```yaml
views:
  - function: get_reserves
    interval: 10s
    table:
      type: unique
      unique_key: _view_key
```

### `views[].headers`

| Property | Value |
|----------|-------|
| Type | `map[string]string` |
| Required | No |

Custom HTTP headers to include in the RPC call for this view function. Useful for authenticated RPC endpoints.

---

## Discover

Class-hash-based contract discovery. When a `ContractDeployed` event from the UDC matches a watched class hash, ibis auto-registers the new contract for indexing.

### `discover[].class_hash`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | Yes |
| Format | `0x`-prefixed hex, 1-64 hex characters |

The class hash to watch for in UDC deploy events. Must be unique across all `discover[]` entries.

### `discover[].group`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | No |
| Format | Lowercase alphanumeric with hyphens only |

Optional logical namespace for discovered contracts.

```yaml
discover:
  - class_hash: "0x..."
    group: amm-pools
    abi: fetch
    events:
      - name: "*"
        table:
          type: log
```

### `discover[].abi`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | Yes |

ABI source for discovered contracts. Same resolution modes as `contracts[].abi`: `fetch`, file path, or contract name.

**Note:** When `shared_tables` is `true`, `abi` must be a named value (not `"fetch"` or a file path) because the ABI name is used as the shared table prefix.

### `discover[].events`

| Property | Value |
|----------|-------|
| Type | `[]EventConfig` |
| Required | Yes (at least one) |

Event/table configuration template applied to all discovered contracts of this class hash.

### `discover[].shared_tables`

| Property | Value |
|----------|-------|
| Type | `bool` |
| Required | No |
| Default | `false` |

When `true`, all discovered contracts of this class hash write to shared tables named `{abi}_{event_name}`. Requires `abi` to be a named value (not `"fetch"` or a file path).

### `discover[].views`

| Property | Value |
|----------|-------|
| Type | `[]ViewConfig` |
| Required | No |

View function polling configuration for discovered contracts.

### `discover[].name_template`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | No |
| Default | `{class_hash_short}_{address_short}` |

Template for naming discovered contracts. Available template variables:

| Variable | Description |
|----------|-------------|
| `{class_hash_short}` | Truncated class hash |
| `{address_short}` | Truncated contract address |
| `{class_hash}` | Full class hash |
| `{address}` | Full contract address |
| `{group}` | Group name (if set) |

```yaml
discover:
  - class_hash: "0xabc123..."
    group: tokens
    abi: erc20
    shared_tables: true
    name_template: "{group}_{address_short}"
    events:
      - name: Transfer
        table:
          type: log
```

---

## Environment Variable Expansion

Any string value in the config can reference environment variables using `${VAR_NAME}` syntax. Unset variables expand to an empty string.

```yaml
rpc: ${IBIS_RPC_URL}
database:
  postgres:
    host: ${IBIS_DB_HOST}
    password: ${IBIS_DB_PASSWORD}
api:
  admin_key: ${IBIS_ADMIN_KEY}
```

Environment variables are expanded before YAML parsing. The pattern `${VAR_NAME}` matches variable names consisting of letters, digits, and underscores (starting with a letter or underscore).

**Common patterns:**

| Variable | Purpose |
|----------|---------|
| `IBIS_RPC_URL` | RPC endpoint (often contains API keys) |
| `IBIS_DB_HOST` | Database host |
| `IBIS_DB_USER` | Database user |
| `IBIS_DB_PASSWORD` | Database password |
| `IBIS_DB_NAME` | Database name |
| `IBIS_ADMIN_KEY` | Admin API authentication key |

---

## Validation Rules Summary

The following constraints are enforced when the config is loaded:

| Rule | Error |
|------|-------|
| `network` is required | `network: required` |
| `network` must be `mainnet`, `sepolia`, or `custom` | `network: must be one of: mainnet, sepolia, custom` |
| `rpc` is required | `rpc: required` |
| `rpc` must use `wss://`, `ws://`, `https://`, or `http://` scheme | `rpc: must use wss://, ws://, https://, or http:// scheme` |
| `database.backend` must be `postgres`, `badger`, or `memory` | `database.backend: must be one of: postgres, badger, memory` |
| When backend is `postgres`: `host`, `user`, and `name` are required | `database.postgres.host: required when backend is postgres` |
| At least one `contracts[]` or `discover[]` entry is required | `contracts: at least one contract or discover entry is required` |
| Each contract must have `name`, `address`, and at least one `events[]` entry | `contracts[N].name: required` |
| Contract addresses must be `0x`-prefixed hex, 1-64 hex characters | `contracts[N].address: must start with 0x` |
| Event `table.type` must be `log`, `unique`, or `aggregation` | `table.type: must be one of: log, unique, aggregation` |
| `unique` tables require `unique_key` | `table.unique_key: required when table type is unique` |
| `aggregation` tables require at least one `aggregate[]` entry | `table.aggregate: required when table type is aggregation` |
| Aggregate `operation` must be `sum`, `count`, or `avg` | `operation: must be one of: sum, count, avg` |
| Factory requires `event`, `child_address_field`, and at least one `child_events[]` | `factory.event: required` |
| View `table.type` must be `log` or `unique` (not `aggregation`) | `table.type: must be one of: log, unique (aggregation not supported for views)` |
| View `interval` is required | `interval: required` |
| View `interval` minimum is `1s` | `interval: minimum interval is 1s` |
| View `calldata[]` elements must be `0x`-prefixed hex felts | `calldata[N]: must start with 0x` |
| Discover `abi` is required | `discover[N].abi: required` |
| Discover `class_hash` must be unique across all entries | `class_hash: duplicate class hash` |
| Discover `group` must be lowercase alphanumeric with hyphens only | `group: must be lowercase alphanumeric with hyphens only` |
| Discover with `shared_tables: true` requires a named ABI (not `fetch` or file path) | `abi: must be a named ABI...` |
| `udc_address` must be valid `0x`-prefixed hex | `indexer.udc_address: must start with 0x` |
| `udc_event.version` must be `auto`, `v0`, or `v1` | `udc_event.version: must be one of: auto, v0, v1` |
| `udc_event` fine-grained overrides not allowed with explicit `v0`/`v1` | `fine-grained overrides are not allowed when version is explicitly v0 or v1` |
| `udc_event.address_key` and `address_data` are mutually exclusive | `address_key and address_data are mutually exclusive` |
| `udc_event.class_hash_key` and `class_hash_data` are mutually exclusive | `class_hash_key and class_hash_data are mutually exclusive` |

---

## Examples

### Full Annotated Example

A comprehensive config demonstrating all features:

```yaml
network: mainnet
rpc: ${IBIS_RPC_URL}

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
    - "https://myapp.com"
  admin_key: ${IBIS_ADMIN_KEY}

indexer:
  start_block: 500000
  pending_blocks: true
  batch_size: 20

contracts:
  # Simple contract: index all events
  - name: ETH
    address: "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7"
    abi: fetch
    events:
      - name: "*"
        table:
          type: log

  # Mixed table types with wildcard override
  - name: Game
    address: "0x0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" # Replace with your contract address
    abi: fetch
    start_block: 600000
    events:
      - name: "*"
        table:
          type: log
      - name: LeaderboardUpdate
        table:
          type: unique
          unique_key: player_address
      - name: VolumeUpdate
        table:
          type: aggregation
          aggregate:
            - column: total_volume
              operation: sum
              field: volume
            - column: trade_count
              operation: count
              field: volume

    # Poll a view function every 30 seconds
    views:
      - function: get_total_supply
        interval: 30s
        table:
          type: unique
          unique_key: _view_key

  # Factory contract with shared tables
  - name: AMM
    address: "0x0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" # Replace with your contract address
    abi: fetch
    events:
      - name: PairCreated
        table:
          type: log
    factory:
      event: PairCreated
      child_address_field: pair
      child_abi: fetch
      shared_tables: true
      child_name_template: "{factory}_{short_address}"
      child_events:
        - name: Swap
          table:
            type: log
        - name: Sync
          table:
            type: unique
            unique_key: contract_address

# Class-hash-based discovery
discover:
  - class_hash: "0xabc123..."
    group: erc20-tokens
    abi: erc20
    shared_tables: true
    name_template: "{group}_{address_short}"
    events:
      - name: Transfer
        table:
          type: log
    views:
      - function: total_supply
        interval: 1m
        table:
          type: unique
          unique_key: _view_key
```

### Minimal Config

The simplest possible config — in-memory storage, no views, no factories:

```yaml
network: sepolia
rpc: wss://starknet-sepolia.example.com

contracts:
  - name: MyContract
    address: "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7"
    abi: fetch
    events:
      - name: "*"
        table:
          type: log
```

This uses defaults: `memory` backend, API on `0.0.0.0:8080`, batch size 10.

### Memory-Only Dev Config

Fast iteration with zero dependencies:

```yaml
network: sepolia
rpc: ${IBIS_RPC_URL}

database:
  backend: memory

api:
  host: localhost
  port: 3000
  cors_origins: ["*"]

indexer:
  pending_blocks: false
  batch_size: 50

contracts:
  - name: TestContract
    address: "0x0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" # Replace with your contract address
    abi: ./target/dev/MyContract.contract_class.json
    events:
      - name: "*"
        table:
          type: log
```

### PostgreSQL Production Config

Production-ready setup with security and persistence:

```yaml
network: mainnet
rpc: ${IBIS_RPC_URL}

database:
  backend: postgres
  postgres:
    host: ${IBIS_DB_HOST}
    port: 5432
    user: ${IBIS_DB_USER}
    password: ${IBIS_DB_PASSWORD}
    name: ibis_production

api:
  host: 0.0.0.0
  port: 8080
  cors_origins:
    - "https://myapp.com"
  admin_key: ${IBIS_ADMIN_KEY}

indexer:
  start_block: 0
  pending_blocks: true
  batch_size: 10

contracts:
  - name: MyContract
    address: "0x0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" # Replace with your contract address
    abi: fetch
    events:
      - name: "*"
        table:
          type: log
```
