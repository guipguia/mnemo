import { useEffect, useState } from 'react'
import { stringify } from 'yaml'
import { api, type Change, type Ref } from './api'
import { Author, Paths, TypeBadge } from './Badges'
import { Diff } from './Diff'
import { ago, fmtTime, fromLocalInput, toLocalInput, type Nav } from './url'

const yaml = (c: Change | null | undefined) => (c?.object ? stringify(c.object) : '')

// One object's timeline: pick a version to see what changed, or ask for its state at any time.
export function ObjectView({ q, nav }: { q: URLSearchParams; nav: Nav }) {
  const ref: Ref = {
    cluster: q.get('cluster') ?? '',
    group: q.get('group') ?? '',
    kind: q.get('kind') ?? '',
    namespace: q.get('namespace') ?? '',
    name: q.get('name') ?? '',
  }
  const at = q.get('at')
  const go = (p: { change?: string; at?: string }) => nav({ ...ref, ...p })

  const [history, setHistory] = useState<Change[] | null>(null)
  const [error, setError] = useState('')
  useEffect(() => {
    api.history(ref).then(setHistory, (e) => setError(e.message))
  }, [ref.cluster, ref.group, ref.kind, ref.namespace, ref.name])

  const idx = history ? Math.max(0, history.findIndex((c) => String(c.id) === q.get('change'))) : -1
  const selected = history?.[idx]
  const previous = history?.[idx + 1]

  return (
    <div className="object">
      <div className="crumbs">
        <a href="?" onClick={(e) => (e.preventDefault(), nav({}))}>
          ← Change feed
        </a>
      </div>
      <h1>
        <span className="kind">{ref.kind}</span> {ref.namespace && <span className="ns">{ref.namespace}/</span>}
        {ref.name}
        <span className="cluster">{ref.cluster}</span>
      </h1>
      {error && <p className="error">Could not load history: {error}</p>}

      <div className="split">
        <aside className="timeline">
          <label className="at">
            State at
            <input type="datetime-local" step="1" value={toLocalInput(at)} onChange={(e) => go({ at: fromLocalInput(e.target.value) })} />
          </label>
          <ol>
            {history?.map((c, i) => (
              <li key={c.id} className={!at && i === idx ? 'selected' : ''}>
                <button onClick={() => go({ change: String(c.id) })}>
                  <span className="when" title={c.time}>
                    {fmtTime(c.time)} <span className="ago">{ago(c.time)}</span>
                  </span>
                  <span>
                    <TypeBadge c={c} /> <Author c={c} />
                  </span>
                  <Paths paths={c.paths} max={2} />
                </button>
              </li>
            ))}
          </ol>
          {history?.length === 1000 && <p className="warn">Showing the newest 1000 versions.</p>}
        </aside>

        <section className="detail">
          {at ? (
            <AtTime ref_={ref} at={at} />
          ) : selected ? (
            <Version key={selected.id} c={selected} prev={previous} />
          ) : (
            history && <p className="empty">mnemo has no history for this object.</p>
          )}
        </section>
      </div>
    </div>
  )
}

function Version({ c, prev }: { c: Change; prev?: Change }) {
  const [full, setFull] = useState<[Change, Change | null] | null>(null)
  const [mode, setMode] = useState<'diff' | 'yaml'>('diff')
  useEffect(() => {
    Promise.all([api.change(c.id), prev ? api.change(prev.id) : null]).then(setFull)
  }, [c.id, prev?.id])

  const title = {
    baseline: 'Already existed when mnemo started watching',
    added: 'Created',
    modified: `${c.paths?.length ?? 0} field${c.paths?.length === 1 ? '' : 's'} changed`,
    deleted: 'Deleted',
  }[c.type]

  return (
    <>
      <div className="version-head">
        <div>
          <strong>{title}</strong> · {fmtTime(c.time)} by <Author c={c} />
          {c.type === 'deleted' && c.author === '' && <p className="hint">Deletions carry no author until audit attribution is enabled.</p>}
        </div>
        <div className="tabs">
          <button className={mode === 'diff' ? 'on' : ''} onClick={() => setMode('diff')}>
            Changes
          </button>
          <button className={mode === 'yaml' ? 'on' : ''} onClick={() => setMode('yaml')} disabled={c.type === 'deleted'}>
            Full YAML
          </button>
        </div>
      </div>
      {!full ? (
        <p>Loading…</p>
      ) : mode === 'diff' ? (
        <Diff before={c.type === 'baseline' ? '' : yaml(full[1])} after={yaml(full[0])} />
      ) : (
        <pre className="code">{yaml(full[0])}</pre>
      )}
    </>
  )
}

function AtTime({ ref_, at }: { ref_: Ref; at: string }) {
  const [state, setState] = useState<Change | null | undefined>(undefined)
  const [error, setError] = useState('')
  useEffect(() => {
    setState(undefined)
    api.at(ref_, at).then(setState, (e) => setError(e.message))
  }, [at])

  if (error) return <p className="error">{error}</p>
  if (state === undefined) return <p>Loading…</p>
  if (state === null) return <p className="empty">This object did not exist at {fmtTime(at)}.</p>
  return (
    <>
      <div className="version-head">
        <div>
          <strong>State at {fmtTime(at)}</strong> · last changed {fmtTime(state.time)} by <Author c={state} />
        </div>
      </div>
      <pre className="code">{yaml(state)}</pre>
    </>
  )
}
