import axios, { AxiosError, type AxiosInstance } from 'axios'

export class ApiError extends Error {
  status: number
  data: unknown

  constructor(message: string, status: number, data: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.data = data
  }
}

export const http: AxiosInstance = axios.create({
  baseURL: '/',
  withCredentials: true,
  headers: { 'Content-Type': 'application/json' },
  // CSRF double-submit: axios reads `dstream_csrf` cookie and copies the
  // value into `X-CSRF-Token` on unsafe-method requests. Server-side
  // `middleware.CSRF` validates the match.
  xsrfCookieName: 'dstream_csrf',
  xsrfHeaderName: 'X-CSRF-Token',
})

http.interceptors.response.use(
  (resp) => resp,
  (err: AxiosError) => {
    if (err.response) {
      const body = err.response.data as { error?: string; message?: string } | string | undefined
      const fallback = err.response.statusText || `HTTP ${err.response.status}`
      const msg =
        typeof body === 'string'
          ? body || fallback
          : body?.error || body?.message || fallback
      return Promise.reject(new ApiError(msg, err.response.status, body))
    }
    return Promise.reject(new ApiError(err.message || 'network error', 0, null))
  },
)

// isUnauthorized is a small helper for route-level error boundaries that
// need to distinguish "session expired / kicked out" from a regular 5xx.
// We treat both 401 and 403 as "send the user back to /login": 403 is what
// the server returns when the session cookie HMAC verifies but the user is
// no longer in any org / has been removed.
export function isUnauthorized(err: unknown): boolean {
  if (err instanceof ApiError) {
    return err.status === 401 || err.status === 403
  }
  return false
}
