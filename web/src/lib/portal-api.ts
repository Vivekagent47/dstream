import axios, { type AxiosInstance } from 'axios'

import type {
  Application,
  Endpoint,
  EndpointWithSecret,
  EventType,
  Message,
  MessageDelivery,
  MessageDeliveryAttempt,
  MessageDetail,
  Page,
  RecoverResult,
} from '#/lib/api'

// The portal token arrives in the URL fragment (#token=...), is stashed here,
// then stripped from the URL. Bearer-authed; no cookies/CSRF (the backend
// exempts Bearer + no-session requests).
const TOKEN_KEY = 'dstream_portal_token'
export function setPortalToken(t: string) {
  sessionStorage.setItem(TOKEN_KEY, t)
}
export function getPortalToken(): string | null {
  return typeof window === 'undefined' ? null : sessionStorage.getItem(TOKEN_KEY)
}
export function clearPortalToken() {
  sessionStorage.removeItem(TOKEN_KEY)
}

const baseURL =
  typeof window === 'undefined' ? process.env.DSTREAM_API_URL || 'http://localhost:8080' : '/'

const http: AxiosInstance = axios.create({
  baseURL,
  headers: { 'Content-Type': 'application/json' },
})
http.interceptors.request.use((cfg) => {
  const t = getPortalToken()
  if (t) cfg.headers.Authorization = `Bearer ${t}`
  return cfg
})
http.interceptors.response.use(
  (resp) => resp,
  (err) => {
    if (err?.response?.status === 401) clearPortalToken()
    return Promise.reject(err)
  },
)

export const portalQk = {
  app: ['portal', 'app'] as const,
  endpoints: ['portal', 'endpoints'] as const,
  endpoint: (id: string) => ['portal', 'endpoints', id] as const,
  endpointAttempts: (id: string) => ['portal', 'endpoints', id, 'attempts'] as const,
  messages: ['portal', 'messages'] as const,
  message: (id: string) => ['portal', 'messages', id] as const,
  messageDeliveries: (id: string) => ['portal', 'messages', id, 'deliveries'] as const,
  messageAttempts: (id: string) => ['portal', 'messages', id, 'attempts'] as const,
  eventTypes: ['portal', 'event-types'] as const,
}

export const portalApi = {
  getApp: () => http.get<Application>('/api/portal/app').then((r) => r.data),

  listEndpoints: () => http.get<Endpoint[]>('/api/portal/endpoints').then((r) => r.data),
  createEndpoint: (input: {
    url: string
    uid?: string
    description?: string
    filter_event_types?: string[]
  }) => http.post<EndpointWithSecret>('/api/portal/endpoints', input).then((r) => r.data),
  getEndpoint: (id: string) =>
    http.get<Endpoint>(`/api/portal/endpoints/${id}`).then((r) => r.data),
  updateEndpoint: (
    id: string,
    input: { url?: string; description?: string; filter_event_types?: string[]; disabled?: boolean },
  ) => http.patch<Endpoint>(`/api/portal/endpoints/${id}`, input).then((r) => r.data),
  deleteEndpoint: (id: string) =>
    http.delete(`/api/portal/endpoints/${id}`).then(() => undefined),
  getEndpointSecret: (id: string) =>
    http.get<{ secret: string }>(`/api/portal/endpoints/${id}/secret`).then((r) => r.data),
  rotateEndpointSecret: (id: string, input?: { secret?: string }) =>
    http
      .post<EndpointWithSecret>(`/api/portal/endpoints/${id}/rotate-secret`, input ?? {})
      .then((r) => r.data),
  testEndpoint: (id: string, input: { event_type: string; payload?: unknown }) =>
    http.post(`/api/portal/endpoints/${id}/test`, input).then((r) => r.data),
  recoverEndpoint: (id: string, input: { since: string }) =>
    http.post<RecoverResult>(`/api/portal/endpoints/${id}/recover`, input).then((r) => r.data),
  listEndpointAttempts: (id: string) =>
    http
      .get<MessageDeliveryAttempt[]>(`/api/portal/endpoints/${id}/attempts`)
      .then((r) => r.data),

  listMessages: (cursor?: string) =>
    http
      .get<Page<Message>>('/api/portal/messages', { params: cursor ? { cursor } : {} })
      .then((r) => r.data),
  getMessage: (id: string) =>
    http.get<MessageDetail>(`/api/portal/messages/${id}`).then((r) => r.data),
  listMessageDeliveries: (id: string) =>
    http
      .get<MessageDelivery[]>(`/api/portal/messages/${id}/deliveries`)
      .then((r) => r.data),
  listMessageAttempts: (id: string) =>
    http
      .get<MessageDeliveryAttempt[]>(`/api/portal/messages/${id}/attempts`)
      .then((r) => r.data),
  replayDelivery: (msgId: string, endpointId: string) =>
    http
      .post<{ delivery_id: string }>(
        `/api/portal/messages/${msgId}/endpoints/${endpointId}/replay`,
      )
      .then((r) => r.data),

  listEventTypes: () => http.get<EventType[]>('/api/portal/event-types').then((r) => r.data),
}
