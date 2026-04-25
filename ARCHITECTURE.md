# AIFStudio Architecture

> Authoritative spec for the AIFStudio engineering team. Read this before writing code.
>
> **Replatform note (2026-04-25).** AIFStudio is the self-hosted Docker Compose
> reincarnation of the previous Cloud Run service (StoryCloud). All GCP
> dependencies (Firestore, GCS, Firebase Auth, Cloud Run, Terraform) have been
> removed in favour of SQLite, the local filesystem, and a self-contained
> session-cookie auth. The Go module name (`storycloud`) is retained
> intentionally to avoid mass-rename churn — only the product name and the
> deployment substrate have changed.

---

## 1. Overview

### 1.1 Purpose

**AIFStudio** is a self-hosted **Interactive Fiction (IF) platform** that lets users:

1. **Discover** published IF titles via the [IFDB](https://ifdb.org/) catalogue.
2. **Play** Z-machine, Glulx, and TADS 2/3 story files in the browser via a POST+wait HTTP transport (one HTTP round trip per user command). Game subprocesses are sandboxed in the AIFStudio container; sessions survive container recycles, deploys, and restarts via a durable save/restore protocol backed by SQLite + a local storage directory mounted as a Docker volume.
3. **Author** Inform 7 stories, compile them inside the container, and play their compiled builds.
4. **Resume** any game via the home page and `/history`, with Continue / Restart / Delete actions tied to the signed-in user account.

Every interactive surface (browse, play, history, projects, builds) requires a signed-in user. Users register and sign in via `email + password` directly against AIFStudio — there is no Firebase, OAuth, or third-party identity provider.

### 1.2 Design goals

| Goal | Approach |
|------|----------|
| Self-contained binary | All HTML/CSS/JS embedded via Go `embed` |
| Self-contained deployment | One Docker Compose stack — no external cloud services required |
| Interactive play | POST+wait HTTP: browser POSTs a command, Go writes to interpreter stdin, waits for 200 ms stdout quiescence, returns output. Interpreter process lives across requests in the in-memory Manager registry. |
| Author toolchain | Inform 7 CLI + Inform 6 + frob bundled in the container image |
| Persistence | SQLite (metadata) + local filesystem (binaries) — no Redis, no Memcached, no external DB |
| Survive container kill | Every live session has durable state (story file + save + transcript) on the mounted volume; subprocess is re-spawnable from scratch on any container start |
| Zero data loss on graceful shutdown | SIGTERM handler drains all active sessions within `SHUTDOWN_DRAIN_TIMEOUT` (default 8 s) |
| Near-zero data loss on hard kill | Auto-save after **every user turn**; worst case = one in-flight turn |
| Safety | Hard limits on download size, archive expansion, subprocess runtime and memory |

### 1.3 Technology stack

| Layer | Technology |
|-------|------------|
| Language | **Go 1.23** (uses Go 1.22+ `ServeMux` pattern matching) |
| HTTP | `net/http` only (POST+wait; no WebSocket, no upgrade) |
| Persistence (metadata) | **SQLite** via `database/sql` + [`modernc.org/sqlite`](https://modernc.org/sqlite) (pure Go, no CGO required) |
| Persistence (binaries) | **Local filesystem** under `STORAGE_PATH` (mounted Docker volume) |
| Authentication | **Local session auth** — bcrypt password hashes + 32-byte random session tokens stored in SQLite; HttpOnly session cookie |
| Pseudonymous identity (legacy) | **Removed.** Every request is gated by the session cookie; the old `sc_user` cookie is gone. |
| IF runtime (Z-machine) | `dfrotz` |
| IF runtime (Glulx) | `glulxe` |
| IF runtime (TADS 2/3) | `frob` from `frobtads` (built from source — not packaged for Debian bookworm) |
| IF authoring | Inform 7 CLI + Inform 6 compiler/library |
| Container | `golang:1.23-alpine` builder → `debian:bookworm-slim` runtime (non-root). Self-contained; no external base-image registry pull. |
| Compute | **Docker Compose** (self-hosted) |
| CI / image delivery | GitHub Actions — `lint → test → docker build → docker push` (Docker Hub). Image: `vpoluyaktov/aifstudio:{latest,<version>}`. **No deployment step** — operators pull from Docker Hub and run `docker compose up` themselves. |

**Why Debian bookworm-slim and not Alpine?** Debian provides `frotz`, `glulxe`, `inform6-compiler`, and `inform6-library` as first-class apt packages; Alpine does not. `frobtads` is built from source because it is not packaged for bookworm.

**Why `modernc.org/sqlite` and not `mattn/go-sqlite3`?** `modernc.org/sqlite` is a pure Go translation of SQLite — no CGO, no C toolchain, no platform-specific build tags. Builds remain `CGO_ENABLED=0` (small static binary, simple multi-stage Dockerfile). Performance is adequate for single-instance Docker Compose; this app's metadata store is not its bottleneck.

### 1.4 Supported story formats

| Extension | Runtime | Interpreter | Notes |
|-----------|---------|-------------|-------|
| `.z3`–`.z8` | Z-machine | `dfrotz` | Infocom / Inform |
| `.zblorb` | Z-machine (Blorb) | `dfrotz` | Loads the Z-code chunk |
| `.ulx` | Glulx | `glulxe` | Modern Inform 7 non-Z target |
| `.gblorb` | Glulx (Blorb) | `glulxe` | Loads the Glulx chunk |
| `.gam` | TADS 2 | `frob` | Auto-detected by file header |
| `.t3` | TADS 3 | `frob` | Same binary as TADS 2 |

The interpreter is **selected once** on first spawn and recorded in `runs.interpreter` (`"dfrotz"` / `"glulxe"` / `"frob"`). On resume, the server spawns exactly the recorded interpreter — never re-inferring from the file extension.

---

## 2. Directory and File Structure

Go module: **`storycloud`** (retained for now to avoid rename churn — see replatform note above).

```
aifstudio/
├── VERSION                                    # "1.0.0" (MAJOR.MINOR.PATCH; CI appends commit count for visibility only)
├── ARCHITECTURE.md                            # This file
├── README.md                                  # Setup, local dev, Docker Compose run, troubleshooting
├── PROGRESS.md                                # Live development progress / sprint log
├── docker-compose.yml                         # Single-service Compose stack (see §8 for full text)
├── .env.example                               # Template for the Compose `env_file: .env` (developers copy to `.env`)
├── .gitignore                                 # Ignores ./data/, .env, build artifacts
│
├── data/                                      # Runtime mount points (gitignored — created on first run)
│   ├── db/                                    # → /app/data/db inside the container; holds aifstudio.db
│   └── storage/                               # → /app/data/storage inside the container; holds blobs (sessions, builds, projects)
│
├── service/                                   # Go application source — built into the AIFStudio image
│   ├── main.go                                # Entrypoint: config → store → auth → server → listen → SIGTERM drain
│   ├── go.mod                                 # module storycloud, go 1.23 (module name retained — do NOT rename)
│   ├── go.sum
│   ├── Dockerfile                             # Self-contained multi-stage build (Go builder → Debian bookworm-slim runtime; installs IF toolchain)
│   ├── .dockerignore
│   │
│   └── internal/
│       ├── config/
│       │   ├── config.go                      # Config struct + Load() from env vars
│       │   └── config_test.go
│       │
│       ├── server/
│       │   ├── server.go                      # Server struct + New() + SetupRoutes() (Go 1.22 patterns); SIGTERM → runner.Manager.Drain
│       │   ├── server_test.go
│       │   ├── routes_test.go
│       │   ├── handlers.go                    # Common helpers shared across handler files
│       │   ├── handlers_pages.go              # UI page handlers: index, game detail, play, create, project, history, login, register
│       │   ├── handlers_health.go             # /health handler
│       │   ├── handlers_auth.go               # /api/auth/{register,login,logout,me} handlers (NEW)
│       │   ├── handlers_ifdb.go               # /api/ifdb/* JSON handlers
│       │   ├── handlers_runs.go               # /api/runs/* JSON handlers
│       │   ├── handlers_projects.go           # /api/projects/* JSON handlers
│       │   ├── handlers_builds.go             # /api/projects/{id}/builds/* handlers
│       │   ├── handlers_history.go            # /api/runs/by-user (history projection)
│       │   ├── handlers_community.go          # Published-projects catalog endpoints
│       │   ├── handlers_ai.go                 # /api/projects/{id}/ai/* (OpenAI streaming)
│       │   ├── handlers_config.go             # /api/config — public app metadata (version, environment)
│       │   ├── ai_limiter.go                  # Per-user token bucket for AI endpoints
│       │   ├── ai_prompts.go                  # System prompts and prompt assembly
│       │   ├── auth_middleware_test.go
│       │   ├── cookie.go                      # Session cookie helpers (set/clear; HttpOnly; SameSite=Strict)
│       │   ├── middleware.go                  # sessionAuthRequired, recover, requestID, logging, CORS, maxBody
│       │   ├── transcript.go
│       │   ├── transcript_test.go
│       │   ├── prompts/system_prompt.txt
│       │   ├── export_test.go
│       │   └── json.go                        # writeJSON / writeError helpers
│       │
│       ├── auth/
│       │   ├── auth.go                        # User + Session structs, SessionAuth (login/register/verify/logout) backed by Store; bcrypt cost 12 (REWRITTEN — no Firebase)
│       │   ├── auth_test.go
│       │   └── testing.go                     # In-test helpers
│       │
│       ├── ifdb/
│       │   ├── client.go                      # IFDB HTTP client: Search, GetGame; retries, rate limit, cache; devsys filter
│       │   ├── client_test.go
│       │   ├── filter_test.go
│       │   ├── model.go                       # IFDB response structs + normalization; devsys map
│       │   └── cache.go                       # In-memory TTL cache
│       │
│       ├── runner/
│       │   ├── manager.go                     # Run session registry (ID → *Session); idle+hard-session sweeps; Drain(ctx)
│       │   ├── session.go                     # Session lifecycle; InterpreterMode FSM; per-session save mutex; <unsavable> sentinel
│       │   ├── save.go                        # Save/restore command sequences; quiescence detector; SAVING/RESTORING filter; unsavable detection
│       │   ├── download.go                    # Artifact fetch with size cap, content-type sniffing
│       │   ├── extract.go                     # Zip/blorb extraction with bomb protection
│       │   ├── interpreter.go                 # SelectInterpreter(ext) → (name, cmd)
│       │   ├── bridge.go                      # HTTP ↔ subprocess stdin/stdout pump; writes a command and blocks until 200 ms quiescence
│       │   ├── transcript.go                  # Transcript buffering + flush on save triggers
│       │   ├── interpreter_test.go
│       │   └── runner_test.go
│       │
│       ├── build/
│       │   ├── manager.go                     # In-process build queue + registry
│       │   ├── compiler.go                    # Inform 7 CLI invocation
│       │   ├── compiler_internal_test.go
│       │   ├── layout.go                      # Create $TMPDIR/build/<uuid>/<Project>.inform/Source/story.ni
│       │   ├── artifacts.go                   # Persist .ulx + build log to local storage via Store.UploadBlob
│       │   └── build_test.go
│       │
│       ├── openai/
│       │   ├── client.go
│       │   ├── stream.go
│       │   ├── extract.go
│       │   └── extract_test.go
│       │
│       ├── store/
│       │   ├── store.go                       # Store interface (metadata + blob + auth surface)
│       │   ├── sqlite.go                      # SQLiteStore — metadata implementation backed by database/sql + modernc.org/sqlite (NEW; replaces firestore.go)
│       │   ├── local_blob.go                  # LocalBlobStore — filesystem implementation of UploadBlob/DownloadBlob/DeleteBlobPrefix (NEW; replaces gcs.go)
│       │   └── store_test.go                  # MockStore + table-driven coverage of the interface
│       │
│       └── templates/
│           ├── templates.go                   # //go:embed *.html + static/*
│           ├── layout.html                    # Shared chrome
│           ├── index.html                     # Search + "Continue your games" panel
│           ├── login.html                     # Local sign-in form (POST /api/auth/login)
│           ├── register.html                  # Local registration form (POST /api/auth/register)
│           ├── game_detail.html               # /games/{ifdbId}
│           ├── play.html                      # /play/{runId}
│           ├── history.html                   # /history
│           ├── create.html                    # /create
│           ├── projects.html                  # /projects (project list)
│           ├── project_detail.html            # /projects/{id}
│           ├── ai_workspace.html              # AI authoring workspace
│           ├── community.html                 # Published-projects catalog
│           ├── check_test.go
│           └── static/
│               ├── app.css
│               ├── app.js
│               ├── auth.js                    # Calls GET /api/auth/me on load; redirects to /login on 401; populates nav user area
│               ├── projects.js
│               ├── community.js
│               └── ai_workspace.js
│
└── .github/workflows/
    └── main.yml                               # lint → test → docker build → docker push (Docker Hub: vpoluyaktov/aifstudio); no Terraform, no deploy step
```

**Removed in the replatform** (do not recreate):
- `terraform/` — the entire Terraform tree is gone. AIFStudio has no infrastructure-as-code surface.
- `docker/base.Dockerfile` and `docker/base-image.digest` — the runtime base image is now built inline in `service/Dockerfile`.
- `.github/workflows/base-image.yml` — no separate base image to publish.
- Any `terraform/{stage,prod}/*.tfvars` files.

---

## 3. Identity Model — Local Sessions

### 3.1 Single source of identity

Every protected request carries a session cookie issued by AIFStudio after a successful login or registration. There is no anonymous mode, no pseudonymous cookie, no third-party identity provider.

| Identifier | Where it lives | Purpose | Lifetime |
|------------|---------------|---------|----------|
| `userId` (`u-<ULID>`) | `users.id` in SQLite | Stable identity for run/project/build ownership | Forever (user can be deleted by admin out-of-band) |
| Session token | `aifstudio_session` cookie + `sessions.id` in SQLite | Bearer credential for every protected route | `SESSION_MAX_AGE` (default 720 h = 30 days), absolute |
| `runId` (`r-<ULID>`) | URL (`/play/<runId>`) and `runs.id` in SQLite | Per-game session — owns the storage prefix, the interpreter, the save | Indefinite. 30 min idle cap on a live subprocess; resume works until the user deletes. |

**Run capability is no longer public.** Visiting `/play/<runId>` requires a valid session, and mutating endpoints (`/start`, `/command`, `/suspend`, `/save`, `/restart`, `/delete`) additionally check `runs.user_id == session.user_id` — a signed-in user cannot take over another user's run by guessing a URL.

### 3.2 Session cookie

```
Set-Cookie: aifstudio_session=<43 base64url chars>;
            Path=/; HttpOnly; SameSite=Strict; Max-Age=2592000
```

Attribute table:

| Attribute | Value | Why |
|-----------|-------|-----|
| Name | `aifstudio_session` | Distinct from any prior product cookie |
| Value | 32 random bytes, base64url-encoded (43 chars, no padding) | Bearer secret; opaque to the client |
| `Path` | `/` | Apply to every route |
| `HttpOnly` | yes | Inaccessible to JS — XSS cannot read it |
| `SameSite` | `Strict` | No cross-site delivery; complete CSRF protection for same-origin app |
| `Max-Age` | `SESSION_MAX_AGE` (default 30 d) | Absolute expiry; matches `sessions.expires_at` |
| `Secure` | set when the request scheme is `https`; **omitted on plain HTTP** so the cookie works in local dev (`http://localhost:8080`) | Production deployments terminate TLS at a reverse proxy in front of AIFStudio |

The cookie is the **only** authentication carrier — there is no `Authorization: Bearer` header path. Because the cookie is sent automatically with `navigator.sendBeacon`, the legacy `?token=` query parameter on `POST /api/runs/{id}/suspend` is **removed**.

### 3.3 Privacy

A session links a browser to a registered email. Logging out (`POST /api/auth/logout`) deletes the session row from SQLite and clears the cookie; the user is fully signed out across all tabs after a one-page reload (server-side state is authoritative — no token caching to invalidate).

---

## 4. API Endpoint Specification

All JSON endpoints respond with `Content-Type: application/json; charset=utf-8`. Error bodies: `{"error": "<message>", "code": "<machine-code>"}`. HTTP semantics: `200`, `201`, `202`, `204`, `400`, `401`, `403`, `404`, `405`, `409`, `410`, `413`, `422`, `429`, `500`, `503`.

**Auth model.** Every route except the explicit allow-list is gated by `sessionAuthRequired` middleware (§7.4). Allow-listed paths: `GET /health`, `GET /login`, `GET /register`, `GET /api/config`, `POST /api/auth/register`, `POST /api/auth/login`, `GET /static/*`, `GET /favicon.ico`. All other routes return 401 (or 303 redirect to `/login` for HTML page routes) when no valid session is presented.

### 4.1 `GET /health`

Unauthenticated health check.

**Response 200:**
```json
{ "status": "ok", "version": "1.0.42", "environment": "production" }
```

Must always return 200 unless shutting down; must not touch SQLite, the storage filesystem, or IFDB.

---

### 4.2 `POST /api/auth/register`

Create a new user account, issue a session, and set the session cookie. **Unauthenticated** (allow-listed).

**Request:**
```json
{
  "email": "alice@example.com",
  "password": "correct horse battery staple",
  "displayName": "Alice"
}
```

**Validation:**
- `email` — RFC 5322-ish (`net/mail.ParseAddress`); lowercased before storage. 1–254 chars after trim.
- `password` — 8–128 chars, no codepoint restrictions beyond `len(password) >= 8`. Hashed with bcrypt cost 12.
- `displayName` — 1–80 chars after trim. UTF-8.

**Response 201:**
```json
{
  "user": {
    "uid": "u-01HXZX5K9V2EQB9M7YPQ3",
    "email": "alice@example.com",
    "displayName": "Alice"
  }
}
```

**Side effect:** `Set-Cookie: aifstudio_session=...` (per §3.2).

**Errors:**

| Status | Code | Cause |
|--------|------|-------|
| `400` | `invalid_email` | Bad email format |
| `400` | `weak_password` | < 8 chars or > 128 |
| `400` | `invalid_display_name` | Empty or > 80 chars |
| `409` | `email_taken` | A user with this email already exists |
| `500` | `internal` | bcrypt or SQLite failure |

---

### 4.3 `POST /api/auth/login`

Verify credentials and issue a session. **Unauthenticated** (allow-listed).

**Request:**
```json
{ "email": "alice@example.com", "password": "correct horse battery staple" }
```

**Behavior:**
1. Lowercase + trim `email`. Look up `users` row by email.
2. If no row, or `bcrypt.CompareHashAndPassword(hash, password)` returns error → `401 invalid_credentials` with constant-time response delay (compare a dummy hash on the no-row path so the timing channel is closed).
3. Generate 32 random bytes; base64url-encode → session ID (43 chars).
4. Insert `sessions` row with `user_id`, `created_at = now()`, `expires_at = now() + SESSION_MAX_AGE`.
5. Set the cookie.
6. Return the same shape as register.

**Response 200:**
```json
{
  "user": {
    "uid": "u-01HXZX5K9V2EQB9M7YPQ3",
    "email": "alice@example.com",
    "displayName": "Alice"
  }
}
```

**Errors:** `400` (malformed body), `401 invalid_credentials`, `429 too_many_requests` (per-IP login rate limit; 10/min default — handled in middleware, not here).

---

### 4.4 `POST /api/auth/logout`

Delete the current session. **Authenticated.** Cookie required.

**Behavior:**
1. Read `aifstudio_session` cookie. If absent or unknown → 204 anyway (idempotent).
2. `store.DeleteSession(ctx, sessionID)`.
3. Clear the cookie: `Set-Cookie: aifstudio_session=; Path=/; HttpOnly; Max-Age=0`.
4. `204`.

---

### 4.5 `GET /api/auth/me`

Return the current user. **Authenticated.**

**Response 200:**
```json
{
  "user": {
    "uid": "u-01HXZX5K9V2EQB9M7YPQ3",
    "email": "alice@example.com",
    "displayName": "Alice"
  }
}
```

**Errors:** `401 auth_required` if no valid session.

Used by `auth.js` on page load to populate the nav user area; if 401, redirect to `/login?next=...`.

---

### 4.6 `GET /api/config`

Public app metadata — replaces the old Firebase config endpoint. **Unauthenticated** (allow-listed).

**Response 200:**
```json
{ "environment": "production", "version": "1.0.42" }
```

No secrets or auth wiring leak through this endpoint; it exists purely for the frontend to render the env banner and footer version.

**Caching:** `Cache-Control: public, max-age=300`.

---

### 4.7 `GET /api/ifdb/search`

Proxies IFDB search. **Authenticated.**

**Query:** `q` (1–200 chars, required, trimmed); `limit` (1–100, default 25, clamped).

**Response 200:**
```json
{
  "query": "zork",
  "count": 3,
  "results": [
    {
      "id": "0dbnusxunq7fw5ro",
      "title": "Zork I",
      "authors": ["Marc Blank", "Dave Lebling"],
      "year": 1980,
      "rating": 4.4,
      "coverArtURL": "https://ifdb.org/viewgame?id=0dbnusxunq7fw5ro&coverart",
      "formats": ["z3"]
    }
  ]
}
```

**Errors:** `400` (bad `q`), `401` (no session), `429` (rate limit), `503` (IFDB upstream error).

**Edge cases:**
- **Empty results** → `200` with `count: 0, results: []`. Never 404.
- **Cache hit** → identical response plus `X-Cache: HIT`.
- **IFDB malformed JSON** → log ERROR, return `503 upstream_invalid`.
- **Devsys filtering** — same as before; see §5.10 and §13.11.
- **Devsys missing/empty** → include (optimistic; the download-link format check filters further).

---

### 4.8 `GET /api/ifdb/games/{id}`

Single IFDB game by TUID. **Authenticated.** Not subject to devsys filtering.

**Path:** `{id}` must match `^[a-z0-9]{10,32}$`.

**Response 200:**
```json
{
  "id": "0dbnusxunq7fw5ro",
  "title": "Zork I",
  "authors": ["Marc Blank", "Dave Lebling"],
  "year": 1980,
  "rating": 4.4,
  "description": "The classic treasure hunt through the Great Underground Empire.",
  "coverArtURL": "https://ifdb.org/viewgame?id=0dbnusxunq7fw5ro&coverart",
  "downloadLinks": [
    { "url": "https://mirror.ifarchive.org/if-archive/games/zcode/zork1.z5", "format": "z5", "size": 84992 },
    { "url": "https://mirror.ifarchive.org/if-archive/games/zcode/zork1.zblorb", "format": "zblorb", "size": 126976 }
  ],
  "formats": ["z5", "zblorb"]
}
```

**Errors:** `400`, `401`, `404`, `429`, `503`.

---

### 4.9 `POST /api/runs`

Creates a new run. **Authenticated.** `runs.user_id = session.user_id`.

**Request:**
```json
{ "sourceType": "ifdb", "ifdbId": "0dbnusxunq7fw5ro", "format": "z5" }
```

Alternates: `{ "sourceType": "url", "artifactUrl": "https://example.com/story.z5" }` · `{ "sourceType": "build", "buildId": "b-..." }`.

**Fields:**

| Field | Required when | Notes |
|-------|---------------|-------|
| `sourceType` | always | `"ifdb"` / `"url"` / `"build"` |
| `ifdbId` | `sourceType == "ifdb"` | `^[a-z0-9]{10,32}$` |
| `format` | optional | One of the supported formats |
| `artifactUrl` | `sourceType == "url"` | `https://` only, not in a denylist |
| `buildId` | `sourceType == "build"` | Must belong to the authenticated user |

**Response 201:**
```json
{
  "id": "r-01HXZX5K2V0EQB9M7YPQ3",
  "sourceType": "ifdb",
  "ifdbId": "0dbnusxunq7fw5ro",
  "title": "Zork I",
  "format": "z5",
  "status": "pending",
  "createdAt": "2026-04-25T17:14:32.104Z",
  "playUrl": "/play/r-01HXZX5K2V0EQB9M7YPQ3",
  "startUrl": "/api/runs/r-01HXZX5K2V0EQB9M7YPQ3/start",
  "commandUrl": "/api/runs/r-01HXZX5K2V0EQB9M7YPQ3/command"
}
```

**Errors:** `400` (validation), `401` (no session), `403` (not build owner), `404` (build missing or not `succeeded`), `413` (artifact > 50 MiB), `429`.

**Edge cases:**
- `sourceType == "ifdb"` without `format` → pick the first compatible from `downloadLinks` (preference `z5`, `z8`, `zblorb`, `ulx`, `gblorb`, `gam`, `t3`). If none → `400 no_compatible_format`.
- IFDB has no `downloadLinks` → `404`.
- `http://` (not HTTPS) `artifactUrl` → `400`.
- `buildId` in `pending` / `failed` → `404`.

---

### 4.10 `GET /api/runs/{id}`

Returns run metadata. **Authenticated.** No ownership check on read (signed-in users can read any run by ID); ownership is enforced on mutating endpoints below.

**Path:** `^r-[0-9A-Z]{26}$`.

**Response 200:**
```json
{
  "id": "r-01HXZX5K2V0EQB9M7YPQ3",
  "sourceType": "ifdb",
  "ifdbId": "0dbnusxunq7fw5ro",
  "title": "Zork I",
  "format": "z5",
  "interpreter": "dfrotz",
  "status": "suspended",
  "createdAt": "2026-04-25T17:14:32.104Z",
  "startedAt": "2026-04-25T17:14:33.902Z",
  "lastActiveAt": "2026-04-25T17:42:11.881Z",
  "lastSaveAt": "2026-04-25T17:42:11.881Z",
  "turnCount": 42,
  "playUrl": "/play/r-01HXZX5K2V0EQB9M7YPQ3",
  "startUrl": "/api/runs/r-01HXZX5K2V0EQB9M7YPQ3/start",
  "commandUrl": "/api/runs/r-01HXZX5K2V0EQB9M7YPQ3/command",
  "transcriptURL": "/api/runs/r-01HXZX5K2V0EQB9M7YPQ3/transcript"
}
```

`status`: `"pending"` / `"running"` / `"suspended"` / `"finished"` / `"failed"`. `transcriptURL` is a same-origin path served by the AIFStudio backend (it streams the transcript file out of `STORAGE_PATH/sessions/<runId>/transcript.txt` — no signed URLs, no GCS).

**Errors:** `400`, `401`, `404`.

---

### 4.11 `GET /api/runs/by-user`

Runs owned by the signed-in user.

**Query:** `limit` (1–50, default 20); `status` (optional filter, comma-separable).

**Response 200:**
```json
{
  "userId": "u-01HXZX5K9V2EQB9M7YPQ3",
  "count": 1,
  "runs": [
    {
      "id": "r-01HXZX5K2V0EQB9M7YPQ3",
      "ifdbId": "0dbnusxunq7fw5ro",
      "title": "Zork I",
      "format": "zblorb",
      "interpreter": "dfrotz",
      "status": "suspended",
      "turnCount": 42,
      "lastActiveAt": "2026-04-25T17:42:11.881Z",
      "lastSaveAt": "2026-04-25T17:42:11.881Z",
      "createdAt": "2026-04-25T17:14:32.104Z",
      "canContinue": true,
      "canRestart": true,
      "canDelete": true,
      "playUrl": "/play/r-01HXZX5K2V0EQB9M7YPQ3"
    }
  ]
}
```

**Field rules:**
- `canContinue` = `status in ("running", "suspended")` AND `savePath != ""` AND `savePath != "<unsavable>"`.
- `canRestart` = `sourceType == "ifdb"` OR (`url` + URL reachable — checked lazily on restart). `build`-sourced runs are restartable if the build still exists.
- `canDelete` = always true for runs the signed-in user owns.

Empty result → `200` with `count: 0, runs: []`. Never 401 (a signed-in user with no runs is not an auth error).

---

### 4.12 Interactive play transport (POST+wait)

Three plain HTTP endpoints. Each user command = one HTTP round trip: client POSTs, server writes to interpreter stdin, waits for 200 ms of stdout silence ("quiescence"), returns accumulated stdout as JSON. Interpreter lives across requests in the in-memory `runner.Manager` registry keyed by `runId`.

**All three endpoints are authenticated and additionally enforce `runs.user_id == session.user_id`.** Mismatch → `403 forbidden`.

**Concurrency:** each run has **at most one command in flight**, guarded by a per-session command mutex. Overlapping POST → `409 busy`.

#### 4.12.1 `GET /api/runs/{id}/start`

Start or resume the interpreter for this run and return its current prompt.

**Behavior — four paths:**

| Condition | Path | Actions |
|-----------|------|---------|
| Session already live in Manager | **HOT** | Zero-input quiescence sample; return current prompt. Idempotent for page reloads. |
| `run.storyPath == ""` (brand new) | **FIRST-TIME** | Resolve → download → extract → select interpreter → write `story.<ext>` to local storage → update `runs.story_path`, `interpreter`, `format` in SQLite → spawn → collect startup banner. |
| `run.savePath == "<unsavable>"` | **UNSAVABLE** | Read `story.<ext>` from local storage → spawn → collect startup banner → respond `"unsavable": true`. |
| `run.savePath != ""` && `!= "<unsavable>"` | **RESUME** | Read `story.<ext>` + `game.sav` from local storage → spawn the **recorded** interpreter (never re-infer from `<ext>`) → restore sequence (§6.3) → collect post-restore prompt → load `transcript.txt` for `replay`. |

**Response 200 (first-time):**
```json
{
  "id": "r-01HXZX5K2V0EQB9M7YPQ3",
  "status": "running",
  "interpreter": "dfrotz",
  "format": "zblorb",
  "turnCount": 0,
  "output": "ZORK I: The Great Underground Empire\n…\nWest of House\nYou are standing in an open field…\n\n> ",
  "replay": "",
  "replayTruncated": false,
  "unsavable": false
}
```

**Response 200 (resume):** same shape with `turnCount` set to the persisted value, `output` containing the restored prompt, and `replay` containing the prior transcript (empty on first-time).

- `replay` — up to last 256 KiB of `STORAGE_PATH/sessions/<runId>/transcript.txt`. `replayTruncated: true` if clipped.
- `unsavable` — `true` on the UNSAVABLE path.
- Invalid UTF-8 replaced with `U+FFFD`.

**Errors:**

| Status | Code | Cause |
|--------|------|-------|
| `400` | `invalid_id` | `{id}` fails regex |
| `401` | `auth_required` | No session |
| `403` | `forbidden` | Run owned by a different user |
| `404` | `not_found` | Run does not exist |
| `409` | `busy` | Another `/start` or `/command` in flight |
| `410` | `session_finished` | Run is `finished` — cannot resume |
| `413` | `download_too_large` / `archive_too_large` | > 50 MiB / > 100 MiB |
| `422` | `unsupported_format` | No interpreter for extension |
| `500` | `spawn_failed` | Interpreter failed to start |
| `500` | `restore_failed` | Both `game.sav` and `game.sav.prev` failed |
| `500` | `save_path_missing` | Cached story or save missing on disk |
| `503` | `upstream_unavailable` | Artifact download failed |

**Edge cases:**
- Idempotent on page reload (HOT path).
- Restore failure → retry once with `game.sav.prev`; second failure → `500 restore_failed`.
- Startup quiescence timeout (2 s) → return whatever was collected.
- No auto-save during `start` — first save triggers on first `command`.

#### 4.12.2 `POST /api/runs/{id}/command`

Forward one user command and return its response.

**Request:** `{ "text": "go north" }` — 1–4096 chars after trim. Trailing `\n` appended if absent. Empty `text` acts as a bare newline (useful for "press any key" prompts).

**Response 200:**
```json
{
  "id": "r-01HXZX5K2V0EQB9M7YPQ3",
  "output": "Forest\nThis is a forest, with trees in all directions.\nTo the east, there appears to be sunlight.\n\n> ",
  "turnCount": 5
}
```

**Errors:**

| Status | Code | Cause |
|--------|------|-------|
| `400` | `invalid_text` | Missing, > 4096 chars, or contains NULs |
| `401` | `auth_required` | No session |
| `403` | `forbidden` | Run owned by a different user |
| `404` | `not_found` | Run does not exist |
| `409` | `busy` | Another command in flight |
| `409` | `not_started` | No live Session — client must call `/start` |
| `410` | `session_finished` | Interpreter exited during wait |
| `500` | `internal` | Unexpected failure |

**410 body shape:**
```json
{
  "error": "session finished",
  "code": "session_finished",
  "output": "You have died.\n*** You have died ***\n",
  "exitCode": 0,
  "transcriptURL": "/api/runs/r-01HXZX5K2V0EQB9M7YPQ3/transcript"
}
```

#### 4.12.3 `POST /api/runs/{id}/suspend`

Save and release. Called by the client on page unload via `navigator.sendBeacon`.

**Request:** empty body or `{}`. Handler ignores body.

**Auth.** The session cookie travels with `sendBeacon` automatically; **no `?token=` query parameter** is supported (the legacy Firebase workaround is removed).

**Behavior:**
1. No live Session → `204`.
2. Acquire `saveMutex` (5 s).
3. Synchronous save (§6.6) with 3 s budget.
4. `status = "suspended"`, `lastActiveAt = now()`.
5. SIGTERM → SIGKILL after 5 s. Remove from Manager.
6. `204`.

**Errors:** `401`, `403`, `404`, `500` (save failed hard; subprocess still killed).

#### 4.12.4 Idle and lifetime sweeps

A single Manager goroutine ticks once per minute and enforces:

| Cap | Default | Trigger | Action |
|-----|---------|---------|--------|
| `RunIdleTimeout` | 30 min | `now - lastCommandAt > cap` | Synchronous save → `suspended` → SIGTERM → drop from registry |
| `RunSessionMax` | 60 min | `now - startedAt > cap` | Same — prevents a pathological client from keeping a subprocess alive forever |

Neither cap fails the next user command: next POST → `409 not_started` → client calls `/start` (RESUME path) → play continues.

---

### 4.13 `POST /api/runs/{id}/save`

Explicit user-triggered save. **Authenticated; ownership-checked.**

**Request:** `{}`.

**Response 200:**
```json
{ "id": "r-01HXZX5K2V0EQB9M7YPQ3", "savedAt": "2026-04-25T17:42:11.881Z", "turnCount": 42 }
```

**Errors:** `401`, `403`, `404`; `409` (not running, or unsavable); `503` (save failure).

---

### 4.14 `POST /api/runs/{id}/restart`

Start the same story over. **Authenticated; ownership-checked.**

**Behavior:**
1. Preempt any live Session: acquire `commandMutex` (2 s; contended → `409 busy`), SIGTERM → SIGKILL (5 s), remove from registry.
2. Delete `STORAGE_PATH/sessions/<runId>/game.sav` and `game.sav.prev`. Keep `story.<ext>`.
3. Create a new `runs` row copying `ifdbId`, `title`, `format`, `sourceType`, `artifactUrl`, `buildId`, `interpreter`, `storyPath` from the original. New `createdAt`, empty `savePath`, `turnCount = 0`, `status = "pending"`, same `userId`.
4. Copy `STORAGE_PATH/sessions/<runId>/story.<ext>` → `STORAGE_PATH/sessions/<newRunId>/story.<ext>`.
5. Mark original `status = "finished"`, set `finishedAt`; transcript stays.
6. Return new run.

**Response 201:** same shape as §4.9 plus `previousRunId`.

**Errors:** `401`, `403`, `404`, `409` (URL-sourced and HEAD failed), `500`.

---

### 4.15 `DELETE /api/runs/{id}`

Permanently remove. **Authenticated; ownership-checked.**

**Behavior:**
1. Load run. Missing → `404`. Mismatch → `403`.
2. Preempt any live Session (same as restart).
3. `store.DeleteRun` — delete local storage prefix `STORAGE_PATH/sessions/<runId>/` recursively, then the SQLite row.
4. `204`.

**Errors:** `401`, `403`, `404`, `500`.

Idempotent: subsequent calls return `404`; clients treat that as success.

---

### 4.16 `POST /api/projects`

Create an Inform 7 project. **Authenticated.**

**Request:**
```json
{
  "name": "The Blue Door",
  "description": "A short mystery in one room.",
  "source": "\"The Blue Door\" by Alex Author.\n\nThe Hallway is a room.\n"
}
```

- `name`: 1–80 chars after trim.
- `description`: 0–`AI_MAX_DESCRIPTION_CHARS` chars (default 2000).
- `source`: 0–500,000 chars. Stored on the local filesystem at `STORAGE_PATH/projects/<projectId>/source.i7` via `Store.PutProjectSource`. **Not** stored in SQLite.

**Response 201:**
```json
{
  "id": "p-01HXZX5K3Q0RTB9M7YPZL",
  "ownerUid": "u-01HXZX5K9V2EQB9M7YPQ3",
  "name": "The Blue Door",
  "description": "A short mystery in one room.",
  "createdAt": "2026-04-25T17:20:11.000Z",
  "updatedAt": "2026-04-25T17:20:11.000Z",
  "latestBuildId": "",
  "published": false
}
```

**Errors:** `400`, `401`, `413`.

---

### 4.17 `GET /api/projects/{id}`

Single project (without source). **Authenticated.** Owner-only. Returns the same shape as §4.16. **Errors:** `401`, `403`, `404`.

`GET /api/projects/{id}/source` returns the Inform 7 source text body as `text/plain; charset=utf-8`. Owner-only.

---

### 4.18 `PUT /api/projects/{id}/source`

Replace Inform 7 source. **Authenticated.** Owner-only.

**Request:** `{ "source": "…" }`

**Response 200:**
```json
{ "id": "p-01HXZX5K3Q0RTB9M7YPZL", "updatedAt": "2026-04-25T17:31:02.441Z", "sourceBytes": 112 }
```

**Errors:** `400`, `401`, `403`, `404`, `413`.

---

### 4.19 `POST /api/projects/{id}/builds`

Enqueue a build. **Authenticated.** Owner-only.

**Request:** `{}`.

**Response 202:**
```json
{
  "id": "b-01HXZX5K4MQS0RTB9M7YPA",
  "projectId": "p-01HXZX5K3Q0RTB9M7YPZL",
  "status": "pending",
  "createdAt": "2026-04-25T17:35:00.001Z",
  "queuePosition": 0
}
```

**Errors:** `401`, `403`, `404`, `409` (another build already `pending`/`running` for this project).

---

### 4.20 `GET /api/projects/{id}/builds/{buildId}`

Build status. **Authenticated.** Owner-only.

**Response 200 (succeeded):**
```json
{
  "id": "b-01HXZX5K4MQS0RTB9M7YPA",
  "projectId": "p-01HXZX5K3Q0RTB9M7YPZL",
  "status": "succeeded",
  "createdAt": "2026-04-25T17:35:00.001Z",
  "startedAt": "2026-04-25T17:35:00.420Z",
  "finishedAt": "2026-04-25T17:35:42.118Z",
  "durationMs": 41698,
  "artifactFormat": "ulx",
  "artifactURL": "/api/projects/p-.../builds/b-.../artifact",
  "logURL": "/api/projects/p-.../builds/b-.../log",
  "errorMessage": ""
}
```

**Response 200 (failed):** same shape with `status: "failed"`, `artifactFormat: ""`, `artifactURL: ""`, `errorMessage` populated.

`status`: `"pending"` / `"running"` / `"succeeded"` / `"failed"` / `"cancelled"`.

`artifactURL` / `logURL` are **same-origin** AIFStudio routes that stream the file from local storage (`STORAGE_PATH/builds/<buildId>/...`). The frontend follows them with the session cookie attached automatically — no signed URLs.

---

### 4.21 Web UI pages

All pages respond `text/html; charset=utf-8`. No server-side data fetches beyond template vars; dynamic content comes from the JSON API via `fetch()`.

| Method | Path                 | Template               | Auth | Purpose |
|--------|----------------------|------------------------|------|---------|
| GET    | `/{$}`               | `index.html`           | yes  | Search + "Continue your games" (exact-match root — see §7.1) |
| GET    | `/login`             | `login.html`           | no   | Local sign-in form |
| GET    | `/register`          | `register.html`        | no   | Local registration form |
| GET    | `/history`           | `history.html`         | yes  | Full "My Games" list |
| GET    | `/games/{ifdbId}`    | `game_detail.html`     | yes  | IFDB detail + Play button |
| GET    | `/play/{runId}`      | `play.html`            | yes  | Terminal UI; POST+wait driver; `sendBeacon` on unload; replay; unsavable banner; voice I/O |
| GET    | `/create`            | `create.html`          | yes  | New Inform 7 project form |
| GET    | `/projects`          | `projects.html`        | yes  | Project list |
| GET    | `/projects/{id}`     | `project_detail.html`  | yes  | Project editor + builds |
| GET    | `/community`         | `community.html`       | yes  | Published-projects catalog |
| GET    | `/static/{file...}`  | `templates/static/*`   | no   | Embedded CSS/JS |

Mismatched methods on these routes return `405` (see §7.1).

---

## 5. Data Models

### 5.1 SQLite database

Single file at `DB_PATH` (default `/app/data/db/aifstudio.db`). Connection string includes `?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)`.

All `*_at` columns are stored as **RFC 3339 strings** (`TEXT`) in UTC for grep-ability and stable ordering, except where a query plan benefits from integer comparison — `ifdb_cache.expires_at` is `INTEGER` Unix milliseconds because the freshness sweep uses range scans.

| Table | PK | Owner | Purpose |
|-------|----|-------|---------|
| `users` | `id` (`u-<ULID>`) | — | Local user accounts |
| `sessions` | `id` (43-char base64url) | `user_id` FK | Active session tokens |
| `runs` | `id` (`r-<ULID>`) | `user_id` FK | Run metadata |
| `projects` | `id` (`p-<ULID>`) | `owner_uid` FK | Inform 7 project |
| `project_sources` | `project_id` (FK) | inherits | Inform 7 source bytes (one row per project; replaces the GCS source.i7 object) |
| `ai_turns` | `id` | `owner_uid` FK | AI conversation turns |
| `builds` | `id` (`b-<ULID>`) | `owner_uid` FK | Build record |
| `ifdb_cache` | `tuid` | — | IFDB game cache |

### 5.2 Schema (canonical CREATE TABLE)

```sql
-- ─── users ────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS users (
  id            TEXT PRIMARY KEY,        -- "u-<ULID>"
  email         TEXT NOT NULL UNIQUE,    -- lowercased
  password_hash TEXT NOT NULL,           -- bcrypt cost 12
  display_name  TEXT NOT NULL,
  created_at    TEXT NOT NULL            -- RFC 3339 UTC
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email ON users(email);

-- ─── sessions ─────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS sessions (
  id          TEXT PRIMARY KEY,                                  -- 43-char base64url
  user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at  TEXT NOT NULL,
  expires_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user_id    ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);

-- ─── runs ─────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS runs (
  id               TEXT PRIMARY KEY,                              -- "r-<ULID>"
  source_type      TEXT NOT NULL,                                 -- "ifdb"|"url"|"build"
  ifdb_id          TEXT NOT NULL DEFAULT '',
  title            TEXT NOT NULL DEFAULT '',
  format           TEXT NOT NULL DEFAULT '',
  artifact_url     TEXT NOT NULL DEFAULT '',
  build_id         TEXT NOT NULL DEFAULT '',
  user_id          TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  status           TEXT NOT NULL,                                 -- "pending"|"running"|"suspended"|"finished"|"failed"
  created_at       TEXT NOT NULL,
  started_at       TEXT,
  finished_at      TEXT,
  last_active_at   TEXT,
  exit_code        INTEGER,
  transcript_path  TEXT NOT NULL DEFAULT '',
  error_code       TEXT NOT NULL DEFAULT '',
  error_message    TEXT NOT NULL DEFAULT '',
  interpreter      TEXT NOT NULL DEFAULT '',                      -- "dfrotz"|"glulxe"|"frob"
  story_path       TEXT NOT NULL DEFAULT '',
  save_path        TEXT NOT NULL DEFAULT '',                      -- "" or "<unsavable>" or "sessions/<id>/game.sav"
  turn_count       INTEGER NOT NULL DEFAULT 0,
  last_save_at     TEXT,
  reconnect_count  INTEGER NOT NULL DEFAULT 0,
  candidate_urls   TEXT NOT NULL DEFAULT '[]',                    -- JSON-encoded []string
  project_id       TEXT NOT NULL DEFAULT ''                       -- set for build-sourced runs; cascade-delete on project delete
);
CREATE INDEX IF NOT EXISTS idx_runs_user_active   ON runs(user_id, last_active_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_status_created ON runs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_runs_project_id    ON runs(project_id);

-- ─── projects ─────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS projects (
  id              TEXT PRIMARY KEY,                              -- "p-<ULID>"
  owner_uid       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name            TEXT NOT NULL,
  description     TEXT NOT NULL DEFAULT '',
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  latest_build_id TEXT NOT NULL DEFAULT '',
  published       INTEGER NOT NULL DEFAULT 0,                    -- 0/1 boolean
  published_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_projects_owner_updated  ON projects(owner_uid, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_projects_published_at   ON projects(published, published_at DESC);

-- ─── project_sources ──────────────────────────────────────────────────────
-- Inform 7 source kept in SQLite (unlike binary artefacts, source is text and
-- is small). One row per project; absent until the first PutProjectSource.
CREATE TABLE IF NOT EXISTS project_sources (
  project_id  TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
  source      TEXT NOT NULL
);

-- ─── ai_turns ─────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS ai_turns (
  id                   TEXT PRIMARY KEY,
  project_id           TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  owner_uid            TEXT NOT NULL,
  kind                 TEXT NOT NULL,                            -- "generate"|"chat"
  user_message         TEXT NOT NULL DEFAULT '',
  assistant_reply      TEXT NOT NULL DEFAULT '',
  source_before        TEXT NOT NULL DEFAULT '',
  source_after         TEXT NOT NULL DEFAULT '',
  model_requested_at   TEXT NOT NULL,
  model_finished_at    TEXT NOT NULL,
  prompt_tokens        INTEGER NOT NULL DEFAULT 0,
  completion_tokens    INTEGER NOT NULL DEFAULT 0,
  model                TEXT NOT NULL DEFAULT '',
  error                TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_ai_turns_project_time
  ON ai_turns(project_id, model_requested_at);

-- ─── builds ───────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS builds (
  id              TEXT PRIMARY KEY,                              -- "b-<ULID>"
  project_id      TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  owner_uid       TEXT NOT NULL,
  status          TEXT NOT NULL,
  created_at      TEXT NOT NULL,
  started_at      TEXT,
  finished_at     TEXT,
  artifact_format TEXT NOT NULL DEFAULT '',
  artifact_path   TEXT NOT NULL DEFAULT '',                      -- "builds/<id>/story.ulx"
  log_path        TEXT NOT NULL DEFAULT '',                      -- "builds/<id>/build.log"
  error_message   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_builds_project_created
  ON builds(project_id, created_at DESC);

-- ─── ifdb_cache ───────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS ifdb_cache (
  tuid       TEXT PRIMARY KEY,
  payload    BLOB NOT NULL,                                      -- JSON-encoded normalized Game
  fetched_at INTEGER NOT NULL,                                   -- Unix ms
  expires_at INTEGER NOT NULL                                    -- Unix ms; freshness query is range scan
);
CREATE INDEX IF NOT EXISTS idx_ifdb_cache_expires_at ON ifdb_cache(expires_at);
```

**Migration strategy.** The store applies these statements idempotently (`CREATE TABLE IF NOT EXISTS`) at startup in `SQLiteStore.New(...)`. There is no separate migration tool; new columns are added by `ALTER TABLE … ADD COLUMN` in the store constructor when the new shape lands. Single-instance Compose deployment keeps this simple — no concurrent migration coordination is needed.

### 5.3 `runs` row — JSON wire shape

```json
{
  "id": "r-01HXZX5K2V0EQB9M7YPQ3",
  "sourceType": "ifdb",
  "ifdbId": "0dbnusxunq7fw5ro",
  "title": "Zork I",
  "format": "zblorb",
  "artifactUrl": "https://mirror.ifarchive.org/.../zork1.zblorb",
  "buildId": "",
  "userId": "u-01HXZX5K9V2EQB9M7YPQ3",
  "status": "suspended",
  "createdAt": "2026-04-25T17:14:32.104Z",
  "startedAt": "2026-04-25T17:14:33.902Z",
  "finishedAt": null,
  "lastActiveAt": "2026-04-25T17:42:11.881Z",
  "exitCode": null,
  "transcriptPath": "sessions/r-01HXZX5K2V0EQB9M7YPQ3/transcript.txt",
  "errorCode": "",
  "errorMessage": "",
  "interpreter": "dfrotz",
  "storyPath": "sessions/r-01HXZX5K2V0EQB9M7YPQ3/story.zblorb",
  "savePath": "sessions/r-01HXZX5K2V0EQB9M7YPQ3/game.sav",
  "turnCount": 42,
  "lastSaveAt": "2026-04-25T17:42:11.881Z",
  "reconnectCount": 1
}
```

Key fields:
- `title` — denormalized from IFDB at creation time.
- `interpreter` — `"dfrotz"`, `"glulxe"`, or `"frob"`; set once on first spawn, never mutated.
- `savePath` — empty until first save; sentinel `"<unsavable>"` for games that refuse to save.
- `reconnectCount` — incremented each time `/start` takes the RESUME path.

**Status transitions** (unchanged from prior architecture): `pending` → `running` → `suspended` ↔ `running` → `finished`; `pending` (abandoned) → swept after `ABANDONED_PENDING_TTL`.

### 5.4 Local filesystem layout

Replaces the GCS bucket layout. Root at `STORAGE_PATH` (default `/app/data/storage`); inside the container, this is the mount point of the host's `./data/storage` directory.

```
$STORAGE_PATH/
  sessions/<runId>/                # Run session state (story file, save, transcript)
    story.<ext>                    #   Cached story binary, byte-for-byte
    game.sav                       #   Latest Quetzal/TADS save (may be absent)
    game.sav.prev                  #   Previous save, one-deep history (may be absent)
    transcript.txt                 #   Full transcript, overwritten on flush
    meta.json                      #   Sidecar metadata (denormalized; SQLite is authoritative)
  builds/<buildId>/                # Build artifacts
    story.ulx                      #   Compiled output (or .gblorb)
    build.log                      #   Inform 7 compile log
  projects/<projectId>/            # Project source + AI turn snapshots
    ai-turns/<turnId>/
      before.i7
      after.i7
```

**Note: project source is in SQLite, not on disk.** `STORAGE_PATH/projects/<projectId>/source.i7` is **not** maintained — text source lives in the `project_sources` table (§5.2). Only the binary AI-turn snapshots (`before.i7`, `after.i7`) live under the project tree on disk, because they're naturally indexed by turn ID and may grow large.

**Session retention.** Indefinite. The only deletion paths are `DELETE /api/runs/{id}` (user-initiated) and the abandoned-pending sweep (§13.7). There is no time-based purge of `sessions/*`.

**Permissions.** The container's non-root `app` user owns every file it writes. The host directory is created on first `docker compose up`; the operator should not need to chown it manually if the container's UID is consistent across restarts.

### 5.5 `meta.json` (sidecar)

Written alongside every save:

```json
{
  "runId": "r-01HXZX5K2V0EQB9M7YPQ3",
  "format": "zblorb",
  "interpreter": "dfrotz",
  "ifdbId": "0dbnusxunq7fw5ro",
  "title": "Zork I",
  "turnCount": 42,
  "lastSaveAt": "2026-04-25T17:42:11.881Z",
  "lastActiveAt": "2026-04-25T17:42:11.881Z",
  "schemaVersion": 1
}
```

Secondary source of truth; the SQLite `runs` row is primary. On conflict, SQLite wins.

### 5.6 Devsys support map

(Unchanged.) IFDB's `devsys` field drives search filtering. Matching rule: case-insensitive, whitespace-trimmed, prefix match on lowercased value.

| Bucket | Match prefixes | Result |
|--------|----------------|--------|
| **Supported** | `"inform"` (5/6/7), `"zil"`, `"z-machine"`, `"zcode"`, `"glulx"`, `"tads"`, `"tads 2"`, `"tads 3"` | Include |
| **Unsupported** | `"hugo"`, `"agt"`, `"adrift"`, `"quest"`, `"alan"`, `"twine"`, `"advsys"`, `"scott"` / `"scott adams"` / `"scottfree"`, `"adl"`, `"magnetic scrolls"`, `"level 9"`, `"gamebook"`, `"choicescript"`, `"tiddlywiki"`, `"ink"`, `"unity"`, `"custom"` | Filter out |
| **Unknown / empty** | any other, or `""` | Include (optimistic) |

Not applied to `GET /api/ifdb/games/{id}`.

---

## 6. Session Persistence Protocol

This section is **normative** and is unchanged in semantics by the replatform — only the durability substrate is different (local FS instead of GCS). Wherever the prior text said "GCS", read "the storage filesystem under `STORAGE_PATH`" via `Store.UploadBlob` / `Store.DownloadBlob`.

### 6.1 Interpreter & save-format compatibility

Quetzal is a common IFF container used by Z-machine and Glulx, but the game-state chunks inside are interpreter-specific and **not cross-compatible**. TADS saves are TADS-native (not Quetzal); the bridge never parses save bytes.

| Constraint | Why |
|------------|-----|
| `runs.interpreter` is written once on first spawn and never mutated | A Glulx save cannot be loaded by Z-machine and vice versa |
| Story file `<ext>` preserved byte-for-byte | Interpreters check container headers |
| A save belongs to exactly one story binary | No save-sharing across uploads |

### 6.2 Save command sequences

All three interpreters are **in-band** — save/restore are typed commands in the input stream.

| Interpreter | Save | Restore | Save format |
|-------------|------|---------|-------------|
| `dfrotz` | `save` + filename + optional `y` | `restore` + filename | Quetzal |
| `glulxe` | `save` + filename + optional `y` | `restore` + filename | Quetzal |
| `frob` | `save` + filename + optional `y` | `restore` + filename | TADS-native (opaque) |

Exact stdin bytes:

```
save\n
/tmp/runs/<runId>/game.sav\n
y\n
```

The `y\n` overwrite-confirm line is always sent. The bridge does not pattern-match prompt wording; it writes the lines unconditionally and reads until quiescence.

### 6.3 Restore command sequences

Issued immediately after spawn on RESUME:

```
restore\n
/tmp/runs/<runId>/game.sav\n
```

### 6.4 Output filtering — the `InterpreterMode` FSM

```
NORMAL      — accumulate all stdout into the command-response buffer
SAVING      — divert stdout into a transient buffer; exit on "Ok.\n" / "Saved.\n" / "Done.\n" or 1 s timeout
RESTORING   — divert stdout into a transient buffer; exit on "Ok.\n" / "Restored.\n" / first non-prompt line after 500 ms silence, or 2 s timeout
```

### 6.5 Save trigger policy

| Trigger | Policy |
|---------|--------|
| Every user command | Async save runs after the response has quiesced; `/command` response is **not** blocked on save I/O. Coalesced per §6.5.1. |
| `POST /suspend` | Synchronous; 3 s budget |
| Process `SIGTERM` | Synchronous; all sessions saved in parallel (capped at 10 concurrent); budget = `SHUTDOWN_DRAIN_TIMEOUT` (default 8 s) |
| `POST /save` | Synchronous; 5 s budget |

#### 6.5.1 Coalescing

At most **one save in flight per session** (per-session `saveMutex`). If a new turn completes while a save is running, a `pending` flag is set; when the current save finishes a single follow-up save is scheduled carrying the latest state.

### 6.6 Save sequence

1. Acquire `saveMutex`.
2. Wait for 200 ms stdout silence (`SAVE_QUIESCENCE_MS`; 2 s timeout).
3. Set FSM = `SAVING`.
4. Write `save\n`, filename, `y\n` to stdin.
5. Wait for FSM → `NORMAL` (≤ `SAVE_TIMEOUT_MS`, 1 s).
6. Verify `/tmp/runs/<runId>/game.sav` exists, size > 0, mtime newer than step 3. **First-ever save failure = unsavable** (§6.9).
7. Promote `game.sav` → `game.sav.prev` (`Store.UploadBlob` source-side rename — for the local FS implementation this is `os.Rename`; if absent, ignored).
8. Upload local `game.sav` → `STORAGE_PATH/sessions/<runId>/game.sav` via `Store.UploadBlob`.
9. Update `meta.json` and write it.
10. Single SQLite `UPDATE runs SET save_path=?, last_save_at=?, turn_count=?, last_active_at=?, status=? WHERE id=?`.
11. Release mutex.

### 6.7 Restore sequence

1. Read `runs.interpreter` (authoritative).
2. Read `story.<ext>` from `STORAGE_PATH/sessions/<runId>/` → `/tmp/runs/<runId>/story.<ext>` (preserve extension).
3. Read `game.sav` from `STORAGE_PATH/sessions/<runId>/` → `/tmp/runs/<runId>/game.sav`.
4. Spawn the recorded interpreter.
5. FSM = `RESTORING`.
6. Write `restore\n`, filename.
7. Wait for FSM → `NORMAL` (≤ `RESTORE_TIMEOUT_MS`, 2 s).
8. Verify via indicators (`Ok.`, `Restored.`, `Game restored.`). On failure retry with `game.sav.prev`. Second failure → `500 restore_failed`.
9. Collect post-restore prompt (200 ms quiescence).
10. Read `transcript.txt` (if present) into `replay`.
11. UPDATE `runs.status = "running"`, `last_active_at`, increment `reconnect_count`.

### 6.8 Transcript replay

After successful restore, `/start` returns the prior transcript in `replay`. Over 256 KiB → last 256 KiB with `replayTruncated: true`.

### 6.9 Unsavable games

(Unchanged.) On first auto-save failure at step 6, mark `runs.save_path = "<unsavable>"`; subsequent saves are skipped; `/start` and `/command` set `unsavable: true`; the play page shows a persistent banner.

### 6.10 Session persistence edge cases (normative)

(Unchanged in semantics — substitute "filesystem object under `STORAGE_PATH`" for "GCS object" throughout.)

| Case | Behavior |
|------|----------|
| `/start` on `pending` with no save | First-time path |
| `/start` on `finished` | `410 session_finished` |
| Format mismatch | Treat as corruption → `save_path_missing` and mark run `failed` |
| Save while producing long output | Quiescence fails; abort; log WARN; retry next command |
| `game.sav` write ok but `meta.json` fails | Accept — `meta.json` secondary |
| `game.sav` write fails | Local file retained; next trigger retries |
| Save at a MORE prompt | `dfrotz -m` disables MORE; glulxe rarely emits it |
| `runs.save_path` set but storage object missing | `500 save_path_missing` (operator deletion) |
| Auto-save while `RESTORING` | Skipped |
| Command during save | `commandMutex` independent of `saveMutex`; bytes buffered (max 4 KiB) if FSM is SAVING/RESTORING |
| Two tabs to same runId | Both share one Session; one gets `409 busy` |
| User types `save` / `restore` in terminal | Forwarded normally |
| Restart on `running` run | Preempt, kill, new run, old marked `finished`, redirect |
| Delete on `running` run | Preempt, kill, delete storage prefix + SQLite row |

---

## 7. Server and Routing Design

### 7.1 Go 1.22 `ServeMux` patterns — critical

Go 1.22's `net/http.ServeMux` supports method-scoped patterns and path wildcards. AIFStudio uses them directly — **no custom router**.

```go
func (s *Server) SetupRoutes() *http.ServeMux {
    mux := http.NewServeMux()

    // Health (allow-listed)
    mux.HandleFunc("GET /health", s.handleHealth)

    // Auth (allow-listed: register/login; authenticated: logout/me)
    mux.HandleFunc("POST /api/auth/register", s.handleAuthRegister)
    mux.HandleFunc("POST /api/auth/login",    s.handleAuthLogin)
    mux.HandleFunc("POST /api/auth/logout",   s.handleAuthLogout)
    mux.HandleFunc("GET /api/auth/me",        s.handleAuthMe)

    // Public app metadata (allow-listed)
    mux.HandleFunc("GET /api/config", s.handleConfig)

    // IFDB
    mux.HandleFunc("GET /api/ifdb/search",      s.handleIFDBSearch)
    mux.HandleFunc("GET /api/ifdb/games/{id}",  s.handleIFDBGame)

    // Runs
    mux.HandleFunc("POST /api/runs",                   s.handleCreateRun)
    mux.HandleFunc("GET /api/runs/by-user",            s.handleRunsByUser)
    mux.HandleFunc("GET /api/runs/{id}",               s.handleGetRun)
    mux.HandleFunc("GET /api/runs/{id}/start",         s.handleRunStart)
    mux.HandleFunc("POST /api/runs/{id}/command",      s.handleRunCommand)
    mux.HandleFunc("POST /api/runs/{id}/suspend",      s.handleRunSuspend)
    mux.HandleFunc("POST /api/runs/{id}/save",         s.handleSaveRun)
    mux.HandleFunc("POST /api/runs/{id}/restart",      s.handleRestartRun)
    mux.HandleFunc("DELETE /api/runs/{id}",            s.handleDeleteRun)
    mux.HandleFunc("GET /api/runs/{id}/transcript",    s.handleRunTranscript)

    // Projects, builds, AI (all behind sessionAuthRequired)
    mux.HandleFunc("POST /api/projects",                            s.handleCreateProject)
    mux.HandleFunc("GET /api/projects/{id}",                        s.handleGetProject)
    mux.HandleFunc("GET /api/projects/{id}/source",                 s.handleGetProjectSource)
    mux.HandleFunc("PUT /api/projects/{id}/source",                 s.handleUpdateProjectSource)
    mux.HandleFunc("POST /api/projects/{id}/builds",                s.handleCreateBuild)
    mux.HandleFunc("GET /api/projects/{id}/builds/{buildId}",       s.handleGetBuild)
    // (additional /api/projects/{id}/ai/* and /community routes omitted for brevity)

    // UI pages
    mux.HandleFunc("GET /{$}",               s.handleIndex)    // exact-match root — see §7.1 note
    mux.HandleFunc("GET /login",             s.handlePageLogin)
    mux.HandleFunc("GET /register",          s.handlePageRegister)
    mux.HandleFunc("GET /history",           s.handlePageHistory)
    mux.HandleFunc("GET /games/{ifdbId}",    s.handlePageGameDetail)
    mux.HandleFunc("GET /play/{runId}",      s.handlePagePlay)
    mux.HandleFunc("GET /create",            s.handlePageCreate)
    mux.HandleFunc("GET /projects",          s.handlePageProjects)
    mux.HandleFunc("GET /projects/{id}",     s.handlePageProjectDetail)
    mux.HandleFunc("GET /community",         s.handlePageCommunity)

    // Static
    mux.Handle("GET /static/", http.StripPrefix("/static/",
        http.FileServer(http.FS(templates.StaticFS))))

    return mux
}
```

**Index route MUST be `GET /{$}` (exact), not `GET /`.** `GET /` is a prefix pattern that swallows 405s on method-mismatches against API routes. A test in `routes_test.go` asserts that `POST /api/runs/bad-id` (wrong method) returns `405`, not 404 or the index HTML.

### 7.2 Server struct

```go
type Server struct {
    cfg     *config.Config
    store   store.Store
    ifdb    *ifdb.Client
    runner  *runner.Manager
    builder *build.Manager
    auth    *auth.SessionAuth        // local session auth (replaces *auth.Verifier)
    tmpl    *TemplateSet
}

func New(cfg *config.Config, st store.Store, ifdbClient *ifdb.Client,
         runner *runner.Manager, builder *build.Manager, a *auth.SessionAuth) *Server
```

### 7.3 Middleware chain

Applied outside → in:

1. `recoverMiddleware` — `defer recover()`, log stack, write `500`.
2. `requestIDMiddleware` — generate `X-Request-ID` if absent.
3. `loggingMiddleware` — structured JSON to stdout.
4. `corsMiddleware` — same-origin only; sets `Vary: Origin`; no `Access-Control-Allow-Origin: *`.
5. `maxBodyMiddleware` — `http.MaxBytesReader` 1 MiB for `PUT /api/projects/{id}/source`; 64 KiB for other JSON endpoints. `POST /command` additionally enforces 4096-char `text` cap in the handler.
6. `sessionAuthRequired` — see §7.4.

Applied to the mux as a single wrap stack at server start.

### 7.4 Authentication — `sessionAuthRequired`

```go
// sessionAuthRequired reads the aifstudio_session cookie, validates it against
// the sessions table, and injects *auth.User into context. Allow-listed paths
// pass through. On failure, JSON 401 for /api/* paths; 303 redirect to /login
// for HTML page routes.
func (s *Server) sessionAuthRequired(next http.Handler) http.Handler
```

**Allow-list** (no session required):
- `GET /health`
- `GET /login`
- `GET /register`
- `GET /api/config`
- `POST /api/auth/register`
- `POST /api/auth/login`
- `GET /static/*` (prefix match)
- `GET /favicon.ico` (if present)

**Validation:**
1. Read `aifstudio_session` cookie. Missing → fail.
2. `store.GetSession(ctx, sessionID)` → row or nil. Nil → fail.
3. `now > expires_at` → fail (also async-delete the row).
4. `store.GetUserByEmail` is **not** used here — fetch the user via `store.GetUserByID` (added in §9.2). Inject `*auth.User`.

**Failure behavior:**
- `r.URL.Path` starts with `/api/` → `writeError(w, 401, "auth_required", "authentication required")`.
- Otherwise (page route) → `http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)`.

`auth.SessionAuth` is the struct that owns the auth surface (Register/Login/Logout/Verify); the middleware delegates to it. `auth.User` has unchanged shape (`{UID, Email, Name}`), and `auth.UserFromContext(ctx)` / `auth.WithUser(ctx, *User)` continue to work — handlers do not need to change to accommodate the new auth backend.

The `?token=` query parameter on `POST /api/runs/{id}/suspend` is **removed** — `navigator.sendBeacon` sends the session cookie automatically.

### 7.5 Templates

```go
//go:embed *.html
var HTMLFS embed.FS

//go:embed static/*
var StaticFS embed.FS
```

`TemplateSet` parses once at `Server.New()`. All pages extend `layout.html` via `{{define "content"}}…{{end}}`.

---

## 8. Docker Compose Stack

The entire production deployment is one `docker-compose.yml` file at the repo root:

```yaml
services:
  aifstudio:
    build:
      context: ./service
      dockerfile: Dockerfile
    image: aifstudio:latest
    container_name: aifstudio
    ports:
      - "8080:8080"
    env_file: .env
    volumes:
      - ./data/db:/app/data/db
      - ./data/storage:/app/data/storage
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:8080/health"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
```

### 8.1 Volumes

Two host directories are mounted into the container:

| Host path | Container path | Contents | Notes |
|-----------|---------------|----------|-------|
| `./data/db` | `/app/data/db` | `aifstudio.db` (and SQLite WAL/SHM files) | One file per DB; survives container recreation |
| `./data/storage` | `/app/data/storage` | `sessions/`, `builds/`, `projects/` blobs | Survives container recreation |

`./data/` is gitignored (`/data/` line in `.gitignore`) and is created automatically by Docker Compose on first `up` if absent. The container's non-root `app` user owns everything inside.

### 8.2 `.env` file (mandatory)

`docker-compose.yml` references `env_file: .env` — Docker Compose **fails to start** if `.env` is missing at the repo root. There is no fallback; environment configuration is delivered exclusively through this file.

**Operator workflow:**

```bash
cp .env.example .env                # check in .env.example; never check in .env
$EDITOR .env                        # set OPENAI_API_KEY and any non-default values
docker compose up -d --build
```

**Source-of-truth files (committed):**

| File | Committed? | Purpose |
|------|-----------|---------|
| `.env.example` | ✅ yes | Reference template listing every env var the operator may set, with placeholder values, comments, and required/optional markers |
| `.env`         | ❌ never | Operator's actual values, including secrets like `OPENAI_API_KEY`. Listed in `.gitignore`. |

**Required variables** (operator MUST set or the service degrades):

| Var | Behaviour if unset / empty |
|-----|----------------------------|
| `OPENAI_API_KEY` | AI endpoints (`/api/projects/{id}/ai/*`) return `503 ai_disabled`; the rest of the app works normally. **Required for AI authoring.** |

**Recommended overrides** (defaults are sane but operators usually want to set explicitly):

| Var | Default | When to override |
|-----|---------|------------------|
| `ENVIRONMENT` | `local` | Set to `production` on the live host so the env banner reads correctly |
| `APP_VERSION` | `dev` | Set to the deployed image tag (e.g. `1.0.42`) so `/health` and the footer report the running version |
| `PORT` | `8080` | Change only if a host-side reverse proxy needs a different port |

**Storage paths** (rarely changed; defaults match the Compose bind mounts):

| Var | Default | Notes |
|-----|---------|-------|
| `DB_PATH` | `/app/data/db/aifstudio.db` | Inside the container — the `./data/db` volume mount makes this durable |
| `STORAGE_PATH` | `/app/data/storage` | Inside the container — backed by the `./data/storage` volume mount |

**Session and runtime tunables** are listed in full in §10 (Configuration). All have defaults; operators only override them when tuning a specific knob.

The complete `.env.example` template lives at `/.env.example` in this repo and MUST stay in sync with `service/internal/config/config.go` — every new env var added to `Config` MUST also appear in `.env.example` with a comment. CI does not currently enforce this; reviewers should flag drift.

### 8.3 Operations

| Command | Effect |
|---------|--------|
| `docker compose up -d --build` | Build the image and start the container in the background. First run creates `./data/`. |
| `docker compose logs -f aifstudio` | Tail structured JSON logs from the service. |
| `docker compose down` | Stop and remove the container; volumes (host bind mounts) persist. |
| `docker compose restart aifstudio` | Trigger SIGTERM drain (§12) and a clean restart. |

### 8.4 Backup

Stop the container (`docker compose stop aifstudio`), then `tar czf aifstudio-backup-<date>.tar.gz ./data/`. Restore by extracting the tarball to the same location and starting the container. SQLite is in WAL mode; the backup must include `aifstudio.db`, `aifstudio.db-wal`, and `aifstudio.db-shm` (or stop cleanly first so WAL is checkpointed and only `aifstudio.db` is needed).

---

## 9. Store Interface

The `store` package wraps metadata (SQLite), blob persistence (local filesystem), and now also the auth surface (users, sessions) behind a single `Store` interface. There is **no separate `auth.Store` sub-interface** — the methods are added directly to `store.Store` so handlers and `auth.SessionAuth` can share the same dependency.

### 9.1 Removed types and methods

- **`store.SignedURL`** type — deleted. There are no signed URLs in the local filesystem.
- **`SignedReadURL`** method — deleted.
- **`SignedProjectSourceURL`** method — deleted.

Frontend / handler code that previously called `SignedReadURL` for transcripts / build artefacts now references AIFStudio same-origin paths instead — `GET /api/runs/{id}/transcript`, `GET /api/projects/{id}/builds/{buildId}/artifact`, `…/log` — and the handler streams the file out of `STORAGE_PATH` via `Store.DownloadBlob`.

### 9.2 Interface definition

```go
package store

import (
    "context"
    "io"
    "time"
)

type Source struct {
    Type        string // "ifdb" | "url" | "build"
    IFDBId      string
    Format      string
    ArtifactURL string
    BuildID     string
}

type Run struct {
    ID              string
    SourceType      string
    IFDBId          string
    Title           string
    Format          string
    ArtifactURL     string
    BuildID         string
    UserID          string
    Status          string
    CreatedAt       time.Time
    StartedAt       *time.Time
    FinishedAt      *time.Time
    LastActiveAt    *time.Time
    ExitCode        *int
    TranscriptPath  string
    ErrorCode       string
    ErrorMessage    string
    Interpreter     string
    StoryPath       string
    SavePath        string
    TurnCount       int
    LastSaveAt      *time.Time
    ReconnectCount  int
    CandidateURLs   []string
    ProjectID       string
}

type Project struct {
    ID            string
    OwnerUID      string
    Name          string
    Source        string `json:"-"` // populated by GetProjectSource only; never serialized
    Description   string
    CreatedAt     time.Time
    UpdatedAt     time.Time
    LatestBuildID string
    Published     bool
    PublishedAt   *time.Time
}

type AITurn struct {
    ID               string
    ProjectID        string
    OwnerUID         string
    Kind             string
    UserMessage      string
    AssistantReply   string
    SourceBefore     string
    SourceAfter      string
    ModelRequestedAt time.Time
    ModelFinishedAt  time.Time
    PromptTokens     int
    CompletionTokens int
    Model            string
    Error            string
}

type Build struct {
    ID             string
    ProjectID      string
    OwnerUID       string
    Status         string
    CreatedAt      time.Time
    StartedAt      *time.Time
    FinishedAt     *time.Time
    ArtifactFormat string
    ArtifactPath   string
    LogPath        string
    ErrorMessage   string
}

type CachedGame struct {
    TUID      string
    Payload   []byte
    FetchedAt time.Time
    ExpiresAt time.Time
}

// Store is the full persistence surface used by handlers and SessionAuth.
// SQLiteStore (sqlite.go + local_blob.go) is the production implementation.
type Store interface {
    // --- Auth: users ---
    // CreateUser inserts a new user. The bcrypt hash is provided by the caller
    // (auth.SessionAuth) so the store doesn't need to know hashing parameters.
    CreateUser(ctx context.Context, u *auth.User, passwordHash string) error
    // GetUserByEmail returns the user and bcrypt password hash. Returns
    // (nil, "", nil) when the email is not found — never returns sql.ErrNoRows.
    GetUserByEmail(ctx context.Context, email string) (*auth.User, string, error)
    // GetUserByID returns the user (no password hash). Returns (nil, nil) when
    // not found.
    GetUserByID(ctx context.Context, uid string) (*auth.User, error)

    // --- Auth: sessions ---
    CreateSession(ctx context.Context, s *auth.Session) error
    // GetSession returns the session row or (nil, nil) when not found OR expired.
    GetSession(ctx context.Context, sessionID string) (*auth.Session, error)
    DeleteSession(ctx context.Context, sessionID string) error
    // DeleteExpiredSessions sweeps sessions with expires_at <= now. Returns
    // the count deleted. Called periodically from a background goroutine.
    DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error)

    // --- Runs ---
    CreateRun(ctx context.Context, r *Run) error
    GetRun(ctx context.Context, id string) (*Run, error)
    UpdateRun(ctx context.Context, r *Run) error
    DeleteRun(ctx context.Context, id string) error
    DeleteAbandonedPendingRuns(ctx context.Context, before time.Time) (int, error)
    ListRunsByUser(ctx context.Context, userID string, limit int) ([]*Run, error)
    ListRunsByProject(ctx context.Context, projectID string, limit int) ([]*Run, error)
    DeleteRunsForProject(ctx context.Context, projectID string) (int, error)

    // --- Projects ---
    CreateProject(ctx context.Context, p *Project) error
    GetProject(ctx context.Context, id string) (*Project, error)
    UpdateProjectSource(ctx context.Context, id, source string, updatedAt time.Time) error
    UpdateProjectMeta(ctx context.Context, id, name, description string, updatedAt time.Time) error
    UpdateProjectLatestBuild(ctx context.Context, id, buildID string) error
    ListProjectsByOwner(ctx context.Context, ownerUID string, limit int) ([]*Project, error)
    GetProjectSource(ctx context.Context, projectID string) (string, error)
    PutProjectSource(ctx context.Context, projectID, source string, updatedAt time.Time) error
    DeleteProjectSource(ctx context.Context, projectID string) error
    GetProjectSourceSize(ctx context.Context, projectID string) (size int64, exists bool, err error)
    UpdateProjectAI(ctx context.Context, p *Project, turn *AITurn) (time.Time, error)
    SetProjectPublished(ctx context.Context, projectID string, published bool, now time.Time) error
    ListPublishedProjects(ctx context.Context, limit int) ([]*Project, error)
    DeleteProject(ctx context.Context, id string) error

    // --- AI conversation ---
    CreateAITurn(ctx context.Context, t *AITurn) error
    ListAIConversation(ctx context.Context, projectID string, limit int) ([]*AITurn, error)
    DeleteAIConversation(ctx context.Context, projectID string) (int, error)
    GetAITurnAfterSource(ctx context.Context, projectID, turnID string) (string, bool, error)

    // --- Builds ---
    CreateBuild(ctx context.Context, b *Build) error
    GetBuild(ctx context.Context, id string) (*Build, error)
    UpdateBuild(ctx context.Context, b *Build) error
    ListBuildsByProject(ctx context.Context, projectID string, limit int) ([]*Build, error)
    DeleteBuildsForProject(ctx context.Context, projectID string) (int, error)

    // --- IFDB cache ---
    GetCachedGame(ctx context.Context, tuid string) (*CachedGame, error) // nil,nil if absent/expired
    PutCachedGame(ctx context.Context, g *CachedGame) error
    ListFreshCachedGames(ctx context.Context, now time.Time) ([]*CachedGame, error)

    // --- Blob storage (local filesystem under STORAGE_PATH) ---
    UploadBlob(ctx context.Context, path, contentType string, r io.Reader) error
    DownloadBlob(ctx context.Context, path string, w io.Writer) error
    // DeleteBlobPrefix removes every file under the given path prefix. Returns
    // the count deleted. Concurrency is bounded but unimportant for a local FS
    // (the bottleneck is syscalls, not network).
    DeleteBlobPrefix(ctx context.Context, prefix string) (int, error)

    // --- Lifecycle ---
    Close() error
}
```

### 9.3 `auth.User` and `auth.Session`

The auth package owns the data structures used at the API boundary:

```go
package auth

import "time"

type User struct {
    UID       string    // "u-<ULID>"
    Email     string    // lowercased
    Name      string    // display name
    CreatedAt time.Time
}

type Session struct {
    ID        string    // 43-char base64url
    UserID    string
    CreatedAt time.Time
    ExpiresAt time.Time
}
```

`auth.WithUser(ctx, *User)` and `auth.UserFromContext(ctx) *User` continue to work, so `handlers_*` files that read the current user via `auth.UserFromContext` need no behavioural changes — only the wiring at the middleware boundary changes.

### 9.4 `SQLiteStore` implementation notes

- Constructor: `NewSQLiteStore(ctx context.Context, dbPath string, blob BlobStore) (*SQLiteStore, error)`. The blob backend is injected — production wires `LocalBlobStore{Root: cfg.StoragePath}`; tests use a tempdir or an in-memory map.
- Open with `sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")`.
- WAL mode is mandatory — the runner does many concurrent UpdateRun calls; rollback-journal mode would block readers during writes.
- Schema applied on open via the canonical `CREATE TABLE IF NOT EXISTS` block in §5.2.
- All time values stored as RFC 3339 UTC strings via `t.UTC().Format(time.RFC3339Nano)` and parsed via `time.Parse(time.RFC3339Nano, s)`.
- All bulk operations use **single SQL statements** with `IN (...)` or filtered `DELETE`. There is no concept of `BulkWriter` for SQLite — a single `DELETE` traverses all matching rows transactionally. (The `BulkWriter` rule from the global standards exists to forbid the deprecated Firestore `Batch()` API; SQLite has no equivalent footgun.)

### 9.5 `LocalBlobStore` implementation notes

- Configured with a single `Root` path (e.g. `/app/data/storage`).
- `UploadBlob(path, contentType, r)` → `os.MkdirAll(filepath.Dir(joinedPath), 0o755)` → `os.WriteFile` (atomic via tempfile + rename to avoid torn writes during SIGTERM).
- `DownloadBlob(path, w)` → `os.Open` → `io.Copy(w, f)`. Returns `os.IsNotExist`-marshalled errors as `ErrBlobNotFound` so handlers can map to 404.
- `DeleteBlobPrefix(prefix)` → `filepath.WalkDir` under `joinedPath`, `os.Remove` each file, finally `os.RemoveAll(joinedPath)` to clean directories. Concurrency: serial. Local FS bottleneck is syscalls; the prior 10-concurrency cap was chosen for GCS round-trip latency and is irrelevant here.
- **Path safety:** every input path is `filepath.Clean`-ed and verified to remain inside `Root` after `filepath.Join`. Reject `..`-traversal with `ErrInvalidPath`. Tests must enumerate adversarial inputs.

### 9.6 Test double

`MockStore` (in `store_test.go`) uses in-memory maps for every collection plus a `map[string][]byte` for blobs. It implements every method on `Store`, including the auth surface, so handler tests do not need a separate auth store mock.

---

## 10. Configuration

Loaded from env vars at startup. `config.Load()` returns `(*Config, error)`; parse errors fail-fast. Loading never opens SQLite or touches the filesystem.

| Field | Env Var | Type | Default | Description |
|-------|---------|------|---------|-------------|
| `Port` | `PORT` | string | `"8080"` | HTTP listen port |
| `Version` | `APP_VERSION` | string | `"dev"` | Version from build |
| `Environment` | `ENVIRONMENT` | string | `"local"` | `local` / `production` (etc.) |
| `DBPath` | `DB_PATH` | string | `"/app/data/db/aifstudio.db"` | SQLite database file path |
| `StoragePath` | `STORAGE_PATH` | string | `"/app/data/storage"` | Root of local blob storage |
| `SessionMaxAge` | `SESSION_MAX_AGE` | duration | `"720h"` | 30 d. Cookie max-age and `sessions.expires_at` offset |
| `IFDBBaseURL` | `IFDB_BASE_URL` | string | `"https://ifdb.org"` | Overridable for tests |
| `IFDBUserAgent` | `IFDB_USER_AGENT` | string | `"AIFStudio/0.1 (contact: vpoluyaktov@gmail.com)"` | |
| `IFDBCacheTTL` | `IFDB_CACHE_TTL` | duration | `"10m"` | |
| `IFDBRateLimitQPS` / `Burst` | `IFDB_RATE_LIMIT_QPS` / `_BURST` | float / int | `5` / `10` | Global bucket |
| `IFDBRateLimitPerIPQPS` / `Burst` | `IFDB_RATE_LIMIT_PER_IP_*` | float / int | `1` / `3` | Per-IP bucket |
| `RunSessionMax` | `RUN_SESSION_MAX` | duration | `"30m"` | Hard lifetime cap |
| `RunIdleTimeout` | `RUN_IDLE_TIMEOUT` | duration | `"10m"` | Idle-sweep threshold |
| `DownloadSizeLimitBytes` | `DOWNLOAD_SIZE_LIMIT_BYTES` | int64 | `52428800` | 50 MiB |
| `ExtractSizeLimitBytes` | `EXTRACT_SIZE_LIMIT_BYTES` | int64 | `104857600` | 100 MiB |
| `ExtractFileLimit` | `EXTRACT_FILE_LIMIT` | int | `100` | Max files per archive |
| `BuildTimeout` | `BUILD_TIMEOUT` | duration | `"5m"` | Inform 7 compile cap |
| `SaveQuiescenceMs` | `SAVE_QUIESCENCE_MS` | int | `200` | Stdout silence = at-prompt |
| `SaveTimeoutMs` | `SAVE_TIMEOUT_MS` | int | `1000` | Inline-save wait |
| `RestoreTimeoutMs` | `RESTORE_TIMEOUT_MS` | int | `2000` | Restore completion |
| `ShutdownDrainTimeout` | `SHUTDOWN_DRAIN_TIMEOUT` | duration | `"8s"` | SIGTERM drain budget |
| `HistoryDefaultLimit` | `HISTORY_DEFAULT_LIMIT` | int | `20` | |
| `AbandonedPendingTTL` | `ABANDONED_PENDING_TTL` | duration | `"1h"` | Orphan-pending age |
| `AbandonedSweepInterval` | `ABANDONED_SWEEP_INTERVAL` | duration | `"15m"` | Orphan sweep cadence |
| `OpenAIAPIKey` | `OPENAI_API_KEY` | string | `""` | Empty disables AI endpoints (503) |
| `OpenAIModel` | `OPENAI_MODEL` | string | `"gpt-5.2"` | |
| `OpenAIBaseURL` | `OPENAI_BASE_URL` | string | `"https://api.openai.com/v1"` | |
| `OpenAITimeout` | `OPENAI_TIMEOUT` | duration | `"300s"` | |
| `AIMaxTurnsPerProject` | `AI_MAX_TURNS_PER_PROJECT` | int | `200` | |
| `AIRateLimitPerUserQPS` / `Burst` | `AI_RATE_LIMIT_PER_USER_*` | float / int | `0.2` / `3` | |
| `AIMaxDescriptionChars` | `AI_MAX_DESCRIPTION_CHARS` | int | `2000` | |
| `AIMaxMessageChars` | `AI_MAX_MESSAGE_CHARS` | int | `16000` | |

**Removed in the replatform** (do not reintroduce):
- `GCP_PROJECT_ID`
- `FIRESTORE_DATABASE_NAME`
- `GCS_BUCKET`
- `FIREBASE_WEB_API_KEY`
- `FIREBASE_AUTH_DOMAIN`
- `SOURCE_SIGNED_URL_TTL`
- `USER_COOKIE_MAX_AGE`

---

## 11. Build and Operations

### 11.0 Two-image build pattern (base + app)

The container image is split into **two** Dockerfiles to keep per-commit CI under two minutes:

| Image | Source | Tag(s) on Docker Hub | Rebuild trigger |
|-------|--------|----------------------|-----------------|
| **Base** (toolchain) | `docker/base.Dockerfile` | `vpoluyaktov/aifstudio-base:latest`, `vpoluyaktov/aifstudio-base:frobtads-v2.0-inform7-10.1.2` | Manual (`workflow_dispatch`) or push to `main` that touches `docker/base.Dockerfile` / `docker/inform7-wrapper.sh` |
| **App** (Go binary) | `service/Dockerfile` (FROM the base image) | `vpoluyaktov/aifstudio:latest`, `vpoluyaktov/aifstudio:<MAJOR.MINOR.commitcount>` | Every push to `main` and every `v*` tag |

**Why split.** The base layer compiles `frob` (TADS interpreter) from source and downloads + extracts the Inform 7 .deb — together ~10–15 minutes. That toolchain changes rarely (only when the user bumps `FROBTADS_TAG` or `INFORM7_DEB_URL`); the Go binary changes on every commit. Building both in one Dockerfile would re-run the toolchain layer on every PR, blowing the CI budget. The split lets the per-commit pipeline finish in under two minutes by pulling the cached base image layer from Docker Hub.

**Toolchain tag invariant.** The immutable tag in `docker/base.Dockerfile` (encoded as `frobtads-<TAG>-inform7-<VERSION>`) MUST match:

1. The `FROBTADS_TAG` and `INFORM7_DEB_URL` ARGs in `docker/base.Dockerfile`.
2. The tag list in `.github/workflows/base-image.yml`.
3. The `FROM vpoluyaktov/aifstudio-base:<tag>` line in `service/Dockerfile`.

When bumping the toolchain: update all three together in the same commit, then run `.github/workflows/base-image.yml` (manually or by touching one of the trigger paths) before the app workflow runs against `main`. The per-commit app build will fail with "manifest unknown" if the new tag has not been pushed.

**Repository layout:**

```
docker/
  base.Dockerfile          # Stage 2 (runtime): debian:bookworm-slim + IF toolchain
  inform7-wrapper.sh       # /usr/local/bin/inform7 shim, baked into the base image
service/
  Dockerfile               # Stage 1: Go builder; Stage 2: FROM the base image + binary
  ...                      # Go source
.github/workflows/
  base-image.yml           # Builds and pushes vpoluyaktov/aifstudio-base
  main.yml                 # Lints, tests, builds, pushes vpoluyaktov/aifstudio
```

The build context for `docker/base.Dockerfile` is the `docker/` directory (so `COPY inform7-wrapper.sh` reads from `docker/inform7-wrapper.sh`). The build context for `service/Dockerfile` remains the `service/` directory (the Go module).

### 11.1 Dockerfiles (two-image, multi-stage)

The runtime image derives from `vpoluyaktov/aifstudio-base` on Docker Hub — it does not install the IF toolchain on every build. Per-commit CI only runs the Go-builder stage and copies the binary on top of the pre-baked base.

**`docker/base.Dockerfile` (toolchain layer):**

```dockerfile
# syntax=docker/dockerfile:1.7
FROM debian:bookworm-slim

ARG FROBTADS_TAG=v2.0
ARG INFORM7_DEB_URL="https://github.com/ganelson/inform/releases/download/v10.1.2/inform7-ide_2.0.0-1_amd64.deb"
ARG INFORM7_DEB_SHA256="2a238e3d2da7b583334cc2cfa4fd88eda6d44b83d8ba8117c0664e0740b6ac40"

RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates tzdata curl wget tar xz-utils unzip bash \
        frotz glulxe inform6-compiler inform6-library; \
    # frobtads v2.0 uses CMake; v1.x ./bootstrap script is gone.
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        build-essential cmake pkg-config \
        libcurl4-openssl-dev libncurses-dev git; \
    git clone --branch "${FROBTADS_TAG}" --depth 1 \
        https://github.com/realnc/frobtads.git /tmp/frobtads; \
    cd /tmp/frobtads && cmake -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr/local . && make -j"$(nproc)" && make install; \
    cd / && rm -rf /tmp/frobtads; \
    wget -O /tmp/inform7.deb "${INFORM7_DEB_URL}"; \
    echo "${INFORM7_DEB_SHA256}  /tmp/inform7.deb" | sha256sum -c; \
    mkdir -p /tmp/inform7-extract /opt/inform7/bin /usr/local/share/inform7; \
    dpkg-deb -x /tmp/inform7.deb /tmp/inform7-extract; \
    cp /tmp/inform7-extract/usr/lib/x86_64-linux-gnu/inform7-ide/inform7 /opt/inform7/bin/inform7; \
    cp /tmp/inform7-extract/usr/lib/x86_64-linux-gnu/inform7-ide/inform6 /opt/inform7/bin/inform6; \
    cp -r /tmp/inform7-extract/usr/share/inform7-ide/. /usr/local/share/inform7/; \
    rm -f "/usr/local/share/inform7/Extensions/Emily Short/Skeleton Keys.i7x"; \
    rm -rf /tmp/inform7.deb /tmp/inform7-extract; \
    apt-get purge -y --auto-remove \
        build-essential cmake pkg-config \
        libcurl4-openssl-dev libncurses-dev git; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        libcurl4 libncurses6 libtinfo6; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*

# Wrapper baked into the base — service/Dockerfile does NOT re-COPY it.
COPY inform7-wrapper.sh /usr/local/bin/inform7
RUN chmod +x /usr/local/bin/inform7

# Sanity check + non-root user + /app/data dirs (see source for full text).
RUN groupadd -r app && useradd -r -g app -M -d /nonexistent -s /usr/sbin/nologin app
RUN mkdir -p /app/data/db /app/data/storage && chown -R app:app /app/data
```

**`service/Dockerfile` (per-commit, fast):**

```dockerfile
# syntax=docker/dockerfile:1.7

# ─── Stage 1: Go builder ─────────────────────────────────────────────────────
FROM golang:1.23-alpine AS gobuilder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/aifstudio .

# ─── Stage 2: Runtime ────────────────────────────────────────────────────────
FROM vpoluyaktov/aifstudio-base:frobtads-v2.0-inform7-10.1.2

COPY --from=gobuilder /out/aifstudio /usr/local/bin/aifstudio

USER app
ENV PORT=8080 \
    TMPDIR=/tmp \
    DB_PATH=/app/data/db/aifstudio.db \
    STORAGE_PATH=/app/data/storage \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/games
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["/usr/local/bin/aifstudio"]
```

`PATH` includes `/usr/games` because Debian installs `frotz`, `dfrotz`, and `glulxe` there. `frob` goes to `/usr/local/bin/frob`. The `app` user, `/app/data` directories, and the `/usr/local/bin/inform7` wrapper are inherited from the base image and MUST NOT be redeclared in `service/Dockerfile`.

**Runtime filesystem:**
- `/tmp` — ephemeral per-session story/save files at `/tmp/runs/<runId>/`; Inform 7 build sandboxes at `/tmp/build/<buildId>/`.
- `/app/data/db` — SQLite database file.
- `/app/data/storage` — durable blob storage (`sessions/`, `builds/`, `projects/`).

**Interpreter selection:**

| Extension | Interpreter | Command |
|-----------|-------------|---------|
| `.z3`–`.z8`, `.zblorb` | `dfrotz` | `dfrotz -p -q -m -w 255 <file>` |
| `.ulx`, `.gblorb` | `glulxe` | `glulxe <file>` |
| `.gam` (TADS 2) / `.t3` (TADS 3) | `frob` | `frob -i plain <file>` |

### 11.2 CI/CD — GitHub Actions (lint, test, build, publish to Docker Hub)

There are **two** workflow files, in line with the two-image pattern (§11.0):

| Workflow | File | Trigger | Output |
|----------|------|---------|--------|
| **App** | `.github/workflows/main.yml` | every push and PR (lint+test); push to `main` and `v*` tags also push the image | `vpoluyaktov/aifstudio:<MAJOR.MINOR.commitcount>` and `:latest` |
| **Base image** | `.github/workflows/base-image.yml` | `workflow_dispatch` (manual) or push to `main` that touches `docker/base.Dockerfile` / `docker/inform7-wrapper.sh` | `vpoluyaktov/aifstudio-base:frobtads-v2.0-inform7-10.1.2` and `:latest` |

There is no Terraform, no GCP, no GKE, no smoke test against a live URL — image publication to Docker Hub is the entire delivery surface. Operators pull `vpoluyaktov/aifstudio:<version>` from Docker Hub and run `docker compose up -d` on the target host (the Compose file references `image: aifstudio:latest` for local builds; production deployments swap the `build:` block for `image: vpoluyaktov/aifstudio:<version>`).

**Pipeline stages (in order):**

```
lint  →  test  →  docker build  →  docker push
```

**Triggers.** Push to any branch and pull requests run `lint` + `test`. Only pushes to `main` (and tag pushes matching `v*`) additionally run `docker build` and `docker push`. Pull requests never push images. Per-branch concurrency groups serialize runs to keep image tagging deterministic.

**Required GitHub repository secrets:**

| Secret | Source | Used by |
|--------|--------|---------|
| `DOCKERHUB_USERNAME` | Docker Hub account name (`vpoluyaktov`) | `docker/login-action` |
| `DOCKERHUB_TOKEN`    | Docker Hub access token (Account Settings → Security → New Access Token; scope: Read/Write/Delete) | `docker/login-action` |

`OPENAI_API_KEY` is **not** a GitHub secret — it is delivered to runtime via the operator's `.env` file (§8.2), not baked into the image.

**Job 1 — `lint-and-test`** (all triggers — push, PR, tag):

| Step | Action |
|------|--------|
| Checkout | `actions/checkout@v4` |
| Setup Go | `actions/setup-go@v5` with `go-version: '1.23'` |
| Cache modules | `actions/cache@v4` keyed on `go.sum` |
| `go mod download` | in `./service` |
| `go vet ./...` | in `./service` |
| `golangci-lint` | `golangci/golangci-lint-action@v6`, `version: latest`, `working-directory: service` |
| `go test ./...` | in `./service`, with `-race` |

**Job 2 — `docker-build-and-push`** (push to `main`; push of `v*` tag) — depends on Job 1:

| Step | Action |
|------|--------|
| Checkout | `actions/checkout@v4` (`fetch-depth: 0` so VERSION + commit count is computable) |
| Compute version | Read `VERSION` (e.g. `1.0.0`); append commit count → `1.0.<count>`. On a `v*` tag push, use the tag's value verbatim. Export as `IMAGE_VERSION`. |
| Setup Buildx | `docker/setup-buildx-action@v3` |
| Login to Docker Hub | `docker/login-action@v3` with `username: ${{ secrets.DOCKERHUB_USERNAME }}` and `password: ${{ secrets.DOCKERHUB_TOKEN }}` |
| Build & push | `docker/build-push-action@v6` with `context: service`, `file: service/Dockerfile`, `push: true`, `tags: vpoluyaktov/aifstudio:latest, vpoluyaktov/aifstudio:${{ env.IMAGE_VERSION }}`, `cache-from: type=gha`, `cache-to: type=gha,mode=max`, `platforms: linux/amd64` (linux/arm64 is **deferred** — `frob` build-from-source on arm64 has not been validated; revisit when a real arm64 host is in scope) |
| Image digest summary | Write the resulting digest to `$GITHUB_STEP_SUMMARY` for traceability |

**Image tagging rules (normative):**

| Trigger | Tags pushed |
|---------|-------------|
| Push to `main` | `vpoluyaktov/aifstudio:latest` AND `vpoluyaktov/aifstudio:<MAJOR.MINOR.commitcount>` (e.g. `1.0.142`) |
| Tag push `v1.2.3` | `vpoluyaktov/aifstudio:latest` AND `vpoluyaktov/aifstudio:1.2.3` |
| Push to feature branch | (nothing — image build is `main`-and-tag-only) |
| Pull request | (nothing) |

`latest` is overwritten on every successful `main` build; the immutable tag is the version-specific one. Operators MUST pin to a specific version in production (`image: vpoluyaktov/aifstudio:1.0.142`); `:latest` is acceptable for staging/eval only.

**No deployment step.** CI does not SSH into a host, does not call `docker compose up`, and does not connect to any environment. Delivery ends at "image is on Docker Hub". The operator triggers the deploy by pulling and restarting Compose on the target host.

### 11.3 GitHub Actions versions (mandatory)

Per `~/.claude/rules/template-standards.md`. Every reference in `.github/workflows/main.yml` MUST use these Node.js 24-compatible versions:

| Action | Version |
|--------|---------|
| `actions/checkout` | `@v4` |
| `actions/setup-go` | `@v5` |
| `actions/cache` | `@v4` |
| `golangci/golangci-lint-action` | `@v6` |
| `docker/setup-buildx-action` | `@v3` |
| `docker/login-action` | `@v3` |
| `docker/build-push-action` | `@v6` |

`google-github-actions/auth`, `google-github-actions/setup-gcloud`, and `hashicorp/setup-terraform` are **not** referenced (no GCP/Terraform in CI).

---

## 12. Graceful Shutdown

`main.go` installs a SIGTERM/SIGINT handler:

1. Stop accepting new HTTP connections.
2. Wait up to 2 s for in-flight `POST /command` handlers to drain.
3. `runner.Manager.Drain(ctx)` with `SHUTDOWN_DRAIN_TIMEOUT` budget (default 8 s).
4. `Drain` iterates resident sessions and calls `Session.SaveAndClose()` in parallel (max 10 concurrent).
5. `SaveAndClose()`: acquire save mutex → save (§6.6, 3 s budget) → `status = "suspended"` → SIGTERM → SIGKILL after 5 s → release.
6. `httpServer.Shutdown(shutCtx)` (30 s ceiling).
7. Process exits; `Store.Close()` runs in deferred order, closing the SQLite DB cleanly so WAL is checkpointed.

Per-session save budget is 3 s; 10 concurrent saves fit in the 8 s default drain budget. Configurable via `SHUTDOWN_DRAIN_TIMEOUT`.

`docker compose stop` sends SIGTERM and waits 10 s by default before SIGKILL — set `--timeout 30` if a longer drain is needed.

No client notification — POST transport is request-scoped. Next `POST /command` on a restarted container returns `409 not_started`; browser re-invokes `/start` (RESUME) and play continues with ~650 ms added once.

---

## 13. Algorithm and Logic Edge Cases

These are normative — tests must exercise each case.

### 13.1 IFDB in-memory cache

- Cold start: empty; first request populates both memory and the SQLite `ifdb_cache` table.
- Expiry boundary: `expires_at == now` is **expired** (strict `>` for freshness). Cache hits carry `X-Cache: HIT`.
- Concurrent same-key fetches: `singleflight.Group` keyed by TUID.
- Negative results: IFDB 404 → cache with `expires_at = now + 60s`, respond `404` from cache.

### 13.2 Token-bucket rate limiter

- Two buckets: global (`IFDB_RATE_LIMIT_QPS`/`BURST`) and per-IP (`PER_IP_*`).
- IP from `X-Forwarded-For` first entry else `RemoteAddr`. Empty → `RemoteAddr`.
- Atomic: must acquire from BOTH buckets; if either denies, respond 429 without consuming the other.
- Continuous refill (`golang.org/x/time/rate`).

### 13.3 Artifact download

- Content-Length present and > 50 MiB → 413 before reading.
- Content-Length absent → `io.LimitReader(resp.Body, 50 MiB + 1)`; over-limit → 413.
- Content-Length lies → LimitReader still caps → 413.
- Zero-byte → 400 `artifact empty`.
- Non-2xx upstream → `/start` returns `503 upstream_unavailable`.

### 13.4 Zip bomb protection

- Entry count cap 100; over → `archive_too_many_files`.
- Cumulative uncompressed size cap 100 MiB; over → abort.
- Per-entry size cap 50 MiB.
- Path traversal (cleaned name `..` or absolute) → `archive_invalid_path`.
- 0 entries → `archive_empty`.
- Single-file archive → extract to `/tmp/runs/<runId>/story.<ext>`.
- Multi-file → prefer largest known-IF-extension file; tie → archive order.
- No known IF extension → `unsupported_format`.

`.zblorb` / `.gblorb` are IFF containers (not zip), detected by magic bytes (`FORM` + `IFRS`) and passed directly to the interpreter.

**TADS via zip:** `.gam` and `.t3` count as known IF extensions for the largest-file rule.

### 13.5 Subprocess lifecycle

(Unchanged.) Spawn failure → `500 spawn_failed`; normal exit → `finished`; non-zero exit → `finished` with the exit code (many IF titles exit non-zero on normal end). Kill on `/suspend`/idle/hard sweep: synchronous save → SIGTERM → SIGKILL after 5 s. Kill on `/restart`/`DELETE`: preempt — SIGTERM → 2 s → SIGKILL.

### 13.6 Build queueing

(Unchanged.) One active build per project. Empty source → still runnable; Inform 7 emits compile error. Timeout (default 5 m) → kill, `failed`, `"build timed out after 5m"`. Artifact missing after success → `failed`.

### 13.7 Abandoned-pending sweep

```
every ABANDONED_SWEEP_INTERVAL (default 15 m):
  for each run where status == "pending" AND created_at < now - ABANDONED_PENDING_TTL:
    store.DeleteRun(ctx, run.ID)
```

**Only touches `pending`.**

### 13.8 Session sweep (NEW)

A second background goroutine ticks once per hour and calls `store.DeleteExpiredSessions(ctx, now)`. This bounds the size of the `sessions` table and runs entirely in SQL (`DELETE FROM sessions WHERE expires_at <= ?`).

### 13.9 Go regex behavior

No regex is used for algorithmic computation. Regex is used **only** for validators: `^r-[0-9A-Z]{26}$`, `^u-[0-9A-Z]{26}$`, `^[a-z0-9]{10,32}$`, `^p-[0-9A-Z]{26}$`, `^b-[0-9A-Z]{26}$`. Anchored; never match empty. If a future feature introduces a regex with a zero-width alternative (`\b`, `(foo)?`, `.*?`), the Backend developer MUST verify expected match counts on empty and minimal inputs with a local Go script before relying on it, and document here.

### 13.10 ULID generation

`github.com/oklog/ulid/v2` with `rand.Reader` entropy. 26 chars, Crockford base32. No dedup on insert.

### 13.11 TADS 2/3 edge cases

(Unchanged.) `knownFormats` includes `"gam"` and `"t3"`; `detectFormat` normalizes IFDB strings; `PreferredFormat` ordering is `["z5","z8","zblorb","ulx","gblorb","gam","t3"]`; `frob -i plain` mode handles both TADS 2 and TADS 3.

### 13.12 Devsys search filtering

(Unchanged — see §5.6 for the table.) Normalization: lowercase + trim. Empty → include. Unknown → include. Prefix match. Filter happens in `parseSearchResponse` before caching.

### 13.13 Path safety in `LocalBlobStore`

Every input path passed to `UploadBlob`, `DownloadBlob`, `DeleteBlobPrefix` is `filepath.Clean`-ed and verified to remain inside `Root` after `filepath.Join`. Adversarial inputs MUST be rejected:

| Input | Result |
|-------|--------|
| `"sessions/r-X/../../etc/passwd"` | `ErrInvalidPath` |
| `"/absolute/path"` | `ErrInvalidPath` |
| `"sessions/r-X/save.qut"` | OK (joined under Root) |
| `""` | `ErrInvalidPath` |

Tests exercise each row.

### 13.14 SQLite concurrency

WAL mode + `busy_timeout=5000` is sufficient for the runner's write pattern (UPDATE runs every command, every save). Tests reproduce a worst-case write storm (10 goroutines × 100 UPDATEs each) and assert no `database is locked` failures.

---

## 14. Storage Strategy & Speed Budget

Explicit constraint: **SQLite + local filesystem only. No Redis, no external cache.**

### 14.1 Role of each store

| Store | Role | Object sizes | Access pattern | RTT |
|-------|------|--------------|----------------|-----|
| SQLite `runs` | Session metadata | ~1–2 KB | Read on `/start`; write on save + command | < 1 ms |
| SQLite `runs WHERE user_id=?` | User history | N rows | `/history` load | 1–5 ms for N ≤ 20 |
| FS `sessions/<runId>/story.<ext>` | Story binary | 50 KB – 5 MB | Read once on `/start` cold; never rewritten | 5–30 ms (page cache) |
| FS `sessions/<runId>/game.sav` | Save (Quetzal/TADS) | 2–30 KB typical, up to ~100 KB | Read on `/start` resume; write on auto-save | 1–5 ms |
| FS `sessions/<runId>/transcript.txt` | Transcript | KB–MB | Read on resume; write on save triggers | 1–10 ms |

### 14.2 Cold-start turn latency budget

First `POST /command` after container restart:

| Step | Latency |
|------|---------|
| TLS + parse | 1–5 ms |
| SQLite `GetRun` | < 1 ms |
| Per-run mutex (uncontended) | <1 ms |
| FS read `story.<ext>` (500 KB) | 10–30 ms |
| FS read `game.sav` (10 KB) | 1–3 ms |
| Spawn interpreter | 50 ms |
| Restore + quiescence | 200 ms |
| Write input, read to quiescence | 100–300 ms |
| AutoSave (inline budget) | 5–20 ms |
| SQLite UPDATE | < 1 ms |
| JSON encode + response | 5 ms |
| **p50 cold turn** | **~400 ms** |
| **p95 cold turn** | ~800 ms |

Warm-turn latency: ~200–400 ms (interpreter CPU dominates).

The new local-FS substrate is materially faster than the prior GCS+Firestore path (no network round trips), which the team should verify post-cutover.

### 14.3 Why not store saves as BLOBs in SQLite?

Save files are 2–30 KB but transcripts grow to MB. Mixing large blobs into SQLite bloats the `.db` file and complicates backup. Filesystem objects are easier to inspect, tar up, and recover individually. The metadata/blob split is preserved for the same reasons it existed under Firestore/GCS.

### 14.4 What lives where

- **SQLite**: who exists, who owns what, where its bytes live.
- **Local filesystem under `STORAGE_PATH`**: the bytes themselves.
- **`/tmp` inside the container**: ephemeral cache while a session is live.
- **Nothing else.**

---

## 15. User Game History

Per-user history is a **view on top of `runs`** — no new table. Sessions persist indefinitely.

### 15.1 Model

| Concept | Implementation |
|---------|----------------|
| "User" | `users.id` (`u-<ULID>`), tied to email |
| "Game in my history" | `runs` row where `user_id == <mine>` |
| "Continue" | Navigate to `/play/<id>` → `/start` RESUME path |
| "Restart" | `POST /api/runs/<id>/restart` → new run, old marked `finished` |
| "Delete" | `DELETE /api/runs/<id>` → storage prefix + SQLite row removed |
| "Start new game" | `POST /api/runs` — `user_id` stamped from session |

### 15.2 History page contract (`GET /history`)

(Unchanged.) Fetches `GET /api/runs/by-user?limit=50` on load. Sections **Active** / **Finished** / **Problems**. Empty state: "You haven't played any games yet."

### 15.3 Delete confirmation (normative)

```
Delete "Zork I"?
This permanently removes your saved game, transcript, and history for
this session. The game itself is not affected — you can start a new one
anytime.

[Cancel]  [Delete]
```

### 15.4 Home page panel

Top 3 runs by `lastActiveAt`, rendered above the search bar. Click row = Continue.

### 15.5 Status badge display (`statusDisplay(run)` in `history.html`)

(Unchanged.) `suspended` + `lastActiveAt` within last 2 h → green **Playing**; older `suspended` → amber **Suspended**; `running` → green **Running**; `finished` → dim **Finished**; anything else → dim with raw label.

---

## 16. Design Decisions and Rationale

1. **Single SQLite file per environment** — covers users, sessions, runs, projects, builds, ai_turns, ifdb_cache. No external metadata service. Rejected: Postgres (operational overhead for a one-tenant deployment), file-per-table (loses transactional safety).
2. **Local filesystem for blobs** — story files, saves, transcripts, build artefacts. Mounted via Docker volume to survive container recycles. Rejected: bytes inside SQLite (mixes large blobs into the .db file, complicates backup), object storage (reintroduces a cloud dependency).
3. **No custom router** — Go 1.22's `ServeMux` is enough. Rejected: `chi`, `gorilla/mux`.
4. **POST+wait HTTP transport (3 endpoints)** — each command = one HTTP round trip; interpreter kept hot in the in-memory Manager. Rejected: WebSocket (Hijacker fragility, connection-coupled lifecycle, in-stream FSM bugs); SSE; long-polling; spawn-per-turn.
5. **In-process build queue** — builds only need to outlive a single request.
6. **Embedded templates + inline JS** — self-contained binary; no separate frontend build.
7. **Root route `GET /{$}`** — exact match so 405s reach clients on method-mismatches.
8. **Same-origin paths for artefact downloads** — transcripts, build artefacts, build logs are served by AIFStudio handlers that stream the file from `STORAGE_PATH`. The session cookie is the access control. Rejected: signed URLs (no third-party storage to sign against; same-origin streaming is simpler).
9. **Strict size/file limits** — mandatory because the runner accepts arbitrary HTTPS URLs.
10. **No ANSI parsing** — simpler and safer.
11. **Local session auth** — bcrypt + 32-byte random session tokens stored in SQLite; HttpOnly + SameSite=Strict cookie. Rejected: JWT (server-side state means a logout is fully revoked; no key rotation surface), Firebase (cloud dependency we are explicitly removing), OAuth providers (out of scope; deferred).
12. **`HttpOnly + SameSite=Strict` cookie** — eliminates XSS theft and full CSRF. SameSite=Strict is acceptable because the app has no inbound third-party links beyond email; no redirect flow is required.
13. **bcrypt cost 12** — ~250 ms on a typical CPU; high enough to make offline cracking expensive, low enough to keep login latency <300 ms.
14. **Inform 7 CLI bundled, not downloaded at runtime** — deterministic builds, no cold-start hit.
15. **Persistent sessions, no TTL** — simpler mental model; only non-user-initiated deletion is the abandoned-pending sweep and the expired-session sweep.
16. **Auto-save every turn** — text-adventure pacing absorbs the save round-trip; eliminates N-turn worst-case loss.
17. **Interpreter recorded in SQLite, not re-inferred from extension** — a `.zblorb` could be renamed; pairing is fixed and must not drift across resume cycles.
18. **In-band save/restore via typed commands** — neither `dfrotz` nor `glulxe` exposes a save API.
19. **Per-session `commandMutex` linearizes turns with `TryLock` → 409 busy** — guarantees single reader/writer on the subprocess; never ties up handler goroutines.
20. **TADS 2/3 via `frob` built from source, `-i plain` mode** — `frobtads` is not in Debian bookworm. Build deps installed + purged in the same RUN layer.
21. **Self-contained Dockerfile (no separate base image)** — replatforming away from a registry-pinned base. Build time per service rebuild is longer (~3 min cold), but supply-chain surface shrinks to: Debian apt, GitHub `realnc/frobtads` at a tag, Inform 7 download. Rejected: keeping a private base image registry (defeats the "self-hosted, zero cloud deps" goal of the replatform).
22. **`modernc.org/sqlite` (pure Go)** — no CGO toolchain in the build image; binary stays statically linked; multi-arch builds remain trivial. Rejected: `mattn/go-sqlite3` (CGO required, complicates cross-compilation and Alpine builds).
23. **Voice I/O via Web Speech API, client-side only, independent persistent toggles** — see §20.
24. **Play page mobile layout driven by `visualViewport`** — see §21.

---

## 17. Backwards Compatibility & Carried Risks

### 17.1 Backwards compatibility

The replatform is a **clean break**. The Cloud Run / Firestore / GCS / Firebase StoryCloud deployment is archived; no automated migration of users, runs, projects, or builds exists. Accept that:

- All anonymous runs from the legacy `sc_user`-cookie era are abandoned.
- All Firebase-authenticated user accounts are abandoned. New users register against the local backend.
- Existing IFDB caches do not transfer; first searches will repopulate the local `ifdb_cache` table.

For the in-progress AIFStudio deployment this is the v1 starting state — there is nothing to migrate.

### 17.2 Risks carried forward (not blocking implementation)

- **Single-instance write contention.** SQLite WAL mode handles the runner's write rate comfortably, but a future scale-out (multiple AIFStudio containers behind a load balancer) would require migrating to Postgres or making SQLite networked (e.g. LiteFS). Not in scope.
- **Backup hygiene.** With no managed DB, the operator owns backups (§8.4). A missed backup = lost user accounts + saves.
- **Container UID drift.** If the host's bind-mounted directory is owned by a UID that disappears (e.g. host user deleted while the container is down), the container's `app` user may fail to read/write on next start. Document `chown -R 999:999 ./data` in README's troubleshooting.
- **Interpreter version drift.** `dfrotz` / `glulxe` from apt advance on image rebuild; older saves may occasionally fail to restore. Mitigation: fall back to `game.sav.prev`. Acceptable for v1.

---

## 18. GitHub Actions Versions (MANDATORY)

Per `~/.claude/rules/template-standards.md`. Every reference in `.github/workflows/main.yml` MUST use these Node.js 24-compatible versions:

| Action | Version |
|--------|---------|
| `actions/checkout` | `@v4` |
| `actions/setup-go` | `@v5` |
| `actions/cache` | `@v4` |
| `golangci/golangci-lint-action` | `@v6` |

`google-github-actions/auth`, `google-github-actions/setup-gcloud`, and `hashicorp/setup-terraform` are not used (no GCP/Terraform in CI).

---

## 19. Quality Self-Check

- [x] Every API endpoint has a concrete JSON request/response example.
- [x] Bulk operations are documented as single SQL statements (the `BulkWriter`-vs-`Batch()` rule is GCP-specific and does not apply to SQLite).
- [x] Root route pattern is `GET /{$}`, not `GET /`.
- [x] Every algorithm documents edge case behavior.
- [x] GitHub Actions versions listed; all Node.js 24-compatible.
- [x] Store interface lists every method signature, including the new auth surface (CreateUser, GetUserByEmail, GetUserByID, CreateSession, GetSession, DeleteSession, DeleteExpiredSessions).
- [x] Directory structure complete; no file undocumented.
- [x] Three-endpoint POST+wait transport fully specified (§4.12, §6).
- [x] Four spawn paths (HOT / FIRST-TIME / UNSAVABLE / RESUME) documented.
- [x] All endpoint error codes enumerated.
- [x] `SHUTDOWN_DRAIN_TIMEOUT` defaults to 8 s.
- [x] Cookie attributes (`HttpOnly; SameSite=Strict; Path=/; Max-Age=2592000`) specified.
- [x] All formats (`.z3`–`.z8`, `.zblorb`, `.ulx`, `.gblorb`, `.gam`, `.t3`) listed with interpreter mapping.
- [x] TADS 2/3 documented end-to-end.
- [x] Devsys filtering documented (§5.6, §13.12).
- [x] Save/restore protocol normative.
- [x] Auto-save fires every user turn with one-save-in-flight coalescing.
- [x] User history model specified.
- [x] Indefinite retention — no time-based purge of `sessions/*`.
- [x] Voice I/O (§20) documented: client-only, no backend.
- [x] Mobile viewport handling (§21) documented.
- [x] All GCP-specific sections (Terraform, Cloud Run, Firestore indexes, GCS lifecycle, Firebase auth, signed URLs, base-image registry, deploy CI) removed.
- [x] Docker Compose stack specified end-to-end (§8).
- [x] `.env` file is mandatory (§8.2); `.env.example` is committed at repo root and lists every config var with REQUIRED/OPTIONAL markers.
- [x] SQLite schema specified end-to-end (§5.2).
- [x] Local filesystem layout specified (§5.4).
- [x] `LocalBlobStore` path-safety requirements specified (§9.5, §13.13).
- [x] CI/CD pipeline (§11.2) is `lint → test → docker build → docker push` to Docker Hub; required secrets `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN`; image tags `vpoluyaktov/aifstudio:{latest,<version>}`.

---

## 20. Voice I/O (Web Speech API)

Voice-driven play lives entirely **client-side** in `service/internal/templates/play.html` using the browser's Web Speech API (`window.speechSynthesis`; `window.SpeechRecognition` / `window.webkitSpeechRecognition`).

**No backend, no third-party service.** No audio bytes leave the browser for AIFStudio. The only audio handling is what the browser performs as a Web Speech API implementation detail.

**Fully optional and independently toggleable.** TTS and STT are separate features with separate persistent on/off toggles. The game remains fully playable with both disabled.

### 20.1 Browser compatibility

| API | Support |
|-----|---------|
| `SpeechSynthesis` (TTS) | All modern browsers |
| `SpeechRecognition` (STT) | Chrome / Edge / Opera. Not Firefox / Safari. |

Inline feature detection on page load:
- **TTS:** `'speechSynthesis' in window` — if false, `speakText()` is a no-op.
- **STT:** `window.SpeechRecognition || window.webkitSpeechRecognition` — if falsy, **both** the STT status-bar toggle and the input-row mic button are hidden.

Both require a secure context (HTTPS). `http://localhost` is a secure context per spec.

### 20.2 UI layout

| Control | Location | States |
|---------|----------|--------|
| `#ttsToggle` | Status bar | `🔇` off (default) ↔ `🔊` on |
| `#sttToggle` | Status bar | `🎙️` dim ↔ bright; hidden when STT unsupported |
| `#micBtn` | Input row | Idle ↔ Listening; hidden when STT unsupported or toggled off |

### 20.3 TTS design

- **Trigger:** after each successful `POST /command`, once `data.output` has rendered, call `speakText(data.output)`. Not called for `/start` responses.
- **Text transformation:** split on `\n`; drop lines whose `trim()` equals `">"`; join with single space; collapse whitespace; trim. Empty → no-op.
- **Cancel-on-new-command:** `sendCommand()` calls `cancelSpeech()` before every POST.
- **Preference:** `localStorage["sc_tts_enabled"]` — `'1'` on; anything else off.

### 20.4 STT design

- **Recognition config:** `continuous=false`, `interimResults=false`, `lang='en-US'`.
- **Auto-submit on `onresult`** — write transcript to input, call `stopMic()`, then `handleEnter()`.
- **Cancel-on-new-command:** `sendCommand()` also calls `stopMic()`.
- **Preference:** `localStorage["sc_stt_enabled"]` — `'1'` on; missing = on when supported else off.

### 20.5 Privacy

Audio does not touch AIFStudio. Server sees only final text, identical to keyboard.

### 20.6 Edge cases (normative)

(Unchanged from the prior architecture — `speakText()` empty / TTS off / unsupported = no-op; STT toggled off while listening cleans up; etc. See implementation comments in `play.html`.)

---

## 21. Mobile UI & Viewport Handling (Play Page)

The play page (`service/internal/templates/play.html`) presents a terminal-style UI that remains usable on mobile under iOS Safari.

### 21.1 Layout contract

The play page is a **fixed-position, full-viewport flex column**. It owns the viewport; document scroll is suppressed.

| Selector | Role | Sizing |
|----------|------|--------|
| `body.play-page` | Body class | `overflow: hidden`; nav/footer hidden |
| `#termContainer` | Outer shell | `position: fixed; top: var(--vt, 0); left: 0; right: 0; height: var(--vh, 100dvh); display: flex; flex-direction: column; overflow: hidden`. **Never set `bottom`.** |
| `.term-statusbar` | Top bar | `flex-shrink: 0` |
| `.unsavable-banner` | Optional warning | `flex-shrink: 0` |
| `#termOutput` | Transcript | `flex: 1; overflow-y: auto` — only scrollable region |
| `.term-input-row` | Prompt + input + mic | `flex-shrink: 0` |

### 21.2 Viewport tracking — CSS custom properties

Two CSS custom properties on `:root`:

| Property | Meaning | Source |
|----------|---------|--------|
| `--vh` | Visible viewport height in px | `visualViewport.height` else `window.innerHeight` |
| `--vt` | Layout-viewport offset inside visual viewport, in px | `visualViewport.offsetTop` else `0` |

### 21.3 `fitViewport()`

IIFE-wrapped function in `{{define "head"}}<script>` so it runs before body paint. Reads `visualViewport.height` / `offsetTop`, writes them to `--vh` / `--vt`, scrolls the transcript to bottom. Listens to `visualViewport.resize` and `visualViewport.scroll` (and `window.resize` as fallback).

### 21.4 Auto-scroll contract

After `appendLine` and after `fitViewport`, set `termOutput.scrollTop = termOutput.scrollHeight`. Always pin to bottom.

### 21.5 Rationale

(Unchanged.) Fixed positioning over flow layout; CSS custom properties not inline styles; both `resize` and `scroll` listeners; `flex-shrink: 0` on toolbar/input; no `dvh` reliance.

---

End of architecture. The Architect commits this file directly to `main`; specialists may begin implementation tasks (#2–#5) once it lands.
