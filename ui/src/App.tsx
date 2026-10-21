import { Feed } from './Feed'
import { ObjectView } from './ObjectView'
import { useURL } from './url'

export function App() {
  const [q, nav] = useURL()
  return (
    <>
      <header>
        <a href="?" onClick={(e) => (e.preventDefault(), nav({}))} className="logo">
          mnemo
        </a>
        <span className="tagline">what changed, and who changed it</span>
      </header>
      <main>{q.get('name') ? <ObjectView q={q} nav={nav} /> : <Feed q={q} nav={nav} />}</main>
    </>
  )
}
