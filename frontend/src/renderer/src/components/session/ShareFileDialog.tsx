import { useQuery } from '@tanstack/react-query'
import { Button } from '@/components/ui/Button'
import { Modal } from '@/components/ui/Modal'
import { useToast } from '@/components/ui/toast'
import { apiBaseUrl, apiFetch } from '@/lib/api/client'
import { readAPIResponse } from '@/lib/api/response'
import { sessionFilePath } from '@/lib/api/sessions'

async function svgAsPNG(file: File): Promise<File> {
  const url = URL.createObjectURL(file)
  const canvas = document.createElement('canvas')
  try {
    const image = new Image()
    image.src = url
    await image.decode()
    const scale = Math.min(2, 4096 / Math.max(image.naturalWidth, image.naturalHeight))
    canvas.width = Math.max(1, Math.round(image.naturalWidth * scale))
    canvas.height = Math.max(1, Math.round(image.naturalHeight * scale))
    const context = canvas.getContext('2d')
    if (!context) throw new Error('Canvas unavailable')
    context.drawImage(image, 0, 0, canvas.width, canvas.height)
    const blob = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, 'image/png'))
    if (!blob) throw new Error('Could not encode PNG')
    return new File([blob], file.name.replace(/\.svg$/i, '.png'), { type: blob.type })
  } finally {
    canvas.width = canvas.height = 0
    URL.revokeObjectURL(url)
  }
}

export function ShareFileDialog({ sessionId, path, onClose }: { sessionId: string; path: string; onClose: () => void }) {
  const toast = useToast()
  const backend = apiBaseUrl()
  const name = path.split(/[\\/]/).pop() || path
  const svg = /\.svg$/i.test(name)
  const canShare = typeof navigator.share === 'function' && typeof navigator.canShare === 'function'
  const prepared = useQuery({
    queryKey: ['shareFile', backend, sessionId, path],
    enabled: canShare,
    gcTime: 0,
    staleTime: Infinity,
    retry: false,
    queryFn: async ({ signal }) => {
      const response = await apiFetch(sessionFilePath(sessionId, path), { signal }, backend)
      if (!response.ok) await readAPIResponse(response)
      const blob = await response.blob()
      const original = new File([blob], name, { type: blob.type })
      const png = svg ? await svgAsPNG(original).catch(() => null) : null
      return { original, png }
    },
  })
  const { original, png } = prepared.data ?? {}

  const share = async (file: File) => {
    const data = { files: [file] }
    if (!navigator.canShare(data)) {
      toast('This file cannot be shared. Open Jaz in Safari to download it.', 'danger')
      return
    }
    onClose()
    try {
      await navigator.share(data)
    } catch (error) {
      if (!(error instanceof DOMException && error.name === 'AbortError')) toast('Could not share file.', 'danger')
    }
  }

  return <Modal open onClose={onClose} title="Share file" size="sm" description={<span className="break-all">{name}</span>}>
    {!canShare ? <p className="text-sm text-ink-2">Open Jaz in Safari to download this file.</p>
      : prepared.isError ? <div className="flex items-center justify-between gap-3">
        <p role="alert" className="text-sm text-danger">Could not read file.</p>
        <Button className="min-h-10" onClick={() => void prepared.refetch()}>Retry</Button>
      </div>
      : !original ? <p role="status" className="text-sm text-ink-2">Preparing file…</p>
      : <div className="flex flex-wrap items-center gap-2">
        <Button variant={svg ? 'secondary' : 'primary'} className="min-h-10" onClick={() => void share(original)}>{svg ? 'Share original SVG' : 'Share file'}</Button>
        {png ? <Button variant="primary" className="min-h-10" onClick={() => void share(png)}>Share as PNG</Button> : null}
        {svg && !png ? <p role="status" className="text-sm text-danger">Could not prepare PNG.</p> : null}
      </div>}
  </Modal>
}
