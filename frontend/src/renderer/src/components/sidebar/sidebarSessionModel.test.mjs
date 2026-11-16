import { describe, expect, test } from 'bun:test'
import {
  SESSION_PAGE_SIZE,
  applySidebarProjectOrder,
  applySessionDragOrder,
  sessionDisplayBlocks,
  sessionPage,
  sidebarSessionSections,
  sortSessionBlocksByActivity,
} from './sidebarSessionModel'
import { sessionFocusScore } from '@/lib/sessionFocus'

const NOW = Date.parse('2026-11-16T12:00:00Z')
const HOUR = 60 * 60 * 1000

function item(id, {
  path = '',
  pinned = false,
  at = id,
  status = 'idle',
  unread = false,
  child = false,
  parentId,
} = {}) {
  const timestamp = typeof at === 'number' ? new Date(at).toISOString() : at
  return {
    child,
    session: {
      id,
      pinned,
      status,
      unread,
      parent_id: parentId,
      runtime_ref: path ? { project_path: path } : undefined,
      updated_at: timestamp,
      last_attention_at: timestamp,
    },
  }
}

describe('sidebar session organization', () => {
  test('shows fifty recent sessions before exposing the remainder', () => {
    const sessions = Array.from({ length: 55 }, (_, index) => item(`session-${index}`))

    const page = sessionPage(sessions, SESSION_PAGE_SIZE)

    expect(page.items).toHaveLength(50)
    expect(page.total).toBe(55)
  })

  test('sorts the flat list by attention time instead of project or parent order', () => {
    const sections = sidebarSessionSections([
      item('older', { at: '2026-08-15T12:00:00Z' }),
      item('newest', { at: '2026-08-17T12:00:00Z' }),
      item('middle', { at: '2026-08-16T12:00:00Z' }),
    ], [])

    expect(sections.recentItems.map((entry) => entry.session.id)).toEqual([
      'newest',
      'middle',
      'older',
    ])
  })

  test('keeps project expansion independent from the no-project page', () => {
    const projectPath = 'ungrouped'
    const projectSessions = Array.from(
      { length: 55 },
      (_, index) => item(`project-${index}`, { path: projectPath }),
    )
    const ungroupedSessions = Array.from(
      { length: 55 },
      (_, index) => item(`ungrouped-${index}`),
    )
    const sections = sidebarSessionSections(
      [item('pinned', { pinned: true }), ...projectSessions, ...ungroupedSessions],
      [{ path: projectPath, name: 'Named project' }],
    )

    const blocks = sessionDisplayBlocks(
      sections.groups,
      sections.ungrouped,
      new Set([projectPath]),
      false,
      new Set(),
    )
    const project = blocks.find((block) => block.kind === 'project')
    const ungrouped = blocks.find((block) => block.kind === 'ungrouped')

    expect(sections.pinnedItems.map((entry) => entry.session.id)).toEqual(['pinned'])
    expect(project?.items).toHaveLength(55)
    expect(ungrouped?.items).toHaveLength(5)
    expect(new Set(blocks.map((block) => block.key)).size).toBe(blocks.length)
  })

  test('shows five conversations per folder and expands only the requested folder', () => {
    const groups = ['physicslab', 'jaz'].map((key) => ({
      key,
      label: key,
      items: Array.from({ length: 12 }, (_, index) => item(`${key}-${index}`)),
    }))
    const initial = sessionDisplayBlocks(groups, [], new Set(), false, new Set())

    expect(initial.map((block) => block.items.length)).toEqual([5, 5])
    expect(initial[0].items.map((entry) => entry.session.id)).toEqual([
      'physicslab-0', 'physicslab-1', 'physicslab-2', 'physicslab-3', 'physicslab-4',
    ])

    const expanded = sessionDisplayBlocks(groups, [], new Set(['physicslab']), false, new Set())
    expect(expanded.map((block) => block.items.length)).toEqual([12, 5])
  })

  test.each([
    '2026-10-07T12:00:00Z',
    '2026-10-05T12:00:00Z',
    '2026-10-01T12:00:00Z',
  ])('keeps every project above Recents with ungrouped activity at %s', (at) => {
    const sections = sidebarSessionSections([
      item('alpha', { path: '/alpha', at: '2026-10-06T12:00:00Z' }),
      item('beta', { path: '/beta', at: '2026-10-04T12:00:00Z' }),
      item('recent', { at }),
    ], [
      { path: '/alpha', name: 'Alpha' },
      { path: '/beta', name: 'Beta' },
    ])

    for (const order of [null, ['project:/beta', 'ungrouped', 'project:/alpha']]) {
      const groups = applySessionDragOrder(sections.groups, order)
      const blocks = sessionDisplayBlocks(groups, sections.ungrouped, new Set(), false, new Set())

      expect(blocks.map((block) => block.key)).toEqual(
        order ? ['project:/beta', 'project:/alpha', 'ungrouped'] : ['project:/alpha', 'project:/beta', 'ungrouped'],
      )
    }
  })

  test('preserves project drag order independently of the no-project block', () => {
    const groups = [
      { key: '/alpha', label: 'Alpha', items: [] },
      { key: '/beta', label: 'Beta', items: [] },
    ]

    const ordered = applySessionDragOrder(groups, ['project:/beta', 'ungrouped', 'project:/alpha'])

    expect(ordered.map((group) => group.key)).toEqual(['/beta', '/alpha'])
  })

  test('reorders visible projects without moving projects absent from the sidebar', () => {
    const projects = [
      { path: '/alpha', name: 'Alpha' },
      { path: '/hidden', name: 'Hidden' },
      { path: '/beta', name: 'Beta' },
    ]

    const ordered = applySidebarProjectOrder(
      projects,
      ['project:/beta', 'ungrouped', 'project:/alpha'],
    )

    expect(ordered.map((project) => project.path)).toEqual(['/beta', '/hidden', '/alpha'])
  })
})


