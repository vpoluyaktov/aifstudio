# syntax=docker/dockerfile:1.7
#
# AIFStudio service image — fast, two-stage build on top of a pre-baked
# base image. The heavy IF toolchain layer (frob built from source,
# Inform 7 .deb extraction, apt installs) lives in the BASE image
# (docker/base.Dockerfile → vpoluyaktov/aifstudio-base on Docker Hub),
# so this Dockerfile only compiles the Go binary and copies it on top.
# That keeps per-commit builds under two minutes — pulling the base
# image's cached toolchain layer is the entire runtime stage.
#
# Stage 1 (gobuilder): static Go binary (Alpine-based Go), pure-Go SQLite
#                      driver (modernc.org/sqlite), CGO disabled.
# Stage 2 (runtime):   FROM the base image (toolchain pre-installed) +
#                      the compiled binary, the runtime user, env, and
#                      health probe. Rebuild the base image (and bump
#                      the tag below) only when the toolchain itself
#                      changes (frob version, Inform 7 release, apt
#                      package set).

# ─── Stage 1: Go builder ─────────────────────────────────────────────────────
FROM golang:1.23-alpine AS gobuilder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# modernc.org/sqlite is pure Go — CGO_ENABLED=0 is correct.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/aifstudio .

# ─── Stage 2: Runtime ────────────────────────────────────────────────────────
# The IF toolchain (frotz, glulxe, inform6, frob, inform7 + wrapper, runtime
# user, /app/data dirs) is already inside this image — see
# docker/base.Dockerfile. To pick up a toolchain change, rebuild the base
# image with `.github/workflows/base-image.yml` and bump the tag below.
FROM vpoluyaktov/aifstudio-base:frobtads-v2.0-inform7-10.1.2

# Bake the version into the image so APP_VERSION doesn't need to be set in .env.
ARG APP_VERSION=dev

# App binary.
COPY --from=gobuilder /out/aifstudio /usr/local/bin/aifstudio

USER app

# /usr/games is on PATH so apt-installed frotz/dfrotz/glulxe resolve.
# DB_PATH and STORAGE_PATH default to the volume mount targets so the
# image works under `docker run` without an `.env` file too.
ENV PORT=8080 \
    TMPDIR=/tmp \
    APP_VERSION=$APP_VERSION \
    DB_PATH=/app/data/db/aifstudio.db \
    STORAGE_PATH=/app/data/storage \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/games

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["/usr/local/bin/aifstudio"]
