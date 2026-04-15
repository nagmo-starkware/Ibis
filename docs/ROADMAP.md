# Ibis Development Roadmap

---

## Phase 3: Nice to Have

### 3.4 Contract Groups & Namespacing

**Description**: Group related contracts under a logical namespace in config. Groups are reflected in API URL prefixes, enabling cleaner organization for multi-contract deployments (e.g., grouping all DEX contracts under a `dex` namespace). This is a foundational organizational primitive that later tasks (cross-contract queries, factory APIs) build on.

**Requirements**:
- [ ] Add optional `group` field to `ContractConfig` in config struct
- [ ] Validate group names (lowercase alphanumeric + hyphens, no special characters)
- [ ] API URL prefix includes group when present: `/v1/{group}/{contract}/{event}`
- [ ] Ungrouped contracts retain current URL pattern: `/v1/{contract}/{event}`
- [ ] Status endpoint (`/v1/status`) shows contracts organized by group
- [ ] Table names optionally prefixed with group: `{group}_{contract}_{event}`
- [ ] Config validation: no duplicate contract names within the same group
- [ ] SSE streaming respects group prefix: `/v1/{group}/{contract}/{event}/stream`

**Implementation Notes**:
- Config shape: `contracts: [{ name: MyDEX, group: dex, address: "0x...", ... }]`
- The group field is optional
- API route registration: if group is set, register `GET /v1/{group}/{contract}/{event}`, otherwise `GET /v1/{contract}/{event}`
- Schema generator: when group is present, `BuildTableSchema` uses `{group}_{contract}_{event}` as table name
- Reserve the group name `_all` (used in 3.5 for cross-contract queries)

### 3.5 Cross-Contract Queries

**Description**: Enable querying events across multiple contracts in a single API request. Supports both group-wide queries (all contracts in a group) and explicit multi-contract queries, returning unified results with contract attribution.

**Requirements**:
- [ ] Group-level query endpoint: `GET /v1/{group}/_all/{event}` returns events of that type from all contracts in the group
- [ ] Multi-contract query parameter: `?contracts=ContractA,ContractB` on any event endpoint
- [ ] Results include `contract_name` and `contract_address` fields for disambiguation
- [ ] Pagination, ordering, and filtering work across the unified result set
- [ ] Count endpoint works across contracts: `GET /v1/{group}/_all/{event}/count`
- [ ] SSE streaming across contracts: `GET /v1/{group}/_all/{event}/stream`
- [ ] `ibis query` CLI supports `--contracts` flag for cross-contract queries

**Implementation Notes**:
- For Postgres: use `UNION ALL` across contract tables with an added `contract_name` column, apply `ORDER BY` and `LIMIT` on the union
- For BadgerDB: merge-sort results from multiple prefix scans using a min-heap
- For in-memory: concatenate and re-sort
- The `_all` path segment is a reserved name — validate that no contract is named `_all` in config validation
- Cross-contract queries on different table types (log vs unique) should return an error — only same-type tables can be unioned
- If shared tables (3.11) are in use, cross-contract queries become simple `WHERE` clauses instead of unions

### 3.6 Proxy Contract Support

**Description**: Detect common Starknet proxy patterns and automatically resolve implementation ABIs. When a contract is a proxy, ibis fetches the implementation's ABI instead of the proxy's minimal ABI.

**Requirements**:
- [ ] Auto-detect proxies when `abi: fetch` is used: if the fetched ABI has very few events and contains upgrade-related functions, attempt implementation resolution
- [ ] Use `starknet_getClassHashAt(address)` to get the current class hash, then `starknet_getClass(class_hash)` to fetch the full implementation ABI
- [ ] Support explicit `implementation` field in contract config for manual proxy resolution: `implementation: "0x..."` (class hash or contract address)
- [ ] Config option to disable auto-proxy-detection: `proxy: false` on a contract entry
- [ ] Log a warning when proxy detection is used, noting that the implementation can change at runtime
- [ ] If proxy resolution fails, fall back to the proxy's own ABI with a warning
- [ ] Unit tests with mock proxy ABIs (minimal proxy ABI + full implementation ABI)

**Implementation Notes**:
- `starknet_getClassHashAt(address)` returns the current class hash for any contract — this is the simplest approach, no storage slot guessing needed
- Proxy detection heuristic: if the fetched ABI has fewer than 3 event definitions and contains `upgrade` or `set_implementation` functions, it's likely a proxy
- For UUPS-style proxies (OpenZeppelin Upgradeable), the class hash at the proxy address IS the implementation's class hash
- Document that users should prefer explicit ABI paths for proxies with known implementations
- Proxy resolution runs once at startup; for runtime upgrades, see task 3.13 (Contract Upgrade Tracking)

### 3.13 Contract Upgrade Tracking

**Description**: Track `replace_class_syscall` upgrades on indexed contracts. When a contract's class hash changes, re-fetch its ABI and update event decoding schemas at the upgrade boundary. Essential for long-running indexers on upgradeable contracts using OpenZeppelin's Upgradeable component.

**Requirements**:
- [ ] Config option per contract: `track_upgrades: true` (default: false)
- [ ] When enabled, also subscribe to `Upgraded(class_hash: ClassHash)` events from the contract
- [ ] On `Upgraded` event: fetch new ABI via `starknet_getClass(new_class_hash)`, rebuild event registry and schemas
- [ ] Handle schema evolution: new events get new tables/columns, changed event fields get new columns (never drop columns), removed events stop indexing forward
- [ ] Store upgrade history in `_ibis_upgrades` table: `(contract_address, block_number, old_class_hash, new_class_hash, timestamp)`
- [ ] API endpoint: `GET /v1/{contract}/upgrades` — list upgrade history for a contract
- [ ] Graceful degradation: if new ABI can't be fetched or parsed, log error and continue with previous ABI
- [ ] Events before and after upgrade are decoded with their respective ABIs (version-aware decoding)

**Implementation Notes**:
- The `Upgraded` event selector is `sn_keccak("Upgraded")` — ibis computes this at startup and adds it to the contract's subscription filter
- If using Cairo components with `#[flat]`, the `Upgraded` event may have a nested key structure — handle both flat and nested variants
- Schema migration on upgrade: use the existing `MigrateTable` method to add new columns for changed event structures
- Version-aware decoding: store `(class_hash, from_block, to_block)` ranges per contract; when decoding historical events, use the ABI that was active at that block number
- For factory children: if one child upgrades independently, only that child's decoding changes. If the factory pushes a batch upgrade, batch the ABI update across children
- This is distinct from proxy support (3.6): proxies resolve the implementation at startup, upgrade tracking handles runtime class hash changes on any contract

### 3.14 Monitoring and Observability

**Description**: Add structured logging, metrics, and health monitoring for production deployments.

**Requirements**:
- [ ] Structured logging with `slog` (Go stdlib)
- [ ] Log levels configurable via config and CLI flag
- [ ] Prometheus metrics endpoint (`/metrics`): blocks processed, events indexed, API latency, DB query duration, connection status
- [ ] Grafana dashboard template for common metrics
- [ ] Alerting rules template (block processing stalled, connection lost, high error rate)

**Implementation Notes**:
- Use Go's `log/slog` with JSON handler for production, text handler for development
- Prometheus client: `github.com/prometheus/client_golang`
- Key metrics: `ibis_blocks_processed_total`, `ibis_events_indexed_total`, `ibis_sync_lag_blocks`, `ibis_api_requests_total`, `ibis_ws_reconnections_total`

### 3.15 Kubernetes and Helm Chart

**Description**: Production-grade Kubernetes deployment configuration with Helm charts.

**Requirements**:
- [ ] Helm chart in `deploy/ibis-chart/` with configurable values
- [ ] Deployment with resource limits, health probes, and graceful shutdown
- [ ] PostgreSQL dependency (can use external or bundled)
- [ ] ConfigMap for `ibis.config.yaml`
- [ ] Secret for database credentials and RPC URLs
- [ ] Horizontal pod autoscaling for API server (separate from indexer)
- [ ] Ingress configuration
- [ ] `make helm-install`, `make helm-upgrade`, `make helm-uninstall` targets

**Implementation Notes**:
- Indexer should run as a single replica (leader election for HA is a future concern)
- API server can scale horizontally since it's read-only
- Consider splitting indexer and API into separate deployments sharing the same database
- Reference zindex's `deploy/` and Helm patterns

### 3.16 Forward Table Type

**Description**: A new `validTableType` called `forward` that forwards decoded event data to a specified URL via HTTP/HTTPS POST. Unlike `log`, `unique`, and `aggregation`, the `forward` type does not store events locally -- ibis acts as a pure event relay, parsing Starknet events via ABI and POSTing structured JSON payloads to an external endpoint. This enables users to build custom backends (their own database, message queue, analytics pipeline, etc.) and use ibis purely as an event decoder and forwarder.

