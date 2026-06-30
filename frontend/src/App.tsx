import { useCallback, useEffect, useRef, useState } from 'react'
import {
  api,
  type Summary, type RequestRow, type DailyStat, type SessionStat,
  type ModelStat, type Pricing,
} from './api'
import { SummaryCards } from './components/SummaryCards'
import { DailyChart } from './components/DailyChart'
import { RequestsTable } from './components/RequestsTable'
import { SessionsTable } from './components/SessionsTable'
import { ModelsTable } from './components/ModelsTable'

type Tab = 'overview' | 'requests' | 'models' | 'sessions'

const PAGE = 50
const LIVE_CAP = 200

export default function App() {
  const [tab, setTab] = useState<Tab>('overview')

  const [summary, setSummary]     = useState<Summary | null>(null)
  const [daily, setDaily]         = useState<DailyStat[]>([])
  const [requests, setRequests]   = useState<RequestRow[]>([])
  const [reqTotal, setReqTotal]   = useState(0)
  const [sessions, setSessions]   = useState<SessionStat[]>([])
  const [modelStats, setModelStats] = useState<ModelStat[]>([])
  const [models, setModels]       = useState<string[]>([])
  const [pricing, setPricing]     = useState<Pricing | null>(null)
  const [error, setError]         = useState<string | null>(null)

  const [loadingSummary,  setLoadingSummary]  = useState(true)
  const [loadingDaily,    setLoadingDaily]    = useState(true)
  const [loadingRequests, setLoadingRequests] = useState(true)
  const [loadingSessions, setLoadingSessions] = useState(true)
  const [loadingModels,   setLoadingModels]   = useState(true)

  const [days,          setDays]          = useState(30)
  const [offset,        setOffset]        = useState(0)
  const [filterModel,   setFilterModel]   = useState('')
  const [filterSession, setFilterSession] = useState('')

  const [live, setLive]         = useState(false)
  const [liveRows, setLiveRows] = useState<RequestRow[]>([])

  const refreshRef = useRef(0)
  const currency = pricing?.currency || 'USD'

  const loadSummary = useCallback(async () => {
    setLoadingSummary(true)
    try { setSummary(await api.summary()) }
    catch (e) { setError(String(e)) }
    finally { setLoadingSummary(false) }
  }, [])

  const loadDaily = useCallback(async (d: number) => {
    setLoadingDaily(true)
    try { setDaily(await api.daily(d)) }
    catch (e) { setError(String(e)) }
    finally { setLoadingDaily(false) }
  }, [])

  const loadRequests = useCallback(async (off: number, model: string, session: string) => {
    setLoadingRequests(true)
    try {
      const res = await api.requests({ limit: PAGE, offset: off, model, session })
      setRequests(res.data)
      setReqTotal(res.total)
    }
    catch (e) { setError(String(e)) }
    finally { setLoadingRequests(false) }
  }, [])

  const loadSessions = useCallback(async () => {
    setLoadingSessions(true)
    try { setSessions(await api.sessions()) }
    catch (e) { setError(String(e)) }
    finally { setLoadingSessions(false) }
  }, [])

  const loadModelStats = useCallback(async () => {
    setLoadingModels(true)
    try { setModelStats(await api.modelStats()) }
    catch (e) { setError(String(e)) }
    finally { setLoadingModels(false) }
  }, [])

  const loadModels = useCallback(async () => {
    try { setModels(await api.models()) }
    catch { /* non-critical */ }
  }, [])

  const loadPricing = useCallback(async () => {
    try { setPricing(await api.pricing()) }
    catch { /* non-critical */ }
  }, [])

  const [cleaning, setCleaning] = useState(false)

  async function handleCleanup() {
    if (!window.confirm('Delete ALL recorded statistics? This cannot be undone.')) return
    setCleaning(true)
    setError(null)
    try {
      await api.cleanup()
      setLiveRows([])
      refreshAll()
    } catch (e) {
      setError(String(e))
    } finally {
      setCleaning(false)
    }
  }

  const refreshAll = useCallback(() => {
    refreshRef.current++
    setError(null)
    void loadSummary()
    void loadDaily(days)
    void loadRequests(offset, filterModel, filterSession)
    void loadSessions()
    void loadModelStats()
    void loadModels()
    void loadPricing()
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [days, offset, filterModel, filterSession])

  // initial load
  useEffect(() => { refreshAll() }, []) // eslint-disable-line react-hooks/exhaustive-deps

  // re-fetch requests when filters/page change (only in browse mode)
  useEffect(() => {
    if (!live) void loadRequests(offset, filterModel, filterSession)
  }, [offset, filterModel, filterSession, live, loadRequests])

  // re-fetch daily when days change
  useEffect(() => { void loadDaily(days) }, [days, loadDaily])

  // live tail via Server-Sent Events
  useEffect(() => {
    if (!live) return
    setLiveRows([])
    const es = new EventSource(api.streamUrl())
    es.onmessage = ev => {
      try {
        const row = JSON.parse(ev.data) as RequestRow
        setLiveRows(prev => [row, ...prev].slice(0, LIVE_CAP))
      } catch { /* ignore malformed event */ }
    }
    es.onerror = () => setError('Live stream disconnected (is the proxy reachable?)')
    return () => es.close()
  }, [live])

  function handleSelectSession(id: string) {
    setFilterSession(id)
    setOffset(0)
    setTab('requests')
  }

  const liveFiltered = liveRows.filter(r =>
    (!filterModel || r.model === filterModel) &&
    (!filterSession || (r.session_id || '').includes(filterSession)),
  )
  const shownRequests = live ? liveFiltered : requests
  const shownTotal = live ? liveFiltered.length : reqTotal

  return (
    <div className="app">
      <header>
        <span style={{ fontSize: 22 }}>🦙</span>
        <h1>Ollama Proxy</h1>
        <span className="badge">Metrics</span>
        <nav>
          {(['overview', 'requests', 'models', 'sessions'] as Tab[]).map(t => (
            <button
              key={t}
              className={tab === t ? 'active' : ''}
              onClick={() => setTab(t)}
            >
              {t.charAt(0).toUpperCase() + t.slice(1)}
            </button>
          ))}
        </nav>
        <button className="refresh-btn" onClick={refreshAll} title="Refresh data">
          ↻ Refresh
        </button>
        <button
          className="cleanup-btn"
          onClick={handleCleanup}
          disabled={cleaning}
          title="Delete all statistics"
        >
          {cleaning ? 'Clearing…' : '🗑 Clear stats'}
        </button>
      </header>

      <main>
        {error && (
          <div className="error-box">
            <strong>Error: </strong>{error}
            {' '}<button onClick={() => setError(null)} style={{ background: 'none', border: 'none', color: 'inherit', cursor: 'pointer', textDecoration: 'underline' }}>dismiss</button>
          </div>
        )}

        {tab === 'overview' && (
          <>
            <SummaryCards data={summary} loading={loadingSummary} currency={currency} />
            <DailyChart
              data={daily}
              loading={loadingDaily}
              days={days}
              onDaysChange={d => { setDays(d) }}
              currency={currency}
            />
            <SessionsTable
              data={sessions.slice(0, 5)}
              loading={loadingSessions}
              onSelectSession={handleSelectSession}
              currency={currency}
            />
          </>
        )}

        {tab === 'requests' && (
          <RequestsTable
            data={shownRequests}
            total={shownTotal}
            loading={loadingRequests && !live}
            offset={offset}
            onOffsetChange={setOffset}
            models={models}
            filterModel={filterModel}
            filterSession={filterSession}
            onFilterModel={setFilterModel}
            onFilterSession={setFilterSession}
            currency={currency}
            live={live}
            onToggleLive={() => setLive(v => !v)}
            exportHref={api.exportUrl({ model: filterModel, session: filterSession })}
          />
        )}

        {tab === 'models' && (
          <ModelsTable data={modelStats} loading={loadingModels} currency={currency} />
        )}

        {tab === 'sessions' && (
          <SessionsTable
            data={sessions}
            loading={loadingSessions}
            onSelectSession={handleSelectSession}
            currency={currency}
          />
        )}
      </main>
    </div>
  )
}
