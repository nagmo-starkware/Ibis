# Deployment Guide

This guide covers deploying ibis in development, staging, and production environments.

---

## Development Setup

For local development and testing, use the in-memory backend to avoid external dependencies:

```yaml
# ibis.config.yaml
network: mainnet
rpc: ${IBIS_RPC_URL}

database:
  backend: memory

api:
  host: 0.0.0.0
  port: 8080

contracts:
  - name: MyContract
    address: "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7"
    abi: fetch
    events:
      - name: "*"
        table:
          type: log
```

Then run:

```bash
ibis run
# or with make:
make run
```

The memory backend stores everything in-process — data is lost on restart. This is ideal for quick iteration and testing config changes.

For hot-reload during development (requires [air](https://github.com/air-verse/air)):

```bash
make dev
```

---

## Docker

### Building the Image

```bash
make docker-build
# equivalent to: docker build -t ibis:latest .
```

The Dockerfile uses a two-stage build:

1. **Builder stage** (`golang:1.25-alpine`): compiles a static binary with `CGO_ENABLED=0`
2. **Runtime stage** (`distroless/static-debian12:nonroot`): minimal image with no shell, runs as non-root

The final image is ~15MB and contains only the `ibis` binary.

### Running the Container

```bash
make docker-run
```

This mounts your local `ibis.config.yaml` read-only and loads environment variables from `.env`:

```bash
docker run --rm \
  --env-file .env \
  -v $(pwd)/ibis.config.yaml:/app/ibis.config.yaml:ro \
  -p 8080:8080 \
  ibis:latest
```

The container entrypoint is `ibis run`, so it starts indexing immediately.

### Environment Variables

Pass variables via `--env-file` or `-e` flags. The config file references them with `${VAR}` syntax:

```yaml
rpc: ${IBIS_RPC_URL}
database:
  postgres:
    password: ${IBIS_DB_PASSWORD}
```

---

## Docker Compose

The included `docker-compose.yaml` runs ibis with PostgreSQL:

```bash
make docker-compose-up    # start ibis + postgres
make docker-compose-down  # stop all services
```

### Included Services

| Service | Image | Port |
|---------|-------|------|
| `ibis` | Built from `Dockerfile` | `8080` (configurable via `IBIS_API_PORT`) |
| `postgres` | `postgres:17-alpine` | `5432` (configurable via `IBIS_DB_PORT`) |

### docker-compose.yaml

```yaml
services:
  ibis:
    build: .
    container_name: ibis
    ports:
      - "${IBIS_API_PORT:-8080}:8080"
    env_file:
      - path: .env
        required: false
    volumes:
      - ./ibis.config.yaml:/app/ibis.config.yaml:ro
      - ibis-data:/app/data
    depends_on:
      postgres:
        condition: service_healthy
    restart: unless-stopped

  postgres:
    image: postgres:17-alpine
    container_name: ibis-postgres
    ports:
      - "${IBIS_DB_PORT:-5432}:5432"
    environment:
      POSTGRES_USER: ${IBIS_DB_USER:-ibis}
      POSTGRES_PASSWORD: ${IBIS_DB_PASSWORD:-ibis}
      POSTGRES_DB: ${IBIS_DB_NAME:-ibis}
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${IBIS_DB_USER:-ibis}"]
      interval: 5s
      timeout: 5s
      retries: 5
    restart: unless-stopped

volumes:
  pgdata:
  ibis-data:
```

### Config Volume

Mount your `ibis.config.yaml` into the container at `/app/ibis.config.yaml`. The compose file does this by default. For custom config locations:

```yaml
volumes:
  - /path/to/my-config.yaml:/app/ibis.config.yaml:ro
```

### Environment File

Create a `.env` file (or copy from `.env.example`):

```bash
cp .env.example .env
```

```env
# .env
IBIS_RPC_URL=wss://starknet-mainnet.your-provider.com
IBIS_DB_USER=ibis
IBIS_DB_PASSWORD=your-secure-password
IBIS_DB_NAME=ibis
IBIS_DB_HOST=postgres       # service name in docker-compose
IBIS_DB_PORT=5432
IBIS_API_PORT=8080
```

When using docker-compose, set `IBIS_DB_HOST=postgres` (the service name) instead of `localhost`.

---

## PostgreSQL Setup

### Manual Setup (Without Docker Compose)

If you're running PostgreSQL separately, create the database and user:

```sql
CREATE USER ibis WITH PASSWORD 'your-secure-password';
CREATE DATABASE ibis OWNER ibis;
GRANT ALL PRIVILEGES ON DATABASE ibis TO ibis;
```

ibis creates and manages its own tables — no manual schema setup is needed.

### Connection Configuration

Configure the connection in `ibis.config.yaml`:

```yaml
database:
  backend: postgres
  postgres:
    host: ${IBIS_DB_HOST}
    port: 5432
    user: ${IBIS_DB_USER}
    password: ${IBIS_DB_PASSWORD}
    name: ${IBIS_DB_NAME}
```

Or hardcode values for simple setups:

```yaml
database:
  backend: postgres
  postgres:
    host: localhost
    port: 5432
    user: ibis
    password: ibis
    name: ibis
```

### PostgreSQL Permissions

ibis needs the following permissions:
- `CREATE TABLE` / `DROP TABLE` — for creating and removing event tables
- `INSERT` / `UPDATE` / `DELETE` — for writing event data and cursors
- `SELECT` — for the REST API query layer

Granting `ALL PRIVILEGES` on the database (as shown above) covers these. For tighter controls, grant `CREATE` on the schema and full DML on all tables.

---

## Production Checklist

### Database Backend

Always use PostgreSQL in production. BadgerDB is for single-machine/embedded use, and memory is for development and testing only.

```yaml
database:
  backend: postgres
```

### Start Block

Set `start_block` to the block number where your contract was deployed (or the earliest block you care about). Setting it to `0` starts from block 0 (genesis), which backfills the entire chain history. Omit `start_block` entirely to start from the latest block (you'll only see new events):

```yaml
indexer:
  start_block: 600000  # block when your contract was deployed
```

Per-contract `start_block` overrides the global setting:

```yaml
contracts:
  - name: OldContract
    start_block: 400000
    # ...
  - name: NewContract
    start_block: 800000
    # ...
```

### Admin API Key

Protect the admin endpoints (`/v1/admin/*`) with an API key:

```yaml
api:
  admin_key: ${IBIS_ADMIN_KEY}
```

When set, all admin requests must include the `X-Admin-Key` header. Without this, admin endpoints (contract registration/deregistration) are unauthenticated.

### CORS Configuration

Configure allowed origins for browser-based API consumers:

```yaml
api:
  cors_origins:
    - "https://your-app.example.com"
    - "https://admin.example.com"
```

Omit `cors_origins` or set to `["*"]` for open access (not recommended in production).

### Logging

ibis logs to stderr using Go's `slog` structured logger. In production, pipe logs to your monitoring stack:

```bash
ibis run 2>&1 | tee /var/log/ibis/indexer.log
```

Or in Docker, logs go to the container's stdout/stderr and are accessible via `docker logs ibis`.

---

## Environment Variables

All `${VAR}` patterns used in the example config:

| Variable | Description | Default |
|----------|-------------|---------|
| `IBIS_RPC_URL` | Starknet RPC endpoint (WSS or HTTP) | — (required) |
| `IBIS_DB_HOST` | PostgreSQL host | `localhost` |
| `IBIS_DB_PORT` | PostgreSQL port | `5432` |
| `IBIS_DB_USER` | PostgreSQL user | `ibis` |
| `IBIS_DB_PASSWORD` | PostgreSQL password | `ibis` |
| `IBIS_DB_NAME` | PostgreSQL database name | `ibis` |
| `IBIS_API_HOST` | API server bind address | `0.0.0.0` |
| `IBIS_API_PORT` | API server port (docker-compose host mapping) | `8080` |
| `IBIS_ADMIN_KEY` | Admin API authentication key | — (optional) |
| `IBIS_NETWORK` | Network name: `mainnet`, `sepolia` | `mainnet` |

### .env File

ibis does not read `.env` files directly — they are loaded by Docker (`--env-file`) or docker-compose (`env_file:`). For local development without Docker, export variables in your shell:

```bash
export IBIS_RPC_URL=wss://starknet-mainnet.your-provider.com
export IBIS_DB_HOST=localhost
export IBIS_DB_USER=ibis
export IBIS_DB_PASSWORD=ibis
export IBIS_DB_NAME=ibis
ibis run
```

---

## Monitoring

### Health Check

```
GET /v1/health
```

Returns `{"status": "ok"}` when the API server is running. Use this for load balancer health checks and container orchestration:

```yaml
# docker-compose healthcheck for ibis
healthcheck:
  test: ["CMD", "wget", "--spider", "-q", "http://localhost:8080/v1/health"]
  interval: 10s
  timeout: 5s
  retries: 3
```

### Sync Status

```
GET /v1/status
```

Returns the current sync progress for each contract:

```json
{
  "current_block": 750000,
  "contracts": [
    {
      "name": "MyContract",
      "address": "0x049d36...",
      "events": 3,
      "current_block": 750000
    }
  ]
}
```

The `current_block` field at the top level is the minimum cursor across all contracts — it represents the global sync position.

### What to Alert On

| Condition | How to Detect | Action |
|-----------|--------------|--------|
| **API down** | `/v1/health` returns non-200 or times out | Restart the ibis container |
| **Sync lag** | `/v1/status` `current_block` falls behind chain head | Check RPC connection, network issues, or backfill in progress |
| **Contract stuck** | One contract's `current_block` lags behind others | Check for RPC errors in logs, possible subscription drop |
| **Connection drops** | Repeated `websocket` or `connection refused` errors in logs | Verify RPC endpoint availability, check rate limits |
| **High memory** | Container memory exceeds baseline by 2x+ | Possible event backlog — check batch_size and pending_blocks settings |

---

## Scaling Considerations

### Architecture

ibis runs as a **single indexer instance** that writes to the database. This is by design — a single writer avoids cursor conflicts and reorg handling complexity.

For read scaling, multiple API readers can connect to the same PostgreSQL database. Run additional ibis instances with indexing disabled (or separate API-only services querying the same database).

### Backfill Duration

Backfill speed depends on:
- **Number of events**: contracts with high event volume take longer
- **Block range**: `start_block` to chain head
- **RPC rate limits**: most providers throttle `starknet_getEvents` calls
- **Batch size**: configurable via `indexer.batch_size` (default: 10 blocks per batch)

Typical estimates:
- A low-activity contract over 100k blocks: minutes
- A high-activity contract (e.g., ETH transfers) over 500k blocks: hours

Once caught up, ibis switches to real-time WebSocket streaming and stays current with minimal lag.

### Pending Blocks

Enable `pending_blocks: true` for near-instant event visibility (events appear before block confirmation). Disable for production use cases where you only want confirmed data:

```yaml
indexer:
  pending_blocks: false  # only confirmed blocks
```

---

## Backup and Recovery

### Cursor-Based Resume

ibis persists a cursor (block number) per contract in the database. On restart, it resumes from where it left off — no data reprocessing is needed for blocks already indexed.

This means:
- **Container restarts** are safe — ibis picks up automatically
- **Crashes** are safe — the cursor advances only after successful writes
- **Upgrades** are safe — stop ibis, update the binary/image, restart

### Database Backup

Standard PostgreSQL backup strategies apply:

```bash
# Logical backup
pg_dump -U ibis -d ibis > ibis_backup.sql

# Restore
psql -U ibis -d ibis < ibis_backup.sql
```

For continuous backup, use PostgreSQL's WAL archiving or your cloud provider's managed backup (e.g., AWS RDS automated snapshots).

### Disaster Recovery

If the database is lost entirely, ibis can re-index from scratch. Set `start_block` to the original value and restart — ibis will backfill all historical events. This is slow for high-volume contracts but produces an identical result.

---

## Example: Minimal Production Setup

1. **Create `.env`**:
   ```env
   IBIS_RPC_URL=wss://starknet-mainnet.your-provider.com
   IBIS_DB_PASSWORD=a-strong-random-password
   IBIS_ADMIN_KEY=your-admin-secret
   ```

2. **Create `ibis.config.yaml`**:
   ```yaml
   network: mainnet
   rpc: ${IBIS_RPC_URL}

   database:
     backend: postgres
     postgres:
       host: ${IBIS_DB_HOST:-postgres}
       port: 5432
       user: ${IBIS_DB_USER:-ibis}
       password: ${IBIS_DB_PASSWORD}
       name: ${IBIS_DB_NAME:-ibis}

   api:
     host: 0.0.0.0
     port: 8080
     admin_key: ${IBIS_ADMIN_KEY}
     cors_origins:
       - "https://your-app.example.com"

   indexer:
     start_block: 600000
     pending_blocks: false
     batch_size: 10

   contracts:
     - name: MyToken
       address: "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7"
       abi: fetch
       events:
         - name: "*"
           table:
             type: log
   ```

3. **Deploy**:
   ```bash
   docker compose up -d
   ```

4. **Verify**:
   ```bash
   curl http://localhost:8080/v1/health
   # {"status":"ok"}

   curl http://localhost:8080/v1/status
   # {"current_block":600042,"contracts":[...]}
   ```
