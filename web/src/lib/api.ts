// Tiny typed fetch wrapper. All paths go through the vite dev proxy in dev
// and same-origin in prod (dashboard is served from the dstream server).

export interface Source {
  id: string
  project_id: string
  name: string
  type: string
  ingest_token: string
  created_at: string
}

export interface Destination {
  id: string
  project_id: string
  name: string
  type: 'http' | 'cli'
  url: string | null
  rate_limit_rps: number | null
  rate_limit_burst: number | null
  max_inflight: number | null
  created_at: string
}

export interface Connection {
  id: string
  source_id: string
  destination_id: string
  enabled: boolean
  max_retries: number
  retry_strategy: 'exponential' | 'linear' | 'fixed' | 'custom'
  retry_base_ms: number
  retry_cap_ms: number
  retry_jitter_pct: number
  custom_retry_schedule: number[] | null
  created_at: string
}

export interface Event {
  id: string
  request_id: string
  connection_id: string
  status: string
  attempt_count: number
  last_attempt_at: string | null
  next_retry_at: string | null
  created_at: string
}

export interface Attempt {
  id: string
  attempt_num: number
  response_status: number | null
  response_body: string | null
  duration_ms: number | null
  error_message: string | null
  attempted_at: string
}

async function req<T>(
  method: string,
  path: string,
  body?: unknown,
  init?: RequestInit,
): Promise<T> {
  const resp = await fetch(path, {
    method,
    credentials: 'include',
    headers: { 'Content-Type': 'application/json', ...(init?.headers || {}) },
    body: body == null ? undefined : JSON.stringify(body),
    ...init,
  })
  if (!resp.ok) {
    const text = await resp.text().catch(() => '')
    throw new Error(`${resp.status} ${resp.statusText}: ${text}`)
  }
  if (resp.status === 204) return undefined as T
  return (await resp.json()) as T
}

export const api = {
  me: () => req<{ user?: { email: string; is_super_admin: boolean }; project_id?: string }>('GET', '/api/me'),

  requestMagicLink: (email: string) =>
    req<void>('POST', '/api/auth/magic-link/request', { email }),
  logout: () => req<void>('POST', '/api/auth/logout'),

  listSources: () => req<Source[]>('GET', '/api/sources'),
  createSource: (input: { name: string; type?: string }) =>
    req<Source>('POST', '/api/sources', input),

  listDestinations: () => req<Destination[]>('GET', '/api/destinations'),
  createDestination: (input: Partial<Destination> & { name: string; type: 'http' | 'cli' }) =>
    req<Destination>('POST', '/api/destinations', input),
  patchDestination: (id: string, input: Partial<Destination>) =>
    req<Destination>('PATCH', `/api/destinations/${id}`, input),

  listConnections: (sourceId: string) =>
    req<Connection[]>('GET', `/api/connections?source_id=${encodeURIComponent(sourceId)}`),
  createConnection: (input: { source_id: string; destination_id: string; enabled?: boolean }) =>
    req<Connection>('POST', '/api/connections', input),
  patchConnection: (id: string, input: Partial<Connection>) =>
    req<Connection>('PATCH', `/api/connections/${id}`, input),

  listEvents: (params?: { limit?: number; offset?: number }) => {
    const q = new URLSearchParams()
    if (params?.limit) q.set('limit', String(params.limit))
    if (params?.offset) q.set('offset', String(params.offset))
    const qs = q.toString()
    return req<Event[]>('GET', `/api/events${qs ? `?${qs}` : ''}`)
  },
  getEvent: (id: string) =>
    req<Event & { attempts: Attempt[] }>('GET', `/api/events/${id}`),
  retryEvent: (id: string) => req<void>('POST', `/api/events/${id}/retry`),
}
