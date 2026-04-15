const BASE = '/admin/api'

export interface Summary {
  total_requests: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  avg_duration_ms: number
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
  request_bytes: number
  response_bytes: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  error_message: string
  client_ip: string
  user_agent: string
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
  error_count: number
}

export interface SessionStat {
  session_id: string
  total_requests: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  avg_duration_ms: number
  first_seen: string
  last_seen: string
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(BASE + path)
  if (!res.ok) throw new Error(`API ${path}: ${res.status} ${res.statusText}`)
  return res.json() as Promise<T>
}

export const api = {
  summary: () => get<Summary>('/summary'),
  requests: (params: { limit?: number; offset?: number; model?: string; session?: string }) => {
    const q = new URLSearchParams()
    if (params.limit)   q.set('limit',   String(params.limit))
    if (params.offset)  q.set('offset',  String(params.offset))
    if (params.model)   q.set('model',   params.model)
    if (params.session) q.set('session', params.session)
    return get<RequestsResponse>(`/requests?${q}`)
  },
  daily:    (days = 30)  => get<DailyStat[]>(`/daily?days=${days}`),
  sessions: (limit = 50) => get<SessionStat[]>(`/sessions?limit=${limit}`),
  models:   ()           => get<string[]>('/models'),
}
