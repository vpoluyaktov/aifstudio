# syntax=docker/dockerfile:1.7
#
# StoryCloud runtime base image — published to
#   gcr.io/dfh-ops-id/storycloud-base:<FROBTADS_TAG>
# and pinned by digest from `service/Dockerfile`.
#
# Rebuild cadence: only when `FROBTADS_TAG` changes (or when the Debian
# base image publishes a security refresh we care about). Per-service
# builds do NOT rebuild this layer — they FROM the pinned digest.
#
# Stage 1 (frobbuilder):   builds `frob` (TADS 2/3 interpreter) from source.
# Stage 2 (glulxebuilder): builds `glulxe` linked against `cheapglk` from
#                          source. The Debian apt `glulxe` package is
#                          linked against glkterm (ncurses) which fails
#                          with "Error opening terminal: unknown." when
#                          spawned with piped stdio (no TTY) — our runner
#                          model. cheapglk provides plain-stdio Glk I/O
#                          and has no curses/terminfo dependency.
# Stage 3 (runtime):       Debian bookworm-slim with frotz / inform6-compiler
#                          / inform6-library from apt + the `frob` and
#                          `glulxe` binaries copied from stages 1 and 2.
#                          No build toolchain leaks into the published image.
#
# Why multi-stage instead of a single RUN layer: we want the published
# base image to contain zero build dependencies (not even apt-purged
# residue in a squashed layer). The runtime stage is the only layer pushed.

