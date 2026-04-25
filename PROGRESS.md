# AIFStudio — Development Progress

> Auto-updated by team-lead. Last updated: 2026-04-25 (replatform to Docker Compose)

---

## Deployment Status

| Environment | Status |
|-------------|--------|
| Cloud Run (StoryCloud) | Archived — replaced by AIFStudio |
| Docker Compose (AIFStudio) | 🔲 Not yet set up |

**Current state:** Replatforming from Cloud Run + Firestore + GCS to a self-hosted
Docker Compose stack with SQLite and local filesystem storage.

---

## In Progress

### Replatform to Docker Compose

**Goal:** Replace the GCP-dependent deployment (Cloud Run, Firestore, GCS, Terraform,
GitHub Actions CI/CD) with a self-contained Docker Compose stack that runs anywhere.

**Storage changes:**
- **Database:** Firestore → SQLite (file-based, no external service required)
- **File storage:** Google Cloud Storage → local filesystem directory
- **Persistence:** both the SQLite database file and the storage directory are mounted
  as Docker volumes outside the container so they survive container restarts and
  image upgrades

**Proposed `docker-compose.yml` volume mounts:**
```yaml
volumes:
  - ./data/db:/app/data/db        # SQLite database file
  - ./data/storage:/app/data/storage  # game binaries, sources, build artifacts
```

**Scope of work:**

| Area | Change |
|------|--------|
| `store/` | Rewrite Firestore store implementation to SQLite (using `database/sql` + `mattn/go-sqlite3` or `modernc.org/sqlite`) |
| `store/store.go` | Interface unchanged — swap implementation only |
| `config/` | Remove GCP project, Firestore, GCS config; add `DB_PATH`, `STORAGE_PATH` env vars |
| `Dockerfile` | Remove GCP SDK; add SQLite driver; expose volume mount points |
| `docker-compose.yml` | New file — service, port mapping, volume mounts for db + storage |
| `terraform/` | Remove (no infrastructure to provision) |
| `.github/workflows/` | Remove or simplify CI (no cloud deploy step) |
| `README.md` | Update setup instructions for Docker Compose |

**Non-goals:** The Go service HTTP API and all HTML templates are unchanged
(except auth-related pages).

- **Owner:** TBD
- **Status:** Planning — not started

---

### Replace Firebase Auth with Local Session Auth

**Goal:** Remove all GCP/Firebase dependencies from auth. Users are stored in
the SQLite database; sessions are validated via a server-side cookie — no
external auth service required.

**Auth design:**
- `users` table — `id` (ULID), `email`, `password_hash` (bcrypt), `display_name`, `created_at`
- `sessions` table — `id` (32-byte random token, base64url), `user_id`, `created_at`, `expires_at`
- Session cookie: `aifstudio_session` (HttpOnly, SameSite=Strict, configurable max-age via `SESSION_MAX_AGE`, default 30 days)
- New API endpoints: `POST /api/auth/register`, `POST /api/auth/login`, `POST /api/auth/logout`, `GET /api/auth/me`
- Middleware: read cookie → validate session in SQLite → inject `*auth.User` into context (same interface as before)
- `POST /api/runs/{id}/suspend` currently accepts `?token=` for `sendBeacon` — replace with session cookie (cookies are sent with `sendBeacon` by default)

**Scope of work:**

| Area | Change |
|------|--------|
| `auth/` | Rewrite: remove Firebase Admin SDK; add `SessionAuth` with `Register`, `Login`, `Logout`, `FromRequest` methods backed by SQLite |
| `server/handlers_auth.go` | New file: `POST /api/auth/register`, `POST /api/auth/login`, `POST /api/auth/logout`, `GET /api/auth/me` |
| `server/middleware.go` | Replace `firebaseAuthRequired` with `sessionAuthRequired`; remove `?token=` suspend workaround |
| `server/routes` | Register new auth endpoints; remove `/api/config` (no Firebase config to expose) |
| `templates/static/auth.js` | Replace Firebase JS SDK with simple `GET /api/auth/me` check; redirect to `/login` if 401; populate nav user area from response |
| `templates/layout.html` | Remove Firebase SDK `<script type="module" src="/static/auth.js">` bootstrap; wire new auth.js |
| `templates/login.html` | Replace Firebase form with plain `POST /api/auth/login` fetch; redirect on success |
| `templates/register.html` | Replace Firebase form with plain `POST /api/auth/register` fetch; redirect on success |
| `config/` | Remove `FIREBASE_WEB_API_KEY`, `FIREBASE_AUTH_DOMAIN`, `GCP_PROJECT_ID`; add `SESSION_MAX_AGE` (default `720h`) |
| `go.mod` | Remove `firebase.google.com/go/v4`, `google.golang.org/api`, all `cloud.google.com/*` auth deps; add `golang.org/x/crypto` (bcrypt) |

**Dependencies:** Blocked by "Replatform to Docker Compose" (SQLite store must
exist before sessions can be stored in it).

