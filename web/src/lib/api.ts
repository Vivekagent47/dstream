// Typed API client. Uses axios under the hood (see `./http`). All paths go
// through the vite dev proxy in dev and same-origin in prod (the dashboard is
// served from the dstream server).

import { http } from './http'

export interface Source {
  id: string
  org_id: string
  name: string
  type: string
  description: string
  allowed_methods: string[]
  enabled: boolean
  ingest_token: string
  created_at: string
  updated_at: string
}

export interface Destination {
  id: string
  org_id: string
  name: string
  type: 'http' | 'cli'
  description: string
  url: string | null
  // auth_configured is a non-sensitive flag: true when the destination has
  // an auth_config blob set on the server. The raw auth_config (HMAC
  // secret, bearer token, etc.) is NEVER sent to the client.
  auth_configured: boolean
  rate_limit_rps: number | null
  rate_limit_burst: number | null
  max_inflight: number | null
  created_at: string
  updated_at: string
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

// Keyset-paginated events page. `next_cursor` is an opaque token; pass it back
// as `cursor` to fetch the following page. Absent when there are no more rows.
export interface EventsPage {
  events: Event[]
  next_cursor?: string
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

export type Role = 'owner' | 'admin' | 'member'

export interface Org {
  id: string
  name: string
  slug: string
  role?: Role
  created_at: string
}

export interface Member {
  user_id: string
  email: string
  name: string | null
  role: Role
  created_at: string
}

export interface Invite {
  id: string
  email: string
  role: 'admin' | 'member'
  expires_at: string
  created_at: string
  invited_by_email?: string
  accepted_at?: string | null
}

export interface APIKey {
  id: string
  name: string
  prefix: string
  created_at: string
  last_used_at: string | null
  expires_at: string | null
}

export interface APIKeyCreateResult {
  id: string
  name: string
  prefix: string
  key: string
}

export interface AuditEntry {
  id: number
  created_at: string
  actor: Record<string, unknown> | null
  action: string
  target: Record<string, unknown> | null
  metadata: Record<string, unknown> | null
}

// AuditPage matches the server envelope from GET /api/audit and
// GET /api/orgs/{id}/audit — `entries` is the row slice, `next_before_id`
// is the keyset cursor for the next page (undefined when no more pages).
export interface AuditPage {
  entries: AuditEntry[]
  next_before_id?: number
}

export interface MeUser {
  id: string
  email: string
  name: string | null
  is_super_admin: boolean
}

export interface MeResponse {
  user?: MeUser
  orgs?: Org[]
  active_org_id?: string
  api_key?: { org_id: string }
}

export interface InvitePeek {
  org_name: string
  email: string
  role: 'admin' | 'member'
  expires_at: string
  requires_login?: boolean
}

export interface InviteAcceptResult {
  org_id?: string
  org_name?: string
  requires_login?: boolean
  email?: string
}

export interface AuditFilters {
  limit?: number
  before_id?: number
  action?: string
  target_type?: string
  actor_user_id?: string
}

export const api = {
  me: () => http.get<MeResponse>('/api/me').then((r) => r.data),

  requestMagicLink: (email: string) =>
    http.post<void>('/api/auth/magic-link/request', { email }).then((r) => r.data),
  // Consumes the single-use token same-origin so the Set-Cookie lands on the
  // web origin. POST (with JSON body) so the request is not a cross-site-
  // reachable GET — protects against login-CSRF / session fixation.
  verifyMagicLink: (token: string) =>
    http.post<void>('/api/auth/magic-link/verify', { token }).then((r) => r.data),
  logout: () => http.post<void>('/api/auth/logout').then((r) => r.data),

  // Orgs
  listMyOrgs: () => http.get<Org[]>('/api/orgs').then((r) => r.data),
  createOrg: (input: { name: string }) => http.post<Org>('/api/orgs', input).then((r) => r.data),
  selectOrg: (org_id: string) =>
    http.post<{ active_org_id: string }>('/api/orgs/select', { org_id }).then((r) => r.data),
  updateOrg: (org_id: string, input: { name: string }) =>
    http.patch<Org>(`/api/orgs/${org_id}`, input).then((r) => r.data),
  deleteOrg: (org_id: string) => http.delete<void>(`/api/orgs/${org_id}`).then((r) => r.data),
  transferOrg: (org_id: string, to_user_id: string) =>
    http.post<void>(`/api/orgs/${org_id}/transfer`, { to_user_id }).then((r) => r.data),

  // Members
  listMembers: (org_id: string) =>
    http.get<Member[]>(`/api/orgs/${org_id}/members`).then((r) => r.data),
  patchMember: (org_id: string, user_id: string, role: Role) =>
    http.patch<Member>(`/api/orgs/${org_id}/members/${user_id}`, { role }).then((r) => r.data),
  removeMember: (org_id: string, user_id: string) =>
    http.delete<void>(`/api/orgs/${org_id}/members/${user_id}`).then((r) => r.data),

  // Invites
  listInvites: (org_id: string) =>
    http.get<Invite[]>(`/api/orgs/${org_id}/invites`).then((r) => r.data),
  createInvite: (org_id: string, input: { email: string; role: 'admin' | 'member' }) =>
    http.post<Invite>(`/api/orgs/${org_id}/invites`, input).then((r) => r.data),
  revokeInvite: (org_id: string, id: string) =>
    http.delete<void>(`/api/orgs/${org_id}/invites/${id}`).then((r) => r.data),
  peekInvite: (token: string) => http.get<InvitePeek>(`/api/invites/${token}`).then((r) => r.data),
  acceptInvite: (token: string) =>
    http.post<InviteAcceptResult>(`/api/invites/${token}/accept`).then((r) => r.data),

  // API keys
  listAPIKeys: (org_id: string) =>
    http.get<APIKey[]>(`/api/orgs/${org_id}/api-keys`).then((r) => r.data),
  createAPIKey: (org_id: string, name: string) =>
    http.post<APIKeyCreateResult>(`/api/orgs/${org_id}/api-keys`, { name }).then((r) => r.data),
  revokeAPIKey: (org_id: string, id: string) =>
    http.delete<void>(`/api/orgs/${org_id}/api-keys/${id}`).then((r) => r.data),

  // Audit
  listAudit: (filters?: AuditFilters) =>
    http.get<AuditPage>('/api/audit', { params: filters }).then((r) => r.data),
  listOrgAudit: (org_id: string, filters?: AuditFilters) =>
    http.get<AuditPage>(`/api/orgs/${org_id}/audit`, { params: filters }).then((r) => r.data),

  // Sources
  listSources: () => http.get<Source[]>('/api/sources').then((r) => r.data),
  getSource: (id: string) => http.get<Source>(`/api/sources/${id}`).then((r) => r.data),
  createSource: (input: { name: string; description?: string }) =>
    http.post<Source>('/api/sources', input).then((r) => r.data),
  updateSource: (
    id: string,
    input: { name?: string; description?: string; allowed_methods?: string[]; enabled?: boolean },
  ) => http.patch<Source>(`/api/sources/${id}`, input).then((r) => r.data),
  deleteSource: (id: string) => http.delete<void>(`/api/sources/${id}`).then((r) => r.data),

  // Destinations
  listDestinations: () => http.get<Destination[]>('/api/destinations').then((r) => r.data),
  getDestination: (id: string) =>
    http.get<Destination>(`/api/destinations/${id}`).then((r) => r.data),
  createDestination: (input: Partial<Destination> & { name: string; type: 'http' | 'cli' }) =>
    http.post<Destination>('/api/destinations', input).then((r) => r.data),
  patchDestination: (id: string, input: Partial<Destination>) =>
    http.patch<Destination>(`/api/destinations/${id}`, input).then((r) => r.data),
  deleteDestination: (id: string) =>
    http.delete<void>(`/api/destinations/${id}`).then((r) => r.data),

  // Connections
  // Server returns all org connections and ignores query params; the optional
  // source_id is sent anyway so intent is visible in the network tab, and
  // callers filter client-side (see sources/$id.tsx).
  listConnections: (sourceId?: string) =>
    http
      .get<Connection[]>('/api/connections', {
        params: sourceId ? { source_id: sourceId } : undefined,
      })
      .then((r) => r.data),
  createConnection: (input: { source_id: string; destination_id: string; enabled?: boolean }) =>
    http.post<Connection>('/api/connections', input).then((r) => r.data),
  patchConnection: (id: string, input: Partial<Connection>) =>
    http.patch<Connection>(`/api/connections/${id}`, input).then((r) => r.data),
  deleteConnection: (id: string) =>
    http.delete<void>(`/api/connections/${id}`).then((r) => r.data),

  // Events
  listEvents: (params?: { limit?: number; cursor?: string }) =>
    http.get<EventsPage>('/api/events', { params }).then((r) => r.data),
  getEvent: (id: string) =>
    http.get<Event & { attempts: Attempt[] }>(`/api/events/${id}`).then((r) => r.data),
  retryEvent: (id: string) => http.post<void>(`/api/events/${id}/retry`).then((r) => r.data),
}

// Stable query keys for react-query. Keep keyed factories here so call sites
// stay in sync with the API surface.
export const qk = {
  me: () => ['me'] as const,
  orgs: () => ['orgs'] as const,
  members: (org_id: string) => ['members', org_id] as const,
  invites: (org_id: string) => ['invites', org_id] as const,
  apiKeys: (org_id: string) => ['api-keys', org_id] as const,
  audit: (filters?: AuditFilters) => ['audit', filters ?? {}] as const,
  sources: () => ['sources'] as const,
  source: (id: string) => ['sources', id] as const,
  destinations: () => ['destinations'] as const,
  destination: (id: string) => ['destinations', id] as const,
  connections: (sourceId: string) => ['connections', sourceId] as const,
  events: (params?: { limit?: number; cursor?: string }) => ['events', params ?? {}] as const,
  event: (id: string) => ['event', id] as const,
}
