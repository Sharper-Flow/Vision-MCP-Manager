#!/usr/bin/env bash
# Real-host qualification runner for the Vision OpenCode plugin.
#
# OpenCode 2.3.2 runs exclusively through the AL-124 lab (VISION_V2_LAB,
# path to dev/opencode-v2-lab/lab): the lab installs and hash-pins the
# binary, runs it offline in its own sandbox, provides the scripted
# loopback provider, and owns the isolation checks (check-isolation with
# its negative control, snapshot-live/compare-live).
#
# A private bwrap sandbox covers the checks the lab cannot express today
# (see the gap list in opencode-plugin/README.md): the AL-46 HTTP
# session-expiry probe, the reconnect probe, the directory-loader probes,
# and a read-only bind of the repository plugin directory. Its 2.3.2
# binary is the lab's pinned artifact unless VISION_V2_BIN overrides it.
# VISION_V1_BIN adds the opt-in OpenCode 1.18.35 checks on the same
# private sandbox.
set -Eeuo pipefail

plugin_dir=$(cd "$(dirname "$0")/.." && pwd)
fixture=$(mktemp -d /tmp/vision-v2-e2e-XXXXXX)
lab=${VISION_V2_LAB:-}
lab_state="${XDG_STATE_HOME:-$HOME/.local/state}/opencode-v2-lab"
iname="vsn60-$$"
instance_dir="$lab_state/instances/$iname"
status=0

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
ok() { printf 'OK:   %s\n' "$*"; }

cleanup() {
  if [[ ${VISION_V2_KEEP:-0} == 1 ]]; then
    printf 'Fixture kept: %s\nLab instance kept: %s\n' "$fixture" "$instance_dir"
  else
    rm -rf "$fixture" "$instance_dir"
  fi
}
trap cleanup EXIT

for command in bwrap npm python3; do command -v "$command" >/dev/null || fail "missing $command"; done
[[ -n $lab && -x $lab ]] || fail "VISION_V2_LAB must point at the executable dev/opencode-v2-lab/lab"

# ---------------------------------------------------------------------------
# Lab artifact: pinned 2.3.2 binary, installed once and hash-verified.
# ---------------------------------------------------------------------------
if [[ ! -f $lab_state/artifact.json ]]; then
  "$lab" install || fail "lab install failed"
fi
artifact_sha=$(python3 -c 'import json;print(json.load(open("'"$lab_state"'/artifact.json"))["executable_sha256"])')
artifact_commit=$(python3 -c 'import json;print(json.load(open("'"$lab_state"'/artifact.json"))["source_commit"])')
lab_bin="$lab_state/artifact/node_modules/@opencode/cli-linux-x64/bin/opencode"
actual_sha=$(sha256sum "$lab_bin" | cut -d' ' -f1)
[[ $actual_sha == "$artifact_sha" ]] || fail "lab binary hash $actual_sha does not match artifact record"
ok "lab artifact sha256=$artifact_sha commit=$artifact_commit"

# ---------------------------------------------------------------------------
# Plugin tarball, installed into the lab instance workspace.
# ---------------------------------------------------------------------------
(cd "$plugin_dir" && npm pack --json --pack-destination "$fixture" >"$fixture/pack.json") || fail "npm pack failed"
tarball=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))[0]["filename"])' "$fixture/pack.json")
"$lab" instance new "$iname" >/dev/null || fail "lab instance creation failed"
npm install --ignore-scripts --prefix "$instance_dir/workspace/install" --no-save --loglevel=error "$fixture/$tarball" >"$fixture/install.log" 2>&1 || fail "plugin install into instance failed"
npm install --ignore-scripts --prefix "$fixture/install" --no-save --loglevel=error "$fixture/$tarball" >"$fixture/private-install.log" 2>&1 || fail "plugin install into private fixture failed"
cp "$plugin_dir/scripts/e2e-v2/mcp-stdio.py" "$instance_dir/workspace/"
mkdir -p "$instance_dir/workspace/broken"
printf 'export default { id: "broken", setup() { throw new Error("deliberately broken plugin") } }\n' >"$instance_dir/workspace/broken/index.js"

lab_check() { # LABEL KIND ASSERT_MODE
  local label=$1 kind=$2 mode=$3 rc=0
  python3 "$plugin_dir/scripts/e2e-v2/lab-prep.py" "$instance_dir" "$kind"
  local env_args=()
  [[ $kind == *on ]] && env_args=(--env OPENCODE_EXPERIMENTAL_CODE_MODE=true)
  "$lab" run "$iname" --timeout 150 --label "$label" --fixture "$instance_dir/workspace/fixture.json" "${env_args[@]}" \
    -- run --title "$label" --model stub/m "qualification probe" \
    >"$fixture/lab-$label.out" 2>"$fixture/lab-$label.err" || rc=$?
  [[ $rc -eq 0 ]] || { printf 'FAIL: %s host exited %s\n' "$label" "$rc" >&2; status=1; }
  python3 "$plugin_dir/scripts/e2e-v2/lab-assert.py" "$label" "$instance_dir/evidence/fixture-requests.jsonl" "$mode" || status=1
}

