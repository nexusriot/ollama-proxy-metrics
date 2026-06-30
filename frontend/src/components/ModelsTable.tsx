import {
  BarChart, Bar, XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer,
} from 'recharts'
import type { ModelStat } from '../api'
import { fmtCost, fmtNum } from '../format'

interface Props {
  data: ModelStat[]
  loading: boolean
  currency: string
}

export function ModelsTable({ data, loading, currency }: Props) {
  const chartData = data.map(m => ({
    model: m.model,
    'Tokens/s': Math.round(m.tokens_per_sec * 10) / 10,
    'Avg ms': Math.round(m.avg_duration_ms),
  }))

  return (
    <>
      <div className="section">
        <div className="section-header">
          <h2>Model Throughput</h2>
          <span style={{ fontSize: 12, color: 'var(--muted)' }}>completion tokens / second</span>
        </div>
        <div className="chart-wrap">
          {loading ? (
            <div style={{ padding: '40px 20px' }}><div className="skeleton" style={{ height: 200 }} /></div>
          ) : data.length === 0 ? (
            <div className="empty">No model data yet.</div>
          ) : (
            <ResponsiveContainer width="100%" height={Math.max(160, 48 * chartData.length)}>
              <BarChart layout="vertical" data={chartData} margin={{ top: 4, right: 24, left: 8, bottom: 0 }}>
                <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" horizontal={false} />
                <XAxis type="number" tick={{ fill: 'var(--muted)', fontSize: 11 }} axisLine={false} tickLine={false} />
                <YAxis
                  type="category" dataKey="model" width={120}
                  tick={{ fill: 'var(--muted)', fontSize: 11 }} axisLine={false} tickLine={false}
                />
                <Tooltip cursor={{ fill: 'rgba(255,255,255,.04)' }} />
                <Bar dataKey="Tokens/s" fill="#22d3ee" radius={[0, 3, 3, 0]} />
              </BarChart>
            </ResponsiveContainer>
          )}
        </div>
      </div>

      <div className="section">
        <div className="section-header">
          <h2>Models</h2>
          <span style={{ fontSize: 12, color: 'var(--muted)' }}>per-model totals</span>
        </div>
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Model</th>
                <th>Requests</th>
                <th>Prompt</th>
                <th>Completion</th>
                <th>Total Tokens</th>
                <th>Avg Duration</th>
                <th>Avg TTFT</th>
                <th>Tokens/s</th>
                <th>Cost</th>
                <th>Errors</th>
              </tr>
            </thead>
            <tbody>
              {loading ? (
                Array.from({ length: 4 }).map((_, i) => (
                  <tr key={i}>
                    {Array.from({ length: 10 }).map((__, j) => (
                      <td key={j}><div className="skeleton" style={{ width: '80%' }} /></td>
                    ))}
                  </tr>
                ))
              ) : data.length === 0 ? (
                <tr><td colSpan={10} className="empty">No model data yet.</td></tr>
              ) : (
                data.map(m => (
                  <tr key={m.model}>
                    <td style={{ fontWeight: 600 }}>{m.model}</td>
                    <td>{m.total_requests.toLocaleString()}</td>
                    <td className="mono">{fmtNum(m.prompt_tokens)}</td>
                    <td className="mono">{fmtNum(m.completion_tokens)}</td>
                    <td className="mono" style={{ fontWeight: 600 }}>{fmtNum(m.total_tokens)}</td>
                    <td className="mono">{Math.round(m.avg_duration_ms)}ms</td>
                    <td className="mono">{m.avg_ttft_ms > 0 ? `${Math.round(m.avg_ttft_ms)}ms` : '—'}</td>
                    <td className="mono">{m.tokens_per_sec > 0 ? m.tokens_per_sec.toFixed(1) : '—'}</td>
                    <td className="mono">{m.cost > 0 ? fmtCost(m.cost, currency) : '—'}</td>
                    <td className="mono" style={{ color: m.error_count > 0 ? 'var(--red)' : 'var(--muted)' }}>
                      {m.error_count}
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </div>
    </>
  )
}
