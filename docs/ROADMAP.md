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

### 3.19 View Function Config, Provider & ABI Decoding

**Description**: Add the foundational infrastructure for view function indexing: config parsing, validation, `starknet_call` support in the provider, and ABI-based decoding of function return values. This task builds all the pieces that the poller (3.20) consumes, without changing the indexing engine's runtime behavior. View function indexing allows users to poll contract read functions at configurable intervals and store results as regular ibis tables with auto-generated REST APIs — capturing on-chain state that is not derivable from events alone (e.g., oracle prices, total supply, governance parameters).

**Requirements**:
- [ ] Add `ViewConfig` struct to `internal/config/config.go` with fields: `Function` (string, required — the Cairo function name), `Calldata` ([]string, optional — hex felt arguments), `Interval` (string, required — Go duration like `"30s"`, `"5m"`), `Table` (TableConfig, required — reuses existing table config with `type: unique` or `type: log`)
- [ ] Add `Views []ViewConfig` field to `ContractConfig` (`yaml:"views,omitempty" json:"views,omitempty"`)
- [ ] Config validation in `internal/config/validate.go`: reject empty `function` names, validate `interval` parses as `time.Duration` (minimum 1s), validate `calldata` entries are valid hex felt strings, validate table type is `log` or `unique` only (aggregation not supported for views), require `unique_key` when table type is `unique`
- [ ] Add `Call(ctx context.Context, contractAddress *felt.Felt, entryPointSelector *felt.Felt, calldata []*felt.Felt, blockID rpc.BlockID) ([]*felt.Felt, error)` method to `StarknetProvider` in `internal/provider/provider.go`, wrapping the underlying `starknet.go` `provider.Call()` RPC method
- [ ] Add `FunctionDef` struct to `internal/abi/types.go` with fields: `Name` (string), `Selector` (*felt.Felt — computed via `sn_keccak`), `Inputs` ([]Member), `Outputs` ([]Member), `StateMutability` (string — must be `"view"`)
- [ ] Extend ABI parser (`internal/abi/parser.go`) to extract `FunctionDef` entries from ABI JSON for functions with `state_mutability: "view"`. Store in a `Functions` map on the `ABI` struct keyed by function name
- [ ] Add `DecodeFunctionOutputs(outputs []Member, felts []*felt.Felt) (map[string]any, error)` to `internal/abi/decoder.go` — decodes the flat `[]*felt.Felt` return value into a typed `map[string]any` using the same offset-tracking pattern as `DecodeEvent`. Output member names become map keys
- [ ] Add `EncodeFunctionCalldata(inputs []Member, args []string) ([]*felt.Felt, error)` to a new `internal/abi/encoder.go` — converts hex string calldata from config into `[]*felt.Felt` for the RPC call. For the static-calldata-only scope, this is a simple hex-to-felt conversion per argument
- [ ] Unit tests: `ViewConfig` validation (valid/invalid intervals, missing fields, bad calldata), `DecodeFunctionOutputs` with various return types (single felt, u256, struct, array), `EncodeFunctionCalldata` with hex strings, provider `Call` with mock RPC

**Implementation Notes**:
- The `FunctionDef` extraction reuses the existing ABI parser's 3-pass architecture. In pass 3 (where events are extracted), also extract function entries where `Type == "function"` and `StateMutability == "view"`. Non-view functions (external/constructor) are ignored.
- `DecodeFunctionOutputs` is structurally identical to `DecodeEventData` — both walk a `[]Member` definition and consume felts from an offset. The difference is that function outputs use the `outputs` field from the ABI instead of `data` members. Consider extracting a shared `decodeMembers(members []Member, felts []*felt.Felt, offset *int)` helper to avoid duplication.
- The provider `Call` method uses `rpc.BlockID{Tag: "latest"}` as the default block parameter. The poller (3.20) will pass this in.
- `starknet.go`'s `rpc.Provider` already has a `Call(ctx, FunctionCall, BlockID)` method — ibis just needs to expose it through `StarknetProvider`. The `FunctionCall` struct takes `ContractAddress`, `EntryPointSelector`, and `Calldata` as `*felt.Felt` values.
- Entry point selectors for functions use the same `sn_keccak(function_name)` as event selectors — reuse `ComputeSelector` from `internal/abi/selector.go`.
- Config YAML shape for views:
  ```yaml
  contracts:
    - name: PriceOracle
      address: "0x049d36570d4e46..."
      abi: fetch
      events:
        - name: PriceUpdated
          table:
            type: log
      views:
        - function: get_price
          calldata: ["0x4554480000000000000000000000000000000000000000000000000000000000"]
          interval: 30s
          table:
            type: unique
            unique_key: _view_key
        - function: total_supply
          interval: 5m
          table:
            type: log
  ```
