# AIFStudio

**AIFStudio** is a self-hosted **Interactive Fiction (IF) platform** that lets users
discover published IF titles via IFDB, play Z-machine, Glulx, and TADS story files in
the browser through a lightweight HTTP terminal UI, author new interactive fiction in
Inform 7 with on-server builds, and resume any game from their personal history.

> **Authoritative spec:** [`ARCHITECTURE.md`](ARCHITECTURE.md) is the single source of
> truth for design, data models, endpoints, storage, and infrastructure. Read it before
> writing code.

---

## Key Features

- **Discover** — browse IFDB's catalogue of published IF titles from inside the app.
  Titles in unsupported formats are filtered out server-side using IFDB's `devsys` metadata.
- **Play in the browser** — Z-machine (`.z3`–`.z8`, `.zblorb`), Glulx (`.ulx`, `.gblorb`),
  and TADS 2/3 (`.gam`, `.t3`), driven by a per-session interpreter subprocess
  (`dfrotz`, `glulxe`, `frob`).
- **Durable sessions** — every run is backed by a save file plus a transcript on local
  storage, with metadata in SQLite. Sessions survive container restarts and resume in
  < 200 ms (p50) plus network round trip.
- **Author in Inform 7** — authenticated users can create projects, compile stories
  via the bundled Inform 7 CLI, and immediately play their own builds.
- **Voice I/O** — optional browser-side Web Speech API integration: TTS reads
  interpreter output, STT captures commands hands-free. Off by default; toggles
  persist in `localStorage`.
- **Auto-save every turn** — output coalescing guarantees at most one in-flight turn
  lost on hard kill; graceful SIGTERM drains all sessions within the configured grace period.
- **Self-contained binary** — all HTML, CSS, and JS are embedded via Go's `embed`
  package. No separate frontend build step.

---

## Architecture Overview

```
           ┌──────────────────────────────┐
 Browser ──┤  Docker container: aifstudio │── SQLite   (users, runs, projects, builds)
   │       │  Go 1.23 + net/http          │── Local FS (binaries, saves, transcripts)
   │       │  dfrotz / glulxe / frob      │── IFDB     (public catalogue, read-only)
   │       │  inform7 (authoring)         │
   │       └──────────────────────────────┘
   │              ▲         │
   │  POST+wait   │         │ subprocess stdin/stdout (per-session, in-memory)
   └──────────────┘         └─── dfrotz / glulxe / frob / inform7 CLI
```

- **Transport:** POST+wait HTTP — each command is one HTTP round trip
  (`POST /api/runs/{runId}/command`); the interpreter lives in memory between requests.
- **Compute:** Single Docker container, managed by Docker Compose. Non-root user (`app`, UID 999).
- **Container images:** Two-image pattern — a pinned base image
  (`vpoluyaktov/aifstudio-base`) carries the heavy IF toolchain (frotz, glulxe, frob,
  inform7); the app image layers only the compiled Go binary on top, keeping CI builds
  under two minutes.
- **Persistence layers:**
  - **SQLite** — users, sessions (auth), runs, projects, builds. WAL mode, stored at
    `DB_PATH` (default `/app/data/db/aifstudio.db`).
  - **Local filesystem** — story files, save files, transcripts, compiled builds, under
    `STORAGE_PATH` (default `/app/data/storage`).
- **Identity:** Local session auth — bcrypt password hashing (cost 12), 32-byte random
  session tokens stored in SQLite, delivered via `aifstudio_session` cookie
  (HttpOnly, SameSite=Strict, 30-day Max-Age by default).
- **Routing:** Go 1.22+ `ServeMux` pattern matching. The root route is `GET /{$}`
  (exact match) so it does not swallow 405 Method Not Allowed from API routes.

See [`ARCHITECTURE.md`](ARCHITECTURE.md) for the complete spec.

---

## Repository Layout

```
aifstudio/
├── VERSION                    # Base version (MAJOR.MINOR); CI appends commit count
├── ARCHITECTURE.md            # Authoritative architecture spec
├── README.md                  # This file
├── docker-compose.yml         # Production deployment (single service)
├── .env.example               # Configuration template — copy to .env
├── docker/
│   └── base.Dockerfile        # IF toolchain base image (frotz, glulxe, frob, inform7)
├── service/                   # Go application source
│   ├── main.go
│   ├── go.mod                 # module aifstudio, go 1.23
│   ├── Dockerfile             # FROM base image + Go binary
│   └── internal/
│       ├── config/            # Env var loading
│       ├── server/            # HTTP handlers, routing, session middleware
│       ├── auth/              # Session auth (bcrypt + SQLite sessions)
│       ├── ifdb/              # IFDB catalogue client + devsys filtering
│       ├── runner/            # Interpreter bridge, save/restore FSM, session lifecycle
│       ├── build/             # Inform 7 compile pipeline
│       ├── store/             # SQLite + local filesystem store interface & impl
│       └── templates/         # Embedded HTML templates (index, play, history, author)
└── .github/workflows/
    ├── main.yml               # lint → test → docker build → push to Docker Hub
    └── base-image.yml         # Builds/publishes docker/base.Dockerfile on demand
```

---

## Quick Start

### Prerequisites

- Docker and Docker Compose

### Running with Docker Compose

