# StoryCloud

**StoryCloud** is a cloud-hosted **Interactive Fiction (IF) platform** that lets users
discover published IF titles via IFDB, play Z-machine, Glulx, and TADS story files in
the browser through a lightweight HTTP terminal UI, author new interactive fiction in
Inform 7 with compile-in-the-cloud builds, and resume any game they have played from
their cookie-scoped history.

> **Authoritative spec:** [`ARCHITECTURE.md`](ARCHITECTURE.md) is the single source of
> truth for design, data models, endpoints, storage, and infrastructure. Read it before
> writing code.

---

## Live URLs

| Environment | URL |
|-------------|-----|
| Staging     | https://storycloud.stage.demo.devops-for-hire.com |
| Production  | https://storycloud.demo.devops-for-hire.com |

---

## Key Features

- **Discover** — browse IFDB's catalogue of published IF titles from inside the app.
  Titles in unsupported formats (Hugo, AGT, Adrift, etc.) are filtered out server-side
  using IFDB's `devsys` metadata.
- **Play in the browser** — Z-machine (`.z3`–`.z8`, `.zblorb`), Glulx (`.ulx`,
  `.gblorb`), and TADS 2/3 (`.gam`, `.t3`), driven by a per-session interpreter
  subprocess (`dfrotz`, `glulxe`, `frob`).
- **Durable sessions** — every run is backed by a Quetzal save file (Z-machine /
  Glulx) or native save (TADS) plus a transcript in GCS, with metadata in Firestore.
  Sessions survive Cloud Run container recycles, deploys, and scale-to-zero, and
  resume in < 200 ms (p50) plus network round trip.
- **Author in Inform 7** — authenticated users can create projects, compile stories
  in the cloud via the bundled Inform 7 CLI, and immediately play their own builds.
- **Cookie-scoped history** — a pseudonymous `sc_user` cookie (opaque ULID, 1-year
  Max-Age) ties every run to a browser; a dedicated `/history` page lists past
  runs with Continue / Restart / Delete actions. No account needed to play.
- **Voice I/O** — optional browser-side Web Speech API integration: TTS reads
  interpreter output, STT captures commands hands-free. Off by default; toggles
  persist in `localStorage`.
- **Auto-save every turn** — output coalescing guarantees at most one in-flight turn
  lost on hard kill; graceful SIGTERM drains all sessions within the 10 s Cloud Run
  grace period (8 s internal budget).
- **Self-contained binary** — all HTML, CSS, and JS are embedded via Go's `embed`
  package. No separate frontend build step.

---

## Architecture Overview

```
           ┌────────────────────────────┐
 Browser ──┤  Cloud Run: storycloud-*   │── Firestore (metadata)
   │       │  Go 1.23 + net/http        │── GCS   (binaries, saves, transcripts)
   │       │  dfrotz / glulxe / frob    │── IFDB  (public catalogue, read-only)
   │       │  inform7 (authoring)       │
   │       └────────────────────────────┘
   │              ▲         │
   │  POST+wait   │         │ subprocess stdin/stdout (per-session, in-memory)
   └──────────────┘         └─── dfrotz / glulxe / frob / inform7 CLI
```

- **Transport:** POST+wait HTTP — each command is one HTTP round trip
  (`POST /api/runs/{runId}/command`); the interpreter lives in memory between
  requests. See ARCHITECTURE.md §8. No WebSocket, no long-polling, no SSE.
- **Compute:** Cloud Run v2 service, per environment (`storycloud-stage`,
  `storycloud-prod`). Non-root container.
- **Container image:** two-stage build on top of a pinned base image
  (`gcr.io/dfh-ops-id/storycloud-base@sha256:…`). The base contains Debian
  bookworm-slim + `frotz`, `glulxe`, `inform6-compiler`/`inform6-library`, and
  `frob` (built from `realnc/frobtads` source — `frobtads` is not in any current
  Debian suite). The service image layers the Go binary and Inform 7 CLI shim on
  top.