- For `unique` view tables, the `unique_key` field identifies which output column to use as the deduplication key. A special convention `_view_key` can be used to mean "single-row, always overwrite" (the poller generates a constant key value).

### 3.20 View Function Poller & Engine Integration

**Description**: Build the polling engine that periodically calls view functions via `starknet_call`, decodes results using the ABI infrastructure from 3.19, stores them as regular table operations, and integrates with ibis's existing engine lifecycle (startup, shutdown, reorg handling). View function tables are registered with the same schema system as event tables, so they automatically get REST API endpoints, SSE streaming, and query support with zero additional API code.

**Requirements**:
- [ ] Create `internal/engine/poller.go` with a `ViewPoller` struct that manages periodic `starknet_call` polling for all configured view functions across all contracts
- [ ] `ViewPoller.Setup(contracts, provider, store)` — for each contract's `ViewConfig` entries: resolve the `FunctionDef` from the parsed ABI, compute the entry point selector, parse calldata from config hex strings, parse the polling interval, and build the `TableSchema` for the view's results
- [ ] `ViewPoller.Run(ctx)` — spawn one goroutine per view function, each running a `time.Ticker` at the configured interval. On each tick: call `provider.Call()` with `BlockID{Tag: "latest"}`, decode the result via `DecodeFunctionOutputs`, build a `store.Operation`, and call `store.ApplyOperations`. Clean shutdown via context cancellation
- [ ] Add startup jitter (up to 10% of the interval) per view function goroutine to spread RPC load and avoid thundering herd on startup
- [ ] Schema generation: extend `internal/schema/generator.go` with `BuildViewSchema(contractName string, funcDef *abi.FunctionDef, viewCfg config.ViewConfig) *types.TableSchema` — maps function output members to table columns using the same `CairoTypeToColumnType` as events. Adds metadata columns: `block_number`, `timestamp`, `_view_key` (synthetic poll identifier). Table name follows `{contract}_{function_name}` convention
- [ ] For `unique` table views: each poll overwrites the previous row (keyed by `unique_key` from config). For `log` table views: each poll appends a new row with the block number at poll time
- [ ] Integrate `ViewPoller` into `Engine.Run()`: create and start the poller alongside the event subscriber. The poller shares the engine's `store`, `provider`, and `context` but runs independently of event processing
- [ ] Wire view schemas into `Engine.Setup()` so they are included in `Engine.Schemas()` — this ensures the API server registers them and they appear in `/v1/status`
- [ ] Reorg handling: when the engine's `handleReorg` fires, signal the `ViewPoller` to immediately re-poll all view functions (re-query current state rather than reverting operation pairs). Use a channel or callback from the engine's reorg handler
- [ ] Skip-if-busy: if a poll tick fires while the previous RPC call for that function is still in flight, skip the tick and log a debug message. Use a non-blocking channel send or `sync.Mutex` per function
- [ ] Error resilience: transient RPC failures (connection reset, timeout, 429) do not terminate the polling goroutine. Log at warn level, increment an error counter, and continue on the next tick. After 10 consecutive failures, log at error level
- [ ] Record `block_number` from a `provider.BlockNumber()` call alongside each poll result, so the stored data is anchored to a specific chain height
- [ ] Add view function status to the `/v1/status` endpoint: for each view function, report `function_name`, `contract`, `interval`, `last_poll_block`, `last_poll_time`, `consecutive_errors`
- [ ] Unit tests: `ViewPoller` lifecycle (setup, run, shutdown), schema generation for view functions, mock RPC returning known felts and verifying decoded + stored values, skip-if-busy behavior, reorg re-poll trigger

