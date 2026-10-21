import { diffLines } from 'diff'
import { useMemo, useState, type ReactNode } from 'react'

type Line = { kind: ' ' | '+' | '-'; text: string }

const CONTEXT = 3

function toLines(before: string, after: string): Line[] {
  const out: Line[] = []
  for (const part of diffLines(before, after)) {
    if (!part.value) continue
    const kind = part.added ? '+' : part.removed ? '-' : ' '
    for (const text of part.value.replace(/\n$/, '').split('\n')) out.push({ kind, text })
  }
  return out
}

// Line diff of two YAML documents; long unchanged runs collapse to a few lines of context.
export function Diff({ before, after }: { before: string; after: string }) {
  const [expanded, setExpanded] = useState(false)
  const lines = useMemo(() => toLines(before, after), [before, after])
  const row = (l: Line, i: number) => (
    <div key={i} className={`line ${l.kind === '+' ? 'add' : l.kind === '-' ? 'del' : ''}`}>
      <span className="sign">{l.kind}</span>
      {l.text}
    </div>
  )

  const rows: ReactNode[] = []
  for (let i = 0; i < lines.length; ) {
    if (lines[i].kind !== ' ' || expanded) {
      rows.push(row(lines[i], i++))
      continue
    }
    let j = i
    while (j < lines.length && lines[j].kind === ' ') j++
    const head = i === 0 ? 0 : CONTEXT
    const tail = j === lines.length ? 0 : CONTEXT
    if (j - i > head + tail + 1) {
      for (let k = i; k < i + head; k++) rows.push(row(lines[k], k))
      rows.push(
        <button key={`gap${i}`} className="gap" onClick={() => setExpanded(true)}>
          ⋯ {j - i - head - tail} unchanged lines
        </button>,
      )
      for (let k = j - tail; k < j; k++) rows.push(row(lines[k], k))
    } else {
      for (let k = i; k < j; k++) rows.push(row(lines[k], k))
    }
    i = j
  }
  return <pre className="code diff">{rows}</pre>
}
