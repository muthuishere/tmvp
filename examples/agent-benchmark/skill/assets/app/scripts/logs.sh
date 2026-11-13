#!/bin/sh
# tmvp: read
# description: app container logs for this env — logs [since, default 15m]
c=$(docker ps --filter "name=acme-app-$APP_ENV-" --format '{{.Names}}' | head -1)
[ -n "$c" ] || { echo "no app container for $APP_ENV" >&2; exit 1; }
docker logs --since "${1:-15m}" "$c" 2>&1 | tail -200
