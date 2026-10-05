#!/bin/sh
# Runs cloudflared only while this node's app answers /healthz.
#
# Cloudflare Tunnel doesn't check origins: a connector whose app is down,
# hung or restarting keeps receiving requests and answers them with 502.
# This guard polls the app and stops cloudflared after GUARD_FAILS failed
# checks in a row, so Cloudflare moves traffic to the other nodes' connectors;
# it starts cloudflared again as soon as the app is healthy.

HEALTH_URL=${HEALTH_URL:-http://app:8080/healthz}
INTERVAL=${GUARD_INTERVAL:-2}
FAILS=${GUARD_FAILS:-2}
TIMEOUT=${GUARD_TIMEOUT:-2}

pid=
bad=0

log() { echo "[tunnel-guard] $*"; }

running() { [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; }

start_tunnel() {
	cloudflared tunnel --no-autoupdate --grace-period 2s run &
	pid=$!
	log "app healthy; cloudflared started (pid $pid)"
}

stop_tunnel() {
	running || { pid=; return; }
	kill -TERM "$pid" 2>/dev/null
	i=0
	while running && [ $i -lt 10 ]; do sleep 0.5; i=$((i + 1)); done
	running && kill -KILL "$pid" 2>/dev/null
	wait "$pid" 2>/dev/null
	pid=
}

trap 'stop_tunnel; exit 0' TERM INT

while :; do
	if wget -q -T "$TIMEOUT" -O /dev/null "$HEALTH_URL" 2>/dev/null; then
		bad=0
		if ! running; then
			[ -n "$pid" ] && { wait "$pid" 2>/dev/null; log "cloudflared exited; restarting"; }
			start_tunnel
		fi
	else
		bad=$((bad + 1))
		if [ "$bad" -ge "$FAILS" ] && running; then
			log "app failed $bad health checks; stopping cloudflared"
			stop_tunnel
		fi
	fi
	# Sleep in the background so TERM is handled straight away.
	sleep "$INTERVAL" &
	wait $!
done