**Requirements**:
- [ ] Add `"forward"` to `validTableTypes` in `internal/config/validate.go`
- [ ] Add `TableTypeForward` variant to the `TableType` enum in `internal/types/types.go`
- [ ] Add `ForwardConfig` fields to `TableConfig`: `URL` (required, string), `Headers` (optional, `map[string]string`), `Timeout` (optional, duration, default 10s), `MaxRetries` (optional, int, default 5)
- [ ] Config validation: `forward` tables REQUIRE a `url` field; reject if missing. Validate URL scheme is `http` or `https`. Apply `expandEnvVars` to URL and header values (e.g., `${WEBHOOK_TOKEN}`)
- [ ] Create `internal/forward/forwarder.go` with a `Forwarder` struct that manages HTTP delivery: singleton `http.Client` with custom `Transport` (connection pooling, 10s timeouts), bounded delivery channel, and a worker goroutine
- [ ] JSON payload format for each forwarded event:
  ```json
  {
    "event_id": "123456:0",
    "contract": "MyContract",
    "event": "Transfer",
    "block_number": 123456,
    "block_hash": "0xabc...",
    "transaction_hash": "0xdef...",
    "log_index": 0,
    "timestamp": 1710072000,
    "status": "ACCEPTED_L2",
    "data": { "from": "0x123...", "to": "0x456...", "amount": "1000" }
  }
  ```
- [ ] Include configurable HTTP headers on every request (supports `Authorization`, API keys, etc.). Always set `Content-Type: application/json`, `User-Agent: ibis-indexer/1.0`, and `X-Ibis-Event-Id: {event_id}` for idempotency
- [ ] Bounded retry with exponential backoff on failure: 5 attempts with delays of 1s, 2s, 4s, 8s, 16s (with jitter). Retry on 429, 5xx, connection errors. Do NOT retry on 4xx (except 429). Respect `Retry-After` header when present
- [ ] Non-blocking delivery: engine sends events to the forwarder via a buffered channel (capacity 10,000). If the channel is full, log a warning and drop the event. Never block the indexing pipeline
- [ ] Engine processor (`internal/engine/processor.go`): when a table's type is `forward`, skip store operations and instead route the decoded event to the `Forwarder`
- [ ] Graceful shutdown: drain the delivery channel and wait for in-flight requests (with a 30s deadline) before exiting
- [ ] Unit tests for the forwarder (delivery, retry, backoff, channel overflow) using `httptest.Server`

**Implementation Notes**:
- The forwarder hooks into the same event processing pipeline as the store -- in `processor.go`, check `schema.TableType == TableTypeForward` before calling `store.ApplyOperations`. Forward events bypass the store entirely.
- Use a single `http.Client` per `Forwarder` instance with `MaxIdleConnsPerHost: 10` and `IdleConnTimeout: 90s`. For forward tables pointing to different URLs, each gets its own `Forwarder` with its own client.
- The config shape nests under the existing `table` block:
  ```yaml
  events:
    - name: Transfer
      table:
        type: forward
        url: "https://my-api.com/events"
        timeout: 10s
        max_retries: 5
        headers:
          Authorization: "Bearer ${WEBHOOK_TOKEN}"
  ```
- Wildcard `"*"` with `type: forward` should work: all events forwarded to the same URL. Specific event entries can override with a different URL or table type.
- The existing `EventBus` (used for SSE) is a separate concern -- forward tables use their own delivery path. An event can be both forwarded (via a `forward` table entry) and stored (via a separate `log`/`unique`/`aggregation` entry for the same event) if the user configures both.
- Jitter implementation: `delay = baseDelay * 2^attempt * (0.5 + rand.Float64()*0.5)` (equal jitter).
- Consider adding a `/v1/status` field showing forward table health (events forwarded, failed, queued) for observability.

### 3.17 Ibis Query CLI --watch Mode (SSE Streaming)

**Description**: Add a `--watch` flag to `ibis query` that connects to the running ibis API server's SSE endpoint and streams new events to the terminal in real-time. This turns `ibis query` into a live tail for indexed events -- like `tail -f` for on-chain data. Instead of querying the database directly, `--watch` acts as an SSE client connecting to the `/v1/{contract}/{event}/stream` endpoint served by `ibis run`.

