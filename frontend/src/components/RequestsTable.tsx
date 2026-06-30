import { useState } from 'react'
import type { RequestRow } from '../api'
import { fmtCost } from '../format'

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
  search: string
  onSearch: (s: string) => void
  since: string
  until: string
  onSince: (s: string) => void
  onUntil: (s: string) => void
  hasFilters: boolean
  onClearFilters: () => void
  currency: string
  live: boolean
  onToggleLive: () => void
  exportHref: string
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

// CopyButton copies text to the clipboard and briefly confirms.
function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <button
      className="copy-btn"
      onClick={() => {
        navigator.clipboard?.writeText(text).then(
          () => { setCopied(true); setTimeout(() => setCopied(false), 1200) },
          () => { /* clipboard unavailable */ },
        )
      }}
    >
      {copied ? 'copied ✓' : 'copy'}
    </button>
  )
}

// tokensPerSec estimates generation throughput, excluding time-to-first-token.
function tokensPerSec(r: RequestRow): number {
  const genMs = r.ttft_ms > 0 ? r.duration_ms - r.ttft_ms : r.duration_ms
  if (genMs <= 0 || r.completion_tokens <= 0) return 0
  return r.completion_tokens / (genMs / 1000)
}

const COLS = 10

export function RequestsTable({
  data, total, loading, offset,
  onOffsetChange,
  models, filterModel, filterSession,
  onFilterModel, onFilterSession,
  search, onSearch, since, until, onSince, onUntil,
  hasFilters, onClearFilters,
  currency, live, onToggleLive, exportHref,
}: Props) {
  const [expandedId, setExpandedId] = useState<number | null>(null)

  const page = Math.floor(offset / PAGE) + 1
  const pages = Math.max(1, Math.ceil(total / PAGE))

  return (
    <div className="section">
      <div className="section-header">
        <h2>Requests</h2>
        <span style={{ fontSize: 12, color: 'var(--muted)' }}>
          {live ? `${data.length} live` : `${total.toLocaleString()} total`}
        </span>
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
          style={{ width: 160 }}
        />
        <input
          placeholder="Search prompt / response…"
          value={search}
          onChange={e => onSearch(e.target.value)}
          style={{ width: 200 }}
        />
        <label className="filter-label" title="Filter requests at or after this time (local)">
          From
          <input type="datetime-local" value={since} disabled={live} onChange={e => onSince(e.target.value)} />
        </label>
        <label className="filter-label" title="Filter requests at or before this time (local)">
          To
          <input type="datetime-local" value={until} disabled={live} onChange={e => onUntil(e.target.value)} />
        </label>
        {hasFilters && (
          <button className="clear-btn" onClick={onClearFilters} title="Clear all filters">✕ clear</button>
        )}
        <button
          className={live ? 'live-btn live-on' : 'live-btn'}
          onClick={onToggleLive}
          title="Stream new requests as they happen"
          style={{ marginLeft: 'auto' }}
        >
          {live ? '● Live' : '○ Live'}
        </button>
        <a className="export-btn" href={exportHref}>⤓ CSV</a>
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
              <th>Cost</th>
              <th>Bytes In/Out</th>
              <th>Session</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              Array.from({ length: 8 }).map((_, i) => (
                <tr key={i}>
                  {Array.from({ length: COLS }).map((__, j) => (
                    <td key={j}><div className="skeleton" style={{ width: '80%' }} /></td>
                  ))}
                </tr>
              ))
            ) : data.length === 0 ? (
              <tr>
                <td colSpan={COLS} className="empty">
                  {live ? 'Waiting for live requests…' : 'No requests yet.'}
                </td>
              </tr>
            ) : (
              data.map(r => (
                <>
                  <tr
                    key={r.id || r.request_id}
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
                    <td className="mono">{r.cost > 0 ? fmtCost(r.cost, currency) : '—'}</td>
                    <td className="mono" style={{ color: 'var(--muted)' }}>
                      {fmtBytes(r.request_bytes)} / {fmtBytes(r.response_bytes)}
                    </td>
                    <td className="mono" style={{ color: 'var(--muted)', maxWidth: 120, overflow: 'hidden', textOverflow: 'ellipsis' }}>
                      {r.session_id || '—'}
                    </td>
                  </tr>
                  {expandedId === r.id && (
                    <tr key={`${r.id}-detail`} style={{ background: 'rgba(99,102,241,.04)' }}>
                      <td colSpan={COLS} style={{ padding: '16px 18px' }}>

                        {/* prompt / response */}
                        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginBottom: 12 }}>
                          <div>
                            <div className="detail-head">
                              <span>Prompt</span>
                              {r.prompt_text && <CopyButton text={r.prompt_text} />}
                            </div>
                            <pre className="detail-text">
                              {r.prompt_text || <span style={{ color: 'var(--muted)', fontStyle: 'italic' }}>— not captured —</span>}
                            </pre>
                          </div>
                          <div>
                            <div className="detail-head">
                              <span>Response</span>
                              {r.response_text && <CopyButton text={r.response_text} />}
                            </div>
                            <pre className="detail-text">
                              {r.response_text || <span style={{ color: 'var(--muted)', fontStyle: 'italic' }}>— not captured —</span>}
                            </pre>
                          </div>
                        </div>

                        {/* metadata row */}
                        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(200px, 1fr))', gap: 6, fontSize: 12, borderTop: '1px solid var(--border)', paddingTop: 10 }}>
                          <div><span style={{ color: 'var(--muted)' }}>Request ID </span><span className="mono" style={{ fontSize: 11 }}>{r.request_id}</span></div>
                          <div><span style={{ color: 'var(--muted)' }}>Client IP </span>{r.client_ip || '—'}</div>
                          <div><span style={{ color: 'var(--muted)' }}>Method </span>{r.method}</div>
                          <div><span style={{ color: 'var(--muted)' }}>TTFT </span>{r.ttft_ms > 0 ? `${r.ttft_ms}ms` : '—'}</div>
                          <div><span style={{ color: 'var(--muted)' }}>Throughput </span>{tokensPerSec(r) > 0 ? `${tokensPerSec(r).toFixed(1)} tok/s` : '—'}</div>
                          <div><span style={{ color: 'var(--muted)' }}>Cost </span>{r.cost > 0 ? fmtCost(r.cost, currency) : '—'}</div>
                          <div><span style={{ color: 'var(--muted)' }}>Session </span><span className="mono" style={{ fontSize: 11 }}>{r.session_id || '—'}</span></div>
                          <div style={{ gridColumn: '1 / -1' }}><span style={{ color: 'var(--muted)' }}>User-Agent </span>{r.user_agent || '—'}</div>
                          {r.error_message && (
                            <div style={{ gridColumn: '1 / -1', color: 'var(--red)' }}>
                              ⚠ {r.error_message}
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

      {!live && (
        <div className="pagination">
          <span>Page {page} of {pages}</span>
          <button disabled={offset === 0} onClick={() => onOffsetChange(0)}>«</button>
          <button disabled={offset === 0} onClick={() => onOffsetChange(Math.max(0, offset - PAGE))}>‹</button>
          <button disabled={offset + PAGE >= total} onClick={() => onOffsetChange(offset + PAGE)}>›</button>
          <button disabled={offset + PAGE >= total} onClick={() => onOffsetChange((pages - 1) * PAGE)}>»</button>
        </div>
      )}
    </div>
  )
}
