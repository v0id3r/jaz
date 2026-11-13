import { afterAll, expect, mock, test } from 'bun:test'
import { EventEmitter } from 'node:events'

const ipcMain = new EventEmitter()
const windows = []
class Window extends EventEmitter {
  visible = false
  focused = true
  throttled = true
  sent = []
  webContents = Object.assign(new EventEmitter(), {
    mainFrame: {},
    getBackgroundThrottling: () => this.throttled,
    setBackgroundThrottling: (value) => {
      this.throttled = value
    },
    send: (...message) => this.sent.push(message),
  })

  constructor({ x = 100, y = 200, height = 78 } = {}) {
    super()
    this.position = [x, y]
    this.height = height
    windows.push(this)
  }
  isFocused() {
    return this.focused
  }
  isVisible() {
    return this.visible
  }
  hide() {
    this.visible = false
  }
  show() {
    this.visible = true
  }
  showInactive() {
    this.visible = true
    this.focused = false
  }
  focus() {
    this.focused = true
    this.emit('focus')
  }
  blur() {
    this.focused = false
    this.emit('blur')
  }
  destroy() {
    this.emit('closed')
  }
  getPosition() {
    return this.position
  }
  setPosition(x, y) {
    for (const coordinate of [x, y]) {
      if (!Number.isInteger(coordinate) || Object.is(coordinate, -0)) {
        throw new TypeError('Window coordinates must be native integers')
      }
    }
    this.position = [x, y]
  }
  getBounds() {
    return { x: this.position[0], y: this.position[1], width: 380, height: this.height }
  }
  setBounds(bounds) {
    this.position = [bounds.x, bounds.y]
    this.height = bounds.height
  }
  setAlwaysOnTop() {}
  setVisibleOnAllWorkspaces() {}
  loadURL() {}
}

mock.module('electron', () => ({
  BrowserWindow: Window,
  ipcMain,
  screen: {
    getCursorScreenPoint: () => ({ x: 0, y: 0 }),
    getDisplayNearestPoint: () => ({ workArea: { x: 0, y: 0, width: 500, height: 298 } }),
    getDisplayMatching: () => ({ workArea: { x: 0, y: 25, width: 1280, height: 775 } }),
  },
}))
mock.module('@main/rendererScheme', () => ({ rendererURL: () => 'app://jaz/index.html' }))
const { attachVoiceOverlay } = await import('./voiceOverlay')
afterAll(() => mock.restore())
const sender = (window) => ({ sender: window.webContents, senderFrame: window.webContents.mainFrame })
const state = { face: null, home: '/bots/original', phase: 'listening', docked: true, muted: false, speakerMuted: false, activity: null, error: '', level: 0, outputLevel: 0 }

test('one overlay follows chat selection and owner focus without activating itself', () => {
  const owner = new Window()
  attachVoiceOverlay(owner)
  try {
    const count = windows.length
    ipcMain.emit('jaz:voice:publish', sender(owner), state)
    expect(windows).toHaveLength(count)
    expect(owner.throttled).toBe(false)

    ipcMain.emit('jaz:voice:publish', sender(owner), { ...state, docked: false })
    const overlay = windows.at(-1)
    expect(overlay.visible).toBe(false)
    ipcMain.emit('jaz:voice:ready', sender(overlay))
    expect(overlay.visible).toBe(true)
    expect(overlay.focused).toBe(false)
    owner.focus()
    expect(overlay.visible).toBe(true)

    ipcMain.emit('jaz:voice:publish', sender(owner), state)
    expect(overlay.visible).toBe(false)
    owner.blur()
    expect(overlay.visible).toBe(true)
    expect(windows).toHaveLength(count + 1)

    ipcMain.emit('jaz:voice:command', sender(overlay), 'return')
    expect(owner.sent.at(-1)).toEqual(['jaz:open-route', '/bots/original'])
    expect(overlay.visible).toBe(false)
    owner.blur()
    ipcMain.emit('jaz:voice:command', sender(overlay), 'muteSpeaker')
    expect(owner.sent.at(-1)).toEqual(['jaz:voice:command', 'muteSpeaker'])
    ipcMain.emit('jaz:voice:publish', sender(owner), null)
    expect(overlay.visible).toBe(false)
    expect(owner.throttled).toBe(true)
  } finally {
    owner.destroy()
  }
  expect(ipcMain.eventNames()).toEqual([])
})

