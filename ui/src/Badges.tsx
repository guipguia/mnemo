import type { Change } from './api'

export const TypeBadge = ({ c }: { c: Change }) => <span className={`badge type-${c.type}`}>{c.type}</span>

export const Author = ({ c }: { c: Change }) => (
  <span className="author">
    {c.author && `${c.author} `}
    <span className={`badge source-${c.source}`}>{c.source}</span>
    {c.drift && (
      <span className="badge drift" title="Manual change to an object managed by GitOps">
        drift
      </span>
    )}
  </span>
)

export const Paths = ({ paths, max = 3 }: { paths?: string[]; max?: number }) =>
  paths?.length ? (
    <span className="paths" title={paths.join('\n')}>
      {paths.slice(0, max).join('  ')}
      {paths.length > max && <span className="more"> +{paths.length - max}</span>}
    </span>
  ) : null
