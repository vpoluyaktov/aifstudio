# syntax=docker/dockerfile:1.7
#
# AIFStudio service image — self-contained, two-stage build.
# ----------------------------------------------------------
# Stage 1 (gobuilder): static Go binary (Alpine-based Go), pure-Go SQLite
#                      driver (modernc.org/sqlite), CGO disabled.
# Stage 2 (runtime):   Debian bookworm-slim with the full IF toolchain
#                      installed in one apt+source-build layer:
#                        - frotz / dfrotz / glulxe       (apt; /usr/games)
#                        - inform6 + inform6 library     (apt)
#                        - frob (TADS 2/3)               (built from
#                          github.com/realnc/frobtads v2.0 — not in any
#                          current Debian suite, must be built from source)
#                        - inform7 + inform6 back-end + Internal tree
#                          (extracted from the upstream inform7-ide .deb,
#                          v10.1.2 "Krypton")
#                        - inform7-wrapper.sh shim at /usr/local/bin/inform7
#                          that drives the I7 → I6 → Glulx pipeline so
#                          compiler.go can keep calling a single binary
#
# Build deps for `frob` are installed and purged in the same RUN layer
# so they do not bloat the final image.

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
FROM debian:bookworm-slim

# IF toolchain pin: bumping FROBTADS_TAG or INFORM7_DEB_URL is a deliberate
# image rebuild. INFORM7_DEB_SHA256 is verified against the downloaded .deb.
ARG FROBTADS_TAG=v2.0
ARG INFORM7_DEB_URL="https://github.com/ganelson/inform/releases/download/v10.1.2/inform7-ide_2.0.0-1_amd64.deb"
ARG INFORM7_DEB_SHA256="2a238e3d2da7b583334cc2cfa4fd88eda6d44b83d8ba8117c0664e0740b6ac40"

RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates tzdata curl wget tar xz-utils unzip bash \
        frotz glulxe inform6-compiler inform6-library; \
    # Build deps for `frob` (TADS 2/3 interpreter — frobtads is not packaged
    # in bookworm/trixie, must be built from source). Purged in the same
    # RUN layer below so they do not stay in the image.
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        build-essential autoconf automake libtool pkg-config \
        libcurl4-openssl-dev libncurses-dev git; \
    git clone --branch "${FROBTADS_TAG}" --depth 1 \
        https://github.com/realnc/frobtads.git /tmp/frobtads; \
    cd /tmp/frobtads; \
    ./bootstrap; \
    ./configure; \
    make -j"$(nproc)"; \
    make install; \
    cd /; \
    rm -rf /tmp/frobtads; \
    # ── Inform 7 CLI bundle (v10.1.2 "Krypton") ─────────────────────────
    # Inform 7 ships only the inform7-ide .deb on Linux. We extract the
    # two headless binaries (inform7, inform6) plus the Internal resource
    # tree (Standard Rules, Inter pipelines, I6 templates) and discard
    # the rest of the IDE.
    wget -O /tmp/inform7.deb "${INFORM7_DEB_URL}"; \
    echo "${INFORM7_DEB_SHA256}  /tmp/inform7.deb" | sha256sum -c; \
    mkdir -p /tmp/inform7-extract /opt/inform7/bin /usr/local/share/inform7; \
    dpkg-deb -x /tmp/inform7.deb /tmp/inform7-extract; \
    cp /tmp/inform7-extract/usr/lib/x86_64-linux-gnu/inform7-ide/inform7 /opt/inform7/bin/inform7; \
    cp /tmp/inform7-extract/usr/lib/x86_64-linux-gnu/inform7-ide/inform6 /opt/inform7/bin/inform6; \
    cp -r /tmp/inform7-extract/usr/share/inform7-ide/. /usr/local/share/inform7/; \
    # Known incompatibility: Emily Short's "Skeleton Keys.i7x" shipped in
    # the v10.1.2 .deb redefines the lock-fitting relation and the "to
    # unlock" verb against an older Standard Rules, causing every compile
    # to fail with a duplicate-definition error (the Problems report phase
    # scans every extension in Internal, so it triggers even for stories
    # that don't Include it). Removing this single file fixes the clash
    # without pruning the rest of the community extensions.
    rm -f "/usr/local/share/inform7/Extensions/Emily Short/Skeleton Keys.i7x"; \
    /opt/inform7/bin/inform7 -version; \
    /opt/inform7/bin/inform6 -help 2>&1 | head -1; \
    rm -rf /tmp/inform7.deb /tmp/inform7-extract; \
    # ── Purge build deps; keep only the runtime libs the compiled
    # `frob` binary actually needs.
    apt-get purge -y --auto-remove \
        build-essential autoconf automake libtool pkg-config \
        libcurl4-openssl-dev libncurses-dev git; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        libcurl4 libncurses6 libtinfo6; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*

# Wrapper that chains inform7 → inform6 → Glulx behind a single entry point
# at /usr/local/bin/inform7 — compiler.go invokes that path verbatim.
COPY scripts/inform7-wrapper.sh /usr/local/bin/inform7
RUN chmod +x /usr/local/bin/inform7

# Sanity check: every interpreter must resolve on PATH in the published
# image. Fails the build loudly if anything is missing.
RUN set -eux; \
    command -v frotz; \
    command -v dfrotz; \
    command -v glulxe; \
    command -v inform6; \
    command -v frob; \
    command -v inform7; \
    /usr/local/bin/frob --version | head -1; \
    /opt/inform7/bin/inform7 -version | head -1; \
    /opt/inform7/bin/inform6 -help 2>&1 | head -1

# Non-root runtime user. /tmp is writable; per-session story/save files
# live at /tmp/runs/<runId>/ and per-build sandboxes at /tmp/build/<buildId>/.
RUN groupadd -r app && useradd -r -g app -M -d /nonexistent -s /usr/sbin/nologin app

# Pre-create the volume mount points so the image works even if the operator
# forgets to bind-mount the host directories. The container's `app` user
# owns everything inside /app/data.
RUN mkdir -p /app/data/db /app/data/storage && chown -R app:app /app/data

# App binary.
COPY --from=gobuilder /out/aifstudio /usr/local/bin/aifstudio

USER app

# /usr/games is on PATH so apt-installed frotz/dfrotz/glulxe resolve.
# DB_PATH and STORAGE_PATH default to the volume mount targets so the
# image works under `docker run` without an `.env` file too.
ENV PORT=8080 \
    TMPDIR=/tmp \
    DB_PATH=/app/data/db/aifstudio.db \
    STORAGE_PATH=/app/data/storage \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/games

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["/usr/local/bin/aifstudio"]