- **Persistence layers:**
  - **Firestore (Native mode)** — users, runs, projects, builds.
  - **GCS** — story files, save files (Quetzal / TADS native), transcripts,
    compiled builds, under `sessions/<runId>/…` and `builds/<buildId>/…`.
  - **No Redis, no Memorystore** — all durable state lives in Firestore + GCS.
- **Identity:**
  - **Play** uses a pseudonymous `sc_user` cookie (ULID, HttpOnly, Secure, Lax, 1 year).
  - **Authoring** uses Firebase ID tokens verified via the Firebase Admin SDK.
- **Routing:** Go 1.22+ `ServeMux` pattern matching. The root route is
  `GET /{$}` (exact match) so it does not swallow 405 Method Not Allowed from
  API routes.
- **IaC:** Terraform 1.6+ with `hashicorp/google ~> 5.0`. Per-environment state
  in GCS buckets (`dfh-stage-tfstate`, `dfh-prod-tfstate`).

See [`ARCHITECTURE.md`](ARCHITECTURE.md) for the complete spec, including:

- §3 Identity model (cookie + run-ID capability URLs)
- §4 Full API endpoint reference with request/response JSON
- §6 Session persistence protocol (save/restore, coalescing, interpreter FSM)
- §8 Transport (POST+wait, per-session mutex, 409/423 semantics)
- §9 Store interface
- §11 Infrastructure and deployment (Terraform + CI/CD)

---

## Repository Layout

```
StoryCloud/
├── VERSION                    # Base version (MAJOR.MINOR); CI appends commit count
├── ARCHITECTURE.md            # Authoritative architecture spec
├── README.md                  # This file
├── docker/
│   ├── base.Dockerfile        # Runtime base image (frotz, glulxe, inform6, frob)
│   └── base-image.digest      # Pinned sha256 digest consumed by service/Dockerfile
├── service/                   # Go application source
│   ├── main.go
│   ├── go.mod                 # module storycloud, go 1.23
│   ├── Dockerfile             # FROM pinned base + Go binary + Inform 7 shim
│   └── internal/
│       ├── config/            # Env var loading
│       ├── server/            # HTTP handlers, routing, cookie middleware
│       ├── auth/              # Firebase ID token verification
│       ├── ifdb/              # IFDB catalogue client + devsys filtering
│       ├── runner/            # Interpreter bridge, save/restore FSM, session lifecycle
│       ├── build/             # Inform 7 compile pipeline
│       ├── store/             # Firestore + GCS store interface & impl
│       └── templates/         # Embedded HTML templates (index, play, history, author)
├── terraform/
│   ├── stage/                 # Staging tfvars + GCS backend
│   └── prod/                  # Production tfvars + GCS backend
└── .github/workflows/
    ├── main.yml               # test → build → push → terraform apply → smoke test
    └── base-image.yml         # Builds/publishes docker/base.Dockerfile + updates digest
```

---

## Local Development

### Prerequisites

- Go 1.23+
- Docker (for container builds)
- `gcloud` CLI authenticated against `dfh-stage-id` (optional, only for Firestore/GCS
  against real projects)
- For full local functional testing: `dfrotz`, `glulxe`, `frob`, and optionally the
  Inform 7 CLI on your `$PATH`

### Running the server

```bash
cd service
go mod download
go test ./...
go vet ./...
go run .
```

The server listens on `$PORT` (default `8080`). Open http://localhost:8080/.

- Without `GCP_PROJECT_ID` set, Firestore and GCS are disabled; play and authoring
  features that persist metadata or binaries will degrade (see `config` package for
  behavior).
- Set `FIRESTORE_EMULATOR_HOST=localhost:8200` + `GCP_PROJECT_ID=demo-local` to run
  against the Firestore emulator.

### Running tests

```bash
cd service
go test ./...
go vet ./...
golangci-lint run ./...
```

Unit tests use the `MockStore` implementation of the `Store` interface; no real
Firestore or GCS access is required.

### Building the container locally

