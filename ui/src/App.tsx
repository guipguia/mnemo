import { useEffect, useState } from 'react'
import { whoami, type Me } from './api'
import { Feed } from './Feed'
import { ObjectView } from './ObjectView'
import { useURL } from './url'

export function App() {
  const [q, nav] = useURL()
  const [me, setMe] = useState<Me | null>(null)
  useEffect(() => {
    whoami().then(setMe, () => setMe(null))
  }, [])
  return (
    <>
      <header>
        <a href="?" onClick={(e) => (e.preventDefault(), nav({}))} className="logo">
          mnemo
        </a>
        <span className="tagline">what changed, and who changed it</span>
        {me && me.mode !== 'none' && (
          <span className="me" title={describeAccess(me)}>
            {me.user.name}
            {me.mode === 'oidc' && <a href="/auth/logout">Sign out</a>}
          </span>
        )}
      </header>
      <main>{q.get('name') ? <ObjectView q={q} nav={nav} /> : <Feed q={q} nav={nav} />}</main>
    </>
  )
}

function describeAccess(me: Me) {
  if (me.access.All) return 'Can see everything'
  const scopes = me.access.Scopes ?? []
  if (!scopes.length) return 'No access: ask an admin to add you to the mnemo policy'
  return 'Can see:\n' + scopes.map((s) => `${s.clusters?.join(', ') ?? 'all clusters'} · ${s.namespaces?.join(', ') ?? 'all namespaces'}`).join('\n')
}
