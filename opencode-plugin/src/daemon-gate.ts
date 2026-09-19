/**
 * Shared Vision daemon availability gate for integration tests.
 *
 * Probes the Admin MCP health endpoint once at module load. Tests gate on
 * `daemonReachable` with it.runIf so a developer without a local daemon gets
 * a skip instead of a failure.
 *
 * CI must never skip silently: a skipped gate is how daemon-integration
 * coverage disappeared while the CI job stayed green. When CI is set and the
 * probe fails, this module throws, which fails collection for every test file
 * that imports it. The CI Test job starts a daemon with scripts/ci-daemon.sh
 * before vitest runs, so this fires only when that step is missing or broken.
 */
const daemonReachable = await fetch("http://localhost:6275/health")
  .then((response) => response.ok)
  .catch(() => false)

if (process.env.CI && !daemonReachable) {
  throw new Error(
    "CI is set but the Vision daemon is not reachable on http://localhost:6275/health. " +
      "Start it with scripts/ci-daemon.sh before running vitest; " +
      "daemon-integration coverage must execute or fail, not skip."
  )
}

export { daemonReachable }
