import type { SessionStat } from '../api'

interface Props {
  data: SessionStat[]
  loading: boolean
  onSelectSession: (id: string) => void
}

function fmtDate(iso: string): string {
  try {
    return new Date(iso).toLocaleString(undefined, {
      month: 'short', day: '2-digit',
      hour: '2-digit', minute: '2-digit',
    })
  } catch {
    return iso
  }
}

export function SessionsTable({ data, loading, onSelectSession }: Props) {
  return (
    <div className="section">
      <div className="section-header">
        <h2>Sessions</h2>
        <span style={{ fontSize: 12, color: 'var(--muted)' }}>click a row to filter requests</span>
      </div>

      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Session ID</th>
              <th>Requests</th>
              <th>Prompt Tokens</th>
              <th>Completion Tokens</th>
              <th>Total Tokens</th>
              <th>Avg Duration</th>
              <th>First Seen</th>
              <th>Last Seen</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              Array.from({ length: 5 }).map((_, i) => (
                <tr key={i}>
                  {Array.from({ length: 8 }).map((__, j) => (
                    <td key={j}><div className="skeleton" style={{ width: '80%' }} /></td>
                  ))}
                </tr>
              ))
            ) : data.length === 0 ? (
              <tr>
                <td colSpan={8} className="empty">
                  No sessions yet. Send requests with an <code>X-Session-ID</code> header,
                  or the proxy will group by client IP.
                </td>
              </tr>
            ) : (
              data.map(s => (
                <tr
                  key={s.session_id}
                  style={{ cursor: 'pointer' }}
                  onClick={() => onSelectSession(s.session_id)}
                  title={`Click to filter requests by session ${s.session_id}`}
                >
                  <td className="mono" style={{ maxWidth: 200, overflow: 'hidden', textOverflow: 'ellipsis' }}>
                    {s.session_id}
                  </td>
                  <td>{s.total_requests.toLocaleString()}</td>
                  <td className="mono">{s.prompt_tokens.toLocaleString()}</td>
                  <td className="mono">{s.completion_tokens.toLocaleString()}</td>
                  <td className="mono" style={{ fontWeight: 600 }}>{s.total_tokens.toLocaleString()}</td>
                  <td className="mono">{Math.round(s.avg_duration_ms)}ms</td>
                  <td style={{ color: 'var(--muted)', fontSize: 12 }}>{fmtDate(s.first_seen)}</td>
                  <td style={{ color: 'var(--muted)', fontSize: 12 }}>{fmtDate(s.last_seen)}</td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}
