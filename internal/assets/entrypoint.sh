#!/bin/bash
# Entrypoint supplied by scaleset-runner to every Runner Container (FR-032).
# It starts the inner Docker daemon, then the ephemeral runner with its JIT
# Config, and prints the Ready marker once the runner listens for Jobs.
set -uo pipefail

marker="${SCALESET_READY_MARKER:?}"
jit_file=/scaleset-runner/jitconfig

jit="$(cat "$jit_file")" || { echo "scaleset-runner: JIT Config missing" >&2; exit 1; }
rm -f "$jit_file"

if command -v dockerd >/dev/null 2>&1; then
	dockerd >/var/log/dockerd.log 2>&1 &
	for _ in $(seq 1 120); do
		docker info >/dev/null 2>&1 && break
		sleep 0.25
	done
	if ! docker info >/dev/null 2>&1; then
		echo "scaleset-runner: inner Docker daemon did not start" >&2
		tail -n 50 /var/log/dockerd.log >&2
		exit 1
	fi
	chmod 0660 /var/run/docker.sock 2>/dev/null || true
fi

cd /home/runner || exit 1
ACTIONS_RUNNER_INPUT_JITCONFIG="$jit" runuser -u runner -w ACTIONS_RUNNER_INPUT_JITCONFIG -- ./run.sh 2>&1 |
	while IFS= read -r line; do
		printf '%s\n' "$line"
		case "$line" in
		*"Listening for Jobs"*) printf '%s\n' "$marker" ;;
		esac
	done
exit "${PIPESTATUS[0]}"
