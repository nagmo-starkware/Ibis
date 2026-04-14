# CLI Reference

Complete reference for all `ibis` CLI commands, flags, and usage patterns.

## Global Flags

These flags are available on all commands:

| Flag | Default | Description |
|------|---------|-------------|
| `--config <path>` | `./ibis.config.yaml` | Path to ibis config file |
| `-h`, `--help` | | Show help for any command |
| `-v`, `--version` | | Print version, commit, and build date |

## `ibis init`

Scaffold an `ibis.config.yaml` by inspecting contracts on-chain. Fetches the contract ABI from the RPC, lists available events, and generates a ready-to-use config file.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--contract <address>` | *(required in non-interactive)* | Contract address(es) to index. Repeatable: `--contract 0xA --contract 0xB` |
| `--name <name>` | *(auto-generated)* | Contract name(s), applied in order to `--contract` addresses. Sets the query identifier used in `ibis query` and REST API paths |
| `--output <path>` | `./ibis.config.yaml` | Output path for generated config |
| `--network <name>` | *(prompted)* | Network: `mainnet`, `sepolia`, or `custom` |
| `--rpc <url>` | *(prompted)* | RPC endpoint URL (WSS or HTTP) |
| `--database <backend>` | *(prompted)* | Database backend: `memory`, `badger`, or `postgres` |
| `--non-interactive` | `false` | Skip interactive prompts, use flag values |

### Interactive Mode (default)

When run without `--non-interactive`, ibis walks you through each configuration step:

1. **Select network** -- choose `mainnet`, `sepolia`, or `custom`
2. **RPC endpoint** -- enter URL (defaults to `https://starknet-rpc.publicnode.com` for mainnet, similar for sepolia)
3. **Database backend** -- choose `memory`, `badger`, or `postgres`
4. **Contract addresses** -- enter one or more `0x...` addresses
5. **Contract naming** -- name each contract (defaults to `Contract_` + first 6 hex chars)
6. **Event selection** -- choose wildcard `*` (all events) or pick specific ones
7. **Table type configuration** -- for each event, choose `log`, `unique`, or `aggregation` and configure fields

```bash
# Interactive: guided config generation
ibis init --contract 0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7
```

### Non-Interactive Mode

Provide all values via flags for CI/scripting use. Events default to wildcard (`*`) with `log` table type.

```bash
# Non-interactive: fully automated
ibis init \
  --contract 0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7 \
  --name STRK \
  --network mainnet \
  --rpc wss://starknet-mainnet.example.com \
  --database postgres \
  --non-interactive
```

In non-interactive mode:
- `--network` defaults to `mainnet` if omitted
- `--database` defaults to `memory` if omitted
- `--contract` is required (at least one)
- `--name` sets human-friendly names; if omitted, names are auto-generated from addresses (e.g., `Contract_04718f`)
- `--rpc` is required for `custom` network; mainnet/sepolia use public defaults
- All events are indexed as `log` tables via wildcard

### Examples

```bash
# Multiple contracts with names
ibis init --contract 0xAAA --contract 0xBBB --name TokenA --name TokenB

# Custom output path
ibis init --contract 0xAAA --output ./configs/my-indexer.yaml

# Sepolia testnet
ibis init --contract 0xAAA --network sepolia
```

### Output

Generates a YAML config file with env var placeholders for sensitive values (e.g., `${IBIS_DB_PASSWORD}`). The file includes a header comment and is ready to use with `ibis run`.

---

## `ibis run`

Start the indexer with the given config. Connects to the Starknet RPC, resolves ABIs, creates database tables, subscribes to events, and starts the REST API server.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--config <path>` | `./ibis.config.yaml` | Path to ibis config file |

### Startup Sequence

1. Load and validate `ibis.config.yaml`
2. Connect to Starknet RPC (WSS or HTTP)
3. Initialize database backend (memory, BadgerDB, or PostgreSQL)
4. Resolve contract ABIs (fetch from chain or load from local path)
5. Build table schemas from ABI event definitions
6. Create database tables (if they don't exist)
7. Start the REST API server
8. Subscribe to contract events (WSS) or begin polling (HTTP)
9. Backfill historical events from `start_block` to current block
10. Enter steady-state: process new blocks as they arrive

### Graceful Shutdown

`ibis run` handles `SIGINT` (Ctrl+C) and `SIGTERM` for graceful shutdown:

- Stops accepting new events
- Finishes processing the current block
- Closes database connections
- Shuts down the API server

```bash
# Start with default config path
ibis run

# Start with custom config
ibis run --config ./configs/production.yaml

# Run in background
ibis run &

# Graceful stop
kill -SIGTERM <pid>
# or simply Ctrl+C in the foreground terminal
```

### Startup Output

```
Loaded config from ./ibis.config.yaml
  Network:  mainnet
  RPC:      wss://starknet-mainnet.example.com
  Backend:  postgres
  API:      0.0.0.0:8080
  Contracts: 2
    - MyToken (0x049d36...): 3 events
    - MyFactory (0x0123ab...): 1 events

