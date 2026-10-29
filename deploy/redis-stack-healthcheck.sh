#!/bin/sh
# Liveness of the search index, not of a port: proves the server answered an authenticated
# FT._LIST and that the answer is a JSON array of index-name strings.
#
# Credential handling contract for this check:
#   * the value is read from the server configuration file at run time and handed to redis-cli
#     through REDISCLI_AUTH, so it never appears in argv (visible in `ps`/`docker inspect`), in
#     Compose's static environment (visible in `docker compose config`), or in this script's
#     output;
#   * what it does cost: for the lifetime of the probe, the value is readable in the process
#     environment of the healthcheck child inside this container - i.e. by a process in the same
#     container running as the same uid, and by root there. It is not visible to the Redis
#     client's peers, to other containers, or to the host's process list. `REDIS_ARGS` is
#     deliberately unused: an argument in the image's own command line is visible in `ps`.
#   * REDISCLI_AUTH is redis-cli's documented non-argv credential channel.
#
# Failure that must exit non-zero: unreadable configuration, missing requirepass, connection
# failure, authentication failure, missing module (unknown command), anything that is not a JSON
# array, and array elements that are not strings. An empty array is healthy: it is what a live
# search module answers before any index exists. A non-empty array of strings is healthy too.
#
# Test seam: REDIS_CLI substitutes the client binary so the whole script can be executed offline
# against synthetic replies; REDIS_STACK_CONF substitutes the configuration path.
set -eu

CONF="${REDIS_STACK_CONF:-/redis-stack.conf}"
CLI="${REDIS_CLI:-redis-cli}"
MIN_SECRET_BYTES=32

fail() {
    # A category and nothing else: the reply is summarised structurally, never echoed raw.
    echo "redis-stack health: $1" >&2
    exit 1
}

[ -r "$CONF" ] || fail "configuration unreadable"

secret=$(awk '/^[ 	]*requirepass[ 	]/{n++; v=$2} END{if (n != 1) exit 1; printf "%s", v}' "$CONF") \
    || fail "requirepass is not exactly one directive"
[ -n "$secret" ] || fail "requirepass is empty"
[ "${#secret}" -ge "$MIN_SECRET_BYTES" ] || fail "requirepass is shorter than the agreed floor"

reply=$(REDISCLI_AUTH="$secret" "$CLI" -2 --json FT._LIST 2>&1) \
    || fail "probe command failed"

case "$reply" in
    *NOAUTH*|*WRONGPASS*|*"invalid password"*) fail "authentication rejected" ;;
    *"unknown command"*|*"Unknown command"*|*"ERR unknown"*) fail "search module missing" ;;
    *"Could not connect"*|*"Connection refused"*|*"connection was closed"*) fail "connection failed" ;;
esac
case "$reply" in
    "(error)"*|*"(error)"*) fail "server returned an error reply" ;;
esac

# Structure: a JSON array whose elements are all strings. Quoted strings are removed first; what
# remains may only be separators. That rejects bare numbers, objects, nested arrays, and any
# non-JSON text, without needing a JSON parser inside the image.
case "$reply" in
    \[*\]) : ;;
    *) fail "reply is not a JSON array" ;;
esac

body=${reply#\[}
body=${body%\]}

quotes=$(printf '%s' "$body" | tr -cd '\"' | wc -c)
[ $((quotes % 2)) -eq 0 ] || fail "unbalanced string in reply"

stripped=$(printf '%s' "$body" | sed 's/"[^"]*"//g')
case "$stripped" in
    *[![:space:],]*) fail "reply contains a non-string element" ;;
esac
case "$body" in
    *[![:space:],\"A-Za-z0-9._:\/-]*) fail "reply contains unexpected characters" ;;
esac

printf 'redis-stack health: ok (indexes=%s)\n' "$((quotes / 2))"
exit 0