```bash
cd service
docker build -t storycloud:dev .
docker run --rm -p 8080:8080 -e PORT=8080 storycloud:dev
```

Inform 7 CLI is conditionally installed via `INFORM7_URL` and `INFORM7_SHA256` build
args; if unset, the image builds without Inform 7 and returns a clear error if you
attempt to compile an Inform 7 project.

---

## Configuration

All configuration is via environment variables. See `service/internal/config/config.go`
and [`ARCHITECTURE.md`](ARCHITECTURE.md) §10 for the complete list. Cloud Run injects
these at deploy time via Terraform.

Commonly-set variables:

| Variable | Purpose | Default |
|----------|---------|---------|
| `PORT` | HTTP listen port | `8080` |
| `GCP_PROJECT_ID` | Firestore + GCS project | — |
| `GCS_BUCKET` | Bucket for sessions and builds | `<project>-storycloud` |
| `FIREBASE_PROJECT_ID` | Firebase project for Admin SDK | same as `GCP_PROJECT_ID` |
| `SESSION_COOKIE_NAME` | Pseudonymous play cookie | `sc_user` |
| `SHUTDOWN_DRAIN_TIMEOUT` | Max drain budget inside Cloud Run grace | `8s` |
| `MAX_STORY_BYTES` | Upload ceiling for story files | `16 MiB` |
| `INTERPRETER_STDOUT_BUFFER` | Bytes buffered before forcing a save checkpoint | `64 KiB` |

---

## Deployment

Deployments are automated via GitHub Actions — **never deploy manually** from dev.

| Branch  | Environment | GCP Project     | Cloud Run service  |
|---------|-------------|-----------------|--------------------|
| `stage` | Staging     | `dfh-stage-id`  | `storycloud-stage` |
| `main`  | Production  | `dfh-prod-id`   | `storycloud-prod`  |

### Pipeline stages (`.github/workflows/main.yml`)

1. **`test`** — `go vet`, `golangci-lint`, `go test ./...`
2. **`build-and-deploy`**
   - Docker build (`FROM` pinned base image digest in `gcr.io/dfh-ops-id`)
   - Push image to GCR (`gcr.io/<project>/storycloud:<version>`)
   - Pre-apply resource check (lists existing GCP resources and fails if conflicts)
   - `terraform init` + `terraform apply` (Cloud Run service update, IAM, Firestore
     database, GCS bucket, DNS CNAME)
   - Post-deploy HTTPS smoke test against the live URL

Pipeline runs are serialized per branch (GitHub Actions concurrency group) to avoid
Terraform state-lock races when a second push arrives mid-apply.

### Base image pipeline (`.github/workflows/base-image.yml`)

Builds `docker/base.Dockerfile`, pushes to `gcr.io/dfh-ops-id/storycloud-base`, and
commits the resulting digest back to `docker/base-image.digest`. To bump:

1. Edit `FROBTADS_TAG` (or other base packages) in `docker/base.Dockerfile`.
2. Run the **base-image** workflow. It commits the new digest to `stage`.
3. Edit the `FROM` line in `service/Dockerfile` to match `docker/base-image.digest`.
4. Push to `stage`; verify staging; merge to `main`.

### Workflow: push to stage → verify → merge to main

```bash
git checkout stage
git pull
# …commits on stage…
git push origin stage
# wait for GitHub Actions to deploy and smoke-test staging
# verify at https://storycloud.stage.demo.devops-for-hire.com
git checkout main
git merge --ff-only stage
git push origin main
# production deploy runs automatically
```

---

## One-Time Manual Steps

### Custom domain mapping (per environment, once per service)

Cloud Run domain mapping requires domain verification in Google Search Console for
the calling identity, so it is not managed by Terraform.

**Staging**

```bash
gcloud beta run domain-mappings create \
  --service=storycloud-stage \
  --domain=storycloud.stage.demo.devops-for-hire.com \
  --region=us-central1 \
  --project=dfh-stage-id
```

**Production**

