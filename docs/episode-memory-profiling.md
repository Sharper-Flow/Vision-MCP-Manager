# Episode Memory Profiling

`scripts/episode-profile` is a no-restart, operator-facing tool for capturing a
Linux procfs memory snapshot of a Vision-managed server (by default `Episode`).
It records resident-set composition from `/proc/<pid>/status` and `Pss` from
`/proc/<pid>/smaps_rollup`, together with the Vision `/v1/servers` entry and
daemon health context. A second invocation with a baseline snapshot classifies
the result as **stable**, **growing**, or **inconclusive**.

## When to use this

- You suspect a Vision-managed process is growing in resident memory.
- You want a baseline before a change and a follow-up measurement after.
- You need memory-composition data (anonymous vs file-backed vs shared) without
  restarting the daemon or the target process.

## Requirements

- Linux with procfs mounted.
- `curl` and `jq` installed.
- Vision daemon running on the default admin port `6275` (or a reachable
  `--addr`).
- The target server must be registered in Vision and currently have a PID.

## Quick start

```bash
# 1. Record a baseline snapshot.
./scripts/episode-profile

# 2. After some workload or time, record a second snapshot and compare.
./scripts/episode-profile --compare ~/.local/share/vision/profiles/Episode-2026-07-30T20-00-00.json
```

By default the script writes the snapshot to
`~/.local/share/vision/profiles/<name>-<timestamp>.json` and prints a summary.
Use `--json` to get machine-readable output on stdout.

## Options

```text
-n, --name NAME         Server name in Vision registry (default: Episode)
-a, --addr URL          Vision admin API base URL (default: http://localhost:6275)
-d, --snapshot-dir DIR  Directory for saved snapshots
-o, --output FILE       Write snapshot or report to FILE (use - for stdout)
-c, --compare FILE      Compare current measurement against baseline FILE
-j, --json              Print machine-readable JSON to stdout
-h, --help              Show this help message
```

### Examples

```bash
# Profile a server with a different name
./scripts/episode-profile --name playwright --json

# Save a snapshot to a specific path
./scripts/episode-profile --output /tmp/episode-baseline.json

# Compare and emit a JSON report
./scripts/episode-profile \
  --compare ~/.local/share/vision/profiles/Episode-2026-07-30T20-00-00.json \
  --json
```

## Snapshot JSON schema

Each snapshot contains:

```json
{
  "timestamp": "2026-07-30T20:00:00Z",
  "server_name": "Episode",
  "pid": 12345,
  "procfs": {
    "status": {
      "VmRSS_kB": 1500123,
      "RssAnon_kB": 1200000,
      "RssFile_kB": 100000,
      "RssShmem_kB": 200000
    },
    "smaps_rollup": {
      "Pss_kB": 1400000
    },
    "read_errors": []
  },
  "context": {
    "server": { /* full /v1/servers/Episode entry */ },
    "daemon_health": { /* GET /health response */ }
  }
}
```

Values are `null` when the metric is unavailable, and `read_errors` lists why.

## Comparison report

When `--compare` is provided, the script emits a report that includes the delta
and a single classification:

```json
{
  "comparison": {
    "pid_changed": false,
    "delta_VmRSS_kB": 10240,
    "delta_VmRSS_percent": 0.68,
    "delta_RssAnon_kB": 5120,
    "delta_RssAnon_percent": 0.43,
    "classification": "stable"
  }
}
```

## Classification rules

The result is always exactly one of:

- **`stable`** — memory did not grow beyond the configured threshold.
- **`growing`** — `VmRSS` or `RssAnon` increased by at least 5% *and* at least
  1,024 kB between the two snapshots, with the same PID and readable procfs.
- **`inconclusive`** — the PID changed, required procfs files are missing or
  unreadable, the server could not be resolved, or the baseline/current snapshot
  is missing required values. The report includes a `reason`.

These thresholds are intentionally conservative. A `growing` result is a signal
to investigate further; it is not by itself proof of a leak.

## Limitations

- Linux only. Procfs is required; non-Linux platforms produce `inconclusive`.
- The script reads `/proc/<pid>/smaps_rollup` for `Pss`. Some kernels or
  container environments may restrict this file.
- It does not restart the daemon or the target process. If you need a fresh
  process baseline, resolve that separately; the script will report PID change
  as `inconclusive`.
- Classification is a trend indicator, not an allocator attribution. For a
  deeper diagnosis, collect allocator profiles from the target process itself.

## Running in tests

The script supports environment overrides for test fixtures:

- `VISION_API_URL` — Vision admin API base URL.
- `PROCFS_ROOT` — alternative root for `/proc`-style lookups.
- `SNAPSHOT_DIR` — snapshot directory.

The Go tests in `scripts/episode_profile_test.go` exercise the snapshot and
comparison paths using fake procfs and a local HTTP server. Run them with:

```bash
go test ./scripts/...
```
