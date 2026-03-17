const DEFAULT_REQUEST_TIMEOUT_MS = 30000
const DEFAULT_BODY_TIMEOUT_MS = 10000
const DEFAULT_HEALTH_TIMEOUT_MS = 5000

function readTimeout(name: string, fallback: number): number {
  const raw = process.env[name]
  if (!raw) {
    return fallback
  }

  const parsed = Number(raw)
  if (!Number.isFinite(parsed) || parsed <= 0) {
    return fallback
  }

  return Math.floor(parsed)
}

export function getVisionRequestTimeoutMs(): number {
  return readTimeout("VISION_REQUEST_TIMEOUT_MS", DEFAULT_REQUEST_TIMEOUT_MS)
}

export function getVisionBodyReadTimeoutMs(): number {
  return readTimeout("VISION_BODY_TIMEOUT_MS", DEFAULT_BODY_TIMEOUT_MS)
}

export function getVisionHealthTimeoutMs(): number {
  return readTimeout("VISION_HEALTH_TIMEOUT_MS", DEFAULT_HEALTH_TIMEOUT_MS)
}