```bash
gcloud beta run domain-mappings create \
  --service=storycloud-prod \
  --domain=storycloud.demo.devops-for-hire.com \
  --region=us-central1 \
  --project=dfh-prod-id
```

### Cross-project IAM for the ops-project base image

Each environment's deploy SA must be able to pull the pinned base image from the
ops-project `gcr.io` Artifact Registry shim (`us`). Apply imperatively (not via
Terraform):

```bash
for env in stage prod; do
  gcloud artifacts repositories add-iam-policy-binding gcr.io \
    --location=us \
    --project=dfh-ops-id \
    --role=roles/artifactregistry.reader \
    --member="serviceAccount:gcp-cloudrun-deploy@dfh-${env}-id.iam.gserviceaccount.com"
done
```

Cloud Run runtime SAs need no grant: the base image is flattened into the service
image at build time, so the final image lives in the same project as the runtime.

### Inform 7 CLI pin (before production authoring launch)

See the TODO block in `service/Dockerfile`. Pin `INFORM7_URL` and `INFORM7_SHA256` to
a specific Linux CLI tarball from https://ganelson.github.io/inform-website/ before
enabling the authoring feature in production.

### Firebase email/password auth bootstrap (per environment, once per project)

`ARCHITECTURE.md §22` specifies Firebase Authentication with the email/password
provider. The following steps have already been applied to `dfh-stage-id` and
`dfh-prod-id`. Documented here for reproducibility on new environments.

The deploy SA (`gcp-cloudrun-deploy@<project>.iam.gserviceaccount.com`) needs
`roles/firebase.admin` on its project to run the REST calls below. Grant once:

```bash
PROJECT=dfh-stage-id    # or dfh-prod-id
gcloud projects add-iam-policy-binding "$PROJECT" \
  --member="serviceAccount:gcp-cloudrun-deploy@${PROJECT}.iam.gserviceaccount.com" \
  --role="roles/firebase.admin" --condition=None
```

Then run this one-time bootstrap script per project (replace `PROJECT` and
`CUSTOM_DOMAIN` appropriately):

```bash
PROJECT=dfh-stage-id
CUSTOM_DOMAIN=storycloud.stage.demo.devops-for-hire.com

gcloud services enable firebase.googleapis.com identitytoolkit.googleapis.com \
  --project="$PROJECT"

TOKEN=$(gcloud auth print-access-token)

# 1. Register the GCP project with Firebase (idempotent; returns 409 if already done).
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  "https://firebase.googleapis.com/v1beta1/projects/${PROJECT}:addFirebase" -d '{}'

# 2. Initialize Identity Platform (Firebase Auth backend).
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  "https://identitytoolkit.googleapis.com/v2/projects/${PROJECT}/identityPlatform:initializeAuth" -d '{}'

# 3. Enable the email/password provider and register the custom authorized domain.
curl -s -X PATCH -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  "https://identitytoolkit.googleapis.com/admin/v2/projects/${PROJECT}/config?updateMask=signIn.email,authorizedDomains" \
  -d "{
    \"signIn\": { \"email\": { \"enabled\": true, \"passwordRequired\": true } },
    \"authorizedDomains\": [
      \"${PROJECT}.firebaseapp.com\",
      \"${PROJECT}.web.app\",
      \"localhost\",
      \"${CUSTOM_DOMAIN}\"
    ]
  }"

# 4. Create the Firebase Web app (skip if one already exists — list with GET /webApps).
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  "https://firebase.googleapis.com/v1beta1/projects/${PROJECT}/webApps" \
  -d '{"displayName": "storycloud-web"}'

# 5. Retrieve the Web SDK config (apiKey + authDomain); paste into <env>.tfvars.
APP_ID=$(curl -s -H "Authorization: Bearer $TOKEN" \
  "https://firebase.googleapis.com/v1beta1/projects/${PROJECT}/webApps" \
  | grep -oE '"appId": "[^"]+"' | head -1 | cut -d'"' -f4)
curl -s -H "Authorization: Bearer $TOKEN" \
  "https://firebase.googleapis.com/v1beta1/projects/${PROJECT}/webApps/${APP_ID}/config"
```

