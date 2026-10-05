#!/usr/bin/env bash
# Hermetic end-to-end test: a gateway built from this tree, real agent
# containers (Claude Code) and a mock Anthropic API. No API keys, no Telegram.
#
# It needs a Docker daemon it may fill with test containers. It refuses to run
# next to a live Praktor deployment; on a workstation use local-dind.sh, which
# runs it inside a throwaway Docker-in-Docker daemon.
#
# Env:
#   E2E_SKIP_BUILD=1   reuse the praktor-*:e2e images
#   E2E_STRICT=1       fail on known bugs instead of reporting them
#   E2E_LOG_DIR        where logs go (default test/e2e/logs)
#   E2E_ALLOW_SHARED_DOCKER=1  skip the live-deployment guard (don't)
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOG_DIR=${E2E_LOG_DIR:-$ROOT/test/e2e/logs}
PORT=${E2E_PORT:-18080}
MOCK_PORT=${E2E_MOCK_PORT:-18081}
API="http://127.0.0.1:${PORT}"
MOCK_API="http://127.0.0.1:${MOCK_PORT}"

GW=praktor-e2e-gw
MOCK=praktor-e2e-mockllm
PROXY=praktor-e2e-sockproxy
GWNET=praktor-e2e-gwnet
AGENT_NET=praktor-net
DATA_VOL=praktor-e2e-data
SOCK_VOL=praktor-e2e-sock
IMG_GW=praktor-gateway:e2e
IMG_AGENT=praktor-agent:e2e
IMG_MOCK=praktor-mockllm:e2e
WEB_PASS=e2e-web-pass
export PRAKTOR_WEB_PASSWORD=$WEB_PASS PRAKTOR_VAULT_PASSPHRASE=e2e-vault-passphrase

mkdir -p "$LOG_DIR"
: >"$LOG_DIR/known-bugs.txt"
: >"$LOG_DIR/results.txt"
START=$(date +%s)
created_agent_net=0

# --- helpers -----------------------------------------------------------------

log() { printf '[%4ss] %s\n' "$(($(date +%s) - START))" "$*"; }
pass() { log "PASS  $*"; echo "PASS  $*" >>"$LOG_DIR/results.txt"; }
fail() {
	log "FAIL  $*"
	echo "FAIL  $*" >>"$LOG_DIR/results.txt"
	exit 1
}
known_bug() {
	log "KNOWN BUG  $*"
	echo "$*" >>"$LOG_DIR/known-bugs.txt"
	echo "BUG   $*" >>"$LOG_DIR/results.txt"
	if [[ -n ${E2E_STRICT:-} ]]; then exit 1; fi
}
note() { log "NOTE  $*"; echo "NOTE  $*" >>"$LOG_DIR/results.txt"; }

# chat BODY [MAX_TIME] [AUTH]: POST /api/chat (BODY "@file" reads a file);
# sets CODE, BODY and ELAPSED.
chat() {
	local body=$1 max=${2:-300} auth=${3:-yes} t0
	local args=(-sS -o "$LOG_DIR/.chat" -w '%{http_code}' --max-time "$max" -H 'Content-Type: application/json')
	[[ $auth == yes ]] && args+=(-u "api:$WEB_PASS")
	t0=$(date +%s)
	CODE=$(curl "${args[@]}" --data-binary "$body" "$API/api/chat" || true)
	ELAPSED=$(($(date +%s) - t0))
	BODY=$(cat "$LOG_DIR/.chat" 2>/dev/null || true)
}

api_get() { curl -sS -u "api:$WEB_PASS" -o /dev/null -w '%{http_code}' --max-time 10 "$API$1" || true; }
api_post() { curl -sS -u "api:$WEB_PASS" -o /dev/null -w '%{http_code}' --max-time 30 -X POST "$API$1" || true; }
mock_stat() { curl -sS --max-time 5 "$MOCK_API/stats" | jq -r ".$1"; }

# wait_for DESCRIPTION TIMEOUT COMMAND...: retry COMMAND until it succeeds.
wait_for() {
	local what=$1 timeout=$2
	shift 2
	local deadline=$(($(date +%s) + timeout))
	until "$@" >/dev/null 2>&1; do
		if (($(date +%s) > deadline)); then fail "timed out after ${timeout}s waiting for $what"; fi
		sleep 1
	done
}