# ---------------------------------------------------------------------------
# Lab-run checks.
# ---------------------------------------------------------------------------
if [[ ${VISION_V2_NEGATIVE:-0} == 1 ]]; then
  lab_check negative-broken broken ten
  if [[ $status -ne 0 ]]; then
    ok "negative control: broken plugin failed the assertions as required"
  else
    printf 'FAIL: negative control unexpectedly passed\n' >&2
  fi
  exit 1
fi

"$lab" snapshot-live "$fixture/live-before.json" || fail "snapshot-live failed"

lab_check installed-root off ten
lab_check codemode-env-on on two
lab_check server-direct server-direct server-direct
lab_check control-off control-off control
lab_check control-on control-on control

if "$lab" check-isolation "$iname" >"$fixture/isolation.json" 2>&1; then
  ok "lab check-isolation passed"
else
  printf 'FAIL: lab check-isolation failed\n' >&2
  cat "$fixture/isolation.json" >&2
  status=1
fi
if "$lab" check-isolation "$iname" --negative-control >"$fixture/isolation-negative.json" 2>&1; then
  printf 'FAIL: lab isolation negative control unexpectedly passed\n' >&2
  status=1
else
  ok "lab isolation negative control failed as required"
fi

# ---------------------------------------------------------------------------
# Private sandbox: checks the lab cannot express.
# ---------------------------------------------------------------------------
v2_bin=${VISION_V2_BIN:-$lab_bin}
[[ -f $v2_bin ]] || fail "no 2.3.2 binary at $v2_bin"
cp "$v2_bin" "$fixture/opencode"
npm install --ignore-scripts --prefix "$fixture" --no-save --loglevel=error @ai-sdk/openai-compatible@3.0.94 >"$fixture/provider-install.log" 2>&1 || fail "provider install failed"
cp "$plugin_dir/scripts/e2e-v2/server.py" "$plugin_dir/scripts/e2e-v2/driver.py" "$fixture/"
mkdir "$fixture/repo-plugin"
v1_bind=()
if [[ -n ${VISION_V1_BIN:-} ]]; then
  : >"$fixture/opencode-v1"
  v1_bind=(--ro-bind "$VISION_V1_BIN" "$fixture/opencode-v1")
fi

bwrap --unshare-net --ro-bind /usr /usr --ro-bind /bin /bin --ro-bind /lib /lib --ro-bind /lib64 /lib64 --ro-bind /etc /etc --proc /proc --dev /dev --dir /home --tmpfs /home/jon --dir /tmp --bind "$fixture" "$fixture" --ro-bind "$plugin_dir" "$fixture/repo-plugin" "${v1_bind[@]}" --chdir "$fixture" --clearenv --setenv HOME "$fixture" --setenv FIXTURE "$fixture" --setenv V1_ENABLED "${VISION_V1_BIN:+1}" --setenv LOADER_PROBE "${VISION_V2_LOADER_PROBE:-0}" --setenv PATH /usr/bin:/bin /bin/sh -c 'version=$("$FIXTURE/opencode" --version) && [ "$version" = "opencode v2.3.2" ] || { printf "FAIL: version %s\n" "$version"; exit 1; }; printf "OK: %s\n" "$version"; exec /usr/bin/python3 "$FIXTURE/driver.py"' || status=$?

# Structural isolation negative control: with the real home bound
# read-only the same probe must detect exposure.
if bwrap --unshare-net --ro-bind /usr /usr --ro-bind /bin /bin --ro-bind /lib /lib --ro-bind /lib64 /lib64 --ro-bind /etc /etc --proc /proc --dev /dev --dir /home --ro-bind "$HOME" /home/jon --ro-bind "$plugin_dir/scripts/e2e-v2/probe.py" /probe.py --clearenv --setenv PATH /usr/bin:/bin /usr/bin/python3 /probe.py >"$fixture/structural-negative.json" 2>&1; then
  printf 'FAIL: structural negative control unexpectedly passed\n' >&2
  status=1
else
  grep -q '"live_paths_hidden": false' "$fixture/structural-negative.json" && ok "structural negative control detects exposure" || { printf 'FAIL: structural negative control output unexpected\n' >&2; cat "$fixture/structural-negative.json" >&2; status=1; }
fi

# ---------------------------------------------------------------------------
# Live-install hash comparison (replaces mtime listings).
# ---------------------------------------------------------------------------
"$lab" snapshot-live "$fixture/live-after.json" || fail "snapshot-live failed"
if "$lab" compare-live "$fixture/live-before.json" "$fixture/live-after.json"; then
  ok "live install unchanged (snapshot-live/compare-live)"
else
  printf 'FAIL: live install changed\n' >&2
  status=1
fi

exit "$status"
