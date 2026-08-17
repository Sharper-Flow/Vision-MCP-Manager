/**
 * Vision Daemon Health Check
 *
 * Provides health checking for the Vision daemon on port 6275.
 * Used to determine if the daemon is running before attempting MCP calls.
 *
 * Contract source: internal/admin/server.go handleHealth (lines 207-251).
 * Port 6275 serves the Admin MCP. It replaced a legacy REST API that returned
 * {"status":"healthy","uptime":...}; the removal is recorded in
 * internal/daemon/daemon.go:194. Responses are:
 *
 *   200 {"status":"ok"}                          daemon up, all servers fine
 *   200 {"status":"degraded","errors":[...]}     daemon up, servers failed or await first probe
 *   503 {"status":"unhealthy"}                   daemon not running
 *
 * Neither `uptime` nor `port` is ever returned.
 */

import { getVisionHealthTimeoutMs } from "./timeouts"

const ADMIN_PORT = 6275

async function readJsonWithTimeout<T>(response: Response, timeoutMs: number): Promise<T> {
  return Promise.race([
    response.json() as Promise<T>,
    new Promise<T>((_, reject) =>
      setTimeout(() => reject(new Error("Response body read timeout")), timeoutMs)
    ),
  ])
}

export interface HealthStatus {
  healthy: boolean
  /** True when the daemon is reachable but managed servers failed or await their first probe. */
  degraded?: boolean
  /** Per-server failure details, present only when degraded. */
  errors?: string[]
  error?: string
}

/**
 * Check if the Vision daemon is running and healthy.
 *
 * @returns Health status object
 */
export async function checkHealth(): Promise<HealthStatus> {
  const healthTimeoutMs = getVisionHealthTimeoutMs()
  const controller = new AbortController()
  const timeoutId = setTimeout(() => controller.abort(), healthTimeoutMs)

  try {
    const response = await fetch(`http://localhost:${ADMIN_PORT}/health`, {
      method: "GET",
      signal: controller.signal,
    })

    clearTimeout(timeoutId)

    if (!response.ok) {
      return {
        healthy: false,
        error: `Unexpected status: ${response.status}`,
      }
    }

    const data = (await readJsonWithTimeout(response, healthTimeoutMs)) as {
      status?: string
      errors?: string[]
    }

    // Explicit over the documented states. An unrecognized status fails closed
    // rather than defaulting either way: silently treating an unknown value as
    // healthy is how the previous "healthy" literal went unnoticed, and
    // silently treating it as unhealthy would disable the tools on a benign
    // contract addition without saying why.
    switch (data.status) {
      case "ok":
        return { healthy: true }
      case "degraded":
        // The daemon itself is up and its management tools work; individual
        // managed servers have failed or await their first probe. Reporting
        // this as unhealthy would gate off the tools needed to diagnose and restart them.
        return {
          healthy: true,
          degraded: true,
          errors: data.errors ?? [],
        }
      default:
        return {
          healthy: false,
          error: `Unrecognized daemon health status: ${JSON.stringify(data.status)}`,
        }
    }
  } catch (err) {
    clearTimeout(timeoutId)

    if (err instanceof Error) {
      if (err.name === "AbortError" || err.message === "Response body read timeout") {
        return {
          healthy: false,
          error: "Connection timeout - daemon may be slow or unresponsive",
        }
      }
      if (err.message.includes("ECONNREFUSED") || err.message.includes("fetch failed")) {
        return {
          healthy: false,
          error: "Connection refused - daemon is not running",
        }
      }
      return {
        healthy: false,
        error: err.message,
      }
    }

    return {
      healthy: false,
      error: "Unknown error checking daemon health",
    }
  }
}

/**
 * Check if daemon is running (simple boolean check).
 */
export async function isDaemonRunning(): Promise<boolean> {
  const status = await checkHealth()
  return status.healthy
}
