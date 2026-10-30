import { existsSync, rmSync, writeFileSync } from 'node:fs'
import { join, relative } from 'node:path'
import { pathToFileURL } from 'node:url'
import { BrowserWindow, app, net, protocol } from 'electron'
import { RENDERER_DOCUMENT, RENDERER_HOST, RENDERER_SCHEME } from '@main/permissions'

const RENDERER_DIR = join(__dirname, '../renderer')
const MIGRATION_PATH = '/storage-migration'

protocol.registerSchemesAsPrivileged([
  { scheme: RENDERER_SCHEME, privileges: { standard: true, secure: true, supportFetchAPI: true, corsEnabled: true, stream: true } },
])

export function rendererURL(hash = ''): string {
  const dev = process.env['ELECTRON_RENDERER_URL']
  if (dev) return dev + hash
  return hash ? `${RENDERER_DOCUMENT}#${hash}` : RENDERER_DOCUMENT
}

export function serveRenderer(): void {
  protocol.handle(RENDERER_SCHEME, (request) => {
    const url = new URL(request.url)
    if (url.host !== RENDERER_HOST) return new Response(null, { status: 404 })
    if (url.pathname === MIGRATION_PATH) return new Response('', { headers: { 'content-type': 'text/html' } })
    const file = join(RENDERER_DIR, decodeURIComponent(url.pathname))
    if (relative(RENDERER_DIR, file).startsWith('..')) return new Response(null, { status: 404 })
    return net.fetch(pathToFileURL(file).toString())
  })
}

// Copies localStorage that earlier versions kept under the file:// origin,
// without overwriting anything the app:// origin already holds.
export async function migrateFileOriginStorage(): Promise<void> {
  const marker = join(app.getPath('userData'), 'renderer-origin-migrated')
  if (process.env['ELECTRON_RENDERER_URL'] || existsSync(marker)) return
  const blank = join(app.getPath('temp'), 'jaz-storage-migration.html')
  const win = new BrowserWindow({ show: false })
  try {
    writeFileSync(blank, '')
    await win.loadFile(blank)
    const entries: [string, string][] = await win.webContents.executeJavaScript('Object.entries(localStorage)')
    await win.loadURL(`${RENDERER_SCHEME}://${RENDERER_HOST}${MIGRATION_PATH}`)
    await win.webContents.executeJavaScript(
      `for (const [k, v] of ${JSON.stringify(entries)}) if (localStorage.getItem(k) === null) localStorage.setItem(k, v)`,
    )
    writeFileSync(marker, '')
  } catch (error) {
    console.error('renderer storage migration failed', error)
  } finally {
    win.destroy()
    rmSync(blank, { force: true })
  }
}