gw_logs() { docker logs "$GW" 2>&1; }
gw_logs_since() { docker logs --since "$1" "$GW" 2>&1; }
running() { [[ $(docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null) == true ]]; }
agents_running() { docker ps --filter 'label=praktor.managed=true' --format '{{.Names}}' | sort | tr '\n' ' '; }

save_logs() {
	set +e
	docker ps -a >"$LOG_DIR/docker-ps.txt" 2>&1
	for c in $GW $MOCK $PROXY $(docker ps -a --filter 'label=praktor.managed=true' --format '{{.Names}}'); do
		docker logs "$c" >"$LOG_DIR/$c.log" 2>&1
	done
	set -e
}

# purge removes everything this test creates (also leftovers of a run that
# was killed before its cleanup).
purge() {
	{
		docker rm -f $GW $MOCK $PROXY
		docker ps -aq --filter 'label=praktor.managed=true' | xargs -r docker rm -f
		docker volume rm $DATA_VOL $SOCK_VOL praktor-wk-e2e-alpha praktor-wk-e2e-beta \
			praktor-home-e2e-alpha praktor-home-e2e-beta
		docker network rm $GWNET
		if ((created_agent_net)); then docker network rm $AGENT_NET; fi
	} >/dev/null 2>&1 || true
}

cleanup() {
	local rc=$?
	set +e
	log "cleanup (exit $rc)"
	save_logs
	purge
	log "logs in $LOG_DIR"
	exit "$rc"
}

# --- guard -------------------------------------------------------------------