**Implementation Notes**:
- The `ViewPoller` follows the same lifecycle pattern as `provider.EventSubscriber`: created during setup, started as a goroutine in `Run()`, stopped via context cancellation. It does NOT share the `e.events` channel — view results go directly to `store.ApplyOperations`.
- For `unique` view tables, the operation is always `OpUpdate` (or `OpInsert` on first poll). The `Key` is the `unique_key` value from the decoded output. For the common single-value case (e.g., `total_supply`), use `_view_key: "latest"` as a constant key so there's always exactly one row.
- For `log` view tables, the operation is always `OpInsert` with key `"{block_number}:{poll_index}"` following the same pattern as event log tables.
- Reorg re-poll strategy: the engine passes a `reorgChan chan struct{}` to the `ViewPoller`. When a reorg notification arrives, the engine sends on this channel. Each view goroutine selects on both its ticker and the reorg channel; on reorg signal, it immediately re-polls regardless of the interval.
- Duration parsing: use `time.ParseDuration(viewCfg.Interval)` during `ViewPoller.Setup()`. The config validation (3.19) already guarantees this parses successfully.
- The `/v1/status` endpoint already reports per-contract info via `engine.Contracts()`. Extend this to include view function metadata by adding a `Views []ViewStatus` field to `ContractInfo` or a separate `ViewFunctions` section in the status response.
- View tables participate in the existing API routing automatically: `GET /v1/PriceOracle/get_price` returns the latest polled value (for unique tables) or historical poll snapshots (for log tables). No new API handler code is needed — the parametric `{contract}/{event}` routes already match.
- For factory contracts with views, each child contract can inherit view configs from the factory template. The poller spawns per-child goroutines. With `shared_tables: true`, all children's poll results go to the same table with a `contract_address` discriminator column.
- Consider adding a `GET /v1/{contract}/{function}/poll` endpoint later (not in this task) that triggers an immediate on-demand poll — useful for debugging and testing.

### 3.21 Configurable UDC Address for Discover Mode

**Description**: The discover feature hardcodes the UDC (Universal Deployer Contract) address to `0x04a64cd09...`, which works on mainnet and Sepolia but fails silently on `starknet-devnet-rs` and custom appchains where the UDC lives at a different address. This makes class-hash-based contract discovery unusable for local development and testing. Adding a configurable `udc_address` field unblocks devnet users and anyone running Starknet appchains with non-standard UDC deployments.

**Requirements**:
- [ ] Add optional `UDCAddress` field (`yaml:"udc_address,omitempty"`) to `IndexerConfig` in `internal/config/config.go`
- [ ] In `applyDefaults()`, set `UDCAddress` to the current hardcoded constant (`0x04a64cd09a853868621d94cae9952b106f2c36a3f81260f85de6696c6b050221`) when the field is empty
- [ ] Config validation: if `udc_address` is provided, validate it as a hex hash via the existing `validateHexHash` helper
- [ ] Update `setupDiscovery()` in `internal/engine/discover.go` to read `e.cfg.Indexer.UDCAddress` instead of the `UDCAddress` constant
- [ ] Keep the `UDCAddress` constant as the exported default for programmatic use, but the engine must prefer the config value
- [ ] Add `udc_address` to the example discover config in `configs/` so users can see the option
- [ ] Unit tests: verify custom UDC address flows through config loading, validation, and into `discoveryState.udcAddress`; verify default is applied when omitted; verify invalid hex is rejected

**Implementation Notes**:
- The change is minimal: one new field on `IndexerConfig`, a default in `applyDefaults()`, a validation check, and a one-line change in `setupDiscovery()` to use `e.cfg.Indexer.UDCAddress` instead of the constant. The existing `discoveryState.udcAddress` field already holds a `*felt.Felt` parsed at setup time — just change what string it parses from.
- Place `udc_address` under `indexer:` (not under `discover:`) because a single ibis instance uses one UDC address for all class-hash discoveries. Multiple UDC addresses per discover entry is not a real-world need — devnets and appchains each have exactly one UDC.
- The `starknet-devnet-rs` UDC address (`0x041a78e741e5af2fec34b695679bc6891742439f7afb8484ecd7766661ad02bf`) emits the same `ContractDeployed` event with the same key/data layout, so no decoder changes are needed.
- Existing tests in `discover_test.go` that reference `UDCAddress` directly should continue to work since the default fills in the same value. New tests should verify that a custom address propagates correctly.
- Config YAML shape:
  ```yaml
  indexer:
    start_block: 0
    udc_address: "0x041a78e741e5af2fec34b695679bc6891742439f7afb8484ecd7766661ad02bf"

  discover:
    - class_hash: "0x47a9dc..."
      abi: fetch
      events:
        - name: "*"
          table:
            type: log
  ```

### 3.22 UDC Event Format Detection & Override