```bash
# 1. Clone the repo
git clone https://github.com/vpoluyaktov/aifstudio.git
cd aifstudio

# 2. Create your config file
cp .env.example .env
# Edit .env — set OPENAI_API_KEY if you want AI features

# 3. Create data directories and set permissions
mkdir -p data/db data/storage
sudo chown -R 999:999 data/

# 4. Start the service
docker compose up -d

# 5. Open http://localhost:9901
```

The service pulls `vpoluyaktov/aifstudio:latest` from Docker Hub automatically.

### First-time setup

Navigate to http://localhost:9901/register to create your account, then sign in at
http://localhost:9901/login.

---

## Configuration

All configuration is via environment variables, loaded from `.env` at startup.
Copy `.env.example` to `.env` and adjust as needed.

| Variable | Purpose | Default |
|----------|---------|---------|
| `OPENAI_API_KEY` | Required for AI features | — |
| `PORT` | HTTP listen port inside the container | `8080` |
| `DB_PATH` | SQLite database file path | `/app/data/db/aifstudio.db` |
| `STORAGE_PATH` | Local blob storage root | `/app/data/storage` |
| `SESSION_MAX_AGE` | Auth session lifetime | `720h` (30 days) |
| `ENVIRONMENT` | Label shown in footer (`local`, `production`, …) | `local` |
| `SHUTDOWN_DRAIN_TIMEOUT` | SIGTERM drain budget before hard stop | `8s` |
| `MAX_STORY_BYTES` | Upload ceiling for story files | `16 MiB` |

See `.env.example` for the full list.

---

## Deployment

Deployments are automated via GitHub Actions — push to `main` triggers lint → test →
Docker build → push to Docker Hub (`vpoluyaktov/aifstudio`).

### Pipeline stages (`.github/workflows/main.yml`)

1. **lint-and-test** — `go vet`, `golangci-lint`, `go test -race ./...`
2. **docker-build-and-push** *(main branch and `v*` tags only)*
   - Computes image version: `MAJOR.MINOR.<commit-count>` on main; tag name on `v*` push
   - Builds `service/Dockerfile` on top of `vpoluyaktov/aifstudio-base`
   - Pushes `vpoluyaktov/aifstudio:latest` and `vpoluyaktov/aifstudio:<version>`
   - `APP_VERSION` is baked into the image via `ARG`/`ENV` — no `.env` override needed

### Pulling a new release on your host

```bash
docker compose pull
docker compose up -d
```

### Base image pipeline (`.github/workflows/base-image.yml`)

Rebuilds the IF toolchain base image and pushes to Docker Hub. Run this manually
(workflow_dispatch) when the toolchain changes (new frobtads, Inform 7 release, etc.).
After it completes, update the `FROM` tag in `service/Dockerfile`.

---

## Local Development

### Running the server directly

```bash
cd service
go mod download
go test ./...
go vet ./...

# Requires SQLite data dirs to exist:
mkdir -p /tmp/aifstudio/db /tmp/aifstudio/storage
DB_PATH=/tmp/aifstudio/db/aifstudio.db \
STORAGE_PATH=/tmp/aifstudio/storage \
go run .
```

The server listens on `$PORT` (default `8080`). Open http://localhost:8080/.

### Running tests

```bash
cd service
go test -race ./...
go vet ./...
golangci-lint run ./...
```

Unit tests use the `MockStore` implementation of the `Store` interface; no real database
or filesystem access is required.

### Building the container locally

```bash
docker build -t aifstudio:dev ./service
docker run --rm -p 8080:8080 \
  -v $(pwd)/data/db:/app/data/db \
  -v $(pwd)/data/storage:/app/data/storage \
  aifstudio:dev
```

---

## GitHub Secrets

| Secret | Purpose |
|--------|---------|
| `DOCKERHUB_USERNAME` | Docker Hub account name for image push |
| `DOCKERHUB_TOKEN`    | Docker Hub access token for image push |

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| `POST /api/runs/{id}/command` returns 409 `busy` | Another request holds the per-run mutex | Retry after current command completes |
| `POST /api/runs/{id}/command` returns 423 `locked` on resume | Run is mid-restore | Frontend auto-retries; see ARCHITECTURE.md |
| SQLite "unable to open database" on startup | Data directory owned by root, not UID 999 | `sudo chown -R 999:999 data/` |
| `inform7 CLI not installed` at compile time | Base image does not include Inform 7 at that path | Check `service/Dockerfile` FROM tag matches current base |
| 405 swallowed as 200 on API routes | Root route registered as `GET /` instead of `GET /{$}` | See ARCHITECTURE.md; `server.go` route registration |
| `dfrotz` / `glulxe` / `frob` not found | `/usr/games` missing from container `PATH` | Confirm `ENV PATH` in `service/Dockerfile` includes `/usr/games` |
| TTS silent in Chrome | Chrome leaves SpeechSynthesis in a paused state | `cancel()` + `resume()` before `speak()` in `play.html` |
| TTS silent in iOS Safari on first play | iOS requires a user gesture to unlock audio | `unlockAudio()` must fire on first `click`/`touchend` |

---

## Contributing

1. Read `ARCHITECTURE.md`.
2. Create a feature branch from `main`.
3. Keep commits atomic; use conventional commits (`type(scope): subject`).
4. Run `go test -race ./...`, `go vet ./...`, and `golangci-lint run ./...` before pushing.
5. Open a pull request into `main`.
