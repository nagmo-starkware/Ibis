---
name: ibis-config
description: "This skill generates ibis.config.yaml files for the Ibis Starknet event indexer from contract addresses, natural language descriptions, or existing contract source code. It should be used when a user wants to create or modify an ibis indexer configuration, asks to index events from a contract, provides a Starknet contract address for indexing, or says things like generate an ibis config, set up indexing for this AMM, or add Transfer events to my config. This skill works both inside and outside of ibis project directories. Triggered by any request involving ibis configuration generation, Starknet event indexing setup, or contract ABI analysis for indexing purposes."
---

# Ibis Config Generator

Generate complete `ibis.config.yaml` files for the Ibis Starknet event indexer by analyzing contract ABIs, recommending table types and view functions, supporting factory contract patterns, and generating discover configs for class hash watching.

## Workflow

### Step 1: Gather Input

Determine the user's intent from one or more of:
- A Starknet contract address (e.g., `0x049d36...`)
- A class hash (for discover mode — e.g., `0x07b3e05...`)
- A network (mainnet or sepolia; default: mainnet)
- A natural language description (e.g., "index all swap events from this AMM")
- A request to modify an existing config
- A contract source directory with Cairo code

If only a contract address is provided, default to mainnet. Ask for the network only if ambiguous.

If the user provides a **class hash** instead of a contract address, proceed to Step 7 (Discover Config Generation) after ABI analysis.

For iterative refinement (modifying existing config), read the current `ibis.config.yaml` first.

### Step 2: Fetch and Analyze the Contract ABI

Fetch the ABI from Starknet RPC:

```bash
bash SKILL_DIR/scripts/fetch_abi.sh <contract_address> [rpc_url]
```

Default RPC endpoints:
- **Mainnet**: `https://starknet-rpc.publicnode.com`
- **Sepolia**: `https://starknet-sepolia-rpc.publicnode.com`

Save output and parse events + views:

```bash
bash SKILL_DIR/scripts/fetch_abi.sh 0x049d36... > /tmp/ibis_abi.json
python3 SKILL_DIR/scripts/parse_events.py /tmp/ibis_abi.json
```

The parse script outputs structured JSON with:
- **Events**: all events with key/data fields, Cairo types, recommended table types, unique key candidates, aggregatable fields, factory candidate events
- **Views**: all view function candidates with inputs, outputs, recommended intervals, recommended table types, and quality assessment (`is_good_candidate`)
- **Summary**: counts, factory candidates, view candidate names

If inside a Cairo project directory, check for local ABI files at `target/dev/*_ContractName.contract_class.json`. Local ABIs provide richer type information — set `abi:` to the file path or contract name for smart discovery.

### Step 3: Present Event Analysis

Display discovered events in a table before generating config:

```
Found N events in contract:

| Event           | Fields                          | Recommended Type | Reason                    |
|-----------------|---------------------------------|------------------|---------------------------|
| Transfer        | from, to, value(u256)           | log              | Transfer event history    |
| BalanceUpdated  | account, balance(u256)          | unique           | Name contains 'Updated'  |
| VolumeAccrued   | pair_id, amount(u128)           | aggregation      | Numeric accumulation      |
```

Highlight:
- Events with numeric fields as aggregation candidates (name the specific fields)
- Events with address key fields as unique table candidates (name the key field)
- Factory candidate events if detected
- CairoTuple fields — note they map to `string (JSON)` in the database

Let the user confirm or override recommendations before proceeding.

### Step 4: View Function Detection

If the parse script found view function candidates, present them to the user:

```
Found M view functions:

| Function        | Inputs         | Returns           | Interval | Table Type | Good Candidate? |
|-----------------|----------------|-------------------|----------|------------|-----------------|
| total_supply    | (none)         | u256              | 30s      | unique     | Yes             |
| get_price       | pair_id(felt)  | u256              | 5s       | log        | Yes             |
| balance_of      | account(addr)  | u256              | 30s      | log        | Yes             |
| get_config      | (none)         | Config(struct)    | 5m       | unique     | Yes             |
| transfer_from   | ...            | ...               | —        | —          | No (not a view) |
```

