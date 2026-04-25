#!/bin/sh
# inform7 wrapper — drives the full Inform 7 → Glulx pipeline.
#
# compiler.go invokes /usr/local/bin/inform7 with:
#   --internal <dir>   built-in material (Standard Rules, Inter pipelines, I6 templates)
#   --external <dir>   user-installed material; caller may pass "." which is read-only
#                      on Cloud Run, so we always redirect to $TMPDIR/inform7-ext
#   --project <dir>    the Inform project directory (*.inform)
#
# Stage 1: Inform 7 → Inform 6 intermediate source (<project>/Build/auto.inf).
# Stage 2: Inform 6 → Glulx story file   (<project>/Build/output.ulx).
#
# Exit codes:
#   0   success — <project>/Build/output.ulx is ready to package
#   1   stage 1 failure (Inform 7 source rejected)
#   2   bad arguments
#   3   stage 2 failure (Inform 6 back-end failure, usually a compiler bug)

set -eu

INTERNAL=""
EXTERNAL=""
PROJECT=""

while [ $# -gt 0 ]; do
  case "$1" in
    --internal|-internal) INTERNAL="${2:-}"; shift 2 ;;
    --external|-external) EXTERNAL="${2:-}"; shift 2 ;;
    --project|-project)   PROJECT="${2:-}"; shift 2 ;;
    --version|-version)   exec /opt/inform7/bin/inform7 -version ;;
    --help|-help|-h)      exec /opt/inform7/bin/inform7 -help ;;
    *)
      echo "inform7 wrapper: unknown arg '$1'" >&2
      exit 2
      ;;
  esac
done

if [ -z "$INTERNAL" ] || [ -z "$PROJECT" ]; then
  echo "inform7 wrapper: --internal and --project are required" >&2
  exit 2
fi

# The caller's --external is intentionally ignored: on Cloud Run the working
# directory is read-only, and -external is only used here as a scratch area
# for the extensions census. A throwaway tmpdir is always correct.
: "${EXTERNAL:=unused}"
EXT="${TMPDIR:-/tmp}/inform7-ext"
mkdir -p "$EXT"

# ─── Stage 1: Inform 7 ──────────────────────────────────────────────────────
# Census is off by default on the CLI, so no extra flag is needed to avoid
# loading every third-party extension in $INTERNAL/Extensions. The Dockerfile
# also removes the specific Skeleton Keys.i7x that shipped incompatible with
# the v10.1.2 Standard Rules (duplicate "to unlock" verb definition).
/opt/inform7/bin/inform7 \
  -internal "$INTERNAL" \
  -external "$EXT" \
  -project "$PROJECT" \
  || { rc=$?; echo "inform7 wrapper: stage 1 (Inform 7) failed (rc=$rc)" >&2; exit 1; }

AUTO_INF="$PROJECT/Build/auto.inf"
OUTPUT_ULX="$PROJECT/Build/output.ulx"

if [ ! -f "$AUTO_INF" ]; then
  echo "inform7 wrapper: stage 1 produced no auto.inf at $AUTO_INF" >&2
  exit 1
fi

# ─── Stage 2: Inform 6 → Glulx ──────────────────────────────────────────────
# -G   target Glulx VM (default is Z-machine, which cannot encode @glk opcodes)
# -wE2 warning level 2, errors as errors
# ~S   suppress strict mode (matches what the IDE sets for Release builds)
# ~X   suppress infix debugger (not needed at runtime)
# +include_path lists the I6 include dirs: the project's own Source/, and the
# Miscellany directory from the Inform 7 install (contains the Glulx library).
/opt/inform7/bin/inform6 -G -wE2~S~X \
  "+include_path=$PROJECT/Source,$INTERNAL/Miscellany" \
  "$AUTO_INF" \
  "$OUTPUT_ULX" \
  || { rc=$?; echo "inform7 wrapper: stage 2 (Inform 6) failed (rc=$rc)" >&2; exit 3; }

if [ ! -f "$OUTPUT_ULX" ]; then
  echo "inform7 wrapper: stage 2 reported success but produced no $OUTPUT_ULX" >&2
  exit 3
fi
