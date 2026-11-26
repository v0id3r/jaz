import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { StrictMode, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { FileReaderLinkProvider, MessageMarkdown } from '@/components/session/MessageMarkdown'
import { apiBaseUrl, setApiBaseUrl } from '@/lib/api/client'
import { FileReaderPanel } from '@/components/session/FileReaderPanel'
import type { FileReference } from '@shared/fileReader'
import { LOCAL_BACKEND_URL } from '@shared/localBackend'

export async function exerciseImages() {
  const remoteURL = new URL('/remote-chart.svg', location.origin)
  remoteURL.hostname = 'localhost'
  const host = document.createElement('div')
  host.style.cssText = 'position:fixed;inset:0;background:var(--color-bg);z-index:9999'
  document.body.append(host)
  const root = createRoot(host)
  const client = new QueryClient()
  const previousBackend = apiBaseUrl()
  setApiBaseUrl(LOCAL_BACKEND_URL)
  function View({ count, suffix }: { count: number; suffix: string }) {
    const [file, setFile] = useState<FileReference | null>(null)
    return <FileReaderLinkProvider sessionId="tabs" onOpen={setFile}>
      <MessageMarkdown text={`![Remote chart](${remoteURL.href})\n\n[Remote page](${remoteURL.origin}/)\n\n[PNG file](icon-1024.png)\n\n[SVG file](cache-chart.svg)\n\n[Generated image](generated.png)`} />
      {Array.from({ length: count }, (_, index) => (
        <MessageMarkdown key={index} text={`![Chart](cache-chart.svg)${suffix}`} />
      ))}
      {file ? <div className="absolute inset-y-0 right-0 w-[55%] bg-bg" data-image-reader>
        <FileReaderPanel sessionId="tabs" fileRef={file} visible onOpenFile={setFile} onClose={() => setFile(null)} />
      </div> : null}
    </FileReaderLinkProvider>
  }
  const render = (count: number, suffix = '') => root.render(
    <StrictMode>
      <QueryClientProvider client={client}>
        <View count={count} suffix={suffix} />
      </QueryClientProvider>
    </StrictMode>,
  )
  const until = async (check: () => boolean) => {
    const deadline = performance.now() + 5000
    while (!check()) {
      if (performance.now() > deadline) {
        throw new Error('Inline image did not load its current file contents')
      }
      await new Promise(resolve => setTimeout(resolve, 20))
    }
  }
  const images = () => [...host.querySelectorAll<HTMLImageElement>('img[alt="Chart"]')]
  try {
    render(1)
    await until(() => images()[0]?.naturalWidth === 12)
    await until(() => [...host.querySelectorAll<HTMLImageElement>('img')].every(image => image.complete && image.naturalWidth > 0))
    const requests = await fetch('/resource-identities').then(response => response.json()) as Record<string, Record<string, string>>
    for (const path of ['/remote-chart.svg', '/favicon.ico']) {
      if (!requests[path] || requests[path].referer || /jaz|electron/i.test(requests[path]['user-agent'])) {
        throw new Error('A remote image or favicon received app identity or referrer metadata: ' + JSON.stringify(requests))
      }
    }
    const originalSource = images()[0].src
    const response = await fetch('/image-fixture', { method: 'POST' })
    if (!response.ok) {
      throw new Error('Could not overwrite the image fixture')
    }
    render(2)
    await until(() => images()[1]?.naturalWidth === 24)
    const updatedSource = images()[1].src
    render(2, '\n\nStreamed text\n\n![Latest](cache-chart.svg)')
    await until(() => host.textContent?.includes('Streamed text') === true)
    await until(() => {
      const latest = [...host.querySelectorAll<HTMLImageElement>('img[alt="Latest"]')]
      return latest.length === 2 && latest.every(image => image.naturalWidth === 24)
    })
    if (images()[0].src !== originalSource || images()[1].src !== updatedSource) {
      throw new Error('Text streaming changed the image request identity')
    }
    for (const [label, width] of [['PNG file', 1024], ['SVG file', 24]] as const) {
      const link = [...host.querySelectorAll<HTMLElement>('[role="link"]')].find((node) => node.textContent === label)
      link?.click()
      await until(() => host.querySelector<HTMLImageElement>('[data-image-reader] img')?.naturalWidth === width)
      const reader = host.querySelector<HTMLElement>('[data-image-reader]')!
      const image = reader.querySelector('img')!.getBoundingClientRect()
      if (image.height > reader.clientHeight || image.width > reader.clientWidth || reader.textContent?.includes('Binary file')) throw new Error('Image file did not fit its side tab')
      reader.querySelector<HTMLButtonElement>('[aria-label="File actions"]')!.click()
      await until(() => Boolean(document.querySelector('[role="menu"][aria-label="File actions"]')))
      const menu = document.querySelector('[role="menu"][aria-label="File actions"]')!
      if (menu.textContent?.includes('Open folder')) throw new Error('A browser without native file access offers Open folder')
      if (label === 'PNG file') {
        await new Promise((resolve) => setTimeout(resolve, 180))
        await window.smoke.capture('image-file-tab')
        const download = [...menu.querySelectorAll<HTMLButtonElement>('button')].find((node) => node.textContent === 'Download file')!
        download.click()
        if (await window.smoke.fileDownload() !== 'icon-1024.png') throw new Error('Download did not retain the image filename and contents')
      } else {
        reader.querySelector<HTMLButtonElement>('[aria-label="File actions"]')!.click()
      }
      await until(() => !document.querySelector('[role="menu"][aria-label="File actions"]'))
    }
    await exerciseInstalledIOSSharing(host, until)
    const generated = [...host.querySelectorAll<HTMLElement>('[role="link"]')].find((node) => node.textContent === 'Generated image')!
    generated.click()
    await until(() => Boolean(host.querySelector('[data-image-reader]')?.textContent?.includes('Couldn’t open this image')))
    if (!(await fetch('/generated-image-fixture', { method: 'POST' })).ok) throw new Error('Could not finish generating the image fixture')
    generated.click()
    const retry = [...host.querySelectorAll<HTMLButtonElement>('[data-image-reader] button')].find((node) => node.textContent === 'Retry')
    if (!retry) throw new Error('A failed image has no recovery action')
    retry.click()
    await until(() => host.querySelector<HTMLImageElement>('[data-image-reader] img')?.naturalWidth === 1024)
    host.querySelectorAll<HTMLButtonElement>('button[aria-label="Expand image: Chart"]')[1].click()
    await until(() => document.querySelector<HTMLImageElement>('[role="dialog"] img')?.naturalWidth === 24)
  } finally {
    root.unmount()
    client.clear()
    host.remove()
    setApiBaseUrl(previousBackend)
  }
}

async function exerciseInstalledIOSSharing(host: HTMLElement, until: (check: () => boolean) => Promise<void>) {
  const matchMedia = window.matchMedia
  const shared: File[] = []
  const properties = {
    platform: 'iPhone',
    canShare: ({ files }: ShareData) => files?.length === 1,
    share: async ({ files }: ShareData) => {
      shared.push(...files!)
      throw new DOMException('Share sheet dismissed', 'AbortError')
    },
  }
  for (const [key, value] of Object.entries(properties)) {
    Object.defineProperty(navigator, key, { configurable: true, value })
  }
  window.matchMedia = (query) => matchMedia.call(window, query === '(display-mode: standalone)' ? 'all' : query)
  const reader = () => host.querySelector<HTMLElement>('[data-image-reader]')!
  const dialog = () => document.querySelector<HTMLElement>('[role="dialog"][aria-label="Share file"]')
  const button = (root: ParentNode | null, label: string) => [...(root?.querySelectorAll('button') ?? [])].find(node => node.textContent === label)
  const openShare = async () => {
    reader().querySelector<HTMLButtonElement>('[aria-label="File actions"]')!.click()
    const action = () => button(document.querySelector('[role="menu"][aria-label="File actions"]'), 'Share file…')
    await until(() => Boolean(action()))
    action()!.click()
    await until(() => Boolean(dialog()))
  }
  const share = async (label: string) => {
    const preview = reader().querySelector('img')
    const url = location.href
    await openShare()
    await until(() => Boolean(button(dialog(), label)))
    button(dialog(), label)!.click()
    await until(() => !dialog())
    if (reader().querySelector('img') !== preview || location.href !== url) {
      throw new Error('Dismissing the system share sheet left the file preview')
    }
  }

  try {
    await openShare()
    dialog()!.querySelector<HTMLButtonElement>('[aria-label="Close"]')!.click()
    await until(() => !dialog())
    if (shared.length) throw new Error('Closing the export dialog shared a file')

    await share('Share original SVG')
    await share('Share as PNG')
    const [svg, png] = shared
    if (svg?.name !== 'cache-chart.svg' || svg.type !== 'image/svg+xml' || !(await svg.text()).includes('fill="blue"')) {
      throw new Error('The share sheet did not receive the current original SVG')
    }
    if (png?.name !== 'cache-chart.png' || png.type !== 'image/png') throw new Error('The SVG export did not produce a named PNG')
    const decoded = await createImageBitmap(png)
    const canvas = new OffscreenCanvas(decoded.width, decoded.height)
    const context = canvas.getContext('2d')!
    context.drawImage(decoded, 0, 0)
    decoded.close()
    if (canvas.width !== canvas.height * 2 || context.getImageData(1, 1, 1, 1).data.join(',') !== '0,0,255,255') {
      throw new Error('PNG export lost the SVG aspect ratio or contents')
    }

    const link = [...host.querySelectorAll<HTMLElement>('[role="link"]')].find(node => node.textContent === 'PNG file')!
    link.click()
    await until(() => reader().querySelector('img')?.naturalWidth === 1024)
    await share('Share file')
    const nativePNG = shared[2]
    if (nativePNG?.name !== 'icon-1024.png' || nativePNG.type !== 'image/png') throw new Error('The share sheet did not receive a PNG')
    const expected = new Uint8Array(await fetch('/icon-1024.png').then(response => response.arrayBuffer()))
    const actual = new Uint8Array(await nativePNG.arrayBuffer())
    if (actual.length !== expected.length || actual.some((byte, index) => byte !== expected[index])) {
      throw new Error('The share sheet did not receive the original PNG bytes')
    }
  } finally {
    window.matchMedia = matchMedia
    for (const key of Object.keys(properties)) Reflect.deleteProperty(navigator, key)
  }
}
