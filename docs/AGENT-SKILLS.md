# Agent Skills Guide

Ibis ships three agent skills for [Claude Code](https://claude.ai/code) that let you interact with the indexer using natural language — generating configs, querying data, and managing contracts at runtime.

Each skill targets a specific workflow:

| Skill | What it does |
|-------|-------------|
| **ibis-config** | Generates `ibis.config.yaml` from contract addresses or natural language |
| **ibis-query** | Translates data questions into REST API calls or CLI commands |
| **ibis-admin** | Manages contracts on a running ibis instance via the admin API |

---

## Installation

**Prerequisites**: [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) installed and authenticated.

```bash
# Install all three ibis skills
npx skills add b-j-roberts/ibis

# Or install individually
npx skills add b-j-roberts/ibis --skill ibis-config
npx skills add b-j-roberts/ibis --skill ibis-query
npx skills add b-j-roberts/ibis --skill ibis-admin
```

After installation, the skills are available in any Claude Code session. No additional configuration required.

---

## ibis-config

**Generates complete `ibis.config.yaml` files** from a contract address, class hash, or plain English description of what you want to index.

### When to use

- Bootstrapping a new indexer config from scratch
- Adding contracts or events to an existing config
- Exploring a contract's ABI to decide which events to index
- Setting up factory contract or class hash discovery configs

### How it works

1. Fetches the contract ABI from Starknet RPC
2. Analyzes events and recommends table types (log, unique, aggregation)
3. Detects factory patterns, view functions, and discovery opportunities
4. Generates a production-ready YAML config with environment variable placeholders

### Example prompts

```
"Generate an ibis config for the STRK token at 0x04718f5a0fc34cc1af16a1cdee98ffb20c31f5cd61d6ab07201858f4287c938d"

"Index Transfer and Approval events from this ERC-20 as log tables"

"Set up a factory config for this AMM — use shared tables for all pool contracts"

"Add the Swap event to my existing config with a unique table keyed on pool_id"

"Create a config that discovers all contracts deployed with this class hash via UDC"

"Index all events from this contract using wildcard, but make Transfer a log table and BalanceUpdate a unique table"
```

### What it produces

A complete, commented `ibis.config.yaml` with:

- Network and RPC endpoint configuration (with `${ENV_VAR}` placeholders)
- Database backend setup (BadgerDB, PostgreSQL, or in-memory)
- Contract definitions with ABI resolution
- Event selections with appropriate table types
- Factory or discovery configuration when detected
- View function polling when applicable

```yaml
# Example output (abbreviated)
network: mainnet
rpc:
  url: ${STARKNET_RPC_URL}
  ws_url: ${STARKNET_WS_URL}

database:
  driver: postgres
  postgres:
    connection_string: ${DATABASE_URL}

contracts:
  - name: StrkToken
    address: "0x04718f5a0fc34cc1af16a1cdee98ffb20c31f5cd61d6ab07201858f4287c938d"
    start_block: 600000
    events:
      - name: Transfer
        table_type: log
      - name: Approval
        table_type: log
```

---

## ibis-query

**Translates natural language questions about your indexed data** into executable REST API `curl` commands or `ibis query` CLI commands.

### When to use

- Exploring indexed data without memorizing filter syntax
- Building queries for specific time ranges, addresses, or amounts
- Checking aggregation results or unique table state
- Streaming real-time events via SSE

### How it works

1. Discovers available data by calling `/v1/status` or `ibis query --list`
2. Maps your question to the right endpoint and query type
3. Constructs filters, ordering, and pagination from natural language
4. Returns an executable command you can run directly

### Example prompts

```
"Show me the 10 most recent Transfer events"

"How many swaps happened in the last 24 hours?"

"What's the total trading volume?"

"Who has the highest score on the leaderboard?"

"List all pool contracts deployed by the factory"

"Stream Transfer events in real time"

"Show me transfers where the amount is greater than 1000 from address 0xabc..."

"What's the current balance for each unique holder?"

"Get the latest price from the oracle view table"
```

### Output formats

The skill generates commands targeting your preferred output format:

| Format | Flag / Header | Use case |
|--------|--------------|----------|
| JSON | `--format json` / default | Programmatic consumption, piping to `jq` |
| Table | `--format table` | Human-readable terminal output |
| CSV | `--format csv` | Spreadsheet import, data analysis |

### Example output

For *"show me the 5 largest transfers"*, the skill produces:

```bash
# REST API (preferred)
curl -s "http://localhost:8080/v1/StrkToken/Transfer?order=amount.desc&limit=5" | jq .

# CLI equivalent
ibis query --contract StrkToken --event Transfer --order amount.desc --limit 5
```

### Natural language mappings

The skill understands common phrasing:

| You say | Maps to |
|---------|---------|
| "more than 1000" | `?amount=gt.1000` |
| "at least 500" | `?amount=gte.500` |
| "top 10" / "highest" | `?order=field.desc&limit=10` |
| "oldest" / "earliest" | `?order=block_number.asc` |
| "today" / "last hour" | `?block_number=gte.{computed}` |
| "from address 0x..." | `?from_address=eq.0x...` |

---

## ibis-admin

**Manages contracts on a running ibis instance** via the admin REST API — register new contracts, deregister old ones, check status, all without restarting the indexer.

### When to use

- Adding a new contract to a live indexer
- Removing a contract you no longer need to track
- Checking indexer health and sync progress
- Listing all currently registered contracts
- Updating event configuration for an existing contract

### Prerequisites

Ibis must be running (`ibis run`). The admin skill communicates with the indexer's HTTP API, so the server must be up and reachable.

If your config sets an `admin_key`, the skill reads it from `ibis.config.yaml` automatically.

### How it works

1. Discovers the ibis server URL and admin key from your config
2. Verifies the server is healthy (`/v1/health`)
3. Executes the requested operation via the admin API
4. Presents the response with context

### Example prompts

```
"Add the STRK token to the indexer with Transfer and Approval events as log tables"

"What's the indexer status?"

"Is ibis running and healthy?"

"Remove MyOldContract from the indexer and drop its tables"

"List all registered contracts"

"Add the Swap event to the DEX contract as an aggregation table"

"Register this factory contract with shared tables for all children"

"How far behind is the indexer?"
```

### Example output

For *"add the STRK token to the indexer"*, the skill produces:

```bash
# Health check first
curl -s http://localhost:8080/v1/health | jq .

# Register the contract
curl -s -X POST http://localhost:8080/v1/admin/contracts \
  -H "Content-Type: application/json" \
  -H "X-Admin-Key: ${IBIS_ADMIN_KEY}" \
  -d '{
    "name": "StrkToken",
    "address": "0x04718f5a0fc34cc1af16a1cdee98ffb20c31f5cd61d6ab07201858f4287c938d",
    "start_block": 600000,
    "events": [
      { "name": "Transfer", "table_type": "log" },
      { "name": "Approval", "table_type": "log" }
    ]
  }' | jq .
```

### Safety

- **Destructive operations require confirmation.** When deregistering a contract with `drop_tables: true`, the skill asks you to confirm before executing.
- **Shared tables are never dropped** during deregistration — they may be in use by other contracts.
- **Dynamically registered contracts persist** to the database and restore automatically on restart.

---

## Skill vs CLI vs API

Each interface serves a different purpose. Use the one that fits your workflow:

| | Agent Skill | CLI (`ibis`) | REST API |
|---|---|---|---|
| **Best for** | Exploration, bootstrapping, one-off tasks | Scripts, pipelines, automation | Application integration |
| **Input** | Natural language | Flags and arguments | HTTP requests |
| **Config generation** | `ibis-config` skill | `ibis init` (interactive) | — |
| **Data queries** | `ibis-query` skill | `ibis query` | `GET /v1/{contract}/{event}` |
| **Contract management** | `ibis-admin` skill | — | `POST /v1/admin/contracts` |
| **Requires ibis running** | Only `ibis-admin` | Only `ibis query` | Yes |
| **Output** | Executable commands + explanation | Direct results | JSON responses |

**Rule of thumb:**
- **Skill** when you're exploring, learning, or doing something once
- **CLI** when you're scripting or need repeatable commands
- **API** when you're building an application that consumes indexed data

---

## Workflow Examples

### Bootstrap a new indexer

Combine skills with manual steps for a complete setup:

1. **Generate config** — Use `ibis-config` to create the initial config:
   > "Generate an ibis config for the STRK token on mainnet with Transfer events"

2. **Review and tweak** — Open the generated `ibis.config.yaml`, adjust start block, database settings, or add events manually

3. **Start the indexer**:
   ```bash
   ibis run
   ```

4. **Explore the data** — Use `ibis-query` to verify events are being indexed:
   > "Show me the latest 5 Transfer events"

5. **Add another contract at runtime** — Use `ibis-admin` to register a new contract without restarting:
   > "Add the ETH token to the indexer with Transfer events"

### Monitor a DEX

1. **Set up factory indexing** — Use `ibis-config`:
   > "Set up a factory config for this AMM DEX — index Swap events with shared tables"

2. **Start indexing**:
   ```bash
   ibis run
   ```

3. **Query trading data** — Use `ibis-query`:
   > "What's the total swap volume in the last 24 hours?"
   >
   > "Show me the 10 largest swaps"
   >
   > "List all pool contracts deployed by the factory"

4. **Check sync progress** — Use `ibis-admin`:
   > "How far behind is the indexer?"

### Add a contract to a production indexer

1. **Check health** — Use `ibis-admin`:
   > "Is ibis healthy?"

2. **Register the contract** — Use `ibis-admin`:
   > "Register the rewards contract at 0x123... with Claim events as a log table, starting from block 800000"

3. **Verify indexing** — Use `ibis-query`:
   > "Show me the most recent Claim events from the rewards contract"

---

## Troubleshooting

### Skill not found

If Claude Code doesn't recognize an ibis skill:

```bash
# Verify installation
npx skills list

# Reinstall
npx skills add b-j-roberts/ibis
```

### ibis-query produces incorrect results

Refine your prompt with more context:

- **Specify the contract name**: "Show transfers *from StrkToken*" instead of just "show transfers"
- **Name the exact event**: "Query the *Swap* event" if ibis indexes multiple events
- **Be explicit about filters**: "transfers where amount is greater than 1000" is clearer than "large transfers"

If the generated command looks wrong, you can ask the skill to explain its reasoning or try a different phrasing.

### ibis-admin can't connect

The admin skill needs a running ibis instance:

1. **Check that ibis is running**: `curl http://localhost:8080/v1/health`
2. **Verify the port**: check your `ibis.config.yaml` under `api.port` (default: `8080`)
3. **Check the admin key**: if your config sets `api.admin_key`, ensure it matches

If ibis is running on a non-default host or port, tell the skill explicitly:
> "The indexer is running at http://my-server:9090 — what's the status?"

### ibis-config generates unexpected table types

The skill uses heuristics to choose table types based on event names and field patterns. If the recommendation doesn't match your use case:

- Override explicitly: "Index the *BalanceUpdate* event as a **log** table, not unique"
- Provide context: "This is a leaderboard contract — use unique tables keyed on player_id"
