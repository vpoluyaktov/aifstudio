#!/bin/sh
set -e
# Ensure the app user owns the data volume directories on every start.
# Bind-mounted host directories are often created by root or the host user,
# so we chown here (as root) before dropping privileges — the same pattern
# used by postgres, mysql, and redis official images.
chown -R app:app /app/data
exec runuser -u app -- /usr/local/bin/aifstudio "$@"
