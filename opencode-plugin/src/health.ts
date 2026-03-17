/**
 * Vision Daemon Health Check
 *
 * Provides health checking for the Vision daemon on port 6275.
 * Used to determine if the daemon is running before attempting MCP calls.
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
  uptime?: string
  port?: number
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
      uptime?: string
      port?: number
    }

    return {
      healthy: data.status === "healthy",
      uptime: data.uptime,
      port: data.port,
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
