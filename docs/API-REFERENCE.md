# Ibis REST API Reference

Complete reference for all REST API endpoints exposed by the ibis indexer.

**Base URL**: `http://localhost:8080/v1` (default; configurable via [`api.host`](CONFIGURATION.md#apihost) and [`api.port`](CONFIGURATION.md#apiport))
**Content-Type**: All responses are `application/json` unless otherwise noted.
**CORS**: Configurable via `api.cors_origins` in [`ibis.config.yaml`](CONFIGURATION.md#apicors_origins). Defaults to `*`.

## Prerequisites

Before using the API, ibis must be installed and running with the API server enabled. See the [Getting Started guide](GETTING-STARTED.md) for installation and setup instructions. The default API address is `http://localhost:8080` — configurable via [`api.host`](CONFIGURATION.md#apihost) (default: `0.0.0.0`) and [`api.port`](CONFIGURATION.md#apiport) (default: `8080`). All examples in this document use the default.

---

## Table of Contents

- [System Endpoints](#system-endpoints)
- [Event Endpoints](#event-endpoints)
- [Factory Endpoints](#factory-endpoints)
- [Discovery Endpoints](#discovery-endpoints)
- [SSE Streaming](#sse-streaming)
- [Admin Endpoints](#admin-endpoints)
- [Query Parameters](#query-parameters)
- [Common Response Fields](#common-response-fields)
- [Error Responses](#error-responses)
- [Pagination](#pagination)
- [Agent Skills](#agent-skills)

---

## System Endpoints

### `GET /v1/health`

Health check endpoint. Returns immediately with no database calls.

**Response** `200 OK`:

```json
{
  "status": "ok"
}
```

**Example**:

```bash
curl http://localhost:8080/v1/health
```

---

### `GET /v1/status`

Returns the full indexer status: global cursor, per-contract progress, factory summaries, and view function status.

**Response** `200 OK`:

```json
{
  "current_block": 850000,
  "contracts": [
    {
      "name": "MyToken",
      "address": "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7",
      "events": 2,
      "current_block": 850000
    },
    {
      "name": "MyDEX",
      "address": "0x04270219d365d6b017231b52e92b3fb5d7c8378b05e9abc97724537a80e93b0f",
      "events": 3,
      "current_block": 849998
    }
  ],
  "factories": {
    "MyDEX": {
      "child_count": 142,
      "synced": 140,
      "backfilling": 2
    }
  },
  "views": {
    "MyToken": {
      "total_supply": {
        "last_call_block": 850000,
        "interval": 100
      }
    }
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `current_block` | `number` | Global cursor — minimum block number across all contracts |
| `contracts` | `array` | Per-contract status entries |
| `contracts[].name` | `string` | Contract name from config |
| `contracts[].address` | `string` | Starknet contract address |
| `contracts[].events` | `number` | Number of events being indexed |
| `contracts[].current_block` | `number` | Last processed block for this contract |
| `factories` | `object` | Factory summary (only present if factories are configured) |
| `factories.{name}.child_count` | `number` | Total discovered child contracts |
| `factories.{name}.synced` | `number` | Children caught up to global cursor |
| `factories.{name}.backfilling` | `number` | Children still backfilling |
| `views` | `object` | View function status (only present if views are configured) |

**Example**:

```bash
curl http://localhost:8080/v1/status
```

---

## Event Endpoints

All event endpoints use the pattern `/v1/{contract}/{event}` where `{contract}` is the contract name and `{event}` is the event name, both as defined in `ibis.config.yaml`. Names are case-insensitive.

### `GET /v1/{contract}/{event}`

List events from a log table with pagination, ordering, and filtering.

**Query Parameters**: See [Query Parameters](#query-parameters) for the full reference.

**Response** `200 OK`:

```json
{
  "data": [
    {
      "contract_address": "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7",
      "contract_name": "MyToken",
      "event_name": "Transfer",
      "block_number": 850000,
      "transaction_hash": "0x...",
      "log_index": 0,
      "timestamp": 1700000000,
      "status": "ACCEPTED_ON_L2",
      "from": "0x1234...",
      "to": "0x5678...",
      "value": "1000000000000000000"
    }
  ],
  "count": 1,
  "limit": 50,
  "offset": 0
}
```

| Field | Type | Description |
|-------|------|-------------|
| `data` | `array` | Array of event objects (see [Common Response Fields](#common-response-fields)) |
| `count` | `number` | Number of events in this response page |
| `limit` | `number` | Limit used for this query |
| `offset` | `number` | Offset used for this query |

**Examples**:

```bash
# List recent transfers (default: 50 most recent)
curl "http://localhost:8080/v1/MyToken/Transfer"

# With pagination
curl "http://localhost:8080/v1/MyToken/Transfer?limit=10&offset=20"

# Filter by sender
curl "http://localhost:8080/v1/MyToken/Transfer?from=eq.0x1234"

# Events after a specific block
curl "http://localhost:8080/v1/MyToken/Transfer?block_number=gte.800000"

# Ascending order by block number
curl "http://localhost:8080/v1/MyToken/Transfer?order=block_number.asc"
```

---

### `GET /v1/{contract}/{event}/latest`

Returns the single most recent event (highest block number, then highest log index).

**Response** `200 OK`:

```json
{
  "data": {
    "contract_address": "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7",
    "contract_name": "MyToken",
    "event_name": "Transfer",
    "block_number": 850000,
    "transaction_hash": "0x...",
    "log_index": 3,
    "timestamp": 1700000000,
    "status": "ACCEPTED_ON_L2",
    "from": "0x1234...",
    "to": "0x5678...",
    "value": "500000000000000000"
  }
}
```

**Response** `404 Not Found` (no events indexed yet):

```json
{
  "error": "no events found"
}
```

**Use cases**: Displaying the most recent activity, checking if a contract is active, getting the latest state.

**Example**:

```bash
curl http://localhost:8080/v1/MyToken/Transfer/latest
```

---

### `GET /v1/{contract}/{event}/count`

Returns the total count of events matching optional filters.

**Query Parameters**: Supports [filter parameters](#filter-operators) only (not `limit`, `offset`, or `order`). If `limit`, `offset`, or `order` are passed, they are silently ignored.

**Response** `200 OK`:

```json
{
  "count": 15230
}
```

**Examples**:

```bash
# Total transfer count
curl "http://localhost:8080/v1/MyToken/Transfer/count"

# Transfers from a specific address
curl "http://localhost:8080/v1/MyToken/Transfer/count?from=eq.0x1234"

# Events after block 800000
curl "http://localhost:8080/v1/MyToken/Transfer/count?block_number=gte.800000"
```

---

### `GET /v1/{contract}/{event}/unique`

Returns the latest entry per unique key from a **unique** table type. Only available for events configured with [`table_type: unique`](CONFIGURATION.md#eventstabletype) in `ibis.config.yaml`.

> **Note**: Calling `/unique` on a non-unique table (e.g., a `log` or `aggregation` table) returns `400 Bad Request` with a descriptive error message indicating the table's actual type.

**Query Parameters**: Same as the [list endpoint](#get-v1contractevent).

**Response** `200 OK`:

```json
{
  "data": [
    {
      "contract_address": "0x...",
      "contract_name": "MyToken",
      "event_name": "Balance",
      "block_number": 850000,
      "transaction_hash": "0x...",
      "log_index": 1,
      "timestamp": 1700000000,
      "status": "ACCEPTED_ON_L2",
      "account": "0x1234...",
      "balance": "5000000000000000000"
    }
  ],
  "count": 1,
  "limit": 50,
  "offset": 0
}
```

**Response** `400 Bad Request` (table type mismatch):

```json
{
  "error": "endpoint /unique is only available for unique table types; 'Transfer' is a log table"
}
```

**Example**:

```bash
# Get all unique balances
curl "http://localhost:8080/v1/MyToken/Balance/unique"

# Filter to a specific account
curl "http://localhost:8080/v1/MyToken/Balance/unique?account=eq.0x1234"
```

---

### `GET /v1/{contract}/{event}/aggregate`

Returns computed aggregate values for an **aggregation** table type. Only available for events configured with [`table_type: aggregation`](CONFIGURATION.md#eventstabletype) in `ibis.config.yaml`.

> **Note**: Calling `/aggregate` on a non-aggregation table (e.g., a `log` or `unique` table) returns `400 Bad Request` with a descriptive error message indicating the table's actual type.

**Response** `200 OK`:

```json
{
  "data": {
    "total_volume": "98234567890000000000000",
    "swap_count": 45123
  }
}
```

The keys in `data` correspond to the `column` names defined in the `aggregates` config. Available aggregate operations are `sum`, `count`, and `avg`.

**Response** `400 Bad Request` (table type mismatch):

```json
{
  "error": "endpoint /aggregate is only available for aggregation table types; 'Transfer' is a log table"
}
```

**Example**:

```bash
curl "http://localhost:8080/v1/MyDEX/Swap/aggregate"
```

---

## Factory Endpoints

Factory endpoints operate on contracts configured with a `factory` block in `ibis.config.yaml`. They list and count discovered child contracts.

### `GET /v1/{factory}/children`

Lists all discovered child contracts for a factory, including metadata from the factory event. Supports pagination, sorting, and filtering.

**Query Parameters**:

| Parameter | Default | Description |
|-----------|---------|-------------|
| `limit` | `50` | Maximum number of results per page (max: 500) |
| `offset` | `0` | Number of results to skip |
| `order` | `deployment_block.desc` | Sort field and direction (`field.asc` or `field.desc`) |
| `{field}` | — | [Filter operators](#filter-operators) on any metadata field (e.g., `?token0=eq.0x...`) |

**Sortable fields**: `name`, `deployment_block`, `current_block`, `status`, `events`, and any promoted metadata field.

**Response** `200 OK`:

```json
{
  "data": [
    {
      "name": "MyDEX_child_0x1234",
      "address": "0x1234...",
      "deployment_block": 800100,
      "current_block": 850000,
      "status": "synced",
      "events": 2,
      "token0": "0xabc...",
      "token1": "0xdef..."
    }
  ],
  "count": 1,
  "total": 142,
  "limit": 50,
  "offset": 0
}
```

| Field | Type | Description |
|-------|------|-------------|
| `data[].name` | `string` | Child contract name (auto-generated) |
| `data[].address` | `string` | Child contract address |
| `data[].deployment_block` | `number` | Block where the factory event was emitted |
| `data[].current_block` | `number` | Last processed block for this child |
| `data[].status` | `string` | Sync status |
| `data[].events` | `number` | Number of event types being indexed |
| `data[].{metadata}` | `any` | Additional fields from the factory event (promoted to top-level) |
| `count` | `number` | Number of results in the current page |
| `total` | `number` | Total number of matching results (before pagination) |
| `limit` | `number` | The limit used for this request |
| `offset` | `number` | The offset used for this request |

**Examples**:

```bash
# List all children (default: newest first, limit 50)
curl "http://localhost:8080/v1/MyDEX/children"

# Paginate: second page of 10
curl "http://localhost:8080/v1/MyDEX/children?limit=10&offset=10"

# Sort by name ascending
curl "http://localhost:8080/v1/MyDEX/children?order=name.asc"

# Filter by metadata field
curl "http://localhost:8080/v1/MyDEX/children?token0=eq.0xabc"

# Combined: filter + sort + paginate
curl "http://localhost:8080/v1/MyDEX/children?token1=eq.0xUSDC&order=deployment_block.asc&limit=20"
```

---

### `GET /v1/{factory}/children/count`

Returns the count of discovered child contracts, with optional metadata filtering.

**Query Parameters**: Same filter support as the children list endpoint.

**Response** `200 OK`:

```json
{
  "count": 142
}
```

**Examples**:

```bash
# Total child count
curl "http://localhost:8080/v1/MyDEX/children/count"

# Count children matching a filter
curl "http://localhost:8080/v1/MyDEX/children/count?token0=eq.0xabc"
```

---

## Discovery Endpoints

### `GET /v1/discover/{classHash}/contracts`

Returns contracts discovered via class hash watching for a specific class hash. Used when the indexer is configured with [`discover`](CONFIGURATION.md#discover) blocks to watch for deployments of known class hashes.

> **Note**: This endpoint returns a bare JSON array (not wrapped in a `data` field), unlike most other endpoints.

**Response** `200 OK`:

```json
[
  {
    "name": "discovered_0x1234",
    "address": "0x1234...",
    "discover_class_hash": "0xabcdef..."
  }
]
```

**Example**:

```bash
curl "http://localhost:8080/v1/discover/0xabcdef.../contracts"
```

---

## SSE Streaming

### `GET /v1/{contract}/{event}/stream`

Server-Sent Events (SSE) endpoint that streams new indexed events in real-time.

**Headers**:

| Header | Value | Description |
|--------|-------|-------------|
| `Content-Type` | `text/event-stream` | SSE content type (set by server) |
| `Cache-Control` | `no-cache` | Disables caching (set by server) |
| `Connection` | `keep-alive` | Persistent connection (set by server) |
| `Last-Event-ID` | `{block}:{logIndex}` | Send on reconnect to replay missed events |

**Query Parameters**: Supports [filter operators](#filter-operators) to filter the stream (e.g., `?from=eq.0x1234`). Filters are applied in-memory on the server — only `eq` and `neq` operators are supported for streaming filters.

**Event Format**:

```
id: 850000:3
data: {"contract_address":"0x...","contract_name":"MyToken","event_name":"Transfer","block_number":850000,"from":"0x1234...","to":"0x5678...","value":"1000000000000000000","status":"ACCEPTED_ON_L2"}

id: 850001:0
data: {"contract_address":"0x...","contract_name":"MyToken","event_name":"Transfer","block_number":850001,"from":"0x9999...","to":"0x1111...","value":"500000000000000000","status":"ACCEPTED_ON_L2"}
```

Each event has:
- **`id`**: Event ID in `{block_number}:{log_index}` format. Used for `Last-Event-ID` reconnection.
- **`data`**: JSON-encoded event object with all decoded fields.

**Reconnection**: When a client reconnects with the `Last-Event-ID` header, the server replays all events that were indexed after that ID. The replay queries the store for events with `block_number >= lastBlock` and filters out events at or before the exact `(block, logIndex)` pair.

**Examples**:

```bash
# Stream all Transfer events
curl -N "http://localhost:8080/v1/MyToken/Transfer/stream"

# Stream with filter
curl -N "http://localhost:8080/v1/MyToken/Transfer/stream?from=eq.0x1234"

# Reconnect from a specific event
curl -N -H "Last-Event-ID: 850000:3" \
  "http://localhost:8080/v1/MyToken/Transfer/stream"
```

---

## Admin Endpoints

Admin endpoints manage contracts dynamically at runtime. All admin endpoints require the `X-Admin-Key` header when [`api.admin_key`](CONFIGURATION.md#apiadmin_key) is set in `ibis.config.yaml`. If no admin key is configured, all requests are allowed.

**Authentication Header**:

```
X-Admin-Key: your-secret-key
```

### `POST /v1/admin/contracts`

Register a new contract for indexing at runtime.

**Request Body** (`application/json`):

```json
{
  "name": "NewToken",
  "address": "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7",
  "abi": "path/to/abi.json",
  "events": [
    {
      "name": "Transfer",
      "table": { "type": "log" }
    }
  ],
  "start_block": 850000
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Unique contract name |
| `address` | `string` | Yes | Starknet contract address |
| `abi` | `string` | No | Path or URL to ABI file (auto-fetched if omitted) |
| `events` | `array` | No | Event configurations (see note below) |
| `start_block` | `number` | No | Block to start indexing from |

> **Event config format**: The API accepts two JSON formats for event configurations. The **nested format** matches the YAML config structure: `{"name": "Transfer", "table": {"type": "log"}}`. A **flat shorthand** is also accepted for convenience: `{"name": "Transfer", "table_type": "log", "unique_key": "account"}`. When both are present, the nested `table` field takes precedence. The flat shorthand supports `table_type` and `unique_key`; for aggregation config, use the nested format.

**Response** `201 Created`:

```json
{
  "status": "registered",
  "name": "NewToken",
  "address": "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7"
}
```

**Example**:

```bash
# Using nested format (matches ibis.config.yaml structure)
curl -X POST http://localhost:8080/v1/admin/contracts \
  -H "Content-Type: application/json" \
  -H "X-Admin-Key: your-secret-key" \
  -d '{
    "name": "NewToken",
    "address": "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7",
    "events": [{"name": "Transfer", "table": {"type": "log"}}]
  }'

# Using flat shorthand
curl -X POST http://localhost:8080/v1/admin/contracts \
  -H "Content-Type: application/json" \
  -H "X-Admin-Key: your-secret-key" \
  -d '{
    "name": "NewToken",
    "address": "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7",
    "events": [{"name": "Transfer", "table_type": "log"}]
  }'
```

---

### `GET /v1/admin/contracts`

List all contracts currently being indexed (both config-defined and dynamically registered).

**Response** `200 OK`:

```json
{
  "contracts": [
    {
      "name": "MyToken",
      "address": "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7",
      "events": 1,
      "current_block": 850000,
      "status": "active",
      "dynamic": false
    },
    {
      "name": "NewToken",
      "address": "0x04270219d365d6b017231b52e92b3fb5d7c8378b05e9abc97724537a80e93b0f",
      "events": 2,
      "current_block": 849500,
      "status": "syncing",
      "dynamic": true,
      "factory_name": "MyFactory"
    }
  ],
  "count": 2
}
```

| Field | Type | Description |
|-------|------|-------------|
| `contracts` | `array` | List of all registered contracts |
| `contracts[].name` | `string` | Contract name from config or registration |
| `contracts[].address` | `string` | Starknet contract address |
| `contracts[].events` | `number` | Number of events being indexed for this contract |
| `contracts[].current_block` | `number` | Last processed block for this contract |
| `contracts[].start_block` | `number` | Block to start indexing from (omitted if 0) |
| `contracts[].status` | `string` | Current status: `active`, `syncing`, `backfilling`, or `error` |
| `contracts[].dynamic` | `boolean` | Whether the contract was dynamically registered (vs. config-defined) |
| `contracts[].factory_name` | `string` | Name of the parent factory (omitted if not a factory child) |
| `contracts[].factory_meta` | `object` | Metadata from factory discovery (omitted if not a factory child) |
| `contracts[].is_factory` | `boolean` | Whether this contract is a factory (omitted if `false`) |
| `count` | `number` | Total number of registered contracts |

**Example**:

```bash
curl http://localhost:8080/v1/admin/contracts \
  -H "X-Admin-Key: your-secret-key"
```

---

### `PUT /v1/admin/contracts/{name}`

Update the configuration of an existing contract. Internally, this deregisters the old contract and re-registers it with the new configuration — either step can fail independently.

**Request Body** (`application/json`): Same structure as the register endpoint. All required fields (`name`, `address`) must be provided since the update performs a full deregister/re-register cycle — partial updates with only the changed fields are not supported.

**Response** `200 OK`:

```json
{
  "status": "updated",
  "name": "NewToken"
}
```

**Response** `400 Bad Request` — invalid or malformed request body:

```json
{
  "error": "invalid request body: unexpected EOF"
}
```

**Response** `404 Not Found` — contract not registered:

```json
{
  "error": "contract not found: UnknownContract"
}
```

**Response** `500 Internal Server Error` — update failed during deregistration or re-registration (e.g., ABI resolution failure):

```json
{
  "error": "update failed: deregistering old contract: ..."
}
```

**Response** `503 Service Unavailable` — dynamic contract management is not available (engine not running):

```json
{
  "error": "dynamic registration not available"
}
```

**Example**:

```bash
curl -X PUT http://localhost:8080/v1/admin/contracts/NewToken \
  -H "Content-Type: application/json" \
  -H "X-Admin-Key: your-secret-key" \
  -d '{
    "name": "NewToken",
    "address": "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7",
    "events": [{"name": "Transfer", "table": {"type": "log"}}],
    "start_block": 860000
  }'
```

---

### `DELETE /v1/admin/contracts/{name}`

Deregister a contract and stop indexing it.

**Query Parameters**:

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `drop_tables` | `boolean` | `false` | If `true`, drops all database tables for this contract |

**Response** `200 OK`:

```json
{
  "status": "deregistered",
  "name": "NewToken",
  "drop_tables": false
}
```

**Response** `500 Internal Server Error` — contract not registered:

```json
{
  "error": "deregistration failed: contract \"NewToken\" not found"
}
```

**Examples**:

```bash
# Deregister but keep data
curl -X DELETE http://localhost:8080/v1/admin/contracts/NewToken \
  -H "X-Admin-Key: your-secret-key"

# Deregister and drop tables
curl -X DELETE "http://localhost:8080/v1/admin/contracts/NewToken?drop_tables=true" \
  -H "X-Admin-Key: your-secret-key"
```

---

## Query Parameters

### Pagination and Ordering

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `limit` | `integer` | `50` | Number of results per page. Min: `1`, Max: `500`. Values above 500 are silently capped to 500 |
| `offset` | `integer` | `0` | Number of results to skip |
| `order` | `string` | `block_number.desc` | Sort order in `{field}.{direction}` format. Direction is `asc` or `desc` |

### Filter Operators

Filters use the format `?{field}={operator}.{value}`. When no operator prefix is provided, `eq` is assumed.

| Operator | Description | Example |
|----------|-------------|---------|
| `eq` | Equal to | `?from=eq.0x1234` or `?from=0x1234` |
| `neq` | Not equal to | `?status=neq.ACCEPTED_ON_L1` |
| `gt` | Greater than | `?block_number=gt.800000` |
| `gte` | Greater than or equal | `?block_number=gte.800000` |
| `lt` | Less than | `?amount=lt.1000000` |
| `lte` | Less than or equal | `?timestamp=lte.1700000000` |

**Multiple filters** can be combined by adding multiple query parameters. All filters are ANDed together:

```bash
# Transfers from 0x1234 after block 800000
curl "http://localhost:8080/v1/MyToken/Transfer?from=eq.0x1234&block_number=gte.800000"
```

**Shorthand**: The `eq.` prefix can be omitted for equality filters:

```bash
# These are equivalent
curl "http://localhost:8080/v1/MyToken/Transfer?from=eq.0x1234"
curl "http://localhost:8080/v1/MyToken/Transfer?from=0x1234"
```

> **Note**: If the value contains a dot (`.`) separator, the prefix **must** be one of the operators listed above. An unrecognized prefix returns `400 Bad Request` with a descriptive error (e.g., `?from=invalid.0x1234` → `"invalid filter operator 'invalid' for field 'from'; valid operators: eq, neq, gt, gte, lt, lte"`). Values without a dot separator default to `eq` (e.g., `?from=0x1234`).

---

## Common Response Fields

Every indexed event contains these system fields alongside decoded event-specific fields:

| Field | Type | Description |
|-------|------|-------------|
| `contract_address` | `string` | Starknet address of the emitting contract |
| `contract_name` | `string` | Contract name from config |
| `event_name` | `string` | Name of the event (e.g., `Transfer`) |
| `block_number` | `number` | Block number where the event was emitted |
| `transaction_hash` | `string` | Hash of the transaction |
| `log_index` | `number` | Position of the event within the block |
| `timestamp` | `number` | Block timestamp (Unix seconds) |
| `status` | `string` | Block finality status: `ACCEPTED_ON_L2`, `ACCEPTED_ON_L1`, or `PRE_CONFIRMED` |

Additionally, each event has its **decoded fields** as defined by the contract ABI. For example, an ERC-20 `Transfer` event would include `from`, `to`, and `value` fields.

---

## Error Responses

All errors follow a consistent format:

```json
{
  "error": "description of what went wrong"
}
```

### Common Status Codes

| Status | Meaning | When |
|--------|---------|------|
| `200` | OK | Successful request |
| `201` | Created | Contract successfully registered |
| `400` | Bad Request | Invalid query parameter (e.g., unrecognized filter operator), invalid request body on admin endpoints, malformed JSON |
| `401` | Unauthorized | Missing or invalid `X-Admin-Key` header |
| `404` | Not Found | Unknown contract/event combination, no events found for `/latest`, unknown factory |
| `500` | Internal Server Error | Database query failure or internal error |
| `503` | Service Unavailable | Returned when a required subsystem is not running (see details below) |

#### 503 Service Unavailable Details

The `503` response means the endpoint depends on a subsystem that isn't active:

- **Admin endpoints** (`/v1/admin/contracts/*`): Return `"dynamic registration not available"` when the indexing engine is not running. The engine is started by `ibis run` and enables dynamic contract registration, updates, and deregistration.
- **Factory & Discover endpoints** (`/v1/{factory}/children`, `/v1/discover/{classHash}/contracts`): Return `"engine not available"` for the same reason — the engine tracks factory children and discovered contracts at runtime.
- **SSE streaming** (`/v1/{contract}/{event}/stream`): Returns `"event streaming not available"` when the event bus is not initialized. The event bus is created by `ibis run` to relay live indexer events to SSE clients.

In the standard CLI, `ibis run` starts both the engine and event bus, so these 503 responses should not occur during normal operation. They act as guards for programmatic use of ibis as a library (e.g., creating an API server without an engine).

---

## Pagination

### Offset-Based Pagination

Use `limit` and `offset` for simple pagination:

```bash
# Page 1
curl "http://localhost:8080/v1/MyToken/Transfer?limit=20&offset=0"

# Page 2
curl "http://localhost:8080/v1/MyToken/Transfer?limit=20&offset=20"

# Page 3
curl "http://localhost:8080/v1/MyToken/Transfer?limit=20&offset=40"
```

### Cursor-Based Pagination

For large datasets, use `block_number` filters for more efficient cursor-based pagination:

```bash
# First page
curl "http://localhost:8080/v1/MyToken/Transfer?limit=100&order=block_number.asc"
# → Note the last event's block_number (e.g., 800100)

# Next page: events after block 800100
curl "http://localhost:8080/v1/MyToken/Transfer?limit=100&order=block_number.asc&block_number=gt.800100"
```

**Best practices for large datasets**:
- Prefer cursor-based pagination over large offsets (offset-based pagination gets slower as offset increases)
- Use ascending order (`order=block_number.asc`) for cursor-based pagination
- Use the `count` endpoint to estimate total results before paginating

---

## Agent Skills

Ibis ships with agent skills that make it easier to interact with the API:

- **`ibis-query`** — Translates natural language questions into REST API calls or CLI queries. Instead of constructing URLs manually, ask questions like "show me the top 10 transfers" or "how many swaps happened today?" and the skill builds the right API call.

- **`ibis-admin`** — Manages contracts on a running ibis instance via the admin endpoints. Register, deregister, update, and inspect contracts using natural language commands.