Copy `apiKey` into `firebase_web_api_key` and `authDomain` into `firebase_auth_domain`
in `terraform/<env>/<env>.tfvars`. Both values are **public-by-design** (the Web API
key is not a secret) and are checked in.

**Equivalent Firebase Console flow** (fallback if the REST calls are unavailable):

1. Add Firebase to the GCP project — https://console.firebase.google.com → **Add project** → select existing GCP project.
2. **Authentication → Sign-in method → Email/Password → Enable.**
3. **Authentication → Settings → Authorized domains** → add the custom domain (e.g. `storycloud.stage.demo.devops-for-hire.com`).
4. **Project settings → Your apps → Add app → Web** → name `storycloud-web`, do **not** enable Firebase Hosting.
5. Copy `apiKey` and `authDomain` from the emitted config into `<env>.tfvars`.

Re-run the bootstrap when onboarding a brand-new GCP project (e.g. a preview env).

---

## GitHub Secrets

| Secret | Scope |
|--------|-------|
| `GCP_STAGE_SA_KEY` | JSON key for deploy SA in `dfh-stage-id` |
| `GCP_PROD_SA_KEY`  | JSON key for deploy SA in `dfh-prod-id`  |

Secrets are configured once per repository and never committed.

---

## Troubleshooting

| Symptom | Likely cause | Where to look |
|---------|--------------|---------------|
| `POST /api/runs/{id}/command` returns 409 `busy` | Another tab or request holds the per-run mutex; expected under concurrent use | ARCHITECTURE.md §8; retry after current command completes |
| `POST /api/runs/{id}/command` returns 423 `locked` on resume | Run is mid-restore; frontend should auto-retry | ARCHITECTURE.md §6 |
| `inform7 CLI not installed` at compile time | `INFORM7_URL` / `INFORM7_SHA256` not pinned in the Docker build | `service/Dockerfile` Inform 7 block |
| 405 swallowed as 200 on API routes | Root route registered as `GET /` instead of `GET /{$}` | ARCHITECTURE.md §7; `server.go` route registration |
| Run history missing after deploy | `sc_user` cookie lost because cookie attributes changed | ARCHITECTURE.md §3; inspect browser cookie — must be HttpOnly, Secure, SameSite=Lax, 1-year Max-Age |
| Terraform apply fails with state lock | Two pipeline runs racing on the same branch | Cancel the older run; concurrency group serializes per branch but manual `terraform apply` elsewhere can collide |
| Sessions lost on deploy | SIGTERM drain budget exceeded | ARCHITECTURE.md §12; reduce `SHUTDOWN_DRAIN_TIMEOUT` or inspect session count before deploy |
| `dfrotz` / `glulxe` / `frob` not found | Container built from wrong base image, or `/usr/games` missing from `PATH` | Confirm `FROM` in `service/Dockerfile` matches `docker/base-image.digest`; `PATH` must include `/usr/games` |
| TTS silent in Chrome | Chrome leaves the SpeechSynthesis engine in a paused state | `play.html` voice block — `cancel()` + `resume()` before `speak()` |
| TTS silent in iOS Safari on first play | iOS requires a user-gesture to unlock audio | `play.html` — `unlockAudio()` must fire on first `click`/`touchend` |

For deeper debugging of session persistence, see `ARCHITECTURE.md` §6 (Session
Persistence Protocol) and §13 (Algorithm and Logic Edge Cases).

---

## Contributing

1. Read `ARCHITECTURE.md`.
2. Create a feature branch from `stage`.
3. Keep commits atomic; use conventional commits (`type(scope): subject`).
4. Run `go test ./...`, `go vet ./...`, and `golangci-lint run ./...` before pushing.
5. Open a PR into `stage`. Merges to `main` are fast-forward only, after staging
   smoke tests pass.