**Description**: The discover code in `handleDiscoveryEvent` hardcodes the v1 (modern Cairo) UDC event layout where `keys[1]` = deployed address and `data[0]` = class hash. The older Cairo 0 UDC puts all fields in the data array (`data[0]` = address, `data[3]` = classHash), with only the event selector in keys. Anyone running ibis against a chain with the older UDC — including early devnet versions and certain appchains — hits silent mismatches. This task adds three layers of UDC format handling: auto-detection from the event shape, a named version enum for the two known layouts, and fine-grained index overrides for truly custom UDC variants.

**Requirements**:
- [ ] Add `UDCEventFormat` struct to `internal/config/config.go` with fields: `Version` (string: `"auto"` | `"v0"` | `"v1"`, default `"auto"`), `AddressKey` / `AddressData` (optional int pointers for key/data index of deployed address), `ClassHashKey` / `ClassHashData` (optional int pointers for key/data index of class hash)
- [ ] Add `UDCEvent *UDCEventFormat` field to `IndexerConfig` (`yaml:"udc_event,omitempty"`)
- [ ] Config validation: reject if both `AddressKey` and `AddressData` are set (mutually exclusive — address is in keys OR data, not both); same for `ClassHashKey`/`ClassHashData`; validate index values are non-negative; validate `version` is one of `auto`, `v0`, `v1`; reject fine-grained overrides when `version` is `v0` or `v1` (overrides only apply with `version: auto` or when version is omitted)
- [ ] Implement auto-detection in `handleDiscoveryEvent`: if `len(keys) >= 4`, use v1 layout (keys[1] = address, data[0] = classHash); if `len(keys) == 1 && len(data) >= 4`, use v0 layout (data[0] = address, data[3] = classHash); if neither matches, log a warning with the actual key/data lengths and skip the event
- [ ] When `version` is explicitly `v0` or `v1`, skip auto-detection and use the fixed layout directly
- [ ] When fine-grained overrides are present (e.g., `address_key: 2`), use the specified indices regardless of auto-detection, falling back to the version's defaults for any unspecified field
- [ ] Add `extractDeployedAddress` and `extractClassHash` helper methods on `discoveryState` (or a `udcEventParser` struct) that encapsulate the format resolution logic, keeping `handleDiscoveryEvent` clean
- [ ] Bounds-check all index accesses against actual `len(keys)` and `len(data)` — return a descriptive error rather than panicking on out-of-range access
- [ ] Log the detected or configured UDC event format at startup (in `setupDiscovery`) so users can verify which layout ibis is using
- [ ] Unit tests: auto-detection with v0-shaped events (1 key, 4+ data), auto-detection with v1-shaped events (4+ keys), explicit `version: v0` override, explicit `version: v1` override, fine-grained index overrides, bounds-check failures, malformed events that match neither pattern

**Implementation Notes**:
- The two known UDC layouts are:
  - **v1 (modern Cairo)**: `keys[0]=selector, keys[1]=address, keys[2]=deployer, keys[3]=unique` / `data[0]=classHash, data[1..n]=calldata, data[n+1]=salt`
  - **v0 (Cairo 0)**: `keys[0]=selector` / `data[0]=address, data[1]=deployer, data[2]=unique, data[3]=classHash, data[4]=calldata_len, data[5..]=calldata, data[last]=salt`
- Auto-detection heuristic is reliable because the two layouts have non-overlapping key counts (1 vs 4+). Edge cases where `len(keys) == 2` or `len(keys) == 3` don't correspond to any known UDC and should trigger the warning path.
- Config YAML shape:
  ```yaml
  indexer:
    udc_address: "0x041a78e7..."
    udc_event:
      version: auto          # auto | v0 | v1 (default: auto)
      # Fine-grained overrides (optional, for custom UDC variants):
      # address_key: 1       # index in keys[] for deployed address
      # class_hash_data: 0   # index in data[] for class hash
  ```
- The `discoveryState` struct should store the resolved format (either from config or first auto-detection) so that subsequent events don't re-run the heuristic. However, if `version: auto`, re-detect per event since a chain could theoretically have both UDC versions at different addresses (though `udc_address` makes this unlikely).
- The `starknet-devnet-rs` UDC uses the v1 layout at a different address (handled by the existing `udc_address` config from 3.21). The v0 layout is found on older `starknet-devnet` (Python) and some early appchains.
- Existing tests in `discover_test.go` construct events with the v1 layout — they should continue passing unchanged since auto-detect will resolve to v1 for those events. Add new test cases for v0 events alongside them.

