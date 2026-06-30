const BASE = '/admin/api'

export interface Summary {
  total_requests: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  avg_duration_ms: number
  cost: number
  unique_sessions: number
  unique_models: string[]
  error_count: number
}

export interface RequestRow {
  id: number
  request_id: string
  session_id: string
  timestamp: string
  endpoint: string
  method: string
  model: string
  stream: boolean
  status_code: number
  duration_ms: number
  ttft_ms: number
  request_bytes: number
  response_bytes: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cost: number
  error_message: string
  client_ip: string
  user_agent: string
  prompt_text: string
  response_text: string
}

export interface RequestsResponse {
  data: RequestRow[]
  total: number
  limit: number
  offset: number
}

export interface DailyStat {
  date: string
  total_requests: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  avg_duration_ms: number
  cost: number
  error_count: number
}

export interface SessionStat {
  session_id: string
  total_requests: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  avg_duration_ms: number
  cost: number
  first_seen: string
  last_seen: string
}

export interface ModelStat {
  model: string
  total_requests: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  avg_duration_ms: number
  avg_ttft_ms: number
  tokens_per_sec: number
  cost: number
  error_count: number
  first_seen: string
  last_seen: string
}

export interface ModelRate {
  prompt_per_1k: number
  completion_per_1k: number
}

export interface Pricing {
  currency: string
  default: ModelRate
  models: Record<string, ModelRate>
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(BASE + path)
  if (!res.ok) throw new Error(`API ${path}: ${res.status} ${res.statusText}`)
  return res.json() as Promise<T>
}

async function post<T>(path: string): Promise<T> {
  const res = await fetch(BASE + path, { method: 'POST' })
  if (!res.ok) throw new Error(`API ${path}: ${res.status} ${res.statusText}`)
  return res.json() as Promise<T>
}

export interface RequestParams {
  limit?: number
  offset?: number
  model?: string
  session?: string
  q?: string
  since?: string
  until?: string
}

function requestsQuery(params: RequestParams): URLSearchParams {
  const q = new URLSearchParams()
  if (params.limit)   q.set('limit',   String(params.limit))
  if (params.offset)  q.set('offset',  String(params.offset))
  if (params.model)   q.set('model',   params.model)
  if (params.session) q.set('session', params.session)
  if (params.q)       q.set('q',       params.q)
  if (params.since)   q.set('since',   params.since)
  if (params.until)   q.set('until',   params.until)
  return q
}

export const api = {
  summary:    ()           => get<Summary>('/summary'),
  cleanup:    ()           => post<{ status: string }>('/cleanup'),
  daily:      (days = 30)  => get<DailyStat[]>(`/daily?days=${days}`),
  sessions:   (limit = 50) => get<SessionStat[]>(`/sessions?limit=${limit}`),
  models:     ()           => get<string[]>('/models'),
  modelStats: ()           => get<ModelStat[]>('/model-stats'),
  pricing:    ()           => get<Pricing>('/pricing'),
  requests:   (params: RequestParams) =>
    get<RequestsResponse>(`/requests?${requestsQuery(params)}`),

  // URL for the export download (CSV by default, or JSON), honoring all filters.
  exportUrl: (params: RequestParams & { format?: 'csv' | 'json' }) => {
    const q = requestsQuery(params)
    if (params.format) q.set('format', params.format)
    return `${BASE}/export?${q}`
  },

  // Open a Server-Sent Events stream of newly recorded requests.
  streamUrl: () => `${BASE}/stream`,
}
