import { useState } from 'react'
import type { RequestRow } from '../api'

const PAGE = 50

interface Props {
  data: RequestRow[]
  total: number
  loading: boolean
  offset: number
  onOffsetChange: (o: number) => void
  models: string[]
  filterModel: string
  filterSession: string
  onFilterModel: (m: string) => void
  onFilterSession: (s: string) => void
}

function fmtBytes(b: number): string {
  if (b >= 1 << 20) return (b / (1 << 20)).toFixed(1) + ' MB'
  if (b >= 1 << 10) return (b / (1 << 10)).toFixed(1) + ' KB'
  return b + ' B'
}

function fmtTime(iso: string): string {
  try {
    return new Date(iso).toLocaleString(undefined, {
      month: 'short', day: '2-digit',
      hour: '2-digit', minute: '2-digit', second: '2-digit',
    })
  } catch {
    return iso
  }
}

export function RequestsTable({
  data, total, loading, offset,
  onOffsetChange,
  models, filterModel, filterSession,
  onFilterModel, onFilterSession,
}: Props) {
  const [expandedId, setExpandedId] = useState<number | null>(null)

  const page = Math.floor(offset / PAGE) + 1
  const pages = Math.max(1, Math.ceil(total / PAGE))

  return (
    <div className="section">
      <div className="section-header">
        <h2>Requests</h2>
        <span style={{ fontSize: 12, color: 'var(--muted)' }}>{total.toLocaleString()} total</span>
      </div>

      <div className="filters">
        <select
          value={filterModel}
          onChange={e => { onFilterModel(e.target.value); onOffsetChange(0) }}
        >
          <option value="">All models</option>
          {models.map(m => <option key={m} value={m}>{m}</option>)}
        </select>
        <input
          placeholder="Filter by session…"
          value={filterSession}
          onChange={e => { onFilterSession(e.target.value); onOffsetChange(0) }}
          style={{ width: 200 }}
        />
      </div>

      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Model</th>
              <th>Endpoint</th>
              <th>Mode</th>
              <th>Status</th>
              <th>Duration</th>
              <th>Tokens (P+C)</th>
              <th>Bytes In/Out</th>
              <th>Session</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              Array.from({ length: 8 }).map((_, i) => (
                <tr key={i}>
                  {Array.from({ length: 9 }).map((__, j) => (
                    <td key={j}><div className="skeleton" style={{ width: '80%' }} /></td>
                  ))}
                </tr>
              ))
            ) : data.length === 0 ? (
              <tr>
                <td colSpan={9} className="empty">No requests yet.</td>
              </tr>
            ) : (
              data.map(r => (
                <>
                  <tr
                    key={r.id}
                    style={{ cursor: 'pointer' }}
                    onClick={() => setExpandedId(expandedId === r.id ? null : r.id)}
                  >
                    <td className="mono">{fmtTime(r.timestamp)}</td>
                    <td>{r.model}</td>
                    <td className="mono" style={{ fontSize: 11 }}>{r.endpoint}</td>
                    <td>
                      <span className={`pill ${r.stream ? 'pill-stream' : 'pill-sync'}`}>
                        {r.stream ? 'stream' : 'sync'}
                      </span>
                    </td>
                    <td>
                      <span className={`pill ${r.status_code < 400 ? 'pill-ok' : 'pill-err'}`}>
                        {r.status_code}
                      </span>
                    </td>
                    <td className="mono">{r.duration_ms}ms</td>
                    <td className="mono">
                      {r.prompt_tokens.toLocaleString()} + {r.completion_tokens.toLocaleString()}
                      {' '}
                      <span style={{ color: 'var(--muted)' }}>= {r.total_tokens.toLocaleString()}</span>
                    </td>
                    <td className="mono" style={{ color: 'var(--muted)' }}>
                      {fmtBytes(r.request_bytes)} / {fmtBytes(r.response_bytes)}
                    </td>
                    <td className="mono" style={{ color: 'var(--muted)', maxWidth: 120, overflow: 'hidden', textOverflow: 'ellipsis' }}>
                      {r.session_id || '—'}
                    </td>
                  </tr>
                  {expandedId === r.id && (
                    <tr key={`${r.id}-detail`} style={{ background: 'rgba(99,102,241,.05)' }}>
                      <td colSpan={9} style={{ padding: '12px 16px' }}>
                        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(220px, 1fr))', gap: 8, fontSize: 12 }}>
                          <div><span style={{ color: 'var(--muted)' }}>Request ID: </span><span className="mono">{r.request_id}</span></div>
                          <div><span style={{ color: 'var(--muted)' }}>Client IP: </span>{r.client_ip}</div>
                          <div><span style={{ color: 'var(--muted)' }}>Method: </span>{r.method}</div>
                          <div style={{ gridColumn: '1 / -1' }}><span style={{ color: 'var(--muted)' }}>User-Agent: </span>{r.user_agent || '—'}</div>
                          {r.error_message && (
                            <div style={{ gridColumn: '1 / -1', color: 'var(--red)' }}>
                              Error: {r.error_message}
                            </div>
                          )}
                        </div>
                      </td>
                    </tr>
                  )}
                </>
              ))
            )}
          </tbody>
        </table>
      </div>

      <div className="pagination">
        <span>Page {page} of {pages}</span>
        <button disabled={offset === 0} onClick={() => onOffsetChange(0)}>«</button>
        <button disabled={offset === 0} onClick={() => onOffsetChange(Math.max(0, offset - PAGE))}>‹</button>
        <button disabled={offset + PAGE >= total} onClick={() => onOffsetChange(offset + PAGE)}>›</button>
        <button disabled={offset + PAGE >= total} onClick={() => onOffsetChange((pages - 1) * PAGE)}>»</button>
      </div>
    </div>
  )
}