test('shows project groups only when they contain visible unpinned sessions', () => {
  const projects = [
    { path: '/empty', name: 'Empty' },
    { path: '/pinned', name: 'Pinned only' },
    { path: '/active', name: 'Active' },
  ]
  const sessions = [item('pinned', { path: '/pinned', pinned: true }), item('thread', { path: '/active' })]
  const sections = sidebarSessionSections(sessions, projects)
  expect(sections.groups.map((group) => [group.key, group.items.length])).toEqual([
    ['/active', 1],
  ])
  const blocks = sessionDisplayBlocks(sections.groups, [], new Set(), false, new Set())
  expect(blocks.map((block) => block.key)).toEqual(['project:/active'])
  expect(sections.pinnedItems.map((entry) => entry.session.id)).toEqual(['pinned'])
  expect(sidebarSessionSections([], projects).groups).toEqual([])

  const removed = sidebarSessionSections(sessions, projects.filter((project) => project.path !== '/active'))
  expect(removed.groups).toEqual([])
  expect(removed.ungrouped.map((entry) => entry.session.id)).toEqual(['thread'])
})


describe('adaptive session focus', () => {
  test('keeps attention states and the open session above decaying recency', () => {
    const sessions = [
      item('recent', { at: NOW }),
      item('current', { at: NOW - 24 * HOUR }),
      item('running', { at: NOW - 10 * 24 * HOUR, status: 'running' }),
      item('unread', { at: NOW - 20 * 24 * HOUR, unread: true }),
      item('error', { at: NOW - 30 * 24 * HOUR, status: 'error' }),
    ]

    const sections = sidebarSessionSections(sessions, [], { now: NOW, currentSessionID: 'current' })

    expect(sections.recentItems.map((entry) => entry.session.id)).toEqual([
      'error', 'unread', 'running', 'current', 'recent',
    ])
  })

  test('decays activity by half in a day and rewards repeated views with a bounded score', () => {
    const fresh = item('fresh', { at: NOW })
    const dayOld = item('day-old', { at: NOW - 24 * HOUR })
    const viewed = item('viewed', { at: NOW - 10 * 24 * HOUR })

    expect(sessionFocusScore(dayOld, { now: NOW })).toBeCloseTo(sessionFocusScore(fresh, { now: NOW }) / 2)
    const once = sessionFocusScore(viewed, { now: NOW, views: { viewed: { at: NOW, count: 1 } } })
    const repeated = sessionFocusScore(viewed, { now: NOW, views: { viewed: { at: NOW, count: 8 } } })
    expect(repeated).toBeGreaterThan(once)
    expect(repeated).toBeLessThanOrEqual(1)
  })

  test('keeps active, open and three recent units visible in a collapsed project', () => {
    const path = '/project'
    const sessions = [
      item('stale', { path, at: NOW - 30 * 24 * HOUR }),
      item('recent-3', { path, at: NOW - 3 * HOUR }),
      item('recent-1', { path, at: NOW - HOUR }),
      item('error', { path, at: NOW - 30 * 24 * HOUR, status: 'error' }),
      item('recent-2', { path, at: NOW - 2 * HOUR }),
      item('current', { path, at: NOW - 10 * 24 * HOUR }),
      item('recent-0', { path, at: NOW }),
    ]
    const focus = { now: NOW, currentSessionID: 'current' }
    const sections = sidebarSessionSections(sessions, [{ path, name: 'Project' }], focus)

    const [block] = sessionDisplayBlocks(
      sections.groups, [], new Set(), false, new Set([path]), focus,
    )

    expect(block.focusItems.map((entry) => entry.session.id)).toEqual([
      'error', 'current', 'recent-0', 'recent-1', 'recent-2',
    ])
  })

  test('ranks projects by their strongest session in activity mode', () => {
    const projects = [
      { path: '/recent', name: 'Recent' },
      { path: '/error', name: 'Error' },
      { path: '/running', name: 'Running' },
      { path: '/stale', name: 'Stale' },
    ]
    const sections = sidebarSessionSections([
      item('recent', { path: '/recent', at: NOW }),
      item('error', { path: '/error', at: NOW - 30 * 24 * HOUR, status: 'error' }),
      item('running', { path: '/running', at: NOW - 20 * 24 * HOUR, status: 'running' }),
      item('stale', { path: '/stale', at: NOW - 30 * 24 * HOUR }),
    ], projects, { now: NOW })
    const blocks = sessionDisplayBlocks(sections.groups, [], new Set(), false, new Set())

    expect(sortSessionBlocksByActivity(blocks, { now: NOW }).map((block) => block.key)).toEqual([
      'project:/error', 'project:/running', 'project:/recent', 'project:/stale',
    ])
  })

  test('keeps a running child attached to its parent through sorting and collapse', () => {
    const path = '/project'
    const sessions = [
      item('recent', { path, at: NOW }),
      item('parent', { path, at: NOW - 30 * 24 * HOUR }),
      item('child', { path, at: NOW - 30 * 24 * HOUR, status: 'running', child: true, parentId: 'parent' }),
    ]
    const focus = { now: NOW }
    const sections = sidebarSessionSections(sessions, [{ path, name: 'Project' }], focus)
    const [block] = sessionDisplayBlocks(
      sections.groups, [], new Set(), false, new Set([path]), focus,
    )

    expect(block.group.items.map((entry) => entry.session.id)).toEqual(['parent', 'child', 'recent'])
    expect(block.focusItems.map((entry) => entry.session.id)).toEqual(['parent', 'child', 'recent'])
    expect(block.focusItems[1].child).toBe(true)
  })
})
