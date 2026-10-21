// Records the README demo GIF from a live hub that has seen demo/incident.sh.
//
//   cd demo && npm i --no-save playwright && npx playwright install chromium
//   node record.mjs http://127.0.0.1:8080 ../docs/demo      # writes demo-light.gif, demo-dark.gif
//
// Needs ffmpeg. Everything shown is the real UI and real data; the script only adds a
// visible cursor and captions.
import { chromium } from 'playwright'
import { execFileSync } from 'node:child_process'
import { mkdtempSync, readdirSync, renameSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const [base = 'http://127.0.0.1:8080', out = 'demo'] = process.argv.slice(2)
const size = { width: 1280, height: 720 }

// Injected into every page: a cursor that glides to targets, and a caption bar.
const overlay = () => {
  addEventListener('DOMContentLoaded', () => {
    const cursor = document.createElement('div')
    cursor.id = 'demo-cursor'
    cursor.innerHTML =
      '<svg width="24" height="24" viewBox="0 0 24 24"><path d="M3 2l7 19 2.5-7.5L20 11z" fill="#111" stroke="#fff" stroke-width="1.5"/></svg>'
    Object.assign(cursor.style, {
      position: 'fixed', left: '0', top: '0', zIndex: 99999, pointerEvents: 'none',
      transform: `translate(${window.__cx ?? 640}px, ${window.__cy ?? 600}px)`,
      transition: 'transform 0.7s cubic-bezier(.4,0,.2,1)',
    })
    const caption = document.createElement('div')
    caption.id = 'demo-caption'
    Object.assign(caption.style, {
      position: 'fixed', left: '50%', bottom: '28px', transform: 'translateX(-50%)', zIndex: 99998,
      background: 'rgba(17,18,20,.92)', color: '#fff', padding: '10px 18px', borderRadius: '10px',
      font: '600 17px/1.3 system-ui, sans-serif', pointerEvents: 'none', opacity: '0', transition: 'opacity .3s',
      boxShadow: '0 6px 24px rgba(0,0,0,.25)', border: '1px solid rgba(255,255,255,.22)', whiteSpace: 'nowrap',
    })
    document.body.append(cursor, caption)
  })
}

async function record(scheme) {
  const browser = await chromium.launch()
  const dir = mkdtempSync(join(tmpdir(), 'mnemo-demo-'))
  const ctx = await browser.newContext({
    viewport: size, colorScheme: scheme, timezoneId: 'UTC', bypassCSP: true,
    recordVideo: { dir, size },
  })
  await ctx.addInitScript(overlay)
  const page = await ctx.newPage()
  const t0 = Date.now()

  const pause = (s) => page.waitForTimeout(s * 1000)
  const caption = (text) =>
    page.evaluate((t) => {
      const c = document.getElementById('demo-caption')
      c.textContent = t
      c.style.opacity = t ? '1' : '0'
    }, text)
  const click = async (locator) => {
    const box = await locator.boundingBox()
    const x = box.x + Math.min(box.width / 2, 40), y = box.y + box.height / 2
    await page.evaluate(([x, y]) => {
      window.__cx = x; window.__cy = y
      document.getElementById('demo-cursor').style.transform = `translate(${x}px, ${y}px)`
    }, [x, y])
    await pause(0.8)
    await locator.click()
  }

  // The incident's hand-edit, from the API: "state at" uses a moment just before it.
  const feed = await (await fetch(`${base}/api/v1/changes?namespace=shop&drift=true&kind=ConfigMap`)).json()
  const before = new Date(new Date(feed.at(-1).time).getTime() - 2000).toISOString().slice(0, 19)

  await page.goto(`${base}/?window=1h`)
  await page.waitForSelector('table.feed tbody tr')
  const start = (Date.now() - t0) / 1000

  await caption('Checkout is failing in prod-eu. What changed in the last hour?')
  await pause(3.2)

  await click(page.getByLabel('Drift only'))
  await page.waitForTimeout(600)
  await caption('Drift: hand-edits to objects Argo CD manages')
  await pause(3)

  await click(page.locator('table.feed a', { hasText: 'checkout-config' }).first())
  await page.waitForSelector('pre.diff')
  await caption('Root cause: PAYMENT_TIMEOUT 30s → 3s, by kubectl-patch')
  await pause(3.5)

  const at = page.locator('.timeline input[type=datetime-local]')
  await click(at)
  await at.fill(before)
  await page.waitForSelector('.detail pre.code:not(.diff)')
  await caption('…and exactly how it looked before the incident')
  await pause(3.2)

  await click(page.getByText('← Change feed'))
  await page.waitForSelector('table.feed tbody tr')
  await click(page.locator('table.feed tr', { hasText: 'prod-eu' }).locator('a', { hasText: 'checkout' }).filter({ hasText: 'Deployment' }).first())
  await page.waitForSelector('.timeline li')
  await caption("Every version, attributed: Argo CD's rollouts, then three hand-edits")
  await pause(3.5)

  await caption('mnemo · what changed, and who changed it')
  await pause(2.5)

  await ctx.close()
  await browser.close()
  const webm = join(dir, readdirSync(dir)[0])
  const gif = join(out, `demo-${scheme}.gif`)
  execFileSync('ffmpeg', ['-y', '-loglevel', 'error', '-ss', String(start), '-i', webm, '-vf',
    'fps=10,scale=1000:-1:flags=lanczos,split[a][b];[a]palettegen=stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle',
    '-loop', '0', `${gif}.tmp.gif`])
  renameSync(`${gif}.tmp.gif`, gif)
  console.log('wrote', gif)
}

for (const scheme of ['light', 'dark']) await record(scheme)
