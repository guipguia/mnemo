import { useEffect, useState } from 'react'
import { api, type Change, type Cluster } from './api'
import { Author, Paths, TypeBadge } from './Badges'
import { ago, fmtTime, fromLocalInput, toLocalInput, type Nav } from './url'

const WINDOWS: Record<string, number> = { '15m': 15, '1h': 60, '6h': 360, '24h': 1440, '7d': 10080 }
const LIMIT = 500

// The home screen: "what changed in the last hour?" across clusters.
export function Feed({ q, nav }: { q: URLSearchParams; nav: Nav }) {
  const filter = Object.fromEntries(q)
  const win = WINDOWS[filter.window] ? filter.window : '1h'
  const until = filter.until // empty: up to now, refreshed live
  const set = (k: string, v: string | undefined) => nav({ ...filter, [k]: v })

  const [clusters, setClusters] = useState<Cluster[]>([])
  const [changes, setChanges] = useState<Change[] | null>(null)
  const [error, setError] = useState('')
  const [hideBaseline, setHideBaseline] = useState(true)

  useEffect(() => {
    api.clusters().then(setClusters, (e) => setError(e.message))
  }, [])

  useEffect(() => {
    const load = () => {
      const end = until ? new Date(until) : new Date()
      const since = new Date(end.getTime() - WINDOWS[win] * 60_000)
      api
        .changes({
          cluster: filter.cluster,
          namespace: filter.namespace,
          kind: filter.kind,
          source: filter.source,
          drift: filter.drift,
          since: since.toISOString(),
          until: until,
          limit: String(LIMIT),
        })
        .then((c) => (setChanges(c), setError('')), (e) => setError(e.message))
    }
    load()
    if (!until) {
      const t = setInterval(load, 10_000)
      return () => clearInterval(t)
    }
  }, [q.toString()])

  const shown = changes?.filter((c) => !(hideBaseline && c.type === 'baseline')) ?? []
  const baselines = (changes?.length ?? 0) - shown.length

  return (
    <>
      <form className="filters" onSubmit={(e) => e.preventDefault()}>
        <label>
          Cluster
          <select value={filter.cluster ?? ''} onChange={(e) => set('cluster', e.target.value)}>
            <option value="">All clusters</option>
            {clusters.map((c) => (
              <option key={c.name}>{c.name}</option>
            ))}
          </select>
        </label>
        <label>
          Namespace
          <input
            defaultValue={filter.namespace}
            placeholder="any"
            onBlur={(e) => set('namespace', e.target.value.trim())}
            onKeyDown={(e) => e.key === 'Enter' && e.currentTarget.blur()}
          />
        </label>
        <label>
          Kind
          <input
            defaultValue={filter.kind}
            placeholder="any, e.g. ConfigMap"
            onBlur={(e) => set('kind', e.target.value.trim())}
            onKeyDown={(e) => e.key === 'Enter' && e.currentTarget.blur()}
          />
        </label>
        <label>
          Source
          <select value={filter.source ?? ''} onChange={(e) => set('source', e.target.value)}>
            <option value="">Any</option>
            <option value="manual">Manual</option>
            <option value="gitops">GitOps</option>
            <option value="controller">Controller</option>
            <option value="unknown">Unknown</option>
          </select>
        </label>
        <label>
          Last
          <select value={win} onChange={(e) => set('window', e.target.value)}>
            {Object.keys(WINDOWS).map((w) => (
              <option key={w}>{w}</option>
            ))}
          </select>
        </label>
        <label>
          Ending at
          <input type="datetime-local" value={toLocalInput(until ?? null)} onChange={(e) => set('until', fromLocalInput(e.target.value))} />
        </label>
        <label className="check">
          <input type="checkbox" checked={filter.drift === 'true'} onChange={(e) => set('drift', e.target.checked ? 'true' : undefined)} />
          Drift only
        </label>
      </form>

      <div className="status">
        {error ? (
          <span className="error">Could not load changes: {error}</span>
        ) : changes === null ? (
          'Loading…'
        ) : (
          <>
            {shown.length} change{shown.length === 1 ? '' : 's'} in the {until ? `${win} before ${fmtTime(until)}` : `last ${win}`}
            {!until && <span className="live">live</span>}
            {changes.length === LIMIT && <span className="warn"> showing newest {LIMIT}, narrow the filters</span>}
            {baselines > 0 && (
              <button className="link" onClick={() => setHideBaseline(false)}>
                show {baselines} baseline record{baselines === 1 ? '' : 's'}
              </button>
            )}
          </>
        )}
      </div>

      {changes !== null && shown.length === 0 && !error ? (
        <p className="empty">
          Nothing changed in this window. Try a longer window, or clear filters.
        </p>
      ) : (
        <div className="feed-wrap">
        <table className="feed">
          <thead>
            <tr>
              <th>Time</th>
              {!filter.cluster && <th>Cluster</th>}
              <th>Object</th>
              <th>Change</th>
              <th>Who</th>
              <th>What</th>
            </tr>
          </thead>
          <tbody>
            {shown.map((c) => (
              <tr key={c.id} className={c.drift ? 'drift-row' : ''}>
                <td className="time" title={`${ago(c.time)} · ${c.time}`}>
                  {fmtTime(c.time).slice(11)}
                  <span className="date">{fmtTime(c.time).slice(0, 10)}</span>
                </td>
                {!filter.cluster && <td>{c.cluster}</td>}
                <td>
                  <a
                    href="#"
                    onClick={(e) => {
                      e.preventDefault()
                      nav({ cluster: c.cluster, group: c.group, kind: c.kind, namespace: c.namespace, name: c.name, change: String(c.id) })
                    }}
                  >
                    <span className="kind">{c.kind}</span> {c.namespace && <span className="ns">{c.namespace}/</span>}
                    {c.name}
                  </a>
                </td>
                <td>
                  <TypeBadge c={c} />
                </td>
                <td>
                  <Author c={c} />
                </td>
                <td>
                  <Paths paths={c.paths} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        </div>
      )}
    </>
  )
}
