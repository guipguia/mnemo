export type ChangeType = 'baseline' | 'added' | 'modified' | 'deleted'

export type Ref = { cluster: string; group: string; kind: string; namespace: string; name: string }

export type Change = Ref & {
  id: number
  version: string
  uid: string
  resourceVersion: string
  time: string
  type: ChangeType
  author: string
  source: 'gitops' | 'controller' | 'manual' | 'unknown'
  drift: boolean
  paths?: string[]
  object?: unknown
}

export type Cluster = { name: string; uid: string; lastSeen: string }

type Params = Record<string, string | undefined>

async function get<T>(path: string, params: Params = {}): Promise<T> {
  const q = new URLSearchParams(Object.entries(params).filter((e): e is [string, string] => !!e[1]))
  const r = await fetch(`/api/v1/${path}${q.size ? `?${q}` : ''}`)
  if (!r.ok) throw new Error(`${r.status}: ${(await r.text()).trim()}`)
  return r.json()
}

export const api = {
  clusters: () => get<Cluster[]>('clusters'),
  changes: (filter: Params) => get<Change[]>('changes', filter),
  change: (id: number) => get<Change>(`changes/${id}`),
  history: (ref: Ref) => get<Change[]>('history', { ...ref, limit: '1000' }),
  // null when the object did not exist at that time.
  at: (ref: Ref, time: string) =>
    get<Change>('at', { ...ref, time }).catch((e: Error) => (e.message.startsWith('404') ? null : Promise.reject(e))),
}