# ─── Stage 1: frob (TADS 2/3) builder ────────────────────────────────────────
FROM debian:bookworm-slim AS frobbuilder
RUN set -eux; \
    export DEBIAN_FRONTEND=noninteractive; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      build-essential \
      ca-certificates \
      cmake \
      curl \
      libcurl4-openssl-dev \
      libncurses-dev \
      pkg-config \
      tar; \
    rm -rf /var/lib/apt/lists/*

# Pinned to realnc/frobtads v2.0 (latest stable as of 2026-04; `frobtads` is
# not available in any current Debian suite, so the TADS 2/3 interpreter
# must be built from upstream). Bump this ARG + rebuild to upgrade.
ARG FROBTADS_TAG=v2.0

WORKDIR /src
RUN curl -fsSL "https://api.github.com/repos/realnc/frobtads/tarball/${FROBTADS_TAG}" \
      -o /tmp/frobtads.tgz && \
    tar -C /src --strip-components=1 -xzf /tmp/frobtads.tgz && \
    rm /tmp/frobtads.tgz

# Build the interpreter only. The TADS 2 and TADS 3 compilers are not
# needed at runtime (StoryCloud runs pre-compiled .gam / .t3 story files).
RUN set -eux; \
    mkdir /build && cd /build; \
    cmake -DCMAKE_BUILD_TYPE=Release \
          -DENABLE_INTERPRETER=ON \
          -DENABLE_T2_COMPILER=OFF \
          -DENABLE_T3_COMPILER=OFF \
          /src >/dev/null; \
    make -j"$(nproc)" frob; \
    strip /build/frob; \
    /build/frob --version | head -1

# ─── Stage 2: glulxe (Glulx VM) builder, linked against cheapglk ────────────
#
# The Debian apt `glulxe` package (/usr/games/glulxe, v0.5.4-1.1) is linked
# against glkterm, which calls ncurses `initscr()` on startup. When spawned
# with piped stdio and no TTY (our runner model in
# service/internal/runner/session.go), initscr() fails with
#   "Error opening terminal: unknown."
# and the process exits before a single byte of game output is produced.
# Setting TERM=xterm/linux/dumb gets past initscr() but all subsequent
# output is littered with cursor-movement and SGR escape sequences — also
# unusable for our text-proxy model.
#
# The fix is cheapglk: a plain-stdio Glk library with no curses dependency.
# `glulxe` + `cheapglk` produces clean, line-oriented text output suitable
# for pipe-through to our WebSocket frontend.
#
# Pinned to the current upstream tags (both repos are low-velocity; latest
# as of 2026-04). Bump these ARGs + rebuild to upgrade. Upstream:
#   https://github.com/erkyrath/cheapglk
#   https://github.com/erkyrath/glulxe
FROM debian:bookworm-slim AS glulxebuilder
RUN set -eux; \
    export DEBIAN_FRONTEND=noninteractive; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      build-essential \
      ca-certificates \
      curl \
      tar; \
    rm -rf /var/lib/apt/lists/*

ARG CHEAPGLK_TAG=cheapglk-1.0.7a
ARG GLULXE_TAG=glulxe-0.6.1

WORKDIR /src
RUN set -eux; \
    mkdir /src/cheapglk /src/glulxe; \
    curl -fsSL "https://api.github.com/repos/erkyrath/cheapglk/tarball/${CHEAPGLK_TAG}" \
      | tar -C /src/cheapglk --strip-components=1 -xz; \
    curl -fsSL "https://api.github.com/repos/erkyrath/glulxe/tarball/${GLULXE_TAG}" \
      | tar -C /src/glulxe --strip-components=1 -xz

# Build cheapglk first (produces libcheapglk.a), then glulxe linked against it.
# The resulting binary has no ncurses/tinfo dependency — verified by ldd.
RUN set -eux; \
    cd /src/cheapglk && make -j"$(nproc)"; \
    cd /src/glulxe && make -j"$(nproc)" \
      GLKINCLUDEDIR=/src/cheapglk \
      GLKLIBDIR=/src/cheapglk \
      GLKMAKEFILE=Make.cheapglk; \
    strip /src/glulxe/glulxe; \
    # Sanity check: no ncurses/tinfo link.
    ldd /src/glulxe/glulxe | grep -E "ncurses|tinfo" && { echo "FATAL: glulxe still links ncurses/tinfo" >&2; exit 1; } || :; \
    # Sanity check: piped stdio, unset TERM, must NOT print "Error opening terminal".
    unset TERM; \
    out=$(echo "" | /src/glulxe/glulxe /dev/null 2>&1 | head -5); \
    echo "$out"; \
    echo "$out" | grep -q "Error opening terminal" && { echo "FATAL: glulxe still uses curses" >&2; exit 1; } || :; \
    echo "$out" | grep -q "Cheap Glk Implementation" || { echo "FATAL: glulxe did not produce cheapglk banner" >&2; exit 1; }

# ─── Stage 3: Runtime base ───────────────────────────────────────────────────
FROM debian:bookworm-slim

# Runtime IF interpreters from apt + shared-lib deps for `frob`.
#   frotz              — Z-machine (.z3–.z8, .zblorb)
#   inform6-compiler   — Inform 6 compiler (dep of Inform 7)
#   inform6-library    — Inform 6 stdlib
#   libcurl4, libncurses6, libtinfo6 — runtime libs required by `frob`
#   ca-certificates, tzdata, curl, wget, tar, xz-utils, unzip, bash — misc
#
# Deliberately NOT installed from apt:
#   glulxe — apt's build links glkterm/ncurses; we bring our own cheapglk
#            build from stage 2 instead. See stage-2 comment for rationale.
#
# The `frob` binary (TADS 2/3 interpreter) is copied in from stage 1 —
# there is no apt package for it in bookworm / trixie. Its shared-lib
# dependencies (libcurl4, libncurses6, libtinfo6) are installed here.
RUN set -eux; \
    export DEBIAN_FRONTEND=noninteractive; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      tzdata \
      curl \
      wget \
      tar \
      xz-utils \
      unzip \
      bash \
      frotz \
      inform6-compiler \
      inform6-library \
      libcurl4 \
      libncurses6 \
      libtinfo6; \
    rm -rf /var/lib/apt/lists/*

COPY --from=frobbuilder   /build/frob          /usr/local/bin/frob
COPY --from=glulxebuilder /src/glulxe/glulxe   /usr/local/bin/glulxe

# ---------------------------------------------------------------------------
# Inform 7 CLI bundle (v10.1.2 "Krypton") + Inform 6 back-end.
#
# The modern Inform 7 project ships Linux binaries only as the `inform7-ide`
# .deb (no standalone CLI tarball), so we pin that .deb by URL + SHA256,
# extract it with dpkg-deb, and install just the two headless binaries we
# need (`inform7` and `inform6`) plus the `Internal` resource tree (Standard
# Rules, Inter pipelines, I6 templates, etc.).
#
# This lives in the base image — not the per-service image — so per-service
# builds do not re-download the 50MB .deb on every CI run.
#
# Compile pipeline for StoryCloud:
#   Stage 1: inform7 → <project>/Build/auto.inf (Inform 6 intermediate)
#   Stage 2: inform6 -G → <project>/Build/output.ulx (Glulx story file)
#
# `compiler.go` in the service invokes a single command at
# /usr/local/bin/inform7, so we install a wrapper there
# (docker/inform7-wrapper.sh) that drives both stages and writes output.ulx
# where the Go build manager expects it.
#
# Bumping the pin:
#   1. Pick a new release at https://github.com/ganelson/inform/releases
#   2. Update INFORM7_DEB_URL and INFORM7_DEB_SHA256 below
#   3. Re-run .github/workflows/base-image.yml to publish a new base image
#      and auto-commit the new digest to docker/base-image.digest
#   4. Verify the .deb layout still has inform7/inform6 under
#      /usr/lib/x86_64-linux-gnu/inform7-ide/ — adjust the cp paths if not.
# ---------------------------------------------------------------------------
ARG INFORM7_DEB_URL="https://github.com/ganelson/inform/releases/download/v10.1.2/inform7-ide_2.0.0-1_amd64.deb"
ARG INFORM7_DEB_SHA256="2a238e3d2da7b583334cc2cfa4fd88eda6d44b83d8ba8117c0664e0740b6ac40"

RUN set -eux; \
    wget -O /tmp/inform7.deb "$INFORM7_DEB_URL"; \
    echo "$INFORM7_DEB_SHA256  /tmp/inform7.deb" | sha256sum -c; \
    mkdir -p /tmp/inform7-extract /opt/inform7/bin /usr/local/share/inform7; \
    dpkg-deb -x /tmp/inform7.deb /tmp/inform7-extract; \
    cp /tmp/inform7-extract/usr/lib/x86_64-linux-gnu/inform7-ide/inform7 /opt/inform7/bin/inform7; \
    cp /tmp/inform7-extract/usr/lib/x86_64-linux-gnu/inform7-ide/inform6 /opt/inform7/bin/inform6; \
    cp -r /tmp/inform7-extract/usr/share/inform7-ide/. /usr/local/share/inform7/; \
    # Known incompatibility: Emily Short's "Skeleton Keys.i7x" shipped in the
    # v10.1.2 .deb was authored against an older Standard Rules and re-defines
    # the lock-fitting relation + the "to unlock" verb, causing every compile
    # to fail with a duplicate-definition error (the Problems report phase
    # scans every extension in Internal, so it triggers even for stories that
    # don't Include it). Removing this single file fixes the clash without
    # pruning the rest of the community extensions.
    rm -f "/usr/local/share/inform7/Extensions/Emily Short/Skeleton Keys.i7x"; \
    /opt/inform7/bin/inform7 -version; \
    /opt/inform7/bin/inform6 -help 2>&1 | head -1; \
    rm -rf /tmp/inform7.deb /tmp/inform7-extract

# Wrapper that chains inform7 + inform6 behind a single entry point so the
# Go build manager in the service image can keep calling
# /usr/local/bin/inform7 unchanged.
COPY inform7-wrapper.sh /usr/local/bin/inform7
RUN chmod +x /usr/local/bin/inform7

# /usr/games is where Debian installs frotz, dfrotz, glulxe. Put it on
# PATH so downstream service images — and the sanity check below — find
# them without absolute paths.
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/games

# Sanity check: every interpreter must resolve on PATH in the published
# image. Build fails loudly if anything is missing.
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

# Record the frobtads tag on the image for traceability. The per-service
# Dockerfile pins this image by digest, so this label is for humans
# inspecting the image (e.g. `docker inspect`), not for build logic.
ARG FROBTADS_TAG=v2.0
LABEL org.storycloud.frobtads-tag="${FROBTADS_TAG}" \
      org.opencontainers.image.source="https://github.com/vpoluyaktov/storycloud" \
      org.opencontainers.image.description="StoryCloud runtime base — Debian bookworm-slim + IF interpreters (frotz, dfrotz, glulxe, inform6, frob/TADS)"
