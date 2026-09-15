# CLAUDE.md

Project-specific context for Claude Code.

## Commands

| Command | Description |
|---------|-------------|
| `make build` | Build static binary to `./bin/ibis` |
| `make run` | Build and run the indexer |
| `make dev` | Run with hot reload (requires `air`) |
| `make test` | Run all tests |
| `make check` | Run fmt, vet, lint, test |
| `make fmt` | Format code |
| `make vet` | Run go vet |
| `make lint` | Run golangci-lint |
| `make docker-build` | Build Docker image |
| `make docker-compose-up` | Start ibis + postgres |
| `make docker-compose-down` | Stop all services |

## Project Structure

```
cmd/ibis/main.go          # CLI entry point (cobra root)
internal/
  abi/                     # ABI parsing, event decoding, selector computation
  api/                     # HTTP server, REST handlers, SSE streaming, event bus
  cli/                     # CLI commands: init, run, serve, query
  config/                  # YAML config loader, validation, ABI resolution
  engine/                  # Core indexing orchestrator, pending blocks, reorg handling
  provider/                # Starknet RPC/WS provider, event subscriber
  schema/                  # ABI-derived table schemas, Postgres/Badger generators
  store/                   # Store interface + backends (badger/, postgres/, memory/)
  types/                   # Shared type definitions
configs/                   # Example ibis.config.yaml files
docs/
  SPEC.md                  # Full architecture spec, data models, design decisions
  ROADMAP.md               # Development roadmap with phased action items
```

## Architecture

Ibis is an event-driven Starknet indexer. Key patterns:

- **Store interface** (`internal/store/store.go`) -- database abstraction with three backends: BadgerDB (embedded), PostgreSQL, in-memory
- **Operation pairs** -- every database write produces an `(add, revert)` pair for safe pending block and reorg handling
- **ABI-driven schemas** -- contract ABIs drive table creation, column types, REST endpoints, and query execution
- **Event subscription** -- uses `starknet_subscribeEvents` per contract via WSS, falls back to `starknet_getEvents` HTTP polling
- **Three table types** -- `log` (append-only), `unique` (last-write-wins by key), `aggregation` (auto-computed sums/counts)

## Tech Stack

- **Go 1.25+** with `net/http` (stdlib router), `spf13/cobra` (CLI)
- **starknet.go** (`NethermindEth/starknet.go`) -- Starknet RPC, WebSocket, selectors
- **BadgerDB v4** -- embedded KV store
- **PostgreSQL** via `pgx/v5` -- production database
- **YAML** via `gopkg.in/yaml.v3` -- config with `${ENV_VAR}` expansion

## Key Files

- `configs/ibis.config.yaml` -- example configuration
- `docs/SPEC.md` -- full specification and design decisions
- `docs/ROADMAP.md` -- development phases and task breakdown
