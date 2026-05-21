#!/usr/bin/env bash
# deploy-local.sh
#
# Build Vision from this dev repo and install the binary to ~/.local/bin/vision.
#
# Single source of truth for "this dev repo HEAD becomes the live binary on
# this machine". Use this so you never run Vision out of the dev tree — there
# is always exactly one installed file at $LOCAL_BIN/vision, and it is a real
# file copy (NEVER a symlink). Symlinks across the dev/install boundary break
# under realpath, backup tools, container mounts, and refactors that move the
# dev path. See ~/.config/opencode/instructions/architecture-discipline.md.
#
# What this script touches:
#   - Builds ./bin/vision via `make build` (proper version ldflags)
#   - Writes      $LOCAL_BIN/vision         (real file, mode 0755)
#   - Optionally  `systemctl --user restart vision.service` (only with --restart)
#
# What it does NOT touch:
#   - ~/.config/vision/servers.yaml         (user-owned runtime config)
#   - ~/.config/systemd/user/vision.service (managed by scripts/install.sh)
#   - ~/.config/opencode/*                  (unrelated)
# This matches DEVELOPMENT.md "Protected Locations".
#
# Default behavior: build + copy, NO service restart. The currently-running
# daemon keeps the OLD binary in memory until you restart it explicitly. Pass
# --restart to flip the live process to the new binary.
#
# Usage:
#   ./scripts/deploy-local.sh             # Build + deploy binary (no restart)
#   ./scripts/deploy-local.sh --restart   # Build + deploy + restart vision.service
#   ./scripts/deploy-local.sh --check     # Compare built vs installed; exit 1 if out of date
#   ./scripts/deploy-local.sh --dry-run   # Preview actions without writing
#   ./scripts/deploy-local.sh --help

set -euo pipefail

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
BINARY_NAME="vision"
BUILT_BINARY="${REPO_ROOT}/bin/${BINARY_NAME}"
LOCAL_BIN="${LOCAL_BIN:-${HOME}/.local/bin}"
DEST="${LOCAL_BIN}/${BINARY_NAME}"
SERVICE_NAME="vision.service"

# ---------------------------------------------------------------------------
# Flags
# ---------------------------------------------------------------------------
MODE="deploy" # deploy | check
DRY_RUN=0
RESTART=0

usage() {
	cat <<EOF
deploy-local.sh — build this repo and install to \$LOCAL_BIN/$BINARY_NAME

Usage:
  $(basename "$0") [--restart] [--check] [--dry-run] [--help]

Options:
  --restart   Restart $SERVICE_NAME after deploy (user systemd unit)
  --check     Compare built binary against installed; exit 1 if different
  --dry-run   Print what would happen without building or writing
  --help      Show this help

Environment:
  LOCAL_BIN   Install directory (default: \$HOME/.local/bin)

Examples:
  $(basename "$0")                  # Build + copy to $DEST
  $(basename "$0") --restart        # Build + copy + flip live daemon
  $(basename "$0") --check          # CI/precommit drift check
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--restart)
		RESTART=1
		shift
		;;
	--check)
		MODE="check"
		shift
		;;
	--dry-run)
		DRY_RUN=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "ERROR: unknown option: $1" >&2
		usage
		exit 1
		;;
	esac
done

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------
if ! command -v go >/dev/null 2>&1; then
	echo "ERROR: go is not in PATH (required to build Vision from source)" >&2
	exit 1
fi

if ! command -v make >/dev/null 2>&1; then
	echo "ERROR: make is not in PATH (required to run 'make build')" >&2
	exit 1
fi

if [[ "$RESTART" -eq 1 ]] && ! command -v systemctl >/dev/null 2>&1; then
	echo "ERROR: --restart requested but systemctl is not in PATH" >&2
	exit 1
fi

# ---------------------------------------------------------------------------
# Compare semantic state (version + commit + dirty), not bytes
# ---------------------------------------------------------------------------
# The Makefile injects buildTime via ldflags, so byte-compare would always
# mismatch on rebuild. Compare the inputs that actually determine behavior:
# - source's `git describe --tags --always --dirty` (matches Makefile VERSION)
# - source's `git rev-parse --short HEAD`           (matches Makefile COMMIT)
# - source's working-tree dirty flag                (always force rebuild when dirty)
# - installed binary's embedded version + commit    (via `vision version --json`)

# Source state
SRC_VERSION="$(cd "$REPO_ROOT" && git describe --tags --always --dirty 2>/dev/null || echo "dev")"
SRC_COMMIT="$(cd "$REPO_ROOT" && git rev-parse --short HEAD 2>/dev/null || echo "unknown")"
if (cd "$REPO_ROOT" && git diff --quiet 2>/dev/null && git diff --cached --quiet 2>/dev/null); then
	SRC_DIRTY=0
else
	SRC_DIRTY=1
fi

# Installed state
WAS_SYMLINK=0
INST_VERSION=""
INST_COMMIT=""
INST_PRESENT=0
if [[ -L "$DEST" ]]; then
	WAS_SYMLINK=1