**Requirements**:
- [ ] Add `--watch` / `-w` boolean flag to the `queryCmd` in `internal/cli/query.go`
- [ ] Add `--api-url` string flag (default: derived from config's `api.host` and `api.port`, e.g., `http://localhost:8080`) to specify the ibis API server URL
- [ ] When `--watch` is set, construct the SSE URL: `{api-url}/v1/{contract}/{event}/stream` with query params from `--filter` and `--contract-address` flags translated to Supabase-style query params (e.g., `?block_number=gte.100&contract_address=eq.0x123`)
- [ ] Implement SSE client using Go stdlib (`net/http` + `bufio.Scanner`): parse `id:` and `data:` lines from the `text/event-stream` response, unmarshal JSON data into event objects
- [ ] Output each received event using the existing `--format` flag: `json` (one JSON object per line, newline-delimited), `table` (print header once, then one row per event), `csv` (print header once, then one row per event)
- [ ] Auto-reconnect on connection loss with exponential backoff (1s, 2s, 4s, 8s, max 30s). Send `Last-Event-ID` header on reconnect to resume from the last received event. Print a warning line (to stderr) on disconnect and reconnect
- [ ] Clean shutdown on SIGINT/SIGTERM: close the HTTP response body gracefully, print a summary line (events received count) to stderr, and exit 0
- [ ] Mutual exclusivity: `--watch` is incompatible with `--latest`, `--count`, `--aggregate`, `--unique`, `--children`, `--children-count`, and `--list`. Return a clear error if combined
- [ ] Unit tests: SSE line parser, URL construction from flags, mutual exclusivity validation. Integration test using `httptest.Server` that serves SSE events and verifies the CLI receives and formats them correctly

**Implementation Notes**:
- The SSE client is intentionally stdlib-only (no `r3labs/sse` or similar) -- the protocol is simple enough (`id: ...\ndata: ...\n\n`) and this avoids adding a dependency. Use `bufio.NewScanner` on the response body with a custom split function or line-by-line reading.
- API URL derivation: read `cfg.API.Host` and `cfg.API.Port` from the loaded config, construct `http://{host}:{port}`. The `--api-url` flag overrides this entirely. If host is `0.0.0.0`, default to `localhost` for the client URL.
- For `--format table` and `--format csv` in watch mode, print the header row on first event, then append data rows as events arrive. This differs from one-shot mode where all events are collected before rendering.
- The reconnect loop should track the last received SSE event ID and send it as `Last-Event-ID` on reconnect. The server's `replayEvents` in `sse.go` already handles this for gap-free delivery.
- Filter flags map to query params: `--filter "block_number=gte.100"` becomes `?block_number=gte.100`, and `--contract-address 0x123` becomes `?contract_address=eq.0x123`. This matches the Supabase-style filtering already used by the REST endpoints (see `parseFiltersFromURL` in `api/query.go`).
- Signal handling: use `signal.NotifyContext` with `os.Interrupt` and `syscall.SIGTERM` to get a cancellable context, then pass it to the HTTP request.

### 3.18 Contract Preset System

**Description**: Provide built-in preset configurations for common Starknet contract standards (ERC20, ERC721, ERC1155). A single `preset: erc20` field in a contract config auto-populates events, table types, aggregation tables, and ships an embedded ABI — eliminating boilerplate for the most common indexing use cases. Presets are composable: users can override any default with explicit event configs.

**Requirements**:
- [ ] Add optional `Preset` field to `ContractConfig` in `internal/config/config.go`
- [ ] Create `internal/config/presets/` package with a `PresetDefinition` struct containing: name, description, embedded ABI JSON (`[]byte`), default `[]EventConfig`, and optional `*FactoryConfig`
- [ ] Implement `erc20` preset with embedded OZ Cairo ERC20 ABI, events: `Transfer` (log) + `Approval` (log) + a `Transfer` aggregation table tracking `total_volume` (sum of `value`) and `transfer_count` (count)
- [ ] Implement `erc721` preset with embedded OZ Cairo ERC721 ABI, events: `Transfer` (log) + `Approval` (log) + `ApprovalForAll` (log) + aggregation tracking `transfer_count` (count)
- [ ] Implement `erc1155` preset with embedded OZ Cairo ERC1155 ABI, events: `TransferSingle` (log) + `TransferBatch` (log) + `ApprovalForAll` (log) + aggregation tracking `transfer_count` (count)
- [ ] Preset application in `Config.Load()`: after YAML parsing and `applyDefaults()`, before `Validate()` — merge preset defaults into `ContractConfig`, with explicit user config always taking precedence
- [ ] When preset provides an embedded ABI and user has not set `abi:`, use the embedded ABI directly (skip chain fetch). If user explicitly sets `abi: fetch` or a file path, that overrides the preset ABI
- [ ] Config validation: reject unknown preset names with a clear error listing available presets
- [ ] Add `ibis presets` (or `ibis init --preset`) CLI subcommand to list available presets with descriptions and their default event configurations
- [ ] Unit tests: preset application, override merging (explicit events override preset defaults), unknown preset rejection, embedded ABI loading
- [ ] Add example configs in `configs/` demonstrating preset usage for each standard

**Implementation Notes**:
- Embed ABI JSON files using Go's `embed` package (`//go:embed` directives) in `internal/config/presets/`. Source canonical ABIs from OpenZeppelin Cairo contracts (latest stable release). Store as `abi_erc20.json`, `abi_erc721.json`, `abi_erc1155.json`.
- Preset merge logic: if `cc.Events` is empty, use preset events entirely. If `cc.Events` is non-empty, the user's events fully replace the preset events (not merged per-event). This keeps the override model simple and predictable.
- For aggregation tables in presets, use a naming convention like `{contract_name}_transfer_stats` to avoid colliding with the log table `{contract_name}_transfer`. This may require supporting multiple table configs per event name, or using a separate event entry with a `table_name` override.
- The embedded ABI approach means presets work offline and without an RPC endpoint — useful for `ibis init` and config validation. The ABI is the canonical OZ Cairo ABI; if a contract has a custom ABI (e.g., additional events beyond the standard), users should use `abi: fetch` to get the full ABI.
- Be aware of the dual ERC20 event layout on Starknet: legacy contracts (early StarkGate ETH/STRK) put `from`/`to` in the data array, while modern OZ Cairo contracts put them in keys. The embedded ABI should match the modern layout; for legacy contracts, users override with `abi: fetch`.
- Preset registry is a simple `map[string]PresetDefinition` in Go code — no external files or plugin system needed. Future presets (AMM, governance) can be added by extending this map.
- Config YAML shape: `contracts: [{ name: STRK, address: "0x...", preset: erc20 }]` — that's all a user needs for a fully functional ERC20 indexer with transfer analytics.

### 3.20 Ibis-Config Skill Revamp/Refactor

**Description**: The ibis-config Claude skill (`~/.claude/skills/ibis-config/`) generates `ibis.config.yaml` files from contract addresses and ABIs. Since it was originally written, the indexer has gained views, discover (class hash watching), UDC configuration, admin API settings, CORS, factory shared tables, per-contract start_block, and CairoTuple support. The skill's SKILL.md workflow, config-schema.md reference, and parse_events.py script are all missing ~50% of the current config surface area. This task is a ground-up revamp to bring the skill in sync with the actual codebase.

**Requirements**:
- [ ] Rewrite `references/config-schema.md` to cover the full current config surface: `discover[]` with `class_hash`, `group`, `shared_tables`, `name_template`; `views[]` with `function`, `calldata`, `interval`, `table` (log/unique only); `api.cors_origins` and `api.admin_key`; `indexer.udc_address` and `indexer.udc_event` (version, address_key/data, class_hash_key/data); per-contract `start_block`; factory `shared_tables` and `child_name_template`; CairoTuple type mapping
- [ ] Update `SKILL.md` workflow to add a new step for **view function detection**: after event analysis, present discovered view functions and recommend polling intervals, let user confirm which views to include
- [ ] Update `SKILL.md` workflow to add a new step for **discover config generation**: guided flow for class hash watching — input class hash, choose group name, ABI source, shared_tables recommendation, name_template, and event/view templates
- [ ] Update `parse_events.py` to extract view function candidates from the ABI: look for `"type": "function"` entries with `"state_mutability": "view"`, identify return types, recommend interval (30s default, 5s for price-like functions, 5m for slow-changing state), output in a `"views"` section alongside `"events"`
- [ ] Update `parse_events.py` to recognize CairoTuple types (`(T1, T2, ...)`) and map them to `"string (JSON)"` in the type mapping
- [ ] Update `SKILL.md` Step 4 (Generate Config) to include `api.admin_key`, `api.cors_origins`, `indexer.udc_address` (with comment for devnet/appchain override), and per-contract `start_block` in the generated output template
- [ ] Update `SKILL.md` Step 5 (Factory Detection) to recommend `shared_tables: true` by default for factories (was already conditional on 10+ children — make it the default recommendation) and include `child_name_template` guidance using factory event field names
- [ ] Add new `SKILL.md` Step 6 for **discover config generation**: detect when user provides a class hash instead of a contract address, guide through group naming, ABI source, shared_tables (recommend true when multiple instances expected), and name_template with available placeholders (`{class_hash_short}`, `{address_short}`, `{group}`)
- [ ] Update the config-schema.md validation rules section with new rules: discover class_hash uniqueness, discover shared_tables requires named ABI, view interval minimum 1s, view table type restricted to log/unique, UDC event mutual exclusivity rules, group name format (lowercase alphanumeric + hyphens)
- [ ] Verify `fetch_abi.sh` still works correctly against current Nethermind free RPC endpoints for both mainnet and sepolia, fix if needed
- [ ] Add example YAML snippets to `config-schema.md` for: a views-only config (polling a token's `total_supply`), a discover config (watching a class hash with shared tables), and a full factory + views combo config

**Implementation Notes**:
- View functions in Cairo ABIs have `"type": "function"` and `"state_mutability": "view"`. The parse_events.py script should filter for these and extract input parameters (for calldata) and output types (for table schema). Functions with no inputs or simple felt inputs are the best candidates.
- For discover config generation, the skill should ask whether the user knows specific deployed instances — if so, suggest using `contracts[]` instead. Discover is for cases where new contracts of a known class will be deployed in the future (e.g., all instances of a custom token).
- The `_view_key` synthetic column is used for unique view tables — when `unique_key: "_view_key"`, the table stores only the latest polled value (single-row mode). This is the recommended default for simple getter views like `total_supply` or `get_price`.
- UDC event format is an advanced config that most users won't need — the skill should only surface it when the user mentions devnet, appchains, Katana, or custom UDC. Default auto-detection handles mainnet/sepolia correctly.
- The existing bash+python script approach is retained. `fetch_abi.sh` handles ABI fetching, `parse_events.py` handles parsing. The Python script gains a new `"views"` output section alongside the existing `"events"` section.
- Config-schema.md should be the single source of truth for all YAML fields, types, defaults, and validation rules — treat it as the authoritative reference that SKILL.md points to.
- Skip the `forward` table type (roadmap 3.16) — it's not implemented yet. Add it in a future skill update when the feature ships.

### 3.21 Ibis-Query Skill Revamp/Refactor

**Description**: The ibis-query Claude skill (`~/.claude/skills/ibis-query/`) translates natural language questions about indexed Starknet data into queries. Since it was written, the indexer has gained view functions, discovery mode, admin API, shared tables beyond factories, SSE streaming, CairoTuple support, and the status endpoint now returns richer data (factory summaries, view statuses, per-contract cursors). The skill's SKILL.md workflow is CLI-first but should be REST API-first. The reference doc has incorrect response formats (aggregate says `{"values": ...}` but actual is `{"data": ...}`), is missing ~40% of current endpoints, and has no coverage of view function queries or discovery queries. This task is a ground-up revamp to bring the skill in sync with the current codebase and shift to a REST API-first approach.

**Requirements**:
- [ ] Rewrite `SKILL.md` to use a REST API-first approach: prefer `curl` commands against the running ibis API server. Fall back to `ibis query` CLI only when explicitly requested or when the API server is not running. Include guidance for constructing base URL from config (`http://{host}:{port}/v1`)
- [ ] Rewrite `SKILL.md` Step 1 (Discover Available Data) to use `GET /v1/status` as the primary discovery method — it returns all contracts, cursors, factory summaries, and view statuses. Keep `ibis query --list` and config-file reading as fallbacks
- [ ] Add view function query coverage to `SKILL.md`: new mapping rules for questions like "what's the current price?", "total supply?", "contract state?" → view tables. Document that view tables have different metadata columns (`block_number`, `timestamp`, `contract_address`, `_view_key`) and lack `transaction_hash`, `log_index`, `event_name`, `status`
- [ ] Add discovery query coverage to `SKILL.md`: map questions like "what contracts were discovered?", "how many instances of class X?" → `GET /v1/discover/{classHash}/contracts` endpoint
- [ ] Update the NL-to-query mapping table in `SKILL.md` to include view-related intents ("current value of...", "latest state of..."), discovery intents, and SSE streaming intents ("stream events", "watch for new...", "live tail")
- [ ] Update filter/ordering mapping to cover view table fields — view tables support `block_number`, `timestamp`, `contract_address`, and `_view_key` as filterable/orderable columns, plus decoded view output fields
- [ ] Rewrite `references/query_reference.md` as a single comprehensive reference covering all current REST API endpoints: event CRUD (`/v1/{contract}/{event}`, `/latest`, `/count`, `/unique`, `/aggregate`, `/stream`), factory (`/v1/{factory}/children`, `/children/count`), discovery (`/v1/discover/{classHash}/contracts`), system (`/v1/health`, `/v1/status`), with correct response formats for each
- [ ] Fix the aggregate response format in the reference doc: actual format is `{"data": {"column": value, ...}}`, not `{"values": {...}}`
- [ ] Add view function table documentation to the reference: how view tables are named (`{contract}_{function}`), their metadata columns, how `_view_key` works for unique vs log view tables, and example queries
- [ ] Update the Cairo type-to-column mapping table to include `CairoTuple → string (JSON)` alongside the existing types
- [ ] Update the config structure reference section to reflect the current `Config` struct: add `discover[]`, `views[]` on contracts, `api.cors_origins`, `api.admin_key`, `indexer.udc_address`, `indexer.udc_event`, per-contract `start_block`, factory `shared_tables`, `child_name_template`, and `contract.dynamic`/`contract.shared_tables`/`contract.discover_class_hash` fields
- [ ] Update `scripts/inspect_config.sh` to extract view functions and discovery configs from `ibis.config.yaml` in addition to contracts and events. Output should clearly separate: contracts (with events), view functions (with intervals), discovery configs (with class hashes), and factory configs
- [ ] Add SSE streaming section to the reference: endpoint format, `Last-Event-ID` header for replay, event format (`id: {block}:{logIndex}\ndata: {json}\n\n`), and example `curl` command for streaming
- [ ] Add shared table documentation: explain that factory children, discovered contracts, and admin-registered contracts can all use shared tables, and that shared tables add a `contract_name` column for disambiguation. Show filtering by child: `?contract_address=eq.0x...`
- [ ] Add example query mappings for common patterns: "show me recent transfers" → `curl .../v1/Token/Transfer?limit=10&order=block_number.desc`, "what's the total volume?" → `curl .../v1/DEX/VolumeUpdate/aggregate`, "current token price?" → `curl .../v1/Oracle/get_price?_view_key=eq.latest`, "list factory children" → `curl .../v1/DEXFactory/children`

**Implementation Notes**:
- The REST API-first shift means the skill's primary output is `curl` commands, not `ibis query` commands. This is more universal — works in any environment where the API server is reachable, doesn't require the ibis binary installed locally, and maps more directly to what developers use in scripts and integrations.
- The `GET /v1/status` endpoint is the best single source of truth for discovery. It returns: `current_block`, `contracts[]` (name, address, events count, cursor), `factories{}` (child_count, synced, backfilling), and `views{}`. The skill should call this first to understand what data is available before constructing queries.
- View tables follow the naming convention `{contract}_{function}` (e.g., `mytoken_total_supply`). The `_view_key` column is a synthetic key — for unique view tables, it deduplicates to keep only the latest polled value. For log view tables, every poll result is appended.
- Discovery endpoints serve a different purpose than event queries — they return contract metadata (addresses, deployment info) rather than indexed event data. The skill should recognize when a user is asking about discovered contracts vs asking about data from those contracts.
- The reference doc should be structured with clear sections: System Endpoints, Event Endpoints, View Endpoints, Factory Endpoints, Discovery Endpoints, SSE Streaming, Query Parameters, Response Formats, Config Reference, Type Mappings. This makes it easy for the SKILL.md prompt to point to specific sections.
- The `inspect_config.sh` script should output structured sections like `=== Contracts ===`, `=== View Functions ===`, `=== Discovery ===`, `=== Factories ===` for easy parsing by the skill. Consider adding a `--json` flag that outputs machine-readable JSON for programmatic use.
- Skip admin API endpoints (register/deregister/update contracts) — the skill is focused on data queries, not operational management. Admin operations are covered by the ibis-admin skill (3.21b).
- Skip the `forward` table type (roadmap 3.16) and `--watch` CLI mode (roadmap 3.17) — neither is implemented yet. Add coverage in future skill updates when those features ship.

### 3.21b Create Ibis-Admin Skill

**Description**: Create a new Claude skill (`~/.claude/skills/ibis-admin/`) that handles contract management and operational monitoring via the ibis HTTP admin API. This is the counterpart to the ibis-query skill (3.21) — ibis-query handles data queries, ibis-admin handles contract lifecycle management. The skill translates natural language requests like "register this ERC20 at 0x123 with Transfer events" into fully-formed `curl` commands against the admin API endpoints (`POST/DELETE/GET/PUT /v1/admin/contracts`), constructs complex JSON registration payloads from conversational input, and provides health/status monitoring. It does NOT cover CLI lifecycle commands (`ibis init`, `ibis run`) or data queries — those belong to other skills.

**Requirements**:
- [ ] Create `~/.claude/skills/ibis-admin/SKILL.md` with skill frontmatter (name: `ibis-admin`, description triggers on "register contract", "add contract to indexer", "remove contract", "indexer status", "is ibis healthy", "list registered contracts", "update contract config")
- [ ] Define the skill workflow Step 1 (Discover Server): determine the ibis API server URL — read `ibis.config.yaml` for `api.host`/`api.port`, default to `http://localhost:8080`, and check if an admin key is configured (`api.admin_key`). Optionally verify the server is reachable via `GET /v1/health`
- [ ] Define the skill workflow Step 2 (Understand Intent): classify the user's request into one of: `register` (add new contract), `deregister` (remove contract), `update` (modify existing contract), `list` (show all contracts), `status` (indexer health and progress), `health` (simple health check)
- [ ] Define the skill workflow Step 3 (Build Request) for `register` intent: translate natural language into a `POST /v1/admin/contracts` JSON body. Support building the full `ContractConfig` payload including `name`, `address`, `abi` (default `"fetch"`), `events[]` (with table type, unique_key, aggregates), `views[]` (with function, interval, table config), `start_block`, and `factory` config. Use conversational context to infer table types (e.g., "track transfers" → log table, "leaderboard" → unique table, "volume tracking" → aggregation table)
- [ ] Define the skill workflow Step 3 for `deregister` intent: construct `DELETE /v1/admin/contracts/{name}` with optional `?drop_tables=true`. Always confirm with user before including `drop_tables=true` since it's destructive
- [ ] Define the skill workflow Step 3 for `update` intent: construct `PUT /v1/admin/contracts/{name}` with updated JSON body. Show what will change vs current config
- [ ] Define the skill workflow Step 3 for `list` intent: construct `GET /v1/admin/contracts` and format the response showing contract name, address, event count, current block, status (active/syncing/backfilling), and whether it's dynamic or a factory child
- [ ] Define the skill workflow Step 3 for `status`/`health` intents: construct `GET /v1/status` or `GET /v1/health` and present results with context — highlight contracts that are behind, factories with backfilling children, views with consecutive errors
- [ ] Define the skill workflow Step 4 (Execute and Present): run the `curl` command, parse the response, present results in human-readable format with contextual interpretation (e.g., "Contract registered successfully — it will start indexing from block 850000" or "3 of 12 factory children are still backfilling")
- [ ] Create `~/.claude/skills/ibis-admin/references/admin_reference.md` with complete admin API reference: all 4 endpoints (`POST`, `DELETE`, `GET`, `PUT` on `/v1/admin/contracts`), plus `GET /v1/health` and `GET /v1/status`, with exact request/response JSON formats, HTTP status codes, error formats (`{"error": "message"}`), and auth header (`X-Admin-Key`)
- [ ] Include in the reference: full `ContractConfig` JSON schema for registration payloads — all fields with types, defaults, and which are required vs optional. Cover nested structures: `events[].table` (type, unique_key, aggregate[]), `views[]` (function, calldata, interval, table, headers), `factory` (event, child_address_field, child_abi, child_events, shared_tables, child_name_template)
- [ ] Include NL-to-table-type mapping rules in SKILL.md: "track/log/record" → `log`, "leaderboard/current state/latest per" → `unique`, "total/sum/count/volume" → `aggregation`, "all events/everything" → wildcard `"*"` with `log` type. Include event name inference: "transfers" → `Transfer`, "swaps" → `Swap`, "approvals" → `Approval`
- [ ] Include guidance for auth handling: if admin key is configured, always include `-H "X-Admin-Key: {key}"` in curl commands. If key is from env var (`${IBIS_ADMIN_KEY}`), use `-H "X-Admin-Key: $IBIS_ADMIN_KEY"`. If no key configured, omit the header
- [ ] Include error handling guidance: map common HTTP status codes to user-friendly explanations — 401 = "Admin key is wrong or missing", 503 = "Engine not running — is `ibis run` started?", 500 with "already registered" = "Contract already exists — use update instead", 404 = "Contract not found — check `ibis admin list`"
- [ ] Add example curl commands for common workflows: register a simple ERC20, register a factory contract with shared tables, deregister with table cleanup, update to add new events, check indexer health

**Implementation Notes**:
- The skill follows the same structure as ibis-query and ibis-config: `SKILL.md` (main prompt), `references/admin_reference.md` (API reference), and optionally a helper script.
- The NL-to-payload translation is the key differentiator. When a user says "add the STRK token at 0x04718f5a0fc34cc1af16a1cdee98ffb20c31f5cd61d6ab07201858f4287c938d with Transfer and Approval events", the skill should generate the complete JSON body with `abi: "fetch"`, two event entries (both `log` type), and appropriate metadata — without requiring the user to manually craft JSON.
- For complex registrations (factories, views, aggregations), the skill should ask follow-up questions rather than guessing. E.g., "You want to track volume — which field contains the volume amount?" or "Should factory children share tables?"
- The `drop_tables=true` flag on deregistration is destructive and irreversible. The skill should ALWAYS confirm before including it, and explain that shared tables won't be dropped (other children use them).
- The status endpoint is the skill's diagnostic tool. When presenting status, highlight actionable insights: contracts stuck at block 0 (never started), large gaps between per-contract cursors (one contract lagging), views with `consecutive_errors > 0` (polling failures).
- Auth handling should be context-aware: check the config file first for `api.admin_key`. If found, include it automatically. If not found, check if the user has mentioned an admin key or environment variable. If the server returns 401, suggest configuring the admin key.
- The skill does NOT generate ibis.config.yaml entries — that's the ibis-config skill's job. The admin skill operates against a RUNNING ibis instance via HTTP. If the user wants to add a contract to the config file (for next restart), redirect them to ibis-config.
- Keep the reference doc focused on the admin API surface. Don't duplicate the data query reference from ibis-query — link to it if needed. The reference should be the single source of truth for request/response formats, status codes, and auth.
- Error response format is always `{"error": "message"}` with appropriate HTTP status code. The skill should parse this and present the error message clearly.

### 3.22 Documentation Setup

**Description**: Comprehensive user-facing documentation for ibis, living as plain markdown files in the `docs/` folder. The current documentation consists of a README with quick-start coverage, a SPEC.md that is outdated (missing factory support, discovery, views, admin API, shared tables, and many config fields), and a ROADMAP.md. This task creates a full documentation suite following the Diátaxis framework: getting started (tutorial), configuration/CLI/API references, conceptual guides for key features, and a deployment guide. Each subtask (3.22a–3.22j) produces one documentation page. Ibis ships three agent skills (`ibis-config`, `ibis-query`, `ibis-admin`) — each relevant documentation page includes a brief callout showing how the skill can assist with that topic, and 3.22j provides a dedicated skills guide.

**Requirements**:
- [ ] 3.22a: Getting Started Guide
- [ ] 3.22b: Configuration Reference
- [ ] 3.22c: REST API Reference
- [ ] 3.22d: CLI Reference
- [ ] 3.22e: Table Types Guide
- [ ] 3.22f: Factory & Discovery Guide
- [ ] 3.22g: Real-Time Streaming (SSE) Guide
- [ ] 3.22h: Deployment Guide
- [ ] 3.22i: Update SPEC.md
- [ ] 3.22j: Agent Skills Guide

**Implementation Notes**: Plain markdown files in `docs/`, readable directly on GitHub. Use real Starknet contract addresses (ETH, STRK) in tutorial/getting-started content for copy-paste usability; use generic placeholders in reference docs for clarity. Each page should be self-contained with internal cross-links to related pages. Follow progressive disclosure — start with the simplest use case and layer complexity. Each guide that maps to a skill should include a brief "Using the agent skill" callout (1–3 sentences + example prompt) linking to the full Agent Skills Guide (3.22j).

---

#### 3.22a Getting Started Guide

**Description**: A step-by-step tutorial that takes a new user from zero to a running ibis indexer with queryable data. This is the primary entry point for new users and should feel effortless. Uses real Starknet contracts (ETH, STRK) so users can follow along with copy-paste commands. Distinct from the README quick start — this is a full walkthrough with explanations.

**Requirements**:
- [ ] Create `docs/GETTING-STARTED.md`
- [ ] Prerequisites section: Go 1.25+ (for source build), or direct binary install. Docker optional for PostgreSQL
- [ ] Installation walkthrough covering all three methods: asdf (recommended), binary release, build from source
- [ ] First indexer tutorial: `ibis init --contract <STRK_address> --network mainnet --database memory` with real STRK token address, explaining each flag
- [ ] Walk through the generated `ibis.config.yaml`, explaining each section (network, rpc, database, contracts, events)
- [ ] Run the indexer: `ibis run`, explain the startup logs (ABI fetching, table creation, subscription, backfill)
- [ ] Query data: demonstrate `ibis query` CLI with `--format table`, `--latest`, `--count`, and `--filter` flags
- [ ] Query via REST API: show `curl` commands against the running API with filters and ordering
- [ ] SSE streaming teaser: one `curl` to the `/stream` endpoint showing real-time events
- [ ] "Next steps" section linking to Configuration Reference, Table Types Guide, Factory Guide, and Agent Skills Guide
- [ ] Troubleshooting section: common issues (RPC connection failures, ABI fetch errors, port conflicts)
- [ ] Brief "Agent skills" callout after the `ibis init` step: mention that `ibis-config` can generate configs from natural language (e.g., "index all Transfer events from the STRK token"), with install command and link to Agent Skills Guide

**Implementation Notes**:
- Use the STRK token contract (`0x04718f5a0fc34cc1af16a1cdee98ffb20c31f5cd61d6ab07201858f4287c938d`) for the walkthrough — it has frequent Transfer events on mainnet, so users will see data immediately.
- The tutorial should work end-to-end with `--database memory` so users don't need PostgreSQL installed for their first try.
- Include expected output snippets for each command so users can verify they're on the right track.
- Keep explanations concise but don't skip the "why" — a first-time user should understand what ibis is doing at each step.

---

#### 3.22b Configuration Reference

**Description**: Complete reference documentation for every field in `ibis.config.yaml`. This is the authoritative source for config syntax, types, defaults, validation rules, and environment variable expansion. Organized hierarchically by config section with examples for each field.

**Requirements**:
- [ ] Create `docs/CONFIGURATION.md`
- [ ] Top-level fields: `network` (mainnet/sepolia/custom), `rpc` (WSS/HTTP endpoint with scheme validation)
- [ ] `database` section: `backend` (postgres/badger/memory), `postgres.*` (host, port, user, password, name), `badger.path` — with defaults and env var examples
- [ ] `api` section: `host`, `port`, `cors_origins`, `admin_key` — with CORS and security guidance
- [ ] `indexer` section: `start_block`, `pending_blocks`, `batch_size`, `udc_address`, `udc_event` (version, address_key/data, class_hash_key/data) — with guidance on when to customize UDC settings
- [ ] `contracts[]` section: `name`, `address`, `abi` (file path / contract name / "fetch"), `start_block`, `events[]`, `views[]`, `factory`
- [ ] `events[]` section: `name` (explicit or `"*"` wildcard), `table.type` (log/unique/aggregation), `table.unique_key`, `table.aggregate[]` — with wildcard override behavior explained
- [ ] `factory` section: `event`, `child_address_field`, `child_abi`, `child_events`, `shared_tables`, `child_name_template` — with template variable reference
- [ ] `views[]` section: `function`, `calldata`, `interval`, `table` (log/unique only), `headers` — with interval minimum (1s) and `_view_key` explanation
- [ ] `discover[]` section: `class_hash`, `group`, `abi`, `events`, `shared_tables`, `views`, `name_template` — with template variable reference
- [ ] Environment variable expansion: `${VAR_NAME}` syntax, where it applies, common patterns for secrets
- [ ] Validation rules summary: all constraints enforced during config loading
- [ ] Full annotated example config showing all features together
- [ ] Minimal example configs: simplest possible config, memory-only dev config, PostgreSQL production config
- [ ] Brief "Agent skill" callout: mention that `ibis-config` can generate complete configs from natural language or a contract address — useful for bootstrapping a config that you then customize manually. Link to Agent Skills Guide

**Implementation Notes**:
- Structure as a single page with a table of contents using markdown heading anchors. Each field gets: type, default value, required/optional, description, example.
- Reference the actual validation logic in `internal/config/validate.go` and defaults in `internal/config/config.go` to ensure accuracy.
- The UDC event section is advanced — note that most users will never need it (auto-detection handles mainnet/sepolia). Only relevant for devnets, appchains, or custom deployer contracts.

---

#### 3.22c REST API Reference

**Description**: Complete reference for every REST API endpoint ibis exposes. Covers request/response formats, query parameters, headers, status codes, and example `curl` commands. This replaces the brief API section in the README with a full reference.

**Requirements**:
- [ ] Create `docs/API-REFERENCE.md`
- [ ] System endpoints: `GET /v1/health` (response format), `GET /v1/status` (full response schema with contracts, cursors, factories, views)
- [ ] Event list endpoint: `GET /v1/{contract}/{event}` — query params (limit, offset, order, field filters), response format (`{"data": [...], "count": N}`), Supabase-style filter operators (eq, neq, gt, gte, lt, lte)
- [ ] Latest event: `GET /v1/{contract}/{event}/latest` — response format, use cases
- [ ] Event count: `GET /v1/{contract}/{event}/count` — response format, filter support
- [ ] Unique entries: `GET /v1/{contract}/{event}/unique` — when available (unique table type only), response format
- [ ] Aggregation: `GET /v1/{contract}/{event}/aggregate` — response format (`{"data": {"column": value}}`), available operations
- [ ] SSE streaming: `GET /v1/{contract}/{event}/stream` — content type, event format, `Last-Event-ID` reconnection, filter params
- [ ] Factory endpoints: `GET /v1/{factory}/children`, `GET /v1/{factory}/children/count` — response formats, metadata filter support
- [ ] Discovery endpoints: `GET /v1/discover/{classHash}/contracts` — response format
- [ ] Admin endpoints: `POST /v1/admin/contracts`, `GET /v1/admin/contracts`, `PUT /v1/admin/contracts/{name}`, `DELETE /v1/admin/contracts/{name}` — request bodies, `X-Admin-Key` header, `?drop_tables=true` param
- [ ] Query parameter reference table: all filter operators with examples
- [ ] Common response fields: `event_id`, `contract_address`, `event_name`, `block_number`, `block_hash`, `transaction_hash`, `log_index`, `timestamp`, `status`, plus decoded event fields
- [ ] Error responses: format, common error codes (400, 404, 500)
- [ ] Pagination patterns: cursor-based, offset-based, best practices for large datasets
- [ ] Brief "Agent skills" callout: mention `ibis-query` for natural language data queries against the API and `ibis-admin` for managing contracts via the admin endpoints. Link to Agent Skills Guide

**Implementation Notes**:
- Reference `internal/api/server.go` for route registration, `internal/api/handlers.go` for response formats, and `internal/api/query.go` for query parameter parsing.
- Use generic placeholder contract/event names in the reference sections. Include a "Try it" box with real contract examples for key endpoints.
- The status endpoint response is complex — include the full JSON schema since it's the primary discovery endpoint.

---

#### 3.22d CLI Reference

**Description**: Complete reference for all `ibis` CLI commands, subcommands, flags, and usage patterns. Expands on the README's CLI section with full flag descriptions, examples, and output format documentation.

**Requirements**:
- [ ] Create `docs/CLI-REFERENCE.md`
- [ ] `ibis init` command: all flags (`--contract`, `--output`, `--network`, `--rpc`, `--database`, `--non-interactive`), interactive vs non-interactive mode behavior, example workflows
- [ ] `ibis run` command: `--config` flag, startup sequence description, graceful shutdown behavior (SIGINT/SIGTERM)
- [ ] `ibis query` command: all flags documented with examples — `--limit`, `--offset`, `--order`, `--filter`, `--unique`, `--aggregate`, `--latest`, `--count`, `--children`, `--children-count`, `--contract-address`, `--format`, `--list`
- [ ] Output format examples: `--format json` (one JSON object), `--format table` (aligned columns), `--format csv` (comma-separated with headers)
- [ ] Filter syntax: `--filter "field=op.value"` with all operators (eq, neq, gt, gte, lt, lte), multiple filters, combining with other flags
- [ ] Factory queries: `--children`, `--children-count`, querying shared table data with `--contract-address`
- [ ] View function queries: querying view tables, `_view_key` filtering
- [ ] `--list` flag: output format, table metadata shown
- [ ] Common usage patterns: "get latest transfer", "count events since block X", "export to CSV", "query factory children"
- [ ] Exit codes and error messages
- [ ] Brief "Agent skill" callout: mention `ibis-query` for translating natural language questions into `ibis query` commands (e.g., "show me the top 10 transfers" becomes the right CLI invocation). Link to Agent Skills Guide

**Implementation Notes**:
- Reference `internal/cli/init.go`, `internal/cli/run.go`, and `internal/cli/query.go` for flag definitions and behavior.
- Include the actual CLI help text (`ibis --help`, `ibis query --help`) as a starting point, then expand with examples and explanations.
- The query command has mutual exclusivity rules (e.g., `--latest` and `--count` can't be combined) — document these clearly.

---

#### 3.22e Table Types Guide

**Description**: Conceptual guide explaining the three table types (log, unique, aggregation), when to use each, how they behave, and how they map to database schemas. This is a "how to think about it" guide, not a reference — it helps users choose the right table type for their use case.

**Requirements**:
- [ ] Create `docs/TABLE-TYPES.md`
- [ ] Overview: ibis creates database tables from event configs; the table type determines storage and query semantics
- [ ] **Log tables**: append-only event log, every event is stored, use cases (transaction history, audit trails, analytics), example config, example queries, database schema (all standard columns + decoded event fields)
- [ ] **Unique tables**: last-write-wins by a configurable key field, only the most recent event per key is stored, use cases (leaderboards, current state, latest price per token), `unique_key` field selection guidance, example config, example queries via `/unique` endpoint
- [ ] **Aggregation tables**: auto-computed aggregate values updated on each event, supported operations (sum, count, avg), use cases (volume tracking, transfer counts, running averages), example config with multiple aggregates, example queries via `/aggregate` endpoint
- [ ] **Wildcard with overrides**: explain the `"*"` pattern — all events default to one type, specific events override. Show a realistic config mixing all three types
- [ ] **Choosing the right type**: decision tree or table mapping common use cases to table types
- [ ] **Database representation**: how each type maps to PostgreSQL tables, BadgerDB keys, and in-memory structures
- [ ] **View function tables**: brief mention that views also produce log or unique tables (link to Factory & Discovery guide for details)

**Implementation Notes**:
- Reference `internal/schema/` for how table schemas are built, and `internal/store/` for how each backend implements the three types.
- Use concrete examples: ERC20 Transfer events as log tables, a game leaderboard as a unique table, volume tracking as an aggregation table.
- The aggregation table is the most complex — include a worked example showing how `sum` and `count` update as events arrive.

---

#### 3.22f Factory & Discovery Guide

**Description**: Guide covering ibis's advanced contract discovery features: factory contracts (automatic child detection), class hash discovery (UDC watching), shared tables, dynamic contract management via admin API, and view function polling. These features are what differentiate ibis from simple event indexers.

**Requirements**:
- [ ] Create `docs/ADVANCED-FEATURES.md`
- [ ] **Factory contracts**: what factories are (contracts that deploy children), config structure (`factory` block), `child_address_field` explanation, child ABI resolution, `child_events` templating
- [ ] **Shared tables**: why (thousands of children would create thousands of tables), how (`shared_tables: true`), `contract_address` column for disambiguation, querying with `?contract_address=eq.0x...`
- [ ] **Child naming**: `child_name_template` with available variables (`{factory}`, `{short_address}`, `{address}`), default template
- [ ] **Factory API**: `GET /v1/{factory}/children`, `GET /v1/{factory}/children/count`, metadata from factory event promoted to queryable fields
- [ ] **Class hash discovery**: concept (watch for deployments of a known class hash via UDC), config structure (`discover[]` block), `class_hash`, `group`, `abi`, `shared_tables`, `name_template`
- [ ] **UDC configuration**: `indexer.udc_address`, `indexer.udc_event` format options (auto/v0/v1), when to customize (devnet, appchains)
- [ ] **Discovery API**: `GET /v1/discover/{classHash}/contracts`
- [ ] **Dynamic contract management**: admin API overview, `POST /v1/admin/contracts` for runtime registration, `DELETE /v1/admin/contracts/{name}` for deregistration, `X-Admin-Key` authentication, use cases
- [ ] **View function polling**: concept (periodically call read-only functions and index results), config structure (`views[]`), `function`, `calldata`, `interval`, `_view_key` for unique tables, log vs unique view tables
- [ ] End-to-end example: configuring a DEX factory (AMM pair factory) with shared tables, child events, and view function polling
- [ ] Brief "Agent skills" callout: mention `ibis-config` for generating factory/discovery configs from natural language, and `ibis-admin` for dynamically registering new contracts at runtime without restarting the indexer. Link to Agent Skills Guide

**Implementation Notes**:
- This is the "power user" guide — it covers features that most indexers don't have. Lead with the use case, then show the config, then explain the mechanics.
- Reference the Jediswap or 10KSwap AMM factory pattern on Starknet as the motivating example for factories.
- For discovery, reference the pattern of watching for all ERC20 deployments of a known class hash.
- Keep the admin API section brief — it's documented fully in the API Reference. Focus on the "why" and "when" here.

---

#### 3.22g Real-Time Streaming (SSE) Guide

**Description**: Guide covering ibis's Server-Sent Events (SSE) streaming for real-time event delivery to clients. Covers the SSE protocol, reconnection with gap-free replay, filtering, and integration patterns for web frontends and backend services.

**Requirements**:
- [ ] Create `docs/SSE-STREAMING.md`
- [ ] **What is SSE**: brief intro to Server-Sent Events, why ibis uses SSE over WebSocket for one-directional event delivery
- [ ] **Endpoint**: `GET /v1/{contract}/{event}/stream`, content type `text/event-stream`
- [ ] **Event format**: `id: {block}:{logIndex}\ndata: {json}\n\n` with example
- [ ] **Reconnection**: `Last-Event-ID` header, automatic gap-free replay, how ibis replays missed events on reconnect
- [ ] **Filtering**: query params on the stream endpoint (same Supabase-style filters as REST), example: `?sender=eq.0x123`
- [ ] **Client examples**: `curl` for terminal, JavaScript `EventSource` for web frontends, Go `net/http` for backend services
- [ ] **Factory streaming**: streaming events from shared tables, filtering by child contract
- [ ] **Best practices**: connection management, error handling, backpressure, when to use SSE vs polling

**Implementation Notes**:
- Reference `internal/api/sse.go` for the server implementation details.
- The JavaScript `EventSource` example is particularly important — most ibis users will be web developers building frontends on top of indexed data.
- Note the 64-event subscriber buffer — if a client falls too far behind, events are dropped (non-blocking delivery). This is by design to protect the indexer.

---

#### 3.22h Deployment Guide

**Description**: Guide for deploying ibis in production environments. Covers Docker, docker-compose, environment configuration, PostgreSQL setup, and production best practices.

**Requirements**:
- [ ] Create `docs/DEPLOYMENT.md`
- [ ] **Development setup**: `--database memory` for quick local dev, `ibis run` with default config
- [ ] **Docker**: `make docker-build`, `make docker-run`, Dockerfile explanation, environment variables
- [ ] **Docker Compose**: `make docker-compose-up`, included PostgreSQL service, config volume mounting, environment file
- [ ] **PostgreSQL setup**: manual PostgreSQL setup (create database, user, permissions), connection string configuration, env var pattern for credentials
- [ ] **Production checklist**: database backend (always PostgreSQL), appropriate `start_block`, `admin_key` for admin endpoints, CORS configuration, log monitoring
- [ ] **Environment variables**: full list of `${VAR}` patterns used in config, `.env` file patterns
- [ ] **Monitoring**: health check endpoint (`/v1/health`), status endpoint (`/v1/status`) for sync progress, what to alert on (sync lag, connection drops)
- [ ] **Scaling considerations**: single indexer instance (writes), multiple API readers possible with shared PostgreSQL, backfill duration estimates
- [ ] **Backup and recovery**: cursor-based resume (ibis picks up where it left off), database backup strategies

**Implementation Notes**:
- Reference the existing `Dockerfile`, `docker-compose.yaml`, and `configs/ibis.config.docker.yaml` for accuracy.
- Production deployment is PostgreSQL-only in practice — BadgerDB is for single-machine/embedded use, memory is for dev/test. Make this clear.
- Include a minimal `docker-compose.yaml` snippet that users can copy-paste.

---

#### 3.22i Update SPEC.md

**Description**: Bring `docs/SPEC.md` up to date with the current state of the codebase. The existing SPEC was written at project inception and is missing major features (factory support, discovery, views, admin API, shared tables, dynamic contracts) and has outdated information (Go version, project structure, store interface, API endpoints). This is a targeted update, not a rewrite — preserve the document's structure and voice while adding missing content and correcting outdated sections.

**Requirements**:
- [ ] Update **Tech Stack** table: Go version to 1.25+, add any new dependencies
- [ ] Update **Project Structure** tree: add missing files/directories that have been added since inception (factory, discovery, view poller, admin handlers, shared table logic, SSE, forward types, etc.)
- [ ] Update **ABI Parser** section: add CairoTuple support, mention any new type handling
- [ ] Update **Config Manager** section: add `discover[]`, `views[]`, `api.cors_origins`, `api.admin_key`, `indexer.udc_address`, `indexer.udc_event`, per-contract `start_block`, factory `shared_tables` and `child_name_template`
- [ ] Update **Indexing Engine** section: add factory child detection and registration, discovery/UDC watching, view function polling, dynamic contract lifecycle
- [ ] Update **Store Interface** section: update the interface definition to match current methods (`CountEvents`, `DropTable`, `SaveDynamicContract`, `GetDynamicContracts`, `DeleteDynamicContract`, `DeleteCursor`, `GetAllCursors`)
- [ ] Update **API Server** section: add factory endpoints, discovery endpoints, admin endpoints, SSE streaming details, CORS
- [ ] Update **Data Models** section: add any new types or updated fields (SharedTable on TableSchema, factory/discovery types)
- [ ] Update **Key Decisions** table: add decisions made since inception (shared tables approach, UDC watching strategy, admin API design, SSE over WebSocket for streaming)
- [ ] Add **Factory & Discovery** section to Core Modules: factory detection flow, shared table mechanics, UDC event parsing, discovery registration
- [ ] Add **View Function Polling** section to Core Modules: polling loop, result indexing, `_view_key` semantics
- [ ] Cross-reference new documentation pages where appropriate (link to Getting Started, API Reference, etc.)

**Implementation Notes**:
- Read the actual codebase (store interface, config struct, API routes, engine methods) to ensure SPEC.md matches reality — don't rely on memory or the roadmap.
- Preserve the existing SPEC.md tone and structure. It's a technical specification, not a user guide — keep it precise and implementation-focused.
- The Store interface has grown significantly — the current interface in SPEC.md is a simplified version. Update it to match the actual `Store` interface in `internal/store/store.go`.
- The project structure tree should match what `ls -R` shows, not what was planned. Add new directories and files, remove any that no longer exist.

---

#### 3.22j Agent Skills Guide

**Description**: Dedicated guide for ibis's three Claude Code agent skills: `ibis-config` (config generation), `ibis-query` (natural language data queries), and `ibis-admin` (runtime contract management). This is the central reference that all other docs link to from their "Agent skill" callouts. Covers installation, what each skill does, example prompts, and how the skills complement the CLI/API workflows.

**Requirements**:
- [ ] Create `docs/AGENT-SKILLS.md`
- [ ] **Overview**: ibis ships three agent skills for Claude Code that let you interact with the indexer using natural language — generating configs, querying data, and managing contracts at runtime
- [ ] **Installation**: `npx skills add b-j-roberts/ibis` (all skills), or individual install commands for `ibis-config`, `ibis-query`, `ibis-admin`. Prerequisites: Claude Code CLI installed
- [ ] **ibis-config skill**: what it does (generates `ibis.config.yaml` from contract addresses or natural language), when to use it (bootstrapping a new config, adding contracts, modifying event selections), example prompts ("generate an ibis config for the STRK token", "add Transfer and Approval events as log tables", "set up a factory config for this AMM"), what it produces (complete YAML config file)
- [ ] **ibis-query skill**: what it does (translates natural language questions into `ibis query` CLI commands or REST API `curl` calls), when to use it (exploring indexed data, building queries without memorizing flag syntax), example prompts ("show me the 10 most recent transfers", "how many swaps happened today?", "what's the total trading volume?", "list all factory children"), output formats (JSON, table, CSV)
- [ ] **ibis-admin skill**: what it does (manages contracts on a running ibis instance via the admin API), when to use it (registering new contracts at runtime, checking indexer health, deregistering contracts), example prompts ("add the STRK token to the indexer", "what's the indexer status?", "remove MyContract and drop its tables", "list all registered contracts"), prerequisite (ibis must be running with `ibis run`)
- [ ] **Skill vs CLI/API comparison table**: when to use which — skill for exploration/bootstrapping, CLI for scripting/automation, API for application integration
- [ ] **Workflow examples**: end-to-end scenarios combining skills with manual steps — e.g., "Use `ibis-config` to generate a config, manually review and tweak it, `ibis run` to start, use `ibis-query` to explore the data, use `ibis-admin` to add another contract at runtime"
- [ ] **Troubleshooting**: skill not found (install instructions), skill produces incorrect query (how to refine prompts), admin skill can't connect (ibis not running or wrong API URL)

**Implementation Notes**:
- This is the one page that gives all three skills equal treatment. Other doc pages just have brief callouts (1–3 sentences) that link here.
- The example prompts should be realistic and demonstrate the range of natural language the skills understand — not just simple cases.
- The ibis-admin skill (3.21b) may not be implemented yet when this doc is written. If so, include it with a note that it's coming soon, since the skill is scoped and the API endpoints already exist.
- Keep the tone practical and example-driven. Users should be able to scan the example prompts and immediately understand what each skill does.
- The comparison table is important — it prevents confusion about when to use a skill vs the CLI vs direct API calls. Skills are best for exploration and one-off tasks; CLI for scripts and pipelines; API for application code.

### 3.23 Factory Children Pagination

**Description**: Add `limit`, `offset`, and `order` support to the factory children endpoint (`GET /v1/{factory}/children`). Currently, this endpoint returns all children at once with no pagination, which is inconsistent with the event list endpoints and will cause performance issues for factories with hundreds or thousands of children (e.g., a DEX factory with many trading pairs). The `/children/count` endpoint should also return the total (pre-pagination) count to support client-side pagination UIs.

**Requirements**:
- [ ] Parse `limit`, `offset`, and `order` query params in `handleFactoryChildren` (reuse existing `parseQuery` from `query.go`)
- [ ] Apply in-memory sorting on the filtered children slice (support `name`, `deployment_block`, `current_block`, `status`, and metadata fields)
- [ ] Default sort: `deployment_block.desc` (newest children first)
- [ ] Apply offset/limit slicing after filtering and sorting
- [ ] Return `listResponse`-style envelope: `{"data": [...], "count": N, "total": T, "limit": L, "offset": O}` where `count` is page size and `total` is total matching (pre-pagination) count
- [ ] Cap `limit` at `maxLimit` (500) and default to `defaultLimit` (50), matching event endpoints
- [ ] Update `handleFactoryChildCount` to remain unchanged (it already returns total count with filters)
- [ ] Add tests for pagination, sorting, offset beyond total, empty results, and combined filter+pagination
- [ ] Update `docs/API-REFERENCE.md` factory children section to document the new query parameters

**Implementation Notes**:
- The factory children data comes from `engine.FactoryChildren()` which returns `[]ContractInfo` from in-memory state — pagination is purely an API-layer concern. No changes to the engine or store interface needed.
- Replace `parseFiltersFromURL(r)` with `parseQuery(r)` in `handleFactoryChildren` to get limit/offset/order for free, then extract filters from the query.
- For sorting: use `slices.SortFunc` with a comparator that handles the `order` field. Metadata fields (promoted from `FactoryMeta`) are `any` type — compare as strings via `fmt.Sprint` for metadata fields, typed comparison for known fields (`deployment_block`, `current_block` as `uint64`).
- The response envelope adds a `total` field not present in `listResponse`. Either extend `listResponse` or use a custom struct for factory children. Prefer a custom struct to avoid breaking existing event endpoints.
- Edge cases: `offset >= total` should return empty `data` with the correct `total`; `limit=0` or negative should use the default.

### 3.24 Fix BadgerDB Range Filter Operators (gt, gte, lt, lte)

**Description**: The BadgerDB store's `toFloat64` helper is missing a `case string` handler, causing all range filter operators (`gt`, `gte`, `lt`, `lte`) to silently return incorrect results. Filter values arrive as strings from HTTP query parameters (e.g., `"850000"`), but `toFloat64` falls through to `return 0` for strings. This makes `lt`/`lte` return zero results and `gt`/`gte` return all results, regardless of the filter value. The same broken function is used by `sortEvents`, so sorting on string-typed data fields is also affected. `eq` and `neq` are unaffected because they compare via `fmt.Sprint`. The Memory store already handles this correctly — the fix is to port the missing `case string` branch.

**Requirements**:
- [ ] Add `case string` to `toFloat64()` in `internal/store/badger/badger.go` to parse numeric strings via `strconv.ParseFloat`, matching the Memory store's implementation
- [ ] Add `case string` to `toUint64()` in `internal/store/badger/badger.go` to parse numeric strings via `strconv.ParseUint`, for consistency
- [ ] Verify `matchFilter` range operators (`gt`, `gte`, `lt`, `lte`) return correct results when filter values are strings (the API query param path)
- [ ] Verify `sortEvents` correctly orders events when data fields are string-typed numerics (e.g., JSON-unmarshaled block numbers)
- [ ] Add integration test that exercises range filters through a real HTTP request → API → BadgerDB round-trip (the existing `TestFilterOperators` unit test passes because it uses raw `int` values, not JSON-unmarshaled strings)
- [ ] Ensure SSE replay filter (`sse.go` line 109, `Value: fmt.Sprintf("%d", block)`) works correctly with the fixed `toFloat64`
- [ ] Run `make check` to confirm no regressions

**Implementation Notes**:
- **Root cause**: `internal/store/badger/badger.go` `toFloat64()` (line ~814) is missing the `case string` branch that exists in `internal/store/memory/memory.go` `toFloat64()` (line ~531). The fix is a 5-line addition.
- **Filter value flow**: API query params (`?block_number=gt.850000`) are parsed in `internal/api/query.go:95-98` where `Value` is always a `string`. BadgerDB's `matchFilter` calls `toFloat64(expected)` on this string value, which returns 0 → all comparisons against 0 produce wrong results.
- The existing unit test `TestFilterOperators` in `badger_test.go` inserts events with `"score": i * 10` (raw `int`), so it never triggers the string path. The integration test should use actual HTTP requests with query param filters to catch the real-world code path.
- `sortEvents` at line ~792 also calls `toFloat64` on field values from `getFieldValue`, which returns `evt.Data[field]` — after JSON round-tripping through BadgerDB, these may be strings. The same fix resolves sorting.
- Consider adding `strconv` to imports if not already present in `badger.go`.

---

## Phase 4: Future

### 4.1 WebSocket Subscriptions for Clients

**Description**: Real-time event streaming via WebSocket connections for client applications, complementing the existing SSE support with bidirectional communication.

**Features**:
- WebSocket endpoint for real-time event subscriptions
- Client-side filtering and subscription management
- Multiple subscription channels per connection
- Heartbeat and automatic reconnection support
- Binary protocol option for high-throughput scenarios

**Rationale**: While SSE covers most real-time use cases, WebSocket subscriptions enable bidirectional communication, client-managed filters, and more efficient multiplexing of multiple event streams over a single connection. Essential for interactive applications like trading platforms.

### 4.2 MCP Server Integration

**Description**: Expose indexed Starknet data as MCP (Model Context Protocol) tools, enabling AI agents to query blockchain state as part of their workflows.

**Features**:
- MCP server that wraps ibis REST API as tools
- Schema-aware tool descriptions generated from ABI
- Natural language-friendly tool interfaces
- Support for complex multi-step queries
- Integration with Claude Desktop and other MCP clients

**Rationale**: As AI agents become primary consumers of structured data, exposing indexed blockchain data via MCP enables a new class of AI-powered applications that can reason about on-chain state without custom integration work.

### 4.3 State Reconstruction Tables

**Description**: Tables that reconstruct current contract state from event history, functioning as materialized views that always reflect the latest on-chain state.

**Features**:
- Define state tables that derive current values from event streams
- Automatic state computation on each relevant event
- Support for complex state transitions (not just last-write-wins)
- Snapshot and restore for fast state recovery
- Custom state reducers defined in config

**Rationale**: Many applications need current state (token balances, game positions, order books) rather than event history. Auto-derived state tables eliminate the need for custom state management code in the application layer.

### 4.4 Multi-Chain Support

**Description**: Extend Ibis beyond Starknet to support other chains that use similar event/log patterns, starting with chains that have Cairo-based execution.

**Features**:
- Chain-agnostic core with chain-specific providers
- Starknet appchain support (Madara, Dojo Katana)
- Potential EVM support for cross-chain indexing
- Unified query API across chains
- Cross-chain event correlation

**Rationale**: The ABI-driven, config-based indexer pattern is not Starknet-specific. Supporting multiple chains (especially Starknet L3s and appchains) multiplies the tool's utility with minimal architectural changes.

### 4.5 Block Processors

**Description**: Optional block-level processing pipeline that subscribes to `newHeads` and processes full blocks, enabling use cases beyond event indexing.

**Features**:
- Subscribe to `starknet_subscribeNewHeads` for block-level data
- Custom block processors that receive full block data (headers, transactions, receipts)
- Built-in processors: block metadata indexing, transaction tracking, gas analytics
- Configurable in `ibis.config.yaml` alongside event subscriptions
- Block processor hooks for custom logic (e.g., track all transactions from a specific sender)

**Rationale**: While event-driven indexing covers most use cases, some applications need block-level data (transaction counts, gas usage trends, block timing). Block processors provide this capability without changing the default event-subscription architecture.

### 4.6 Plugin System

**Description**: An extensible plugin architecture that allows users to add custom event processing, transformations, and integrations without modifying the core indexer.

**Features**:
- Go plugin interface for custom event processors
- WASM plugin support for language-agnostic extensions
- Pre-built plugins: webhook notifications, Slack alerts, custom aggregations
- Plugin marketplace or registry
- Hot-reload plugin updates without indexer restart

**Rationale**: Every indexing use case has unique requirements beyond what a config file can express. A plugin system lets power users extend Ibis without forking, while keeping the core simple for the majority of users.

### 4.7 Add REST API Proxy to `ibis query` for Memory Backend

**Description**: The `ibis query` CLI command opens its own database connection via `openStore(cfg)`. With the `memory` backend, this creates a separate empty in-memory store that cannot see data from the running `ibis run` process — making all CLI queries return zero results. This task adds automatic REST API proxying: when the configured backend is `memory`, `ibis query` forwards the query to the running API server instead of opening a local store, giving users a seamless CLI experience regardless of backend.

**Requirements**:
- [ ] Detect `memory` backend in `runQuery()` by checking `cfg.Database.Backend == "memory"`
- [ ] When memory backend is detected, probe the API server via `GET /v1/health` on the configured `api.host:api.port`
- [ ] If the health check succeeds, proxy the query through the REST API instead of opening a store
- [ ] Map CLI flags to REST API query parameters: `--limit` → `?limit=`, `--offset` → `?offset=`, `--order` → `?order=`, `--filter` → `?field=op.value`
- [ ] Handle `--latest` by calling `/v1/{contract}/{event}/latest`
- [ ] Handle `--count` by calling `/v1/{contract}/{event}/count`
- [ ] Handle `--unique` by calling `/v1/{contract}/{event}/unique`
- [ ] Handle `--aggregate` by calling `/v1/{contract}/{event}/aggregate`
- [ ] Handle `--children` and `--children-count` by calling `/v1/{factory}/children` and `/v1/{factory}/children/count`
- [ ] Format proxied responses using the same `--format` flag (json/table/csv) as direct store queries
- [ ] If the health check fails (API not running), return a clear error: `"memory backend requires a running ibis instance — start 'ibis run' first, or switch to badger/postgres for offline CLI queries"`
- [ ] Add `--api` flag as an explicit override to force REST proxy mode regardless of backend
- [ ] Add tests for the proxy path using `httptest.Server` to mock the API

**Implementation Notes**:
- The proxy logic should live in a new `proxyQuery()` function in `internal/cli/query.go` called from `runQuery()` when memory backend is detected or `--api` flag is set
- Build the target URL from `cfg.API.Host` and `cfg.API.Port` (default `0.0.0.0:8080`); when host is `0.0.0.0`, use `localhost` for the HTTP client
- Use `net/http` client (stdlib) — no external dependency needed
- Parse the JSON response from the API, then feed the decoded events into the existing `outputEvents()` / `outputCount()` / `outputAggregation()` formatters so all `--format` options work unchanged
- The REST API response structs (`listResponse`, `latestResponse`) are defined in `internal/api/handlers.go:12-17` — the proxy needs to decode these envelopes to extract the inner data
- `--list` already works without a store (reads from config), so no proxy needed there
- The `--api` flag enables using the proxy with any backend, useful for querying a remote ibis instance
