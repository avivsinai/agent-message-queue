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

function engine(on: On) {
  mock.env(on, { HOME: '/home/me' })
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('session.version', () => ({ value: { version: '2.1.310', base: '2.1.310' } }))
  on('session.cwd', () => ({ value: '/work/proj' }))
  on('command.register', () => ({ value: { command: 'board' } }))
  on('ui.panes', () => ({ value: [] }))
  on('ui.open', () => ({ value: { isPlaced: true } }))
  on('ui.status', () => ({ value: undefined }))
  return mock.clock(on, { now: NOW })
}

test('/board draws the board a running serve publishes, escapes as plain text', async ($, on) => {
  const clock = engine(on)
  let fetches = 0
  on('fs.read', (_, e) => {
    expect(e.path).toBe('/home/me/.amq/board/serve.json')
    return { value: JSON.stringify({ url: 'http://127.0.0.1:4242', token: 'abc123', pid: 7, started_at: '' }) }
  })
  on('http.fetch', (_, e) => {
    fetches++
    expect(e.url).toBe('http://127.0.0.1:4242/abc123/model.json')
    return { value: { status: 200, ok: true, headers: {}, text: JSON.stringify(MODEL) } }
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

  // Refreshes every minute while open.
  await clock.advance(60_000)
  expect(fetches).toBe(2)
})

test('/board runs amq-board --json in the session directory when no serve runs', async ($, on) => {
  const clock = engine(on)
  on('fs.read', () => {
    return { deny: 'ENOENT: no such file' }
  })
  on('process.run', (_, e) => {
    expect(e.argv).toEqual(['amq-board', '--json'])
    expect(e.init?.cwd).toBe('/work/proj')
    return { value: { exitCode: 0, stdout: JSON.stringify(MODEL), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })

  await $.session.start({ cwd: '/work/proj', surface: 'terminal', isInteractive: true })
  await $.command.run(BOARD)
  await clock.settle()

  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect((await ui.find({ type: 'Text', text: /Waiting on you/ }))?.text).toBe('Waiting on you (1)')
  await ui.unmount()
})
