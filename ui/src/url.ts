import { useEffect, useState } from 'react'

// All view state lives in the query string, so any screen can be pasted into an incident channel.
export type Nav = (params: Record<string, string | undefined>) => void

export function useURL(): [URLSearchParams, Nav] {
  const [search, setSearch] = useState(location.search)
  useEffect(() => {
    const onPop = () => setSearch(location.search)
    addEventListener('popstate', onPop)
    return () => removeEventListener('popstate', onPop)
  }, [])
  const nav: Nav = (params) => {
    const q = new URLSearchParams(Object.entries(params).filter((e): e is [string, string] => !!e[1]))
    const next = q.size ? `?${q}` : location.pathname
    history.pushState(null, '', next)
    setSearch(location.search)
  }
  return [new URLSearchParams(search), nav]
}

export const fmtTime = (iso: string) => {
  const d = new Date(iso)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

export const ago = (iso: string) => {
  const s = Math.round((Date.now() - new Date(iso).getTime()) / 1000)
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}

// <input type="datetime-local"> works in local time without a zone; the API wants RFC 3339.
export const fromLocalInput = (v: string) => (v ? new Date(v).toISOString().replace(/\.\d{3}Z$/, 'Z') : undefined)
export const toLocalInput = (iso: string | null) => (iso ? fmtTime(iso).replace(' ', 'T') : '')
