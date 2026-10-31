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
  commit?: Commit // GitOps changes only, from GET changes/{id}
}

// The Argo CD Application or Flux Kustomization sync that made a change.
export type Commit = { kind: string; namespace: string; name: string; revision: string; repo?: string; url?: string }

// A Kubernetes Event about the object (Ref) or, for workloads, one of its ReplicaSets or Pods.
export type KubeEvent = Ref & {
  uid: string
  type: 'Normal' | 'Warning'
  reason: string
  message: string
  source: string
  count: number
  firstTime: string
  lastTime: string
}

export type Cluster = { name: string; uid: string; lastSeen: string }

export type Me = {
  mode: 'none' | 'oidc' | 'proxy'
  user: { name: string; groups?: string[] }
  access: { All: boolean; Scopes: { clusters?: string[]; namespaces?: string[] }[] | null }
}

type Params = Record<string, string | undefined>

// An expired OIDC session: go through login again and come back to the same screen.
const relogin = () => location.assign(`/auth/login?rd=${encodeURIComponent(location.pathname + location.search)}`)
let mode: Me['mode'] = 'none'


async function get<T>(path: string, params: Params = {}): Promise<T> {
  const q = new URLSearchParams(Object.entries(params).filter((e): e is [string, string] => !!e[1]))
  const r = await fetch(`/api/v1/${path}${q.size ? `?${q}` : ''}`)
  if (r.status === 401 && mode === 'oidc') relogin()
  if (!r.ok) throw new Error(`${r.status}: ${(await r.text()).trim()}`)
  return r.json()
}

export async function whoami(): Promise<Me> {
  const r = await fetch('/auth/me')
  if (!r.ok) throw new Error(`${r.status}: ${(await r.text()).trim()}`)
  const me: Me = await r.json()
  mode = me.mode
  return me
}

export const api = {
  clusters: () => get<Cluster[]>('clusters'),
  changes: (filter: Params) => get<Change[]>('changes', filter),
  change: (id: number) => get<Change>(`changes/${id}`),
  history: (ref: Ref) => get<Change[]>('history', { ...ref, limit: '1000' }),
  events: (ref: Ref) => get<KubeEvent[]>('events', { ...ref, limit: '1000' }),
  // null when the object did not exist at that time.
  at: (ref: Ref, time: string) =>
    get<Change>('at', { ...ref, time }).catch((e: Error) => (e.message.startsWith('404') ? null : Promise.reject(e))),
}