Explain:
- **Unique with `_view_key`**: For simple getters with no inputs (e.g., `total_supply`). Stores only the latest polled value.
- **Log**: For views with inputs or when historical polling data matters (e.g., price feeds tracked over time).
- **Interval recommendations**: 5s for price-like data, 30s for balances/supplies, 5m for slow-changing state.

Let the user confirm which views to include and adjust intervals/table types as needed. Users may choose to skip views entirely.

### Step 5: Generate the Config

Read `references/config-schema.md` for complete schema specification, defaults, and validation rules.

#### Table Type Heuristics

**Log** (default for most events):
- Transfer, Swap, Mint, Burn, Deposit, Withdraw, Approve, Claim, Trade, Order
- Any event representing a discrete action

**Unique** (last-write-wins):
- Names containing: Update, Changed, Set, Modify, State, Balance, Position, Config, Leaderboard, Score, Status
- Events with an address key field + state data fields
- Set `unique_key` to the most identifying key field (prefer address fields)

**Aggregation** (auto-computed):
- Names containing: Volume, Count, Total, Accumulated, Fee, Revenue
- Events with numeric data fields suitable for sum/avg
- Recommend: `sum` for amounts/volumes, `count` for occurrences, `avg` for rates/prices

#### Generation Rules

1. **Contract name**: Descriptive PascalCase. Derive from contract type if unknown (e.g., "StarknetETH", "JediSwapRouter")
2. **Network/RPC**: Default to mainnet HTTP. Add comment that WSS is preferred for production
3. **Database**: Default `memory` for quick testing. Comment suggesting `postgres` for production
4. **Start block**: Default 0 (latest). Use specific block for historical indexing. Support per-contract `start_block` overrides
5. **ABI**: `fetch` for on-chain. Local path or smart name if in Cairo/Scarb project
6. **Wildcard**: Use `name: "*"` when user wants all events with same table type. Override specifics as needed
7. **Env vars**: `${VAR_NAME}` for secrets (passwords, API keys, admin keys)
8. **API config**: Include `api.admin_key: ${IBIS_ADMIN_KEY}` as a commented-out option. Include `api.cors_origins` when user mentions a frontend or web app
9. **UDC**: Only include `indexer.udc_address` when user mentions devnet, appchains, or Katana. Default auto-detection handles mainnet/sepolia

#### Output Format

```yaml
# Generated by ibis-config skill
# Docs: https://github.com/...

network: mainnet
rpc: https://starknet-rpc.publicnode.com  # WSS preferred for production

database:
  backend: memory  # Use postgres for production
  # postgres:
  #   host: ${IBIS_DB_HOST}
  #   port: 5432
  #   user: ${IBIS_DB_USER}
  #   password: ${IBIS_DB_PASSWORD}
  #   name: ${IBIS_DB_NAME}

api:
  host: 0.0.0.0
  port: 8080
  # cors_origins:
  #   - "http://localhost:3000"
  # admin_key: ${IBIS_ADMIN_KEY}

indexer:
  start_block: 0
  pending_blocks: true
  batch_size: 10
  # udc_address: "0x..."  # Override for devnet/appchains

contracts:
  - name: MyContract
    address: "0x049d..."
    abi: fetch
    # start_block: 850000  # Per-contract override
    events:
      - name: "*"
        table:
          type: log
    # views:
    #   - function: total_supply
    #     interval: 30s
    #     table:
    #       type: unique
    #       unique_key: _view_key
```

Write config to `./ibis.config.yaml` unless user specifies another path.

### Step 6: Factory Contract Detection

If the parse script identifies factory candidate events, or user mentions factory/AMM/pool deployment:

1. Identify the factory event (e.g., `PairCreated`, `PoolDeployed`)
2. Identify the child address field (ContractAddress-type data field)
3. Ask if children share the same ABI (typically yes for AMM pairs)
4. **Recommend `shared_tables: true` by default** for factory contracts. Shared tables prevent table explosion and are the right choice for nearly all factories. Only suggest `shared_tables: false` if user explicitly says they'll have very few children (<10)
5. Generate `child_events` with wildcard default + specific overrides for unique/aggregation events
6. Set `child_name_template` using meaningful factory event field names:
   - Use `{factory}_{token0}_{token1}` when factory event contains token/pair identifiers
   - Use `{factory}_{short_address}` as fallback when no meaningful fields are available
   - Available placeholders: `{factory}`, `{short_address}`, and any field from the factory event