API server listening on 0.0.0.0:8080
Starting indexer...
```

---

## `ibis query`

Query indexed event data directly from the configured database without needing the API server running. Connects to the same database backend specified in the config file.

```
ibis query [contract] [event] [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--limit <n>` | `50` | Maximum number of results |
| `--offset <n>` | `0` | Number of results to skip |
| `--order <field.dir>` | `block_number.desc` | Ordering: `field.asc` or `field.desc` |
| `--filter <expr>` | | Field filter (repeatable). See [Filter Syntax](#filter-syntax) |
| `--unique` | `false` | Query unique table entries instead of log entries |
| `--aggregate` | `false` | Query aggregation results |
| `--latest` | `false` | Return only the most recent event |
| `--count` | `false` | Return count of matching events |
| `--children` | `false` | List factory child contracts |
| `--children-count` | `false` | Count factory child contracts |
| `--contract-address <addr>` | | Filter by contract address (for factory shared tables) |
| `--format <fmt>` | `json` | Output format: `json`, `table`, or `csv` |
| `--list` | `false` | List all available tables/events |
| `--config <path>` | `./ibis.config.yaml` | Path to ibis config file |

### Mutual Exclusivity

Some flags cannot be combined:

- `--latest` and `--count` serve different purposes and should not be used together
- `--unique` and `--aggregate` target different table types
- `--children` / `--children-count` only require a contract name (no event argument)
- `--list` ignores all other flags and arguments

### Filter Syntax

Filters use the format `--filter "field=op.value"`:

```
--filter "field=op.value"
```

Where `op` is one of:

| Operator | Meaning |
|----------|---------|
| `eq` | Equal to (default if no operator given) |
| `neq` | Not equal to |
| `gt` | Greater than |
| `gte` | Greater than or equal to |
| `lt` | Less than |
| `lte` | Less than or equal to |

If no operator prefix is given, `eq` is assumed:

```bash
# These are equivalent:
--filter "sender=0x123"
--filter "sender=eq.0x123"
```

Multiple filters are ANDed together:

```bash
ibis query MyContract Transfer \
  --filter "block_number=gte.100000" \
  --filter "sender=0x049d36..."
```

### Output Formats

#### JSON (`--format json`, default)

Outputs a JSON array of objects. Each object contains all columns for the event.

```bash
$ ibis query MyContract Transfer --limit 2
[
  {
    "block_number": 850000,
    "log_index": 3,
    "transaction_hash": "0xabc...",
    "sender": "0x123...",
    "recipient": "0x456...",
    "amount": "1000000000000000000"
  },
  {
    "block_number": 849999,
    "log_index": 1,
    "transaction_hash": "0xdef...",
    "sender": "0x789...",
    "recipient": "0x123...",
    "amount": "500000000000000000"
  }
]
```

#### Table (`--format table`)

Outputs aligned columns for terminal display. Metadata columns appear first (`block_number`, `log_index`, `transaction_hash`, etc.), followed by event-specific fields sorted alphabetically.

```bash
$ ibis query MyContract Transfer --limit 2 --format table
block_number  log_index  transaction_hash  sender    recipient  amount
------------  ---------  ----------------  ------    ---------  ------
850000        3          0xabc...          0x123...  0x456...   1000000000000000000
849999        1          0xdef...          0x789...  0x123...   500000000000000000
```

#### CSV (`--format csv`)

Outputs comma-separated values with a header row. Useful for piping to other tools or importing into spreadsheets.

```bash
$ ibis query MyContract Transfer --limit 2 --format csv
block_number,log_index,transaction_hash,sender,recipient,amount
850000,3,0xabc...,0x123...,0x456...,1000000000000000000
849999,1,0xdef...,0x789...,0x123...,500000000000000000
```

### Listing Tables

Use `--list` to see all available tables derived from the config:

```bash
$ ibis query --list
Available tables:

  MyContract (0x049d36...)
    * (all ABI events)  type=log
    LeaderboardUpdate    type=unique       table=mycontract_leaderboardupdate

  MyFactory (0x0123ab...)
    Swap                 type=log          table=myfactory_swap
```

### Basic Event Queries

```bash
# Query Transfer events (default: 50 results, newest first, JSON)
ibis query MyContract Transfer

# Limit and offset for pagination
ibis query MyContract Transfer --limit 10 --offset 20

# Order by block number ascending
ibis query MyContract Transfer --order block_number.asc

# Order by a decoded field
ibis query MyContract Transfer --order amount.desc
```

### Filtered Queries

```bash
# Events from a specific sender
ibis query MyContract Transfer --filter "sender=0x049d36..."

# Events after a specific block
ibis query MyContract Transfer --filter "block_number=gte.100000"

# Combine filters (AND logic)
ibis query MyContract Transfer \
  --filter "block_number=gte.100000" \
  --filter "block_number=lt.200000"
```

### Latest Event

```bash
# Get the single most recent Transfer event
ibis query MyContract Transfer --latest
```

### Count Events

```bash
# Count all Transfer events
ibis query MyContract Transfer --count

# Count with filters
ibis query MyContract Transfer --count --filter "block_number=gte.100000"
```

### Unique Table Queries

For events configured with `table.type: unique`:

```bash
# Get current unique entries (e.g., leaderboard)
ibis query MyContract LeaderboardUpdate --unique

# With filtering and ordering
ibis query MyContract LeaderboardUpdate --unique --order score.desc --limit 10
```

### Aggregation Queries

For events configured with `table.type: aggregation`:

```bash
# Get aggregation results (e.g., total volume, trade count)
ibis query MyContract VolumeUpdate --aggregate
```

Aggregation output shows column-value pairs:

```bash
$ ibis query MyContract VolumeUpdate --aggregate --format table
COLUMN      VALUE
------      -----
sum_volume  1234567890
count_trades  4521
```

### Factory Queries

#### Listing Child Contracts

```bash
# List all children of a factory
ibis query MyFactory --children

# Count children
ibis query MyFactory --children-count

# Filter children by metadata
ibis query MyFactory --children --filter "deployment_block=gte.100000"

# Format as table
ibis query MyFactory --children --format table
```

Child contract output includes:

| Column | Description |
|--------|-------------|
| `name` | Auto-generated child contract name |
| `address` | Child contract address |
| `deployment_block` | Block where the child was detected |
| `current_block` | Last indexed block for this child |
| `events` | Number of event types being indexed |
| *(metadata)* | Any factory metadata fields |

#### Querying Shared Tables

When factory contracts use `shared_tables: true`, all child events are in one table. Use `--contract-address` to filter to a specific child:

```bash
# All Swap events across all factory children
ibis query MyFactory Swap

# Swap events from a specific child contract
ibis query MyFactory Swap --contract-address 0x0dead...

# Count swaps for a specific child
ibis query MyFactory Swap --count --contract-address 0x0dead...
```

### View Function Queries

Contracts with view function polling produce log or unique tables. Query them the same way as event tables. To filter by the view key:

```bash
# Query view function results
ibis query MyContract BalanceOf --unique

# Filter by the view key field
ibis query MyContract BalanceOf --filter "_view_key=0x049d36..."
```

### Export to CSV

```bash
# Export all Transfer events to a file
ibis query MyContract Transfer --limit 0 --format csv > transfers.csv

# Export with filters
ibis query MyContract Transfer --format csv --filter "block_number=gte.100000" > recent_transfers.csv
```

---

## Common Usage Patterns

### Get the latest transfer

```bash
ibis query MyContract Transfer --latest
```

### Count events since a specific block

```bash
ibis query MyContract Transfer --count --filter "block_number=gte.800000"
```

### Export data to CSV for analysis

```bash
ibis query MyContract Transfer --format csv --limit 10000 > transfers.csv
```

### Query factory children

```bash
# List all children
ibis query MyFactory --children --format table

# Query a specific child's events
ibis query MyFactory Swap --contract-address 0x0dead... --limit 20
```

### Paginate through results

```bash
# Page 1
ibis query MyContract Transfer --limit 50 --offset 0
# Page 2
ibis query MyContract Transfer --limit 50 --offset 50
# Page 3
ibis query MyContract Transfer --limit 50 --offset 100
```

---

## Exit Codes

| Code | Meaning |
|------|---------|
| `0` | Success |
| `1` | Error (config not found, invalid flags, database connection failure, query error, etc.) |

Error messages are printed to stderr. For example:

```
Error: loading config: open ./ibis.config.yaml: no such file or directory
Error: unknown database backend: redis
Error: usage: ibis query <contract> <event>
  use --list to see available tables
Error: invalid filter format: "badfilter" (expected field=value or field=op.value)
Error: unknown format: xml (use json, table, or csv)
```

---

## Shell Completion

Generate shell completion scripts using the built-in `completion` command:

```bash
# Bash
ibis completion bash > /etc/bash_completion.d/ibis

# Zsh
ibis completion zsh > "${fpath[1]}/_ibis"

# Fish
ibis completion fish > ~/.config/fish/completions/ibis.fish

# PowerShell
ibis completion powershell > ibis.ps1
```

---

## Agent Skill

The `ibis-query` agent skill translates natural language questions into `ibis query` commands. Instead of constructing filters and flags manually, you can ask questions like:

- "Show me the top 10 transfers by amount"
- "How many swaps happened since block 800000?"
- "What's the latest leaderboard state?"

The skill reads your `ibis.config.yaml`, identifies the right tables and flags, and builds the corresponding CLI invocation. See the [Agent Skills Guide](https://github.com/b-j-roberts/ibis) for setup instructions.
