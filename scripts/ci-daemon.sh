#!/usr/bin/env bash
set -uo pipefail

# Start/stop a Vision daemon for CI.
#
# The opencode-plugin integration tests gate on a live daemon on port 6275,
# and the shared gate in src/daemon-gate.ts refuses (does not skip) when CI is
# set and the probe fails. The CI Test job runs this script around the plugin
# check to guarantee that daemon.
#
# The daemon runs with an empty-registry fixture ('servers: {}'):
# config.Validate accepts zero servers, and the Admin MCP server registers its
# ten management tools independent of operator config, so the fixture is
# deterministic and still exercises exactly the daemon-internal behaviour the
# tests assert.
#
# Usage: ci-daemon.sh start|stop
#
# start is idempotent: when something already answers on the health endpoint,
# it starts nothing and stop later removes nothing. Only a daemon this script
# started is ever stopped.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

WORK_DIR="${TMPDIR:-/tmp}/vision-ci-daemon"
CONFIG_PATH="${WORK_DIR}/servers.yaml"
LOG_PATH="${WORK_DIR}/daemon.log"
PID_PATH="${WORK_DIR}/daemon.pid"
BIN_PATH="${WORK_DIR}/vision"

HEALTH_URL="http://localhost:6275/health"
HEALTH_TIMEOUT_SECS=30

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

health_ok() {
    curl -fsS -m 2 "$HEALTH_URL" >/dev/null 2>&1
}

cmd_start() {
    if health_ok; then
        printf 'Vision daemon already healthy at %s; not starting another\n' "$HEALTH_URL"
        return 0
    fi

    mkdir -p "$WORK_DIR" || fail "cannot create work dir: $WORK_DIR"
    printf 'servers: {}\n' > "$CONFIG_PATH"

    (cd "$REPO_ROOT" && go build -o "$BIN_PATH" ./cmd/vision) || fail "go build ./cmd/vision failed"

    "$BIN_PATH" daemon start -c "$CONFIG_PATH" > "$LOG_PATH" 2>&1 &
    printf '%s\n' "$!" > "$PID_PATH"

    for _ in $(seq 1 "$HEALTH_TIMEOUT_SECS"); do
        if health_ok; then
            printf 'Vision daemon healthy at %s (pid %s, config %s)\n' \
                "$HEALTH_URL" "$(cat "$PID_PATH")" "$CONFIG_PATH"
            return 0
        fi
        if ! kill -0 "$(cat "$PID_PATH")" 2>/dev/null; then
            printf 'daemon log (%s):\n' "$LOG_PATH" >&2
            cat "$LOG_PATH" >&2 || true
            fail "daemon exited before becoming healthy"
        fi
        sleep 1
    done

    printf 'daemon log (%s):\n' "$LOG_PATH" >&2
    cat "$LOG_PATH" >&2 || true
    fail "daemon did not become healthy within ${HEALTH_TIMEOUT_SECS}s: $HEALTH_URL"
}

cmd_stop() {
    if [[ ! -f "$PID_PATH" ]]; then
        printf 'no CI daemon to stop (%s absent); nothing to do\n' "$PID_PATH"
        return 0
    fi

    local pid
    pid="$(cat "$PID_PATH")"
    if kill -0 "$pid" 2>/dev/null; then
        kill "$pid" 2>/dev/null || true
        for _ in $(seq 1 15); do
            kill -0 "$pid" 2>/dev/null || break
            sleep 1
        done
        if kill -0 "$pid" 2>/dev/null; then
            printf 'WARN: daemon pid %s did not exit after SIGTERM; sending SIGKILL\n' "$pid" >&2
            kill -9 "$pid" 2>/dev/null || true
        fi
        printf 'Vision CI daemon stopped (pid %s)\n' "$pid"
    else
        printf 'CI daemon pid %s already exited\n' "$pid"
    fi
    rm -f "$PID_PATH"
}

case "${1:-}" in
    start) cmd_start ;;
    stop) cmd_stop ;;
    *)
        fail "usage: $0 start|stop"
        ;;
esac