Example factory config:

```yaml
factory:
  event: PairCreated
  child_address_field: pair
  child_abi: fetch
  shared_tables: true                              # Recommended default
  child_name_template: "{factory}_{token0}_{token1}"  # Use meaningful field names
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
```

### Step 7: Discover Config Generation

When the user provides a **class hash** instead of a contract address, or mentions watching for new contract deployments:

1. **Clarify intent**: Ask whether the user knows specific deployed instances. If so, suggest using `contracts[]` instead. Discover is for cases where new contracts of a known class will be deployed in the future.

2. **Gather details**:
   - Class hash (required)
   - Group name (recommend one — lowercase alphanumeric + hyphens, e.g., `game-instances`, `my-tokens`)
   - ABI source: If `shared_tables` will be true, a **named ABI** is required (not `"fetch"` or a file path). Guide user to provide one.
   - Events to index (fetch and analyze ABI as in Step 2 if possible, or ask user)
   - View functions to poll (from Step 4 analysis)

3. **Recommend shared_tables**: Default to `true` when multiple instances are expected. Explain that shared tables prevent table explosion and use the ABI name as the table prefix.

4. **Set name_template**: Recommend a template using available placeholders:
   - `{class_hash_short}` — first 8 hex chars of class hash
   - `{address_short}` — first 8 hex chars of deployed address
   - `{class_hash}` / `{address}` — full values
   - `{group}` — group name (if set)
   - Default: `"{group}_{address_short}"` when group is set, `"{class_hash_short}_{address_short}"` otherwise

5. **Generate config**:

```yaml
discover:
  - class_hash: "0x07b3e05f..."
    group: my-tokens
    abi: MyToken                     # Named ABI (required for shared_tables)
    shared_tables: true
    name_template: "{group}_{address_short}"
    events:
      - name: "*"
        table:
          type: log
      - name: BalanceUpdated
        table:
          type: unique
          unique_key: account
    views:
      - function: total_supply
        interval: 30s
        table:
          type: unique
          unique_key: _view_key
```

### Step 8: Iterative Refinement

Support modification requests against existing configs:
- "Add Transfer events" → add new event entry
- "Make price table unique by pair_id" → change type, add unique_key
- "Add a factory for this AMM" → add factory section
- "Change database to postgres" → update database section with env var placeholders
- "Add another contract" → append to contracts array
- "Use aggregation for volume with sum on amount" → add aggregation config
- "Add view polling for total_supply" → add views section
- "Watch for new deployments of class 0x07b..." → add discover entry
- "Enable CORS for my frontend" → add cors_origins
- "Set up admin API" → add admin_key

Read existing config, apply changes, write updated file.

## Key Behaviors

- Always fetch and analyze the real ABI before generating config — never guess event structures
- Present event analysis AND view function candidates with recommendations before generating final config
- Default to simple configs (memory backend, log tables); let users escalate complexity
- For production: recommend postgres backend and WSS RPC
- Note dynamic contract registration capability (`POST /v1/admin/contracts`) but focus on config file generation
- If ABI fetch fails (proxy contract, network error), suggest using a local ABI file path
- Clean up `/tmp/ibis_abi.json` after config generation
- Quote all contract addresses and class hashes in YAML (they are strings, not hex numbers)
- Metadata columns (block_number, transaction_hash, log_index, timestamp, contract_address, event_name, status) are auto-added to all event tables — do not include them in event configs
- View metadata columns (block_number, timestamp, contract_address, _view_key) are different from event metadata — do not confuse them
- CairoTuple types `(T1, T2, ...)` map to `string (JSON)` — note this in event analysis when tuple fields are present
- UDC configuration is advanced — only surface when user mentions devnet, appchains, Katana, or custom UDC
- For discover configs: `shared_tables: true` requires a named ABI, not `"fetch"` — guide the user accordingly
- Factory `shared_tables: true` is the recommended default — suggest it proactively

## Reference

For detailed schema, validation rules, defaults, factory patterns, discover config, view functions, UDC settings, Cairo type mappings, and aggregation specs: read `references/config-schema.md`.
