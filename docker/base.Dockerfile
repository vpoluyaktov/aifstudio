# syntax=docker/dockerfile:1.7
#
# AIFStudio base image — heavy IF toolchain layer.
# -------------------------------------------------
# This image bakes the slow-changing runtime: Debian bookworm-slim plus
# every interactive-fiction interpreter the service shells out to:
#
#   - frotz / dfrotz             (apt; /usr/games)
#   - glulxe                     (built from source against cheapglk —
#                                 apt's glulxe links glkterm/ncurses and
#                                 fails with "Error opening terminal: unknown"
#                                 when spawned without a TTY)
#   - inform6 + inform6 library  (apt)
#   - frob (TADS 2/3)            (built from
#     github.com/realnc/frobtads v2.0 — not in any current Debian suite,
#     must be built from source)
#   - inform7 + inform6 back-end + Internal tree
#     (extracted from the upstream inform7-ide .deb, v10.1.2 "Krypton")
#   - inform7-wrapper.sh shim at /usr/local/bin/inform7
#     that drives the I7 → I6 → Glulx pipeline so compiler.go can keep
#     calling a single binary
#
# This Dockerfile is rebuilt rarely — only when its contents change. The
# resulting image is published to Docker Hub as
# `vpoluyaktov/aifstudio-base:<toolchain-tag>` and is the FROM target of
# the per-commit `service/Dockerfile`. That two-image split keeps per-PR
# CI under two minutes by letting the app build skip the ~10–15 min
# toolchain layer entirely.
#
# Build context for this file is the `docker/` directory itself, so
# `COPY inform7-wrapper.sh ...` reads docker/inform7-wrapper.sh.
#
# ENV / EXPOSE / HEALTHCHECK / ENTRYPOINT and the Go-binary COPY all live
# in the per-commit `service/Dockerfile`, NOT here.

# ─── Stage 1: glulxe (Glulx VM) builder, linked against cheapglk ─────────────
#
# The Debian apt `glulxe` package (/usr/games/glulxe) is linked against
# glkterm, which calls ncurses initscr() on startup. When spawned with piped
# stdio and no TTY (our runner model), initscr() fails with
#   "Error opening terminal: unknown."
# and the process exits before producing any game output.
# Setting TERM=xterm/linux/dumb gets past initscr() but subsequent output is
# littered with cursor-movement escape sequences — unusable for our text-proxy.
#
# The fix is cheapglk: a plain-stdio Glk library with no curses dependency.
# glulxe + cheapglk produces clean, line-oriented text output suitable for
# pipe-through to our WebSocket frontend.
#
# Pinned to current upstream tags. Bump + rebuild base image to upgrade.
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

# Build cheapglk (produces libcheapglk.a), then glulxe linked against it.
# Verify: no ncurses/tinfo link, and no "Error opening terminal" on piped run.
RUN set -eux; \
    cd /src/cheapglk && make -j"$(nproc)"; \
    cd /src/glulxe && make -j"$(nproc)" \
      GLKINCLUDEDIR=/src/cheapglk \
      GLKLIBDIR=/src/cheapglk \
      GLKMAKEFILE=Make.cheapglk; \
    strip /src/glulxe/glulxe; \
    ldd /src/glulxe/glulxe | grep -E "ncurses|tinfo" && { echo "FATAL: glulxe still links ncurses/tinfo" >&2; exit 1; } || :; \
    unset TERM; \
    out=$(echo "" | /src/glulxe/glulxe /dev/null 2>&1 | head -5); \
    echo "$out"; \
    echo "$out" | grep -q "Error opening terminal" && { echo "FATAL: glulxe still uses curses" >&2; exit 1; } || :; \
    echo "$out" | grep -q "Cheap Glk Implementation" || { echo "FATAL: glulxe did not produce cheapglk banner" >&2; exit 1; }

# ─── Stage 2: Runtime base ────────────────────────────────────────────────────
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
        frotz inform6-compiler inform6-library; \
    # NOTE: glulxe intentionally NOT installed from apt — apt's build links
    # glkterm/ncurses and fails without a TTY. We use the cheapglk build from
    # stage 1 instead (copied below).
    # Build deps for `frob` (TADS 2/3 interpreter — frobtads is not packaged
    # in bookworm/trixie, must be built from source). Purged in the same
    # RUN layer below so they do not stay in the image.
    # frobtads v2.0 switched from autotools to CMake — `cmake` is required,
    # `autoconf`/`automake`/`libtool` are NOT (the v1.x ./bootstrap script
    # is gone in the v2.0 source tree).
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        build-essential cmake pkg-config \
        libcurl4-openssl-dev libncurses-dev git; \
    git clone --branch "${FROBTADS_TAG}" --depth 1 \
        https://github.com/realnc/frobtads.git /tmp/frobtads; \
    cd /tmp/frobtads; \
    cmake -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr/local .; \
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
        build-essential cmake pkg-config \
        libcurl4-openssl-dev libncurses-dev git; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        libcurl4 libncurses6 libtinfo6; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*

# cheapglk-linked glulxe (no ncurses dependency, clean stdio output).
COPY --from=glulxebuilder /src/glulxe/glulxe /usr/local/bin/glulxe

# Wrapper that chains inform7 → inform6 → Glulx behind a single entry point
# at /usr/local/bin/inform7 — compiler.go invokes that path verbatim. The
# build context for THIS Dockerfile is `docker/`, so the wrapper is read
# from docker/inform7-wrapper.sh (NOT service/scripts/...).
COPY inform7-wrapper.sh /usr/local/bin/inform7
RUN chmod +x /usr/local/bin/inform7

# Sanity check: every interpreter must resolve on PATH in the published
# image. Fails the build loudly if anything is missing. /usr/games is set
# inline here because ENV PATH is intentionally NOT declared in this base
# image — the per-commit `service/Dockerfile` sets the runtime PATH on top.
RUN set -eux; \
    export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/games; \
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
