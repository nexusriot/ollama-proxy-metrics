import { useState } from 'react'
import {
  BarChart, Bar, XAxis, YAxis, CartesianGrid,
  Tooltip, Legend, ResponsiveContainer,
} from 'recharts'
import type { DailyStat } from '../api'

interface TooltipPayloadItem {
  color: string
  name: string
  value: number
}

function CustomTooltip({ active, payload, label }: {
  active?: boolean
  payload?: TooltipPayloadItem[]
  label?: string
}) {
  if (!active || !payload?.length) return null
  return (
    <div className="chart-tooltip">
      <div className="ct-label">{label}</div>
      {payload.map(p => (
        <div key={p.name} style={{ color: p.color }}>
          {p.name}: <strong>{p.value.toLocaleString()}</strong>
        </div>
      ))}
    </div>
  )
}

interface Props {
  data: DailyStat[]
  loading: boolean
  days: number
  onDaysChange: (d: number) => void
}

export function DailyChart({ data, loading, days, onDaysChange }: Props) {
  const [view, setView] = useState<'tokens' | 'requests' | 'duration'>('tokens')

  const chartData = data.map(d => ({
    date: d.date.slice(5), // MM-DD
    'Prompt':      d.prompt_tokens,
    'Completion':  d.completion_tokens,
    'Requests':    d.total_requests,
    'Avg ms':      Math.round(d.avg_duration_ms),
    errors:        d.error_count,
  }))

  return (
    <div className="section">
      <div className="section-header">
        <h2>Daily Usage</h2>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          {(['tokens', 'requests', 'duration'] as const).map(v => (
            <button
              key={v}
              onClick={() => setView(v)}
              style={{
                background: view === v ? 'var(--accent)' : 'var(--border)',
                border: 'none',
                color: view === v ? '#fff' : 'var(--muted)',
                borderRadius: 5,
                cursor: 'pointer',
                fontSize: 12,
                padding: '4px 10px',
              }}
            >
              {v.charAt(0).toUpperCase() + v.slice(1)}
            </button>
          ))}
          <select
            className="days-select"
            value={days}
            onChange={e => onDaysChange(Number(e.target.value))}
          >
            <option value={7}>7 days</option>
            <option value={14}>14 days</option>
            <option value={30}>30 days</option>
            <option value={90}>90 days</option>
          </select>
        </div>
      </div>

      <div className="chart-wrap">
        {loading ? (
          <div style={{ padding: '40px 20px' }}>
            <div className="skeleton" style={{ height: 200 }} />
          </div>
        ) : data.length === 0 ? (
          <div className="empty">No data yet — send some requests through the proxy.</div>
        ) : (
          <ResponsiveContainer width="100%" height={260}>
            <BarChart data={chartData} margin={{ top: 4, right: 16, left: 0, bottom: 0 }}>
              <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" vertical={false} />
              <XAxis
                dataKey="date"
                tick={{ fill: 'var(--muted)', fontSize: 11 }}
                axisLine={false}
                tickLine={false}
              />
              <YAxis
                tick={{ fill: 'var(--muted)', fontSize: 11 }}
                axisLine={false}
                tickLine={false}
                width={50}
              />
              <Tooltip content={<CustomTooltip />} cursor={{ fill: 'rgba(255,255,255,.04)' }} />
              <Legend
                wrapperStyle={{ fontSize: 12, color: 'var(--muted)', paddingTop: 8 }}
              />

              {view === 'tokens' && (
                <>
                  <Bar dataKey="Prompt"     stackId="t" fill="#6366f1" radius={[0,0,2,2]} />
                  <Bar dataKey="Completion" stackId="t" fill="#22d3ee" radius={[2,2,0,0]} />
                </>
              )}
              {view === 'requests' && (
                <Bar dataKey="Requests" fill="#34d399" radius={[3,3,0,0]} />
              )}
              {view === 'duration' && (
                <Bar dataKey="Avg ms" fill="#fbbf24" radius={[3,3,0,0]} />
              )}
            </BarChart>
          </ResponsiveContainer>
        )}
      </div>
    </div>
  )
}
