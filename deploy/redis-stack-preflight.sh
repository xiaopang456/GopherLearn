#!/bin/sh
# Refuse to start the Redis Stack server unless its own configuration file is present and
# conforms to this stack's access-control contract. The image's /entrypoint.sh only sets
# CONFFILE when /redis-stack.conf exists and otherwise starts redis-server with no configuration
# at all, so "file missing" cannot be left to the entrypoint: it would come up without any
# access control. Nothing here prints the file, a line of it, or the credential.
#
# Sealed provenance of that behaviour:
#   tutorial/v2-j1-knowledge/frozen/image-entry-evidence/entrypoint.sh.txt:8-10,25-36
#   tutorial/v2-j1-knowledge/frozen/image-entry-evidence/entry-manifest.json (image 7.4.0-v8,
#   platform digest sha256:39adcbb0...)
#
# Test seams are environment variables with production defaults, so the same script can be run
# against synthetic fixtures offline: REDIS_STACK_CONF picks the configuration path,
# REDIS_STACK_ENTRYPOINT picks what gets exec'd after the checks pass.
set -eu

CONF="${REDIS_STACK_CONF:-/redis-stack.conf}"
ENTRY="${REDIS_STACK_ENTRYPOINT:-/entrypoint.sh}"
MIN_SECRET_BYTES=32

refuse() {
    # Category only. No path contents, no values, never to stdout.
    echo "redis-stack preflight: $1" >&2
    exit 1
}

[ -f "$CONF" ] || refuse "configuration missing"
[ -r "$CONF" ] || refuse "configuration unreadable"
[ -s "$CONF" ] || refuse "configuration empty"

# Exactly one requirepass directive, at the start of the line, is the contract. Zero means the
# server would start unauthenticated; two means two operators think they own one credential.
count=$(awk '/^[ 	]*requirepass[ 	]/{n++} END{print n+0}' "$CONF") || refuse "configuration unreadable"
[ "$count" -eq 1 ] || refuse "requirepass must appear exactly once"

secret=$(awk '/^[ 	]*requirepass[ 	]/{print $2}' "$CONF") || refuse "configuration unreadable"
[ -n "$secret" ] || refuse "requirepass value is empty"

# The accepted form is an unquoted single token: quoting characters or embedded whitespace mean
# the server and the healthcheck read different values. An unfilled template placeholder is
# treated as no credential at all.
case "$secret" in
    REPLACE_*) refuse "requirepass is an unfilled placeholder" ;;
esac
case "$secret" in
    *[!A-Za-z0-9._~+\-/]*) refuse "requirepass carries quoting or whitespace" ;;
esac
[ "${#secret}" -ge "$MIN_SECRET_BYTES" ] || refuse "requirepass is shorter than the agreed floor"

# Only now hand over to the image's own entrypoint. The exec keeps the same process, so Docker
# still owns start and stop, and the arguments stay exactly what the image declares.
exec "$ENTRY"