### 3.23 Shared Tables for Discovered & Admin-Registered Contracts

**Description**: Extend shared table support beyond factory children to class-hash-discovered contracts and admin-registered contracts. Right now `shared_tables` only works for factory children — the `RegisterContract` path passes `nil` for `buildOpts`, and the discover path creates per-instance tables for every discovered contract. But the whole point of class-hash discovery is "these are all the same contract type" — they should naturally share tables. The existing `contract_address` column in log tables already supports per-contract filtering, so shared tables work without schema changes. This task reuses the exact shared-table machinery that factory children already use (`BuildOptions`, `sharedSchemas` caching, composite unique keys) and wires it into the two paths that currently lack it.

**Requirements**:
- [ ] Add `SharedTables bool` field (`yaml:"shared_tables" json:"shared_tables"`) to `DiscoverConfig` in `internal/config/config.go`
- [ ] Config validation: when `shared_tables: true` on a discover entry, require `abi` to be a named value (not `"fetch"` or a file path) so there is a clean table prefix — reject with a descriptive error otherwise
- [ ] In `handleDiscoveryEvent` / discovery registration path (`internal/engine/discover.go`): when `dc.SharedTables` is true, follow the `registerSharedChild` pattern — first discovered contract creates shared tables using `BuildOptions{SharedTable: true, FactoryName: dc.ABI}`, subsequent discoveries reuse cached shared schemas from `discoveryState`
- [ ] Add a `sharedSchemas map[string][]*types.TableSchema` field to `discoveryState` (keyed by class hash) to cache shared schemas across discoveries of the same class hash
- [ ] Set `ContractConfig.SharedTables = true` and `ContractConfig.FactoryName = dc.ABI` on discovered child configs when the discover entry has `shared_tables: true`, so schemas rebuild correctly on restart
- [ ] In `RegisterContract` (`internal/engine/engine.go`): when `cc.SharedTables` is true and `cc.FactoryName` is set, pass `&schema.BuildOptions{SharedTable: true, FactoryName: cc.FactoryName}` instead of `nil` — enabling admin-registered contracts to write to shared tables
- [ ] For admin registration with shared tables: if shared tables already exist in the store (created by a prior registration with the same `FactoryName`), skip `CreateTable` (use `MigrateTable` or no-op) — same idempotency as factory shared children
- [ ] Shared table naming follows existing convention: `{abi_name}_{event_name}` (e.g., `optiontoken_writertokendeployed`) — matching how factory shared tables use `{factory_name}_{event_name}`
- [ ] Unit tests in `discover_test.go`: two contracts discovered with same class hash + `shared_tables: true` produce one set of shared tables; verify `contract_address` column present; verify second discovery reuses cached schemas
- [ ] Unit tests for `RegisterContract`: verify that `SharedTables: true` + `FactoryName` on ContractConfig produces shared table schemas; verify `nil` buildOpts behavior unchanged when fields are unset

**Implementation Notes**:
- The discover registration path in `discover.go` currently calls `registerWithABI(ctx, cc, abi)` which always passes `nil` opts. With `shared_tables: true`, switch to the `registerSharedChild` pattern: check `discoveryState.sharedSchemas[classHash]` — if nil, build schemas with `BuildOptions` and create tables; if non-nil, reuse cached schemas and skip table creation.
- The ABI name (e.g., `OptionToken` from `abi: OptionToken`) serves as the `FactoryName` for `BuildOptions`. This is the table prefix — all discovered instances of that class hash write to `optiontoken_Swap`, `optiontoken_Transfer`, etc. This parallels how factory shared tables use the parent contract's `Name`.
- For `RegisterContract`, the fix is a 3-line change: check `cc.SharedTables && cc.FactoryName != ""`, and if so, construct `BuildOptions` instead of passing `nil`. The engine's setup phase at `engine.go:534` already handles this pattern for persisted contracts on restart.
- The `contract_address` column is automatically added by `BuildSchemas` when `SharedTable: true` (see `generator.go:99-102` adding `contract_name`). Log tables also include `contract_address` as a standard metadata column. Both enable per-contract filtering via the existing query system (`?contract_address=eq.0x123`).
- Deregistration of a discovered shared-table contract should NOT drop the shared tables (same behavior as factory children — see `engine.go:349-359` where `sch.SharedTable` skips `DropTable`). This already works because the `SharedTable` flag on `TableSchema` is set.
- Config YAML shape for discover:
  ```yaml
  discover:
    - class_hash: "0x47a9dc..."
      group: uponly
      abi: OptionToken
      shared_tables: true    # all discovered instances write to optiontoken_{event}
      events:
        - name: "*"
          table:
            type: log
  ```
