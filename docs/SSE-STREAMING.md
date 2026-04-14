# Real-Time Streaming (SSE)

Ibis provides real-time event delivery via Server-Sent Events (SSE). As the indexer processes new blocks, events are pushed to connected clients over a persistent HTTP connection. SSE is a simple, browser-native protocol that works over standard HTTP — no WebSocket upgrade required.

---

## Table of Contents

- [Why SSE?](#why-sse)
- [Endpoint](#endpoint)
- [Event Format](#event-format)
- [Reconnection & Replay](#reconnection--replay)
- [Filtering](#filtering)
- [Client Examples](#client-examples)
  - [curl](#curl)
  - [JavaScript (EventSource)](#javascript-eventsource)
  - [Go](#go)
- [Factory Streaming](#factory-streaming)
- [Best Practices](#best-practices)

---

## Why SSE?

Ibis uses SSE instead of WebSocket for real-time event delivery because the communication is **one-directional**: the server pushes events to clients, and clients don't need to send data back. SSE offers several advantages for this use case:

- **Built into browsers** — the `EventSource` API is native, no library needed
- **Automatic reconnection** — browsers reconnect automatically with `Last-Event-ID`
- **Standard HTTP** — works through proxies, load balancers, and CDNs without special configuration
- **Simple protocol** — plain text over HTTP, easy to debug with `curl`

---

## Endpoint

```
GET /v1/{contract}/{event}/stream
```

| Detail | Value |
|--------|-------|
| Content-Type | `text/event-stream` |
| Cache-Control | `no-cache` |
| Connection | `keep-alive` |

The `{contract}` and `{event}` path parameters match the contract name and event name from your `ibis.config.yaml`.

**Example**: If your config defines a contract named `MyToken` with a `Transfer` event:

```
GET /v1/MyToken/Transfer/stream
```

---

## Event Format

Each SSE event contains two fields:

```
id: {block_number}:{log_index}
data: {json}

```

- **`id`** — uniquely identifies the event by its position on-chain. Used for reconnection replay.
- **`data`** — JSON-encoded object with all decoded event fields plus metadata.

**Example output**:

```
id: 850000:3
data: {"block_number":850000,"log_index":3,"contract_address":"0x049d36...","from":"0x1234...","to":"0x5678...","amount":"1000000000000000000"}

id: 850001:0
data: {"block_number":850001,"log_index":0,"contract_address":"0x049d36...","from":"0x9999...","to":"0x1111...","amount":"500000000000000000"}

```

> **Note**: Each event ends with two newlines (`\n\n`). This is part of the SSE protocol — it signals the end of an event.

---

## Reconnection & Replay

SSE supports **gap-free replay** via the `Last-Event-ID` header. When a client disconnects and reconnects, it can send the ID of the last event it received. Ibis replays all events that were indexed after that point, ensuring no events are missed.

**How it works**:

1. Client connects and receives events, each with an `id` field
2. Client stores the most recent `id` (browsers do this automatically)
3. Connection drops (network issue, server restart, etc.)
4. Client reconnects with the `Last-Event-ID` header set to the last received ID
5. Ibis queries the store for events with `block_number >= lastBlock`
6. Events at or before the exact `(block, logIndex)` pair are filtered out
7. Remaining events are replayed in order, then live streaming resumes

```
Client                              Ibis
  |                                   |
  |--- GET /v1/MyToken/Transfer/stream -->
  |                                   |
  |<-- id: 850000:3, data: {...} -----|
  |<-- id: 850001:0, data: {...} -----|
  |                                   |
  |    ~~~ connection drops ~~~       |
  |                                   |
  |--- GET (Last-Event-ID: 850001:0) -->
  |                                   |
  |<-- id: 850001:1, data: {...} -----| (replayed)
  |<-- id: 850002:0, data: {...} -----| (replayed)
  |<-- id: 850002:1, data: {...} -----| (live)
  |                                   |
```

Browsers handle this automatically — the `EventSource` API stores the last event ID and resends it on reconnection. For non-browser clients, you must manage this yourself.

---

## Filtering

The stream endpoint supports the same **Supabase-style filter operators** as the REST API. Filters are passed as query parameters and applied server-side — only matching events are sent to the client.

**Format**: `?field=operator.value`

| Operator | Description | Example |
|----------|-------------|---------|
| `eq` | Equals | `?from=eq.0x1234` |
| `neq` | Not equals | `?status=neq.REVERTED` |

> **Note**: Only `eq` and `neq` operators are supported for live streaming filters. The full set of operators (`gt`, `gte`, `lt`, `lte`) is supported on replay queries and REST endpoints.

**Shorthand**: If no operator prefix is given, `eq` is assumed. These are equivalent:

```
?from=eq.0x1234
?from=0x1234
```

**Multiple filters**: All filters are combined with AND logic.

```
GET /v1/MyToken/Transfer/stream?from=eq.0x1234&to=eq.0x5678
```

This streams only Transfer events where `from` is `0x1234` **and** `to` is `0x5678`.

---

## Client Examples

### curl

The simplest way to test SSE streaming. Use the `-N` flag to disable output buffering:

```bash
# Stream all Transfer events
curl -N "http://localhost:8080/v1/MyToken/Transfer/stream"

# Stream with a filter
curl -N "http://localhost:8080/v1/MyToken/Transfer/stream?from=eq.0x1234"

# Reconnect from a specific event (replay missed events)
curl -N -H "Last-Event-ID: 850000:3" \
  "http://localhost:8080/v1/MyToken/Transfer/stream"
```

### JavaScript (EventSource)

The browser-native `EventSource` API handles connection management and automatic reconnection:

```javascript
const url = "http://localhost:8080/v1/MyToken/Transfer/stream";
const source = new EventSource(url);

source.onmessage = (event) => {
  const data = JSON.parse(event.data);
  console.log(`Block ${data.block_number}: ${data.from} -> ${data.to} (${data.amount})`);
};

source.onerror = (error) => {
  // EventSource automatically reconnects with Last-Event-ID.
  // This handler fires on every reconnection attempt.
  console.warn("SSE connection error, reconnecting...");
};
```

**With filters**:

```javascript
const url = "http://localhost:8080/v1/MyToken/Transfer/stream?from=eq.0x1234";
const source = new EventSource(url);

source.onmessage = (event) => {
  const transfer = JSON.parse(event.data);
  console.log("Filtered transfer:", transfer);
};
```

**React example** — using SSE in a component:

```jsx
import { useEffect, useState } from "react";

function TransferFeed({ contract, event }) {
  const [transfers, setTransfers] = useState([]);

  useEffect(() => {
    const source = new EventSource(
      `http://localhost:8080/v1/${contract}/${event}/stream`
    );

    source.onmessage = (e) => {
      const data = JSON.parse(e.data);
      setTransfers((prev) => [data, ...prev].slice(0, 100)); // Keep last 100
    };

    return () => source.close(); // Cleanup on unmount
  }, [contract, event]);

  return (
    <ul>
      {transfers.map((t) => (
        <li key={`${t.block_number}:${t.log_index}`}>
          {t.from} → {t.to}: {t.amount}
        </li>
      ))}
    </ul>
  );
}
```

> **Tip**: The `EventSource` API automatically sends `Last-Event-ID` on reconnection. If your ibis instance restarts, the browser will reconnect and receive all events it missed — no extra code needed.

### Go

For backend services consuming ibis events:

```go
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

func main() {
	url := "http://localhost:8080/v1/MyToken/Transfer/stream"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Fatal(err)
	}

	// Set Last-Event-ID to resume from a previous position.
	// req.Header.Set("Last-Event-ID", "850000:3")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var lastID string

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "id: ") {
			lastID = strings.TrimPrefix(line, "id: ")
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")

			var event map[string]any
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				log.Printf("parse error: %v", err)
				continue
			}

			fmt.Printf("[%s] %v -> %v: %v\n",
				lastID, event["from"], event["to"], event["amount"])
		}
	}

	if err := scanner.Err(); err != nil {
		log.Fatal(err)
	}
}
```

> **Tip**: In production, wrap the connection in a retry loop. On disconnect, reconnect with `Last-Event-ID` set to the last received ID.

---

## Factory Streaming

When using ibis's [factory pattern](CONFIGURATION.md) with `shared_tables: true`, all child contracts' events are written to the same table. The SSE stream endpoint works the same way — you stream from the shared table and receive events from all child contracts.

**Streaming all events from a shared table**:

```bash
curl -N "http://localhost:8080/v1/MyDEX/Swap/stream"
```

This streams Swap events from **all** child contracts discovered by the `MyDEX` factory.

**Filtering by child contract**:

Use the `contract_address` filter to stream events from a specific child:

```bash
# Only stream Swap events from one specific pool
curl -N "http://localhost:8080/v1/MyDEX/Swap/stream?contract_address=eq.0xabc123..."
```

**Filtering by factory metadata**:

If the factory event includes metadata fields (e.g., `token0`, `token1`), these are included in the event data and can be filtered:

```bash
# Stream swaps for pools involving a specific token
curl -N "http://localhost:8080/v1/MyDEX/Swap/stream?token0=eq.0xeth..."
```

---

## Best Practices

### Connection Management

- **Always close connections when done**. In JavaScript, call `source.close()` on component unmount or page unload. Abandoned connections waste server resources.
- **Let the browser handle reconnection**. The `EventSource` API reconnects automatically with `Last-Event-ID`. Don't implement your own retry logic on top of it.
- **For non-browser clients**, implement a reconnection loop with exponential backoff. Store the last received event ID and send it as `Last-Event-ID` on reconnect.

### Error Handling

- **HTTP errors** (404, 503) are returned as standard JSON error responses before the SSE stream starts. Check the response status before parsing SSE events.
- **503 Service Unavailable** means the event bus is not configured — streaming is not available for this ibis instance.
- **Connection drops** are normal. Network interruptions, server restarts, and deployments all cause disconnects. Design your client to handle reconnection gracefully.

### Backpressure & Slow Clients

Ibis uses a **64-event subscriber buffer** per connected client. If a client falls behind (e.g., slow network, heavy processing), the buffer fills up and **new events are dropped** for that client. This is by design — it protects the indexer from being slowed down by slow consumers.

To avoid missing events due to buffer overflow:

- **Process events quickly**. Offload heavy work to a background queue instead of blocking the event handler.
- **Use `Last-Event-ID`** on reconnect. Even if some live events were dropped, the replay mechanism queries the store and delivers them on the next connection.
- **Monitor for gaps**. Track `block_number` and `log_index` in your client. If you detect a gap, reconnect with `Last-Event-ID` to trigger a replay.

### SSE vs Polling

| Use SSE when... | Use polling when... |
|-----------------|---------------------|
| You need real-time updates | You need a one-time data snapshot |
| Building live dashboards or feeds | Running periodic batch jobs |
| Sub-second latency matters | Latency of seconds/minutes is acceptable |
| Clients maintain long-lived connections | Clients are short-lived (serverless, cron) |

> **Tip**: You can combine both approaches. Use the [REST API](API-REFERENCE.md) to load initial data, then switch to SSE for real-time updates.

---

*See also: [API Reference](API-REFERENCE.md) | [Getting Started](GETTING-STARTED.md) | [Configuration](CONFIGURATION.md)*