test('only the owning frames control a call; document loss clears the overlay', () => {
  const owner = new Window()
  const foreign = new Window()
  attachVoiceOverlay(owner)
  try {
    const count = windows.length
    ipcMain.emit('jaz:voice:publish', sender(foreign), { ...state, docked: false })
    ipcMain.emit('jaz:voice:publish', { ...sender(owner), senderFrame: {} }, { ...state, docked: false })
    expect(windows).toHaveLength(count)
    ipcMain.emit('jaz:voice:publish', sender(owner), { ...state, docked: false })
    const overlay = windows.at(-1)
    ipcMain.emit('jaz:voice:ready', sender(foreign))
    expect(overlay.visible).toBe(false)
    ipcMain.emit('jaz:voice:ready', sender(overlay))
    ipcMain.emit('jaz:voice:command', sender(foreign), 'exit')
    ipcMain.emit('jaz:voice:command', { ...sender(overlay), senderFrame: {} }, 'exit')
    expect(owner.sent).toEqual([])
    ipcMain.emit('jaz:voice:drag', sender(foreign), { x: 30, y: 50 })
    ipcMain.emit('jaz:voice:drag', sender(overlay), { x: 10, y: 10 })
    ipcMain.emit('jaz:voice:drag', sender(overlay), { x: 35, y: 0 })
    expect(overlay.position).toEqual([125, 190])
    ipcMain.emit('jaz:voice:drag', sender(overlay), null)
    ipcMain.emit('jaz:voice:drag', sender(overlay), { x: 100, y: 100 })
    ipcMain.emit('jaz:voice:drag', sender(overlay), { x: 90, y: 115 })
    expect(overlay.position).toEqual([115, 205])
    expect(owner.sent).toEqual([])

    owner.webContents.emit('did-start-navigation', {}, '', true, true)
    owner.webContents.emit('did-start-navigation', {}, '', false, false)
    expect(overlay.visible).toBe(true)
    owner.webContents.emit('did-start-navigation', {}, '', false, true)
    expect(overlay.visible).toBe(false)
    expect(owner.throttled).toBe(true)
    ipcMain.emit('jaz:voice:command', sender(overlay), 'exit')
    expect(owner.sent).toEqual([])
  } finally {
    owner.destroy()
  }
})

test('fractional drag coordinates cross both screen origins without passing negative zero to Electron', () => {
  const owner = new Window()
  attachVoiceOverlay(owner)
  try {
    ipcMain.emit('jaz:voice:publish', sender(owner), { ...state, docked: false })
    const overlay = windows.at(-1)
    ipcMain.emit('jaz:voice:drag', sender(overlay), { x: 100.4, y: 200.4 })
    ipcMain.emit('jaz:voice:drag', sender(overlay), { x: 10, y: 0.2 })
    expect(overlay.position).toEqual([10, 0])
    ipcMain.emit('jaz:voice:drag', sender(overlay), { x: 0.2, y: 10 })
    expect(overlay.position).toEqual([0, 10])
    ipcMain.emit('jaz:voice:drag', sender(overlay), { x: -10, y: -20 })
    expect(overlay.position).toEqual([-10, -20])
    ipcMain.emit('jaz:voice:drag', sender(overlay), null)
  } finally {
    owner.destroy()
  }
})

test('the transcript grows the overlay down from the pill and stays on screen', () => {
  const owner = new Window()
  attachVoiceOverlay(owner)
  try {
    ipcMain.emit('jaz:voice:publish', sender(owner), { ...state, docked: false })
    const overlay = windows.at(-1)
    ipcMain.emit('jaz:voice:resize', sender(owner), 300)
    ipcMain.emit('jaz:voice:resize', { ...sender(overlay), senderFrame: {} }, 300)
    ipcMain.emit('jaz:voice:resize', sender(overlay), Number.NaN)
    expect(overlay.getBounds()).toEqual({ x: 100, y: 200, width: 380, height: 78 })

    ipcMain.emit('jaz:voice:resize', sender(overlay), 177.6)
    expect(overlay.getBounds()).toEqual({ x: 100, y: 200, width: 380, height: 178 })
    ipcMain.emit('jaz:voice:resize', sender(overlay), 700)
    expect(overlay.getBounds()).toEqual({ x: 100, y: 100, width: 380, height: 700 })
    ipcMain.emit('jaz:voice:resize', sender(overlay), 900)
    expect(overlay.position).toEqual([100, 25])
    ipcMain.emit('jaz:voice:resize', sender(overlay), 78)
    expect(overlay.getBounds()).toEqual({ x: 100, y: 200, width: 380, height: 78 })

    ipcMain.emit('jaz:voice:drag', sender(overlay), { x: 0, y: 500 })
    ipcMain.emit('jaz:voice:drag', sender(overlay), { x: 10, y: 450 })
    ipcMain.emit('jaz:voice:drag', sender(overlay), null)
    ipcMain.emit('jaz:voice:resize', sender(overlay), 300)
    expect(overlay.getBounds()).toEqual({ x: 110, y: 150, width: 380, height: 300 })
  } finally {
    owner.destroy()
  }
  expect(ipcMain.eventNames()).toEqual([])
})
