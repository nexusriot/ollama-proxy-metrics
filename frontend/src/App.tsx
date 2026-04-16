import { useCallback, useEffect, useRef, useState } from 'react'
import { api, type Summary, type RequestRow, type DailyStat, type SessionStat } from './api'
import { SummaryCards } from './components/SummaryCards'
import { DailyChart } from './components/DailyChart'
import { RequestsTable } from './components/RequestsTable'
import { SessionsTable } from './components/SessionsTable'

type Tab = 'overview' | 'requests' | 'sessions'

const PAGE = 50

export default function App() {
  const [tab, setTab] = useState<Tab>('overview')

  const [summary, setSummary]   = useState<Summary | null>(null)
  const [daily, setDaily]       = useState<DailyStat[]>([])
  const [requests, setRequests] = useState<RequestRow[]>([])
  const [reqTotal, setReqTotal] = useState(0)
  const [sessions, setSessions] = useState<SessionStat[]>([])
  const [models, setModels]     = useState<string[]>([])
  const [error, setError]       = useState<string | null>(null)

  const [loadingSummary,  setLoadingSummary]  = useState(true)
  const [loadingDaily,    setLoadingDaily]    = useState(true)
  const [loadingRequests, setLoadingRequests] = useState(true)
  const [loadingSessions, setLoadingSessions] = useState(true)

  const [days,          setDays]          = useState(30)
  const [offset,        setOffset]        = useState(0)
  const [filterModel,   setFilterModel]   = useState('')
  const [filterSession, setFilterSession] = useState('')

  const refreshRef = useRef(0)

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

  const loadModels = useCallback(async () => {
    try { setModels(await api.models()) }
    catch { /* non-critical */ }
  }, [])

  const [cleaning, setCleaning] = useState(false)

  async function handleCleanup() {
    if (!window.confirm('Delete ALL recorded statistics? This cannot be undone.')) return
    setCleaning(true)
    setError(null)
    try {
      await api.cleanup()
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
    void loadModels()
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [days, offset, filterModel, filterSession])

  // initial load
  useEffect(() => { refreshAll() }, []) // eslint-disable-line react-hooks/exhaustive-deps

  // re-fetch requests when filters/page change
  useEffect(() => {
    void loadRequests(offset, filterModel, filterSession)
  }, [offset, filterModel, filterSession, loadRequests])

  // re-fetch daily when days change
  useEffect(() => { void loadDaily(days) }, [days, loadDaily])

  function handleSelectSession(id: string) {
    setFilterSession(id)
    setOffset(0)
    setTab('requests')
  }

  return (
    <div className="app">
      <header>
        <span style={{ fontSize: 22 }}>🦙</span>
        <h1>Ollama Proxy</h1>
        <span className="badge">Metrics</span>
        <nav>
          {(['overview', 'requests', 'sessions'] as Tab[]).map(t => (
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
            <SummaryCards data={summary} loading={loadingSummary} />
            <DailyChart
              data={daily}
              loading={loadingDaily}
              days={days}
              onDaysChange={d => { setDays(d) }}
            />
            <SessionsTable
              data={sessions.slice(0, 5)}
              loading={loadingSessions}
              onSelectSession={handleSelectSession}
            />
          </>
        )}

        {tab === 'requests' && (
          <RequestsTable
            data={requests}
            total={reqTotal}
            loading={loadingRequests}
            offset={offset}
            onOffsetChange={setOffset}
            models={models}
            filterModel={filterModel}
            filterSession={filterSession}
            onFilterModel={setFilterModel}
            onFilterSession={setFilterSession}
          />
        )}

        {tab === 'sessions' && (
          <SessionsTable
            data={sessions}
            loading={loadingSessions}
            onSelectSession={handleSelectSession}
          />
        )}
      </main>
    </div>
  )
}
