import type { Summary } from '../api'

function fmt(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M'
  if (n >= 1_000)     return (n / 1_000).toFixed(1) + 'K'
  return String(n)
}

function fmtMs(ms: number): string {
  if (ms >= 60_000) return (ms / 60_000).toFixed(1) + 'm'
  if (ms >= 1_000)  return (ms / 1_000).toFixed(1) + 's'
  return ms.toFixed(0) + 'ms'
}

interface Props {
  data: Summary | null
  loading: boolean
}

export function SummaryCards({ data, loading }: Props) {
  if (loading || !data) {
    return (
      <div className="cards">
        {Array.from({ length: 7 }).map((_, i) => (
          <div key={i} className="card">
            <div className="skeleton" style={{ width: '60%', marginBottom: 12 }} />
            <div className="skeleton" style={{ width: '40%', height: 28 }} />
          </div>
        ))}
      </div>
    )
  }

  const cards = [
    { label: 'Total Requests',   value: fmt(data.total_requests),    sub: 'all time' },
    { label: 'Prompt Tokens',    value: fmt(data.prompt_tokens),      sub: 'all time' },
    { label: 'Completion Tokens',value: fmt(data.completion_tokens),  sub: 'all time' },
    { label: 'Total Tokens',     value: fmt(data.total_tokens),       sub: 'prompt + completion' },
    { label: 'Avg Duration',     value: fmtMs(data.avg_duration_ms),  sub: 'per request' },
    { label: 'Unique Sessions',  value: fmt(data.unique_sessions),    sub: data.unique_models.join(', ') || 'no sessions' },
    { label: 'Errors',           value: fmt(data.error_count),        sub: 'upstream + write errors', accent: data.error_count > 0 },
  ]

  return (
    <div className="cards">
      {cards.map(c => (
        <div key={c.label} className="card">
          <div className="label">{c.label}</div>
          <div className="value" style={c.accent ? { color: 'var(--red)' } : undefined}>
            {c.value}
          </div>
          <div className="sub">{c.sub}</div>
        </div>
      ))}
    </div>
  )
}
