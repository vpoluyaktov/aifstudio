# syntax=docker/dockerfile:1.7
#
# StoryCloud Cloud Run image — two-stage build
# ---------------------------------------------
# Stage 1 (gobuilder): static Go binary for Cloud Run (Alpine-based Go).
# Stage 2 (runtime):   Pinned runtime base image (see docker/base-image.digest)
#                      that already ships frotz / glulxe / inform6-compiler /
#                      inform6-library + the `frob` (TADS 2/3) binary + the
#                      Inform 7 compiler (inform7 / inform6 + /usr/local/bin/
#                      inform7 wrapper + the Internal resource tree). We
#                      layer only app-specific content on top: the non-root
#                      runtime user and the Go binary itself.
#
# Why the base image split: `frob` must be built from source (`frobtads` is
# not in any current Debian suite) and the Inform 7 .deb is ~50MB to
# download and extract. Baking both into an upstream base image —
# rebuilt only when FROBTADS_TAG or INFORM7_DEB_URL changes — keeps
# per-service builds fast and deterministic. The base image is pinned
# by digest so rebuilds are bit-for-bit reproducible; bumping the base
# is a deliberate edit of the `FROM` line.
#
# Runtime filesystem: only /tmp is writable on Cloud Run. The app writes
# per-session data under /tmp/runs/<runId>/ and /tmp/build/<buildId>/.

# ─── Stage 1: Go builder ─────────────────────────────────────────────────────
FROM golang:1.23-alpine AS gobuilder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/storycloud .

# ─── Stage 2: Runtime ────────────────────────────────────────────────────────
# Pinned to the digest recorded in docker/base-image.digest. The base image
# is built by .github/workflows/base-image.yml and pushed to
# gcr.io/dfh-ops-id/storycloud-base. The "Sync FROM pin in service/Dockerfile"
# step in that workflow rewrites the FROM line below in the same commit that
# updates docker/base-image.digest — do not hand-edit the digest here unless
# you've also pushed a matching base image.
#
# What the base image already provides:
#   - frotz / dfrotz / glulxe      — Z-machine + Glulx interpreters
#   - inform6-compiler + library   — Inform 6 (apt)
#   - frob                         — TADS 2/3 interpreter (built from source)
#   - /opt/inform7/bin/inform7     — Inform 7 compiler v10.1.2 (from .deb)
#   - /opt/inform7/bin/inform6     — Inform 6 back-end shipped with I7
#   - /usr/local/share/inform7/    — I7 Internal resource tree (Standard
#                                    Rules, Inter pipelines, I6 templates)
#   - /usr/local/bin/inform7       — wrapper that chains I7 → I6 → Glulx
FROM gcr.io/dfh-ops-id/storycloud-base@sha256:36af9c1b054af5aacb675928df2650b976d01ee117abfb7e70e0057504f97266

# Non-root runtime user. Only /tmp is writable at runtime.
RUN groupadd -r app && useradd -r -g app -M -d /nonexistent -s /usr/sbin/nologin app

# App binary.
COPY --from=gobuilder /out/storycloud /usr/local/bin/storycloud

USER app
# PATH is inherited from the base image (which already includes /usr/games
# so frotz/dfrotz/glulxe resolve). PORT/TMPDIR are app-specific.
ENV PORT=8080 \
    TMPDIR=/tmp
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["/usr/local/bin/storycloud"]
