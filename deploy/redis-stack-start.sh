#!/bin/sh
# Start the Redis Stack server as the container's own process.
#
# The image's own /entrypoint.sh runs redis-server as a child of a shell. The stop
# signal therefore reaches the shell and not the server: the shell dies, the server
# keeps running, and the container is killed once the grace period is over - without
# the final snapshot a normal shutdown would have written. This script does the same
# work as the image's script and hands the server over with exec, so the server is
# the process Docker stops, and it can write that snapshot on the way out.
#
# The command line is the image's own, taken from the sealed evidence:
#   tutorial/v2-j1-knowledge/frozen/image-entry-evidence/entrypoint.sh.txt:8-36
# The image is digest-pinned, so this copy cannot drift away from it silently: a
# different digest is a different image, and the command line is re-read from it.
#
# Nothing here prints the configuration or the credential.
set -eu

BASEDIR=/opt/redis-stack
cd "${BASEDIR}"

CMD="${BASEDIR}/bin/redis-server"
CONFFILE=""
if [ -f /redis-stack.conf ]; then
    CONFFILE=/redis-stack.conf
fi

if [ -z "${REDIS_DATA_DIR:-}" ]; then
    REDIS_DATA_DIR=/data
fi

# The search module is the reason this stack is a Stack image at all; the other
# modules are loaded the way the image loads them.
if [ -z "${REDISEARCH_ARGS:-}" ]; then
    REDISEARCH_ARGS="MAXSEARCHRESULTS 10000 MAXAGGREGATERESULTS 10000"
fi

# exec, not a plain call: the server has to become this process, so that the stop
# signal reaches it and so that its exit status is the container's exit status.
exec "${CMD}" \
${CONFFILE} \
--dir ${REDIS_DATA_DIR} \
--protected-mode no \
--daemonize no \
--loadmodule /opt/redis-stack/lib/rediscompat.so \
--loadmodule /opt/redis-stack/lib/redisearch.so ${REDISEARCH_ARGS} \
--loadmodule /opt/redis-stack/lib/redistimeseries.so ${REDISTIMESERIES_ARGS:-} \
--loadmodule /opt/redis-stack/lib/rejson.so ${REDISJSON_ARGS:-} \
--loadmodule /opt/redis-stack/lib/redisbloom.so ${REDISBLOOM_ARGS:-} \
--loadmodule /opt/redis-stack/lib/redisgears.so v8-plugin-path /opt/redis-stack/lib/libredisgears_v8_plugin.so ${REDISGEARS_ARGS:-} \
${REDIS_ARGS:-}