- Config YAML shape for admin registration (POST /v1/contracts):
  ```json
  {
    "name": "NewOptionToken",
    "address": "0x123...",
    "abi": "fetch",
    "shared_tables": true,
    "factory_name": "OptionToken",
    "events": [{"name": "*", "table": {"type": "log"}}]
  }
  ```

### 3.24 Admin & Discovery View Functions

**Description**: View function polling currently only works for static contracts defined in `ibis.config.yaml`. Two dynamic contract registration paths — class-hash discovery (`discover:` config) and admin registration (`POST /v1/admin/contracts`) — do not wire up view functions, even though the underlying `ViewPoller` machinery is fully functional. This task closes the gap so that discovered and admin-registered contracts can poll view functions with the same semantics as statically configured contracts.

**Requirements**:
- [x] Add `Views []ViewConfig` field (`yaml:"views,omitempty" json:"views,omitempty"`) to `DiscoverConfig` in `internal/config/config.go`
- [x] Copy `dc.Views` to `cc.Views` when building `ContractConfig` in `handleDiscoveryEvent` (`internal/engine/discover.go:282-289`)
- [x] Add `validateViews(dc.Views, prefix)` call in `validateDiscover` (`internal/config/validate.go`) for each discover entry
- [x] Add `ViewPoller.AddContract(ctx context.Context, cs *contractState) ([]*types.TableSchema, error)` method that: builds entries via `buildEntry()`, appends to `vp.entries` (with mutex protection), spawns per-function polling goroutines, and returns view schemas
- [x] Protect `ViewPoller.entries` with a `sync.Mutex` so `AddContract` is safe to call concurrently with `Status()` and `Run()`
- [x] Call `ViewPoller.AddContract()` from `RegisterContract` (`engine.go`) when the contract has views — create view tables, add schemas to `contractState`, and notify the API server
- [x] Call `ViewPoller.AddContract()` from `registerWithABI` (`factory.go`) and `registerSharedDiscoveredChild` (`discover.go`) when the contract has views
- [x] Ensure `ViewPoller.SetOnEvent` is called during engine setup regardless of whether initial entries exist, so dynamically added views fire SSE callbacks
- [x] Unit tests: discovered contract with views triggers view polling; admin-registered contract with views triggers view polling; AddContract on a running poller spawns goroutines that actually poll
- [ ] Integration test: discover config with `views:` section — verify view tables are created and polled after UDC event is processed

**Implementation Notes**:
- The `ViewPoller.Run()` method currently returns immediately when `entries` is empty. Dynamic view addition via `AddContract` should not depend on `Run()` being active — `AddContract` spawns its own goroutines with the provided context, making it safe to add views whether or not the poller's `Run()` goroutine was started. This avoids needing to change `Run()` behavior.
- The engine always creates a `ViewPoller` in `setup()` (engine.go:584), so `e.poller` is never nil during runtime. But the `Run()` goroutine only starts if `HasEntries()` was true at launch (engine.go:468). For dynamically added views, `AddContract` manages its own goroutine lifecycle.
- The `onEvent` callback is currently set on the poller only when `HasEntries()` is true at startup (engine.go:469). Move the `SetOnEvent` call before the HasEntries check so dynamic additions inherit the callback.
- Three registration paths need the same view wiring: `RegisterContract` (admin API), `registerWithABI` (factory children + non-shared discovery), and `registerSharedDiscoveredChild` (shared-table discovery). Extract a helper like `e.startViewsForContract(ctx, cs)` to avoid duplicating the AddContract + CreateTable + schema registration logic in all three places.
- For shared-table discovered contracts with views, view table naming should follow the shared convention (`{abi_name}_{function_name}`) so all instances of the same class hash share view tables too. This parallels how event shared tables work.
- Config YAML shape for discover with views:
  ```yaml
  discover:
    - class_hash: "0x47a9dc..."
      abi: OptionToken
      shared_tables: true
      events:
        - name: "*"
          table:
            type: log
      views:
        - function: get_strike
          interval: 5m
          table:
            type: unique
            unique_key: _view_key
  ```

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
