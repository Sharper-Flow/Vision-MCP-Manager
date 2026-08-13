#!/usr/bin/env bash
set -uo pipefail

# Exercise configure_opencode without running the installer's main function.
# The production script currently invokes main unconditionally, so source only
# the function definitions before that invocation. This keeps the test from
# downloading binaries, installing services, or touching the real HOME.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER="${SCRIPT_DIR}/install.sh"
TEST_ROOT="$(mktemp -d)"
EXTRACTED="${TEST_ROOT}/install-functions.sh"
CONTROLLED_PATH="${TEST_ROOT}/bin"
FAILURES=0

cleanup() {
    rm -rf "$TEST_ROOT"
}
trap cleanup EXIT

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    FAILURES=$((FAILURES + 1))
}

if [[ ! -f "$INSTALLER" ]]; then
    fail "installer missing: $INSTALLER"
    exit 1
fi

# Stop before the current unguarded main invocation. Keep the extraction
# assertion explicit so a future installer change cannot silently execute the
# installer from this test.
awk '
    /^[[:space:]]*main[[:space:]]+"\$@"[[:space:]]*$/ { exit }
    { print }
' "$INSTALLER" > "$EXTRACTED"

if ! grep -q '^configure_opencode()[[:space:]]*{' "$EXTRACTED"; then
    fail "configure_opencode was not extracted"
fi
if grep -q '^[[:space:]]*main[[:space:]]*"\$@"' "$EXTRACTED"; then
    fail "extracted source still contains the installer entrypoint"
fi

# Deliberately exclude the host PATH (and jq). This makes the no-jq branch
# deterministic while retaining only commands configure_opencode needs.
mkdir -p "$CONTROLLED_PATH"
for command_name in cat mkdir mktemp mv; do
    ln -s "$(command -v "$command_name")" "${CONTROLLED_PATH}/${command_name}"
done

assert_isolated_home() {
    local case_home="$1"
    if [[ ! -d "$case_home" || "$case_home" != "$TEST_ROOT/"* ]]; then
        fail "case HOME is not an isolated temporary directory: $case_home"
    fi
}

invoke_configure() {
    local case_home="$1"
    CASE_OUTPUT=''
    CASE_STATUS=0

    if CASE_OUTPUT=$(
        /usr/bin/env -i HOME="$case_home" PATH="$CONTROLLED_PATH" /usr/bin/bash --noprofile --norc -c '
            set -euo pipefail
            source "$1"
            [[ "$HOME" == "$2" ]]
            configure_opencode
        ' test-shell "$EXTRACTED" "$case_home" 2>&1
    ); then
        CASE_STATUS=0
    else
        CASE_STATUS=$?
    fi
}

assert_guidance() {
    local output="$1"
    local label="$2"
    local token
    for token in existing manual vision_init restart; do
        if [[ "$token" == vision_init ]]; then
            if ! [[ "$output" =~ vision[_[:space:]]init ]]; then
                fail "$label guidance omits vision_init"
            fi
        elif ! [[ "${output,,}" == *"$token"* ]]; then
            fail "$label guidance omits $token"
        fi
    done
}

assert_vision_declaration() {
    local config_file="$1"
    if ! python3 - "$config_file" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as stream:
    document = json.load(stream)
vision = document["mcp"]["vision"]
assert vision == {
    "type": "remote",
    "url": "http://localhost:6275/mcp",
    "enabled": True,
}
PY
    then
        fail "new JSONC does not contain the complete Vision declaration"
    fi
}

run_none_case() {
    local case_home="$TEST_ROOT/none"
    local config_dir="$case_home/.config/opencode"
    local jsonc_file="$config_dir/opencode.jsonc"
    local json_file="$config_dir/opencode.json"
    mkdir -p "$case_home"
    assert_isolated_home "$case_home"

    invoke_configure "$case_home"
    if [[ "$CASE_STATUS" -ne 0 ]]; then
        fail "none case returned status $CASE_STATUS: $CASE_OUTPUT"
    fi
    [[ -f "$jsonc_file" ]] || fail "none case did not create opencode.jsonc"
    [[ ! -e "$json_file" ]] || fail "none case created legacy opencode.json"
    [[ -f "$jsonc_file" ]] && assert_vision_declaration "$jsonc_file"
}

run_existing_case() {
    local name="$1"
    local sentinel="$2"
    local case_home="$TEST_ROOT/$name"
    local config_dir="$case_home/.config/opencode"
    local config_file="$config_dir/opencode.$([[ "$name" == jsonc ]] && printf jsonc || printf json)"
    local other_file="$config_dir/opencode.$([[ "$name" == jsonc ]] && printf json || printf jsonc)"
    mkdir -p "$config_dir"
    assert_isolated_home "$case_home"
    printf '%s' "$sentinel" > "$config_file"

    invoke_configure "$case_home"
    if [[ "$CASE_STATUS" -ne 0 ]]; then
        fail "$name sentinel case returned status $CASE_STATUS: $CASE_OUTPUT"
    fi
    [[ ! -e "$other_file" ]] || fail "$name sentinel case created the other config format"
    if ! cmp -s <(printf '%s' "$sentinel") "$config_file"; then
        fail "$name sentinel bytes changed"
    fi
    assert_guidance "$CASE_OUTPUT" "$name"
}

run_both_case() {
    local case_home="$TEST_ROOT/both"
    local config_dir="$case_home/.config/opencode"
    local jsonc_file="$config_dir/opencode.jsonc"
    local json_file="$config_dir/opencode.json"
    local jsonc_sentinel='// preserve this JSONC\n{"mcp":{"keep-jsonc":true}}\n'
    local json_sentinel='{"mcp":{"keep-json":true}}\n'
    mkdir -p "$config_dir"
    assert_isolated_home "$case_home"
    printf '%b' "$jsonc_sentinel" > "$jsonc_file"
    printf '%b' "$json_sentinel" > "$json_file"

    invoke_configure "$case_home"
    if [[ "$CASE_STATUS" -eq 0 && ! "${CASE_OUTPUT,,}" =~ ambiguous ]]; then
        fail "both case neither returned nonzero nor reported ambiguity"
    fi
    if ! cmp -s <(printf '%b' "$jsonc_sentinel") "$jsonc_file"; then
        fail "both case changed JSONC sentinel bytes"
    fi
    if ! cmp -s <(printf '%b' "$json_sentinel") "$json_file"; then
        fail "both case changed JSON sentinel bytes"
    fi
}

run_none_case
run_existing_case jsonc $'// preserve this JSONC\n{"mcp":{"keep":true}}\n'
run_existing_case json '{"mcp":{"keep":true}}\n'
run_both_case

if [[ "$FAILURES" -ne 0 ]]; then
    printf '%d installer configuration test(s) failed\n' "$FAILURES" >&2
    exit 1
fi
