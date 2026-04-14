# Table Types Guide

Ibis creates database tables from your event configurations. The **table type** determines how events are stored and queried — whether every event is kept as a historical record, only the latest value per key is retained, or running aggregates are computed automatically.

This guide explains each table type, when to use it, and how it works under the hood.

---

## Table of Contents

- [Overview](#overview)
- [Log Tables](#log-tables)
- [Unique Tables](#unique-tables)
- [Aggregation Tables](#aggregation-tables)
- [Wildcard with Overrides](#wildcard-with-overrides)
- [Choosing the Right Type](#choosing-the-right-type)
- [Database Representation](#database-representation)
- [View Function Tables](#view-function-tables)

---

## Overview

Every event configured in `ibis.config.yaml` produces a database table. The `table.type` field controls how that table behaves:

| Type | Behavior | Stores | Best For |
|------|----------|--------|----------|
| `log` | Append-only | Every event | Transaction history, audit trails, analytics |
| `unique` | Last-write-wins by key | Latest event per key | Leaderboards, balances, current state |
| `aggregation` | Auto-computed aggregates | Running totals only | Volume tracking, counts, averages |

The default type is `log` if not specified.

---

## Log Tables

Log tables are **append-only**: every event becomes a new row. Nothing is overwritten or deduplicated.

### When to Use

- Transaction history (all transfers for a token)
- Audit trails (every governance vote)
- Analytics (time-series event data)
- Any case where you need the complete event record

### Configuration

```yaml
contracts:
  - name: STRK
    address: "0x04718f5a0fc34cc1af16a1cdee98ffb20c31f5cd61d6ab07201858f4287c938d"
    abi: fetch
    events:
      - name: Transfer
        table:
          type: log
```

No additional fields are required — `type: log` is all you need.

### Querying

**REST API:**

```bash
# Get all Transfer events (paginated)
curl "http://localhost:8080/v1/STRK/Transfer?limit=10&order=block_number.desc"

# Filter by sender
curl "http://localhost:8080/v1/STRK/Transfer?from=eq.0x123..."

# Count total events
curl "http://localhost:8080/v1/STRK/Transfer/count"
```

**CLI:**

```bash
# Latest 10 transfers
ibis query STRK Transfer --limit 10 --order block_number.desc

# Filter by sender
ibis query STRK Transfer --filter "from=eq.0x123..."

# Count all transfers
ibis query STRK Transfer --count
```

### What Gets Stored

Every event row includes system columns plus decoded event fields:

| Column | Description |
|--------|-------------|
| `block_number` | Block the event was emitted in |
| `transaction_hash` | Transaction that emitted the event |
| `log_index` | Position within the block's event log |
| `timestamp` | Block timestamp |
| `contract_address` | Contract that emitted the event |
| `event_name` | Name of the event |
| `status` | `accepted` or `pending` |
| *...event fields* | Decoded fields from the ABI (e.g., `from`, `to`, `value`) |

---

## Unique Tables

Unique tables maintain **only the latest event per key**. When a new event arrives with the same key value, it overwrites the previous row. Think of it as a live snapshot of current state.

### When to Use

- Leaderboards (latest score per player)
- Current balances (latest balance per account)
- Price feeds (latest price per token pair)
- Any "current state" view where history doesn't matter

### Configuration

```yaml
contracts:
  - name: GameLeaderboard
    address: "0x..."
    abi: fetch
    events:
      - name: ScoreUpdate
        table:
          type: unique
          unique_key: player_address
```

The `unique_key` field is **required** and must match a field name from the event's ABI. Each distinct value of `player_address` gets exactly one row — the most recent event.

### Querying

Unique tables have a dedicated `/unique` endpoint that returns one row per key:

**REST API:**

```bash
# Get all current scores (one per player)
curl "http://localhost:8080/v1/GameLeaderboard/ScoreUpdate/unique"

# Filter to a specific player
curl "http://localhost:8080/v1/GameLeaderboard/ScoreUpdate/unique?player_address=eq.0x456..."
```

**CLI:**

```bash
# All current scores
ibis query GameLeaderboard ScoreUpdate --unique

# Specific player
ibis query GameLeaderboard ScoreUpdate --unique --filter "player_address=eq.0x456..."
```

> **Note:** The underlying log of all events is still stored. The `/unique` endpoint provides the deduplicated view, while the base endpoint (`/v1/GameLeaderboard/ScoreUpdate`) still returns the full event history.

### How It Works

When a `ScoreUpdate` event arrives with `player_address = 0xABC`:

1. The event is appended to the log (like a log table)
2. The unique index for key `0xABC` is updated to point to this new event
3. Querying `/latest` returns only the most recent event per `player_address`

The result: one row per player, always reflecting their latest score.

---

## Aggregation Tables

Aggregation tables **automatically compute running totals** as events arrive. Instead of storing individual events for later analysis, ibis maintains pre-computed values that update in real time.

### When to Use

- Volume tracking (total trade volume for a DEX)
- Event counting (number of transfers, swaps, or votes)
- Running averages (average trade size, average gas cost)

### Supported Operations

| Operation | Description | Example |
|-----------|-------------|---------|
| `sum` | Running total of a field's values | Total volume traded |
| `count` | Number of events processed | Total number of swaps |
| `avg` | Running average of a field's values | Average transfer amount |

### Configuration

```yaml
contracts:
  - name: MyDEX
    address: "0x..."
    abi: fetch
    events:
      - name: Swap
        table:
          type: aggregation
          aggregate:
            - column: total_volume
              operation: sum
              field: amount
            - column: swap_count
              operation: count
              field: amount
            - column: avg_swap_size
              operation: avg
              field: amount
```

Each entry in the `aggregate` array defines one computed value:

| Field | Description |
|-------|-------------|
| `column` | Name of the output column in the aggregation result |
| `operation` | One of `sum`, `count`, `avg` |
| `field` | Source event field to aggregate |

You can define multiple aggregates on the same event, even using different source fields.

### Querying

Aggregation tables have a dedicated `/agg` endpoint:

**REST API:**

```bash
curl "http://localhost:8080/v1/MyDEX/Swap/agg"
```

```json
{
  "data": {
    "total_volume": 1500000,
    "swap_count": 42,
    "avg_swap_size": 35714.29
  }
}
```

**CLI:**

```bash
ibis query MyDEX Swap --aggregate
```

### Worked Example

Suppose three `Swap` events arrive with `amount` values of `100`, `250`, and `150`:

| After Event | `total_volume` (sum) | `swap_count` (count) | `avg_swap_size` (avg) |
|-------------|---------------------|----------------------|----------------------|
| Swap: 100 | 100 | 1 | 100.00 |
| Swap: 250 | 350 | 2 | 175.00 |
| Swap: 150 | 500 | 3 | 166.67 |

Each event updates the aggregates incrementally — ibis doesn't re-scan the entire table, it applies deltas.

> **Note:** The underlying events are still stored in the log. The `/agg` endpoint returns the pre-computed aggregates, while the base endpoint still returns individual events.

---

## Wildcard with Overrides

The `"*"` wildcard lets you index **all events** from a contract's ABI with a default table type, then override specific events with different configurations.

### Basic Wildcard

Index every event as a log table:

```yaml
contracts:
  - name: MyContract
    address: "0x..."
    abi: fetch
    events:
      - name: "*"
        table:
          type: log
```

This indexes every event defined in the contract's ABI — no need to list them individually.

### Wildcard with Overrides

Set a default, then customize specific events:

```yaml
contracts:
  - name: MyDEX
    address: "0x..."
    abi: fetch
    events:
      # Default: all events are log tables
      - name: "*"
        table:
          type: log

      # Override: track latest price per pair
      - name: PriceUpdate
        table:
          type: unique
          unique_key: pair_id

      # Override: aggregate swap volume
      - name: Swap
        table:
          type: aggregation
          aggregate:
            - column: total_volume
              operation: sum
              field: amount
            - column: swap_count
              operation: count
              field: amount
```

**How it works:**

1. The `"*"` wildcard tells ibis to index all ABI events with `type: log`
2. `PriceUpdate` and `Swap` are explicitly configured, so their configs override the wildcard default
3. All other events (e.g., `Transfer`, `Approval`) use the wildcard's `type: log`

This is the most common pattern for production configs — capture everything as a log, then add specialized handling for events that need unique or aggregation behavior.

---

## Choosing the Right Type

Use this decision guide to pick the right table type for your use case:

```
Do you need the complete history of every event?
├── Yes → log
└── No
    ├── Do you need the latest value per entity (user, token, pair)?
    │   └── Yes → unique (set unique_key to the entity identifier)
    └── Do you need running totals, counts, or averages?
        └── Yes → aggregation
```

### Common Patterns

| Use Case | Type | Key/Aggregates |
|----------|------|----------------|
| Token transfer history | `log` | — |
| Current balance per account | `unique` | `unique_key: account` |
| Leaderboard (latest score per player) | `unique` | `unique_key: player_address` |
| Latest price per trading pair | `unique` | `unique_key: pair_id` |
| Total trade volume | `aggregation` | `sum` on `amount` |
| Number of swaps | `aggregation` | `count` on any field |
| Average transaction size | `aggregation` | `avg` on `amount` |
| Audit trail / compliance log | `log` | — |
| Governance vote history | `log` | — |

> **Tip:** When in doubt, start with `log`. You can always query log tables for aggregates and unique values manually — the specialized types just make those queries faster and automatic.

---

## Database Representation

Each table type maps differently to the underlying storage backend.

### PostgreSQL

**Log tables** are standard SQL tables with an insert for every event:

```sql
CREATE TABLE IF NOT EXISTS strk_transfer (
    block_number BIGINT,
    transaction_hash TEXT,
    log_index BIGINT,
    timestamp BIGINT,
    contract_address TEXT,
    event_name TEXT,
    status TEXT,
    "from" TEXT,
    "to" TEXT,
    value TEXT
);
```

**Unique tables** add a unique index on the key column. New events upsert (insert or update on conflict):

```sql
-- Table structure is the same as log
CREATE UNIQUE INDEX idx_leaderboard_scoreupdate_unique_player_address
    ON leaderboard_scoreupdate (player_address);

-- Inserts use ON CONFLICT ... DO UPDATE
INSERT INTO leaderboard_scoreupdate (...) VALUES (...)
ON CONFLICT (player_address) DO UPDATE SET ...;
```

For shared tables (factory children writing to the same table), the unique constraint is composite:

```sql
CREATE UNIQUE INDEX idx_table_unique_key
    ON table_name (contract_address, player_address);
```

**Aggregation tables** create a companion `_agg` table with a single row of running totals:

```sql
-- Event log table (same as log type)
CREATE TABLE IF NOT EXISTS mydex_swap (...);

-- Companion aggregation table
CREATE TABLE IF NOT EXISTS mydex_swap_agg (
    id SERIAL PRIMARY KEY,
    total_volume DOUBLE PRECISION DEFAULT 0,
    swap_count DOUBLE PRECISION DEFAULT 0,
    avg_swap_size DOUBLE PRECISION DEFAULT 0,
    avg_swap_size__sum DOUBLE PRECISION DEFAULT 0,   -- internal
    avg_swap_size__count DOUBLE PRECISION DEFAULT 0  -- internal
);
```

The `__sum` and `__count` columns are internal bookkeeping for computing averages — they're not exposed in query results.

### BadgerDB

BadgerDB uses key prefixes to organize data:

| Prefix | Purpose | Key Format |
|--------|---------|------------|
| `evt:` | Event log (forward) | `evt:{table}:{block}:{logIndex}` |
| `rev:` | Event log (reverse) | `rev:{table}:{invertedBlock}:{logIndex}` |
| `unq:` | Unique index | `unq:{table}:{uniqueKey}` |
| `agg:` | Aggregation values | `agg:{table}` (JSON blob) |

Dual indices (`evt:` and `rev:`) enable efficient ascending and descending queries without full table scans.

### In-Memory

The in-memory backend uses Go maps:

- **Log**: `map[table]map["{block}:{logIndex}"]event`
- **Unique**: `map[table]map["{uniqueKey}"]event`
- **Aggregation**: `map[table]map["{column}"]float64`

Data is lost on restart — use this backend for development and testing only.

---

## View Function Tables

[View function polling](ADVANCED-FEATURES.md) periodically calls read-only contract functions and indexes the results. View tables support **log** and **unique** types only — aggregation is not available for views.

### View as Log

Each poll appends a new row, building a history of the view function's return values over time:

```yaml
views:
  - function: get_total_supply
    interval: 60s
    table:
      type: log
```

### View as Unique

Each poll overwrites the previous value, keeping only the latest result:

```yaml
views:
  - function: get_balance
    calldata: ["0x123..."]
    interval: 30s
    table:
      type: unique
      unique_key: _view_key
```

When `unique_key` is set to `_view_key`, the table maintains a single row that always reflects the latest poll result. You can also use a decoded return field as the key if the view returns data for multiple entities.

> **Note:** View tables use different metadata columns than event tables — they include `_view_key` but omit `log_index`, `transaction_hash`, `event_name`, and `status`.

---

*For configuration field details, see the [Configuration Reference](CONFIGURATION.md). For querying patterns, see the [API Reference](API-REFERENCE.md).*