- **Owner:** TBD
- **Status:** Planning — not started

---

## Completed Sprints

**Version History + AI Quality Sprint (v1.0.170–v1.0.179)**

- **Version History panel** — slide-in drawer in AI Workspace showing all AI turns
  oldest-to-newest. Each entry shows kind badge (Generate/Chat), relative timestamp,
  and first 80 chars of user message. Clicking an entry with source loads a read-only
  preview. "Restore this version" button: confirmation dialog → `PUT /api/projects/{id}/source`
  → copies text into source editor → "✓ Version restored" toast.
- **History endpoints** — `GET /api/projects/{id}/history`, `GET /history/{turnId}/source`,
  `PUT /api/projects/{id}/source` + `GetAITurnAfterSource` store method.
- **Rule 37 (After going)** — forbids splitting radiation/tick effects into
  `After going:` + `Every turn:`. Correct pattern: `After going: ... continue the action.`
  for per-move side-effects. `Every turn:` for ambient ticks. Verified against
  Inform 7 v10.1.2 (both `if the current action is the going action` and
  `Every turn when the player is going` are rejected by the compiler).
- **BUILD_TIMEOUT** raised from 2m to 5m — extension-heavy games (18+ extensions,
  including 118k-word Unicode Full Character Names) routinely exceeded the old limit.
- **AI message limit** raised from 4,000 to 16,000 chars — test transcripts are a
  legitimate use case. Frontend truncates to 10,000 chars (keeping the tail) as a
  safety net.
- **GCS bucket unification** — merged `dfh-{env}-storycloud-artifacts` and
  `dfh-{env}-storycloud-sources` into a single `dfh-{env}-storycloud` bucket.
  AI turn snapshots stored at `ai-turns/{projectId}/{turnId}/{before,after}.i7`.
- **Orphaned artifact cleanup** — fixed 3 gaps in delete cascade (build logs,
  transcript path prefix, run docs); `DeleteRunsForProject` added to Store interface.
- **Firestore index** — `builds` collection composite index on `projectId + createdAt`
  to fix "failed to list builds" on project deletion.
- **Run Test feature** — `POST /api/builds/{buildId}/test` SSE endpoint; spawns
  glulxe with 30s timeout, sends "test me\n", streams output, detects win via
  `*** you have won ***`. On failure pre-fills chat input (user presses Send manually
  to avoid token-wasting auto-loop).
- **Game description extraction** — AI `generate` response parsed for `<DESCRIPTION>`
  block; stored in `project.Description` for the My Projects listing.
- **System prompt rules 34–37** — starting room syntax, repeat-with for inventory
  random selection, `<DESCRIPTION>` block on initial generate, `Every turn:` for
  tick mechanics.

---

## Architecture Decisions (locked)

1. **Transport** — POST+wait HTTP (not WebSocket). Each command = one HTTP round trip. Interpreter lives in memory between requests.
2. **Grace period** — 10s hardcoded by Cloud Run v2; ShutdownDrainTimeout=8s
3. **Sessions persist forever** — no TTL; user has "Delete game" button
4. **No cookie disclosure** — sc_user cookie set silently
5. **Unsavable games** — one-time banner: "This game does not support saves…"
6. **Multi-tab** — commands serialize on per-run mutex; 423 busy returned to second tab
7. **Save cadence** — after every user turn, with coalescing (at most 1 in-flight save per session)
8. **Idle sweep** — Manager sweeps idle sessions every minute; calls SuspendAndStop on sessions idle > IdleTimeout
9. **TADS interpreter** — `frob` built from source in the Dockerfile (realnc/frobtads v2.0); `frobtads` is not available as a Debian apt package.

---

## Future Considerations

**AI Token Cost — Conversation History Window**

On every AI call (Generate or Chat), the full conversation history is sent to the
model: system prompt + current source + all prior turns with reconstructed Inform 7
fences. A 25-turn project can reach 80,000–100,000 input tokens per call. This
scales linearly with the number of turns and will become expensive on long projects.

Options to evaluate when this becomes a priority:

- **Sliding window** — include only the N most recent turns (e.g., last 10) rather
  than the full history. Older turns are dropped from context.
- **Turn summarisation** — replace older turns with a compact AI-generated summary
  of what changed and why, keeping recent turns verbatim.
- **Selective inclusion** — always include the first turn (initial generate) plus
  the last N turns; the first turn captures the game concept and structure that
  later turns build on.

No urgency — current projects are short and token cost is acceptable. Revisit when
projects routinely exceed 30–40 turns or when OpenAI billing becomes a concern.

---

## Known Issues

- No open issues. All features live in both environments.

---

## Agent Roster

| Agent | Role | Status |
|-------|------|--------|
| architect | Architect | Released |
| backend-developer | Backend Developer | Pending (transcript compression) |
| frontend-developer | Frontend Developer | Released |
| qa-engineer | QA Engineer | Pending (transcript compression tests) |
| devops | DevOps Engineer | Released |