if [[ -z ${E2E_ALLOW_SHARED_DOCKER:-} ]]; then
	if docker ps -a --format '{{.Names}}' | grep -qx praktor; then
		echo "A container named 'praktor' exists: this looks like a live deployment." >&2
		echo "Run test/e2e/local-dind.sh instead (isolated Docker daemon)." >&2
		exit 2
	fi
	foreign=$(docker network inspect $AGENT_NET -f '{{range .Containers}}{{.Name}} {{end}}' 2>/dev/null | tr ' ' '\n' | grep -v '^praktor-e2e-' | tr '\n' ' ' || true)
	if [[ -n ${foreign// /} ]]; then
		echo "$AGENT_NET already has containers ($foreign); refusing to share it. Use local-dind.sh." >&2
		exit 2
	fi
fi
trap cleanup EXIT
purge

# --- build -------------------------------------------------------------------

if [[ -z ${E2E_SKIP_BUILD:-} ]]; then
	log "building images (gateway, agent, mock)"
	docker build -q -t $IMG_MOCK -f "$ROOT/test/e2e/mockllm/Dockerfile" "$ROOT" >/dev/null
	docker build -q -t $IMG_GW --build-arg VERSION=e2e -f "$ROOT/Dockerfile" "$ROOT" >/dev/null &
	gw_build=$!
	docker build -q -t $IMG_AGENT -f "$ROOT/Dockerfile.agent" "$ROOT" >/dev/null
	wait $gw_build
	log "images built"
fi

# --- bring up ----------------------------------------------------------------

SOCK_GID=$(docker run --rm -v /var/run/docker.sock:/var/run/docker.sock alpine:3.23 stat -c %g /var/run/docker.sock)
docker network create $GWNET >/dev/null
if ! docker network inspect $AGENT_NET >/dev/null 2>&1; then
	docker network create $AGENT_NET >/dev/null
	created_agent_net=1
fi
docker run -d --name $MOCK --network $AGENT_NET --network-alias mockllm \
	-p "127.0.0.1:${MOCK_PORT}:8000" $IMG_MOCK >/dev/null
wait_for "mock API" 30 curl -fsS "$MOCK_API/healthz"

CFG_DIR=$(mktemp -d)
chmod 755 "$CFG_DIR" # docker cp keeps modes; the gateway runs as uid 10321
cat >"$CFG_DIR/praktor.yaml" <<'YAML'
defaults:
  image: "praktor-agent:e2e"
  model: "mock-model"
  max_running: 3
  idle_timeout: 10m

agents:
  alpha:
    description: "Handles every message unless it says ROUTE-TO-BETA."
    workspace: e2e-alpha
    env: &mock
      ANTHROPIC_BASE_URL: "http://mockllm:8000"
      ANTHROPIC_AUTH_TOKEN: "dummy-not-a-key"
      ANTHROPIC_SMALL_FAST_MODEL: "mock-model"
      ANTHROPIC_DEFAULT_HAIKU_MODEL: "mock-model"
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1"
      DISABLE_AUTOUPDATER: "1"
  beta:
    description: "Only for messages that contain ROUTE-TO-BETA."
    workspace: e2e-beta
    env: *mock

router:
  default_agent: alpha

web:
  enabled: true
  port: 8080
YAML
chmod 644 "$CFG_DIR/praktor.yaml"

# wait_gateway DESCRIPTION: wait for the web server; fail at once if it exited.
wait_gateway() {
	local deadline=$(($(date +%s) + 60))
	until [[ $(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "$API/api/auth/check" 2>/dev/null) == 401 ]]; do
		running $GW || fail "gateway exited while waiting for $1: $(docker logs --tail 5 $GW 2>&1)"
		(($(date +%s) > deadline)) && fail "timed out waiting for $1"
		sleep 1
	done
}

# run_gateway [extra docker args...]: (re)create the gateway on a plain bridge
# network with a custom hostname, so it must attach itself to praktor-net.
run_gateway() {
	docker logs $GW >>"$LOG_DIR/$GW.previous.log" 2>&1 || true
	docker rm -f $GW >/dev/null 2>&1 || true
	docker create --name $GW --hostname $GW --network $GWNET \
		-p "127.0.0.1:${PORT}:8080" --group-add "$SOCK_GID" \
		-v $DATA_VOL:/data \
		-e PRAKTOR_CONFIG=/etc/praktor/praktor.yaml \
		-e PRAKTOR_WEB_PASSWORD -e PRAKTOR_VAULT_PASSPHRASE -e TZ=UTC \
		"$@" $IMG_GW >/dev/null
	docker cp "$CFG_DIR/." "$GW:/etc/praktor" >/dev/null
	docker start $GW >/dev/null
}

log "starting gateway"
run_gateway -v /var/run/docker.sock:/var/run/docker.sock
wait_gateway "gateway web server"

# --- phase 1: happy path -----------------------------------------------------

wait_for "self-attach log line" 60 sh -c "docker logs $GW 2>&1 | grep -q 'attached gateway to agent network'"
pass "gateway logs 'attached gateway to agent network'"
aliases=$(docker inspect $GW -f "{{json (index .NetworkSettings.Networks \"$AGENT_NET\").Aliases}}")
[[ $aliases == *"$GW"* ]] || fail "gateway on $AGENT_NET without its hostname alias: $aliases"
pass "gateway joined $AGENT_NET with alias $GW"
gw_logs | grep -q 'telegram token not set, bot disabled' || fail "gateway did not start without a Telegram token"
pass "gateway runs without Telegram (no token)"

[[ $(curl -sS -o /dev/null -w '%{http_code}' "$API/api/status") == 401 ]] || fail "/api/status without auth is not 401"
[[ $(api_get /api/status) == 200 ]] || fail "/api/status with auth is not 200"
pass "auth required on /api"

log "first chat: starts the alpha container (Claude Code against the mock)"
chat '{"message":"PING one","agent":"alpha"}' 300
[[ $CODE == 200 ]] || fail "chat: HTTP $CODE $BODY"
[[ $(jq -r .agent <<<"$BODY") == alpha && $(jq -r .reply <<<"$BODY") == "MOCK-REPLY: PING one"* ]] ||
	fail "chat reply: $BODY"
pass "POST /api/chat returns the mock's reply (${ELAPSED}s, cold start)"
(($(mock_stat streaming) > 0)) || fail "mock saw no streaming request"
note "mock stats after first chat: $(curl -sS "$MOCK_API/stats")"

chat '{"message":"PING two","agent":"alpha"}' 120
[[ $CODE == 200 && $(jq -r .reply <<<"$BODY") == "MOCK-REPLY: PING two"* ]] || fail "warm chat: $CODE $BODY"
pass "warm chat (${ELAPSED}s)"

# Documented error codes.
expect_code() { # NAME WANT BODY [AUTH]
	chat "$3" 30 "${4:-yes}"
	[[ $CODE == "$2" ]] || fail "$1: HTTP $CODE (want $2) $BODY"
	pass "$1 -> $2"
}
expect_code "no credentials" 401 '{"message":"PING x","agent":"alpha"}' no
expect_code "invalid JSON" 400 '{"message":'
expect_code "empty message" 400 '{"message":"   "}'
expect_code "unknown agent" 404 '{"message":"PING","agent":"nope"}'
expect_code "@swarm" 400 '{"message":"@swarm alpha,beta: x"}'
expect_code "timeout out of range" 400 '{"message":"PING","timeout":601}'
{
	printf '{"message":"'
	head -c 1100000 /dev/zero | tr '\0' x
	printf '"}'
} >"$LOG_DIR/.big.json"
expect_code "body over 1 MiB" 413 "@$LOG_DIR/.big.json"

log "504: a reply slower than the timeout"
chat '{"message":"PING SLOW 8","agent":"alpha","timeout":3}' 30
[[ $CODE == 504 && $ELAPSED -le 6 ]] || fail "slow reply: HTTP $CODE after ${ELAPSED}s, want 504 after ~3s"
pass "timeout -> 504 after ${ELAPSED}s"
wait_for "slow run to finish" 30 sh -c "[ \$(curl -sS $MOCK_API/stats | jq .in_flight) = 0 ]"
sleep 2
chat '{"message":"PING three","agent":"alpha"}' 60
[[ $CODE == 200 && $(jq -r .reply <<<"$BODY") == "MOCK-REPLY: PING three"* ]] ||
	fail "the late reply of a timed-out request answered another request: $BODY"
pass "a late reply never answers the next request"

log "smart routing (no agent): routes through alpha to beta"
chat '{"message":"PING ROUTE-TO-BETA"}' 300
[[ $CODE == 200 && $(jq -r .agent <<<"$BODY") == beta ]] || fail "smart routing: HTTP $CODE $BODY"
pass "smart routing picked beta (${ELAPSED}s)"
[[ $(agents_running) == "praktor-agent-alpha praktor-agent-beta " ]] || fail "agents running: $(agents_running)"

# --- phase 2: break it -------------------------------------------------------

log "BREAK: kill the agent container in the middle of a run"
slow_before=$(mock_stat slow_started)
chat_bg_out="$LOG_DIR/.kill-chat"
(
	chat '{"message":"PING SLOW 60","agent":"alpha","timeout":20}' 60
	echo "$CODE $ELAPSED" >"$chat_bg_out"
) &
bg=$!
wait_for "the slow run to reach the mock" 30 sh -c "[ \$(curl -sS $MOCK_API/stats | jq .slow_started) -gt $slow_before ]"
docker logs praktor-agent-alpha >"$LOG_DIR/praktor-agent-alpha.before-kill.log" 2>&1 || true
docker kill praktor-agent-alpha >/dev/null
wait $bg
read -r code elapsed <"$chat_bg_out"
[[ $code == 504 ]] || fail "request whose agent was killed: HTTP $code, want 504"
((elapsed <= 25)) || fail "request whose agent was killed took ${elapsed}s (timeout 20s)"
pass "agent killed mid-request -> 504 after ${elapsed}s"
running $GW || fail "gateway died after the agent was killed"
[[ $(api_get /api/status) == 200 ]] || fail "gateway unhealthy after agent kill"
pass "gateway healthy after agent kill"

chat '{"message":"PING after-kill","agent":"alpha","timeout":30}' 45
if [[ $CODE == 200 ]]; then
	pass "next message after the kill restarted the agent"
else
	known_bug "after its container died, messages to the agent are lost (HTTP $CODE after ${ELAPSED}s): the gateway still lists it as running and publishes to a dead container until the idle timeout; no Docker event watch"
	[[ $(api_post /api/agents/definitions/alpha/stop) == 200 ]] || fail "stop alpha"
	chat '{"message":"PING after-stop","agent":"alpha"}' 300
	[[ $CODE == 200 ]] || fail "after stopping alpha via the API: HTTP $CODE $BODY"
	pass "POST /api/agents/definitions/alpha/stop recovers the agent"
fi

log "BREAK: disconnect the gateway from $AGENT_NET"
docker network disconnect $AGENT_NET $GW
sleep 3
chat '{"message":"PING cut-off","agent":"beta","timeout":15}' 30
note "chat to a running agent with the gateway off $AGENT_NET: HTTP $CODE after ${ELAPSED}s"
[[ $CODE == 504 ]] || fail "expected 504 when agents can't reach NATS, got $CODE $BODY"
running $GW && [[ $(api_get /api/status) == 200 ]] || fail "gateway unhealthy after network disconnect"
if gw_logs_since 60s | grep -q 'attached gateway to agent network'; then
	note "the gateway re-attached itself"
else
	note "the gateway does not re-attach after being removed from $AGENT_NET at runtime (attach runs once at startup); a restart fixes it"
fi
pass "gateway survives losing $AGENT_NET"

log "BREAK: restart the gateway (state must persist)"
t_restart=$(date -u +%Y-%m-%dT%H:%M:%SZ)
docker stop -t 30 $GW >/dev/null
[[ -z $(agents_running) ]] || fail "agent containers left after gateway stop: $(agents_running)"
pass "gateway stop removes its agent containers"
docker start $GW >/dev/null
wait_gateway "gateway after restart"
wait_for "self-attach after restart" 60 sh -c "docker logs --since $t_restart $GW 2>&1 | grep -q 'attached gateway to agent network'"
pass "re-attached to $AGENT_NET after restart"
hist=$(curl -sS -u "api:$WEB_PASS" "$API/api/agents/definitions/alpha/messages")
grep -q 'PING one' <<<"$hist" || fail "message history lost across restart"
pass "message history persisted across restart"
chat '{"message":"PING after-restart","agent":"alpha"}' 300
[[ $CODE == 200 ]] || fail "chat after restart: HTTP $CODE $BODY"
pass "chat works after restart (${ELAPSED}s)"

log "BREAK: Docker API goes away under a running gateway"
docker run -d --name $PROXY -v /var/run/docker.sock:/var/run/docker.sock -v $SOCK_VOL:/sock \
	alpine/socat UNIX-LISTEN:/sock/docker.sock,unlink-early,fork,mode=666 UNIX-CONNECT:/var/run/docker.sock >/dev/null
wait_for "socket proxy" 20 docker run --rm -v $SOCK_VOL:/sock alpine:3.23 test -S /sock/docker.sock
docker stop -t 30 $GW >/dev/null
t_proxy=$(date -u +%Y-%m-%dT%H:%M:%SZ)
run_gateway -v $SOCK_VOL:/sock -e DOCKER_HOST=unix:///sock/docker.sock
wait_gateway "gateway via socket proxy"
wait_for "self-attach via proxy" 60 sh -c "docker logs --since $t_proxy $GW 2>&1 | grep -q 'attached gateway to agent network'"
chat '{"message":"PING via-proxy","agent":"alpha"}' 300
[[ $CODE == 200 ]] || fail "chat through the socket proxy: HTTP $CODE $BODY"
docker rm -f $PROXY >/dev/null
chat '{"message":"PING no-docker","agent":"alpha"}' 60
[[ $CODE == 200 ]] || fail "running agent stopped answering without the Docker API: HTTP $CODE"
pass "running agent keeps answering without the Docker API"
chat '{"message":"PING no-docker","agent":"beta","timeout":15}' 30
[[ $CODE == 504 ]] || fail "starting an agent without the Docker API: HTTP $CODE (want 504) $BODY"
running $GW && [[ $(api_get /api/status) == 200 ]] || fail "gateway unhealthy without the Docker API"
gw_logs | grep -q 'execute message failed' || fail "no error logged for the failed agent start"
pass "without the Docker API: 504, error logged, gateway healthy"
docker run -d --name $PROXY -v /var/run/docker.sock:/var/run/docker.sock -v $SOCK_VOL:/sock \
	alpine/socat UNIX-LISTEN:/sock/docker.sock,unlink-early,fork,mode=666 UNIX-CONNECT:/var/run/docker.sock >/dev/null
sleep 2
chat '{"message":"PING docker-back","agent":"beta"}' 300
[[ $CODE == 200 ]] || fail "agent start after the Docker API came back: HTTP $CODE $BODY"
pass "recovers when the Docker API comes back"

# --- teardown checks ---------------------------------------------------------

if docker logs $GW 2>&1 | grep -Eq 'panic:|goroutine [0-9]+ \[|http: panic serving'; then
	fail "gateway panicked (see $GW.log)"
fi
pass "no panics in gateway logs"
docker stop -t 30 $GW >/dev/null
[[ -z $(agents_running) ]] || fail "agent containers left after final stop: $(agents_running)"
pass "all agent containers cleaned up"
log "done in $(($(date +%s) - START))s; known bugs: $(wc -l <"$LOG_DIR/known-bugs.txt")"
