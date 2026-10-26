import { expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'

const PANE = {
  plugin: 'amq-board',
  component: 'Pane',
  requestId: 'amq-board',
  props: {
    title: 'AMQ board',
    isFocused: false,
    bodyColumns: 60,
    placement: 'dock',
    scroll: { offset: 0, bodyRows: 30 },
    view: {},
  },
} as const

const NOW = Date.parse('2026-10-25T09:01:00Z')

const BOARD = {
  command: 'board',
  args: '',
  origin: { kind: 'composer' },
  presentation: { isFullscreen: true, columns: 200 },
} as const

const SERVE_JSON = JSON.stringify({ url: 'http://127.0.0.1:4242', token: 'abc123', pid: 7, started_at: '' })

// A model as amq-board --json prints it, cut to what the pane reads. The
// title carries an ANSI escape that must reach the pane as plain text.
const MODEL = {
  schema: 'amq/board/v1',
  generated_at: '2026-10-25T09:00:00Z',
  dir: '/work/proj',
  project: 'proj',
  sources: [
    { name: 'amq', status: 'ok', partial: false },
    { name: 'github', status: 'degraded', partial: false },
  ],
  columns: [
    {
      id: 'waiting_on_you',
      cards: [
        {
          title: '\x1b[31mReview the parser\x1b[0m',
          reason: { text: 'review requested', since: '2026-10-25T08:00:00Z' },
          actions: [{ command: 'amq reply --id m1' }],
        },
      ],
    },
    { id: 'in_flight', cards: [{}, {}] },
    { id: 'blocked', cards: [{}] },
    { id: 'unclaimed', cards: [] },
  ],
}

type World = {
  // The engine's record of the pane; a test sets it false to close the pane.
  isOpen: boolean
  // What the pane started: serve fetches and amq-board runs (spawned or run).
  fetches: number
  boardRuns: number
  // kill -0 answers this exit code for the serve pid.
  killExit: number
  statuses: (string | undefined)[]
  // The amq-board run answers nothing for this long, on the mocked clock.
  hangMs: number
}

function engine(on: On) {
  const world: World = { isOpen: false, fetches: 0, boardRuns: 0, killExit: 0, statuses: [], hangMs: 0 }
  mock.env(on, { HOME: '/home/me' })
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('session.version', () => ({ value: { version: '2.1.310', base: '2.1.310' } }))
  on('session.cwd', () => ({ value: '/work/proj' }))
  on('command.register', () => ({ value: { command: 'board' } }))
  on('ui.open', () => {
    world.isOpen = true
    return { value: { isPlaced: true } }
  })
  on('ui.close', () => {
    world.isOpen = false
    return { value: undefined }
  })
  on('ui.panes', () => ({
    value: world.isOpen ? [{ id: 'amq-board', title: 'AMQ board', isShown: true, isFocused: false, isPlaced: true }] : [],
  }))
  on('ui.status', (_, e) => {
    world.statuses.push(e.text)
    return { value: undefined }
  })
  on('http.fetch', (_, e) => {
    world.fetches++
    expect(e.url).toBe('http://127.0.0.1:4242/abc123/model.json')
    return { value: { status: 200, ok: true, headers: {}, text: JSON.stringify(MODEL) } }
  })
  on('process.run', (_, e) => {
    if (e.argv[0] === 'kill') {
      expect(e.argv).toEqual(['kill', '-0', '7'])
      return { value: { exitCode: world.killExit, stdout: '', stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
    }
    world.boardRuns++
    return { value: { exitCode: 0, stdout: JSON.stringify(MODEL), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  const clock = mock.clock(on, { now: NOW })
  on('process.spawn', async function* (_, e) {
    world.boardRuns++
    expect(e.argv).toEqual(['amq-board', '--json'])
    expect(e.cwd).toBe('/work/proj')
    if (world.hangMs > 0) await clock.sleep(world.hangMs)
    yield { stream: 'stdout' as const, text: JSON.stringify(MODEL) }
    return { value: { code: 0, signal: null } }
  })
  return { world, clock }
}

test('/board draws the board a running serve publishes, escapes as plain text', async ($, on) => {
  const { world, clock } = engine(on)
  on('fs.read', (_, e) => {
    expect(e.path).toBe('/home/me/.amq/board/serve.json')
    return { value: SERVE_JSON }
  })

  await $.session.start({ cwd: '/work/proj', surface: 'terminal', isInteractive: true })
  await $.command.run(BOARD)
  await clock.settle()

  for (const surface of ['terminal', 'desktop'] as const) {
    const ui = await $.ui.mount({ ...PANE, surface })
    expect((await ui.find({ type: 'Text', text: /parser/ }))?.text).toBe('[31mReview the parser[0m')
    expect((await ui.find({ type: 'Text', text: /review requested/ }))?.text).toBe('review requested · 1h ago')
    expect((await ui.find({ type: 'Text', text: /In flight/ }))?.text).toBe('In flight 2 · Blocked 1 · Unclaimed 0')
    expect((await ui.find({ type: 'Text', text: /sources/ }))?.text).toBe('sources: amq ok · github degraded')
    expect((await ui.find({ type: 'Text', text: /generated/ }))?.text).toBe('generated 2026-10-25T09:00:00Z')
    await ui.unmount()
  }
  expect(world.statuses.at(-1)).toBe('board: 1 waiting · 2 in flight')

  // Refreshes every minute while open.
  await clock.advance(60_000)
  expect(world.fetches).toBe(2)
  expect(world.boardRuns).toBe(0)
})

// Installed check for agent-message-queue-868.10: /board had no terminal close path.
test('/board opens and closes the panel and stops its refresh runs', async ($, on) => {
  const { world, clock } = engine(on)
  on('fs.read', () => ({ deny: 'ENOENT: no such file' }))

  await $.session.start({ cwd: '/work/proj', surface: 'terminal', isInteractive: true })
  await $.command.run(BOARD)
  await clock.settle()

  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect((await ui.find({ type: 'Text', text: /Waiting on you/ }))?.text).toBe('Waiting on you (1)')
  await ui.unmount()
  expect(world.boardRuns).toBe(1)

  await $.command.run(BOARD)
  await clock.settle()
  expect(world.isOpen).toBe(false)
  expect(world.statuses.at(-1)).toBeUndefined()
  await clock.advance(180_000)
  expect(world.boardRuns).toBe(1)
})

// Codex review of PR #1216: a refresh that was looking for serve when the
// pane closed went on to start the 90 s amq-board run and set the status.
test('a pane closed during serve discovery starts no amq-board run', async ($, on) => {
  const { world, clock } = engine(on)
  on('fs.read', () => {
    world.isOpen = false
    return { deny: 'ENOENT: no such file' }
  })

  await $.session.start({ cwd: '/work/proj', surface: 'terminal', isInteractive: true })
  await $.command.run(BOARD)
  await clock.settle()
  await clock.advance(180_000)

  expect(world.boardRuns).toBe(0)
  expect(world.statuses.filter(text => text !== undefined)).toEqual([])
})

// Codex review of PR #1216: a pane closed while a refresh awaited its
// update had its cleared status set again by that refresh.
test('a pane closed during the awaited update leaves no status', async ($, on) => {
  const { world, clock } = engine(on)
  on('fs.read', () => ({ value: SERVE_JSON }))
  on('state.set', (_, e, next) => {
    world.isOpen = false
    return next(e)
  })

  await $.session.start({ cwd: '/work/proj', surface: 'terminal', isInteractive: true })
  await $.command.run(BOARD)
  await clock.settle()

  expect(world.fetches).toBe(1)
  expect(world.statuses.filter(text => text !== undefined)).toEqual([])
})

// Codex review of PR #1216: an amq-board run that never answers held the
// refresh, and with it every later refresh, past the 90 s deadline.
test('an amq-board run that does not answer ends the refresh at 90 s', async ($, on) => {
  const { world, clock } = engine(on)
  world.hangMs = 10 * 60_000
  on('fs.read', () => ({ deny: 'ENOENT: no such file' }))

  await $.session.start({ cwd: '/work/proj', surface: 'terminal', isInteractive: true })
  await $.command.run(BOARD)
  await clock.settle()
  await clock.advance(90_000)

  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect((await ui.find({ type: 'Text', text: /no answer/ }))?.text).toBe('amq-board: no answer in 90 s')
  await ui.unmount()
})

// Codex review of PR #1216: the panel used a serve.json whose pid was dead.
test('a serve.json whose pid is not alive is ignored', async ($, on) => {
  const { world, clock } = engine(on)
  world.killExit = 1
  on('fs.read', () => ({ value: SERVE_JSON }))

  await $.session.start({ cwd: '/work/proj', surface: 'terminal', isInteractive: true })
  await $.command.run(BOARD)
  await clock.settle()

  expect(world.fetches).toBe(0)
  expect(world.boardRuns).toBe(1)
})