fi
if [[ -e "$DEST" ]] && [[ -x "$DEST" ]]; then
	INST_PRESENT=1
	# Parse `vision version --json`. Defensive: tolerate older binaries.
	if INST_JSON="$("$DEST" version --json 2>/dev/null)"; then
		# Single-line JSON — extract with sed; avoid hard jq dependency.
		INST_VERSION="$(printf '%s' "$INST_JSON" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"
		INST_COMMIT="$(printf '%s' "$INST_JSON" | sed -n 's/.*"commit":"\([^"]*\)".*/\1/p')"
	fi
fi

# Decide
OUT_OF_DATE=1
REASON=""
if [[ "$WAS_SYMLINK" -eq 1 ]]; then
	REASON="installed path is a symlink — must replace with real file"
elif [[ "$INST_PRESENT" -eq 0 ]]; then
	REASON="no binary installed at ${DEST}"
elif [[ -z "$INST_VERSION" ]] || [[ -z "$INST_COMMIT" ]]; then
	REASON="installed binary did not report version/commit (pre-version-flag build)"
elif [[ "$SRC_DIRTY" -eq 1 ]]; then
	REASON="working tree is dirty — uncommitted changes force rebuild"
elif [[ "$SRC_VERSION" != "$INST_VERSION" ]] || [[ "$SRC_COMMIT" != "$INST_COMMIT" ]]; then
	REASON="source ${SRC_VERSION}@${SRC_COMMIT} != installed ${INST_VERSION}@${INST_COMMIT}"
else
	OUT_OF_DATE=0
fi

echo "==> State"
echo "    source:    ${SRC_VERSION} @ ${SRC_COMMIT}$([[ "$SRC_DIRTY" -eq 1 ]] && echo ' (dirty)')"
if [[ "$INST_PRESENT" -eq 1 ]]; then
	echo "    installed: ${INST_VERSION:-?} @ ${INST_COMMIT:-?}$([[ "$WAS_SYMLINK" -eq 1 ]] && echo ' (symlink)')"
else
	echo "    installed: <none at ${DEST}>"
fi

# --check mode: report and exit; never build or write
if [[ "$MODE" == "check" ]]; then
	if [[ "$OUT_OF_DATE" -eq 1 ]]; then
		echo "==> Out of date: ${REASON}"
		exit 1
	fi
	echo "==> Up to date"
	exit 0
fi

# ---------------------------------------------------------------------------
# Build (only when deploy needed)
# ---------------------------------------------------------------------------
if [[ "$OUT_OF_DATE" -eq 0 ]]; then
	echo "==> No build needed (${DEST} already up to date)"
else
	echo "==> Building ${BINARY_NAME}"
	echo "    reason: ${REASON}"
	echo "    output: ${BUILT_BINARY}"
	if [[ "$DRY_RUN" -eq 1 ]]; then
		echo "    dry-run: would run 'make build' in ${REPO_ROOT}"
	else
		(cd "$REPO_ROOT" && make build)
		if [[ ! -x "$BUILT_BINARY" ]]; then
			echo "ERROR: 'make build' did not produce ${BUILT_BINARY}" >&2
			exit 1
		fi
	fi
fi

# ---------------------------------------------------------------------------
# Deploy
# ---------------------------------------------------------------------------
if [[ "$OUT_OF_DATE" -eq 0 ]]; then
	: # nothing to copy
elif [[ "$DRY_RUN" -eq 1 ]]; then
	echo "==> dry-run: would install ${BUILT_BINARY} -> ${DEST} (mode 0755)"
	[[ "$WAS_SYMLINK" -eq 1 ]] && echo "    dry-run: would first remove symlink at ${DEST}"
else
	mkdir -p "$LOCAL_BIN"
	if [[ "$WAS_SYMLINK" -eq 1 ]]; then
		rm "$DEST"
		echo "    removed legacy symlink: ${DEST}"
	fi
	install -m 0755 "$BUILT_BINARY" "$DEST"
	echo "==> Deployed: ${DEST}"
fi

# Rebind for summary
NEEDS_DEPLOY=$OUT_OF_DATE

# ---------------------------------------------------------------------------
# Optional service restart
# ---------------------------------------------------------------------------
if [[ "$RESTART" -eq 1 ]]; then
	if [[ "$DRY_RUN" -eq 1 ]]; then
		echo "==> dry-run: would run 'systemctl --user restart ${SERVICE_NAME}'"
	else
		echo "==> Restarting ${SERVICE_NAME}"
		systemctl --user restart "${SERVICE_NAME}"
		# brief wait for supervisor to come up
		sleep 1
		if systemctl --user is-active --quiet "${SERVICE_NAME}"; then
			echo "    service active"
		else
			echo "    WARN: ${SERVICE_NAME} is not active after restart" >&2
			echo "    inspect: journalctl --user -u ${SERVICE_NAME} -n 50" >&2
			exit 1
		fi
	fi
elif [[ "$NEEDS_DEPLOY" -eq 1 ]] && [[ "$DRY_RUN" -eq 0 ]]; then
	echo ""
	echo "    Note: ${SERVICE_NAME} is still running the OLD binary in memory."
	echo "          Restart to flip the live process:"
	echo "            systemctl --user restart ${SERVICE_NAME}"
	echo "          Or re-run with --restart:"
	echo "            $0 --restart"
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
echo "==> Done."
if [[ "$DRY_RUN" -eq 1 ]]; then
	echo "    dry-run: no files written, no services touched"
elif [[ "$NEEDS_DEPLOY" -eq 0 ]]; then
	echo "    binary:  unchanged at ${DEST}"
else
	echo "    binary:  deployed to ${DEST}"
fi
if [[ "$RESTART" -eq 1 ]] && [[ "$DRY_RUN" -eq 0 ]]; then
	echo "    service: restarted"
elif [[ "$NEEDS_DEPLOY" -eq 1 ]] && [[ "$DRY_RUN" -eq 0 ]]; then
	echo "    service: NOT restarted (pass --restart to flip live)"
fi
