import { join } from 'node:path'
import { rendererURL } from '@main/rendererScheme'
import { BrowserWindow, ipcMain, screen, type IpcMainEvent } from 'electron'
import type { VoiceCommand, VoiceOverlayState } from '@shared/voice'

export function attachVoiceOverlay(owner: BrowserWindow): void {
  let overlay: BrowserWindow | null = null
  let state: VoiceOverlayState | null = null
  let loaded = false
  let dragOrigin: { x: number; y: number; windowX: number; windowY: number } | null = null
  // Where the pill sits; the transcript below it may push the window up.
  let top = 0
  const throttled = owner.webContents.getBackgroundThrottling()

  const present = () => {
    if (!state || (state.docked && owner.isFocused())) {
      overlay?.hide()
      return
    }
    if (!overlay) {
      const { workArea } = screen.getDisplayNearestPoint(screen.getCursorScreenPoint())
      const size = { width: 380, height: 78 }
      top = workArea.y + workArea.height - size.height - 20
      overlay = new BrowserWindow({
        title: 'Jaz Voice',
        ...size,
        x: workArea.x + workArea.width - size.width - 20,
        y: top,
        show: false,
        frame: false,
        transparent: true,
        resizable: false,
        fullscreenable: false,
        minimizable: false,
        maximizable: false,
        skipTaskbar: true,
        hasShadow: false,
        backgroundColor: '#00000000',
        ...(process.platform === 'darwin' ? { type: 'panel' as const } : {}),
        webPreferences: {
          preload: join(__dirname, '../preload/index.js'),
          contextIsolation: true,
          nodeIntegration: false,
          sandbox: false,
          backgroundThrottling: false,
          additionalArguments: ['--jaz-voice-window'],
        },
      })
      overlay.setAlwaysOnTop(true, 'floating')
      overlay.setVisibleOnAllWorkspaces(true, { visibleOnFullScreen: true, skipTransformProcessType: true })
      overlay.once('closed', () => {
        overlay = null
        loaded = false
        dragOrigin = null
      })
      void overlay.loadURL(rendererURL())
    }
    if (loaded && !overlay.isVisible()) {
      overlay.showInactive()
    }
  }

  const update = (next: VoiceOverlayState | null) => {
    if (!next) {
      dragOrigin = null
    }
    if (Boolean(next) !== Boolean(state)) {
      owner.webContents.setBackgroundThrottling(next ? false : throttled)
    }
    state = next
    overlay?.webContents.send('jaz:voice:state', state)
    present()
  }
  const publish = (event: IpcMainEvent, next: VoiceOverlayState | null) => {
    if (event.sender === owner.webContents && event.senderFrame === owner.webContents.mainFrame) {
      update(next)
    }
  }
  // The overlay, when the event comes from its own page.
  const sentByOverlay = (event: IpcMainEvent) =>
    overlay && event.sender === overlay.webContents && event.senderFrame === overlay.webContents.mainFrame ? overlay : null
  const ready = (event: IpcMainEvent) => {
    if (!sentByOverlay(event)) {
      return
    }
    loaded = true
    event.sender.send('jaz:voice:state', state)
    present()
  }
  const command = (event: IpcMainEvent, action: VoiceCommand) => {
    if (!state || !sentByOverlay(event)) {
      return
    }
    if (action === 'return') {
      owner.show()
      owner.focus()
      owner.webContents.send('jaz:open-route', state.home)
    } else if (['mute', 'muteSpeaker', 'exit', 'reconnect'].includes(action)) {
      owner.webContents.send('jaz:voice:command', action)
    }
  }
  const clear = () => update(null)
  const drag = (event: IpcMainEvent, point: { x: number; y: number } | null) => {
    const target = sentByOverlay(event)
    if (!state || !target) {
      return
    }
    if (!point) {
      dragOrigin = null
      return
    }
    if (!Number.isFinite(point.x) || !Number.isFinite(point.y)) {
      return
    }
    if (!dragOrigin) {
      const [windowX, windowY] = target.getPosition()
      dragOrigin = { ...point, windowX, windowY }
    } else {
      // Electron rejects -0 as an integer coordinate; adding 0 normalizes it.
      top = Math.round(dragOrigin.windowY + point.y - dragOrigin.y) + 0
      target.setPosition(Math.round(dragOrigin.windowX + point.x - dragOrigin.x) + 0, top)
    }
  }
  const resize = (event: IpcMainEvent, height: number) => {
    const target = sentByOverlay(event)
    if (!target || !Number.isFinite(height) || height <= 0) {
      return
    }
    const bounds = target.getBounds()
    const { workArea } = screen.getDisplayMatching(bounds)
    const next = Math.round(height)
    target.setBounds({ ...bounds, y: Math.max(workArea.y, Math.min(top, workArea.y + workArea.height - next)), height: next })
  }
  const navigate = (_event: Electron.Event, _url: string, sameDocument: boolean, mainFrame: boolean) => {
    if (mainFrame && !sameDocument) {
      clear()
    }
  }
  ipcMain.on('jaz:voice:publish', publish)
  ipcMain.on('jaz:voice:ready', ready)
  ipcMain.on('jaz:voice:command', command)
  ipcMain.on('jaz:voice:drag', drag)
  ipcMain.on('jaz:voice:resize', resize)
  owner.on('focus', present)
  owner.on('blur', present)
  owner.on('hide', present)
  owner.webContents.on('did-start-navigation', navigate)
  owner.webContents.on('render-process-gone', clear)
  owner.once('closed', () => {
    ipcMain.removeListener('jaz:voice:publish', publish)
    ipcMain.removeListener('jaz:voice:ready', ready)
    ipcMain.removeListener('jaz:voice:command', command)
    ipcMain.removeListener('jaz:voice:drag', drag)
    ipcMain.removeListener('jaz:voice:resize', resize)
    overlay?.destroy()
  })
}
