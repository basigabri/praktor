#!/usr/bin/env bash
# Runs the end-to-end test inside a throwaway Docker-in-Docker daemon, so it
# can never touch a live Praktor deployment (containers, praktor-net, volumes)
# on this machine. Image layers are cached in the praktor-e2e-dind-cache
# volume between runs; E2E_DIND_CLEAN=1 removes it afterwards.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
NAME=praktor-e2e-dind
CACHE=praktor-e2e-dind-cache
OUT=${E2E_LOG_DIR:-$ROOT/test/e2e/logs}

cleanup() {
	local rc=$?
	rm -rf "$OUT"
	mkdir -p "$OUT"
	docker cp "$NAME:/logs/." "$OUT" >/dev/null 2>&1 || true
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	if [[ -n ${E2E_DIND_CLEAN:-} ]]; then docker volume rm "$CACHE" >/dev/null 2>&1 || true; fi
	echo "logs in $OUT"
	exit "$rc"
}
trap cleanup EXIT

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --privileged --name "$NAME" -e DOCKER_TLS_CERTDIR= \
	-v "$CACHE:/var/lib/docker" -v "$ROOT:/src:ro" docker:29-dind >/dev/null
for _ in $(seq 60); do
	docker exec "$NAME" docker info >/dev/null 2>&1 && break
	sleep 1
done
docker exec "$NAME" apk add --no-cache -q bash curl jq coreutils
docker exec -e E2E_LOG_DIR=/logs -e E2E_SKIP_BUILD -e E2E_STRICT "$NAME" bash /src/test/e2e/run.sh
