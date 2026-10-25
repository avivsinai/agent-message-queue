import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'

import type { BoardStrip, BoardView } from '../types'

// The pane and its refresh period while it is open.
const PANE = 'amq-board'
const EVERY_MS = 60_000
const RUN_TIMEOUT_MS = 90_000
// More than any board prints: a run past it is stopped.
const MAX_OUTPUT = 4 * 1024 * 1024
const STALE_MS = 5 * 60_000
// The first Claude Code whose plugin API this module is written against.
const MIN_VERSION = [2, 1, 308] as const

const EMPTY: BoardView = {
  status: 'loading the board…',
  strips: [],
  waiting: 0,
  inFlight: 0,
  blocked: 0,
  unclaimed: 0,
  sources: '',
  project: '',
  generatedAt: '',
  isStale: false,
}

const view = atom({ plugin: 'amq-board', key: 'view' } as const, EMPTY)

// Module state: a reload drops it, as it drops the engine's timers and
// kills the module's children.
let timer: { cancel: () => void } | undefined
// Bumped by start and stop: a refresh started under an older value belongs
// to a pane that has closed since, and draws and starts nothing.
let generation = 0
let isRunning = false
// Kills the amq-board run in flight.
let killRun: (() => void) | undefined
let last: BoardModel | undefined

// The parts of the amq/board/v1 model the pane reads. Every field is
// optional: the model comes from another process and is checked here.
type BoardModel = {
  generated_at?: unknown
  dir?: unknown
  project?: unknown
  sources?: unknown
  columns?: unknown
}

/**
 * Makes one value safe to draw: tabs and line breaks become spaces, and
 * every other control (C0, DEL, C1) or format character is dropped, so an
 * escape sequence in a title cannot restyle or blank the pane.
 */
export function clean(value: unknown, limit = 160): string {
  if (typeof value !== 'string') return ''
  const text = value
    .replace(/[\t\n\r\v\f\u2028\u2029]/g, ' ')
    .replace(/[\p{Cc}\p{Cf}]/gu, '')
  const chars = Array.from(text)
  return chars.length > limit ? chars.slice(0, limit - 1).join('') + '…' : text
}

function list(value: unknown): Record<string, unknown>[] {
  return Array.isArray(value)
    ? value.filter((x): x is Record<string, unknown> => typeof x === 'object' && x !== null)
    : []
}

function field(value: unknown, key: string): unknown {
  return typeof value === 'object' && value !== null ? (value as Record<string, unknown>)[key] : undefined
}

function time(value: unknown): number | undefined {
  if (typeof value !== 'string') return undefined
  const ms = Date.parse(value)
  // Go's zero time (year 1) means unknown.
  return Number.isFinite(ms) && ms > 0 ? ms : undefined
}

function age(ms: number): string {
  if (ms < 60_000) return '<1m'
  if (ms < 3_600_000) return `${Math.floor(ms / 60_000)}m`
  if (ms < 48 * 3_600_000) return `${Math.floor(ms / 3_600_000)}h`
  return `${Math.floor(ms / 86_400_000)}d`
}

/** Projects a model onto what the pane draws; ages are as of generation. */
export function project(model: BoardModel, now: number): BoardView {
  const generated = time(model.generated_at)
  const columns = list(model.columns)
  const cards = (id: string) => list(columns.find(c => c.id === id)?.cards)
  const strips: BoardStrip[] = cards('waiting_on_you').map(card => {
    const since = time(field(card.reason, 'since'))
    const command = list(card.actions)[0]?.command
    return {
      title: clean(card.title),
      reason: clean(field(card.reason, 'text')),
      age: since !== undefined && generated !== undefined ? `${age(generated - since)} ago` : 'since unknown',
      command: clean(command, 4096),
    }
  })
  const sources = list(model.sources)
    .map(s => `${clean(s.name, 40)} ${clean(s.status, 20)}${s.partial === true ? ' (partial)' : ''}`)
    .join(' · ')
  return {
    status: '',
    strips,
    waiting: strips.length,
    inFlight: cards('in_flight').length,
    blocked: cards('blocked').length,
    unclaimed: cards('unclaimed').length,
    sources,
    project: clean(model.project) || clean(model.dir),
    generatedAt: clean(model.generated_at, 40),
    isStale: generated === undefined || now - generated > STALE_MS,
  }
}

function within(cwd: string, dir: unknown): boolean {
  if (typeof dir !== 'string' || dir === '') return false
  const root = dir.replace(/\/+$/, '')
  return cwd === root || cwd.startsWith(root + '/')
}

/** Whether pid is a live process; true where no kill command can tell. */
async function isAlive($: EngineInterface, pid: number): Promise<boolean> {
  try {
    const ran = await $.process.run(['kill', '-0', String(pid)], { timeoutMs: 5_000 })
    return ran.exitCode === 0
  } catch {
    // No kill command (Windows): the token on the fetch still has to match.
    return true
  }
}

/** Reads the model from a running amq-board serve for this project, if any. */
async function fromServe($: EngineInterface, cwd: string): Promise<BoardModel | undefined> {
  const home = await $.env.get('HOME')
  if (!home) return undefined
  let record: unknown
  try {
    const text = await $.fs.read(`${home}/.amq/board/serve.json`)
    record = JSON.parse(typeof text === 'string' ? text : '')
  } catch {
    return undefined
  }
  const url = field(record, 'url')
  const token = field(record, 'token')
  const pid = field(record, 'pid')
  // Loopback only, and a token that is one path segment.
  if (typeof url !== 'string' || !/^http:\/\/127\.0\.0\.1:\d+$/.test(url)) return undefined
  if (typeof token !== 'string' || !/^[0-9a-f]+$/.test(token)) return undefined
  // A record whose serve is gone is stale.
  if (typeof pid !== 'number' || !Number.isInteger(pid) || pid <= 0) return undefined
  if (!(await isAlive($, pid))) return undefined
  try {
    const res = await $.http.fetch(`${url}/${token}/model.json`)
    if (!res.ok) return undefined
    const model = JSON.parse(res.text) as BoardModel
    // serve reads one project; another project's board is not this one's.
    return within(cwd, model.dir) ? model : undefined
  } catch {
    return undefined
  }
}

/** Runs amq-board --json; stop() or the 90 s deadline kills the run. */
async function fromRun($: EngineInterface, cwd: string): Promise<BoardModel | string> {
  const run = $.process.spawn({ argv: ['amq-board', '--json'], cwd })
  const kill = () => void run.return({ code: null, signal: 'SIGTERM' }).catch(() => {})
  let isTimedOut = false
  const deadline = $.clock.after(RUN_TIMEOUT_MS, () => {
    isTimedOut = true
    kill()
  })
  killRun = kill
  let stdout = ''
  let stderr = ''
  try {
    let step = await run.next()
    while (!step.done) {
      if (step.value.stream === 'stdout') stdout += step.value.text
      else if (stderr.length < 4096) stderr += step.value.text
      if (stdout.length > MAX_OUTPUT) {
        kill()
        return 'amq-board: --json printed more than 4 MiB'
      }
      step = await run.next()
    }
    if (isTimedOut) return 'amq-board: no answer in 90 s'
    const code = step.value?.code
    if (code !== 0) {
      const first = stderr.split('\n').find(line => line.trim() !== '') ?? `exit ${code ?? 'by signal'}`
      return `amq-board: ${clean(first)}`
    }
  } catch (err) {
    const text = err instanceof Error ? err.message : String(err)
    return /ENOENT|not found|no such file/i.test(text) ? 'amq-board not found' : `amq-board: ${clean(text)}`
  } finally {
    deadline.cancel()
    if (killRun === kill) killRun = undefined
  }
  try {
    return JSON.parse(stdout) as BoardModel
  } catch {
    return 'amq-board: --json printed no JSON'
  }
}

/**
 * Whether a refresh started under gen may go on: no stop since, and the
 * pane still open by the engine's record, which a close the module never
 * heard of (an unload) also ends.
 */
async function isCurrent($: EngineInterface, gen: number): Promise<boolean> {
  if (gen !== generation || !timer) return false
  if (!(await $.ui.panes()).some(pane => pane.id === PANE)) {
    if (gen === generation) stop($)
    return false
  }
  return gen === generation
}

async function tick($: EngineInterface): Promise<void> {
  if (isRunning) return
  isRunning = true
  const gen = generation
  try {
    if (!(await isCurrent($, gen))) return
    const cwd = await $.session.cwd()
    const served = await fromServe($, cwd)
    if (!(await isCurrent($, gen))) return
    const got = served ?? (await fromRun($, cwd))
    if (!(await isCurrent($, gen))) return
    const now = await $.clock.now()
    let next: BoardView
    if (typeof got === 'string') {
      // Keep the last board on screen, marked by the failure.
      next = last ? { ...project(last, now), status: got } : { ...EMPTY, status: got }
    } else {
      last = got
      next = project(got, now)
    }
    await update($, view, () => next)
    $.ui.status(`board: ${next.waiting} waiting · ${next.inFlight} in flight`)
  } catch (err) {
    if (gen === generation && timer) {
      await update($, view, old => ({ ...(old ?? EMPTY), status: `amq-board: ${clean(String(err))}` }))
    }
  } finally {
    isRunning = false
    // A pane opened again while this refresh ran gets its own refresh now.
    if (gen !== generation && timer) void tick($)
  }
}

function start($: EngineInterface): void {
  if (timer) return
  generation++
  timer = $.clock.every(EVERY_MS, () => void tick($))
  void tick($)
}

function stop($: EngineInterface): void {
  generation++
  timer?.cancel()
  timer = undefined
  killRun?.()
  killRun = undefined
  $.ui.status(undefined)
}

async function isSupported($: EngineInterface): Promise<boolean> {
  const base = (await $.session.version()).base
  // A build that names no release is a development build: let it run.
  if (!base) return true
  const parts = base.split(/[.-]/).slice(0, 3).map(Number)
  for (let i = 0; i < MIN_VERSION.length; i++) {
    const have = parts[i] ?? 0
    if (Number.isNaN(have)) return true
    if (have !== MIN_VERSION[i]) return have > MIN_VERSION[i]!
  }
  return true
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    try {
      if (await isSupported($)) {
        await $.command.register({ name: 'board', description: 'Open the AMQ fleet board in a side panel' })
        // Only after a reload can the pane already be open: resume its refresh.
        if ((await $.ui.panes()).some(pane => pane.id === PANE)) start($)
      }
    } catch {
      // An engine without these calls keeps loading everything else.
    }
    return next(e)
  }).catch(($, e, next) => next(e))

  on('command.run', { command: 'board' }, async ($, e) => {
    const opened = await $.ui.open({ id: PANE, title: 'AMQ board' })
    start($)
    return opened.isPlaced
      ? { text: 'AMQ board opened; it refreshes every minute while open.' }
      : { text: `AMQ board opened; it waits for a surface that can show it (${opened.reason}).` }
  }).catch(() => ({ text: 'AMQ board could not open.' }))

  // An older engine that refuses these events still loads the two above.
  try {
    registerPane(on)
  } catch {
    // No pane on this engine; skills and other plugins load as before.
  }
}

function registerPane(on: Parameters<Register>[0]): void {
  on('ui.close', { id: PANE }, async ($, e, next) => {
    stop($)
    return next(e)
  }).catch(($, e, next) => next(e))

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const { Box, Text, Button } = $.ui.resolve(e)
    const v = await read($, view)
    const width = Math.max(20, e.props.bodyColumns ?? 40)
    return (
      <Box flexDirection="column" width={width}>
        {v.project !== '' && <Text weight="bold">{v.project}</Text>}
        {v.status !== '' && <Text color="warning">{v.status}</Text>}
        <Text weight="bold">Waiting on you ({v.waiting})</Text>
        {v.generatedAt !== '' && v.strips.length === 0 && <Text dimColor>nothing waits on you</Text>}
        {v.strips.map((strip, i) => (
          <Box flexDirection="column" marginBottom={1}>
            <Text>{strip.title}</Text>
            <Text dimColor>
              {strip.reason} · {strip.age}
            </Text>
            {strip.command !== '' && (
              <Button key={`copy-${i}`} plain onPress={press => void $.ui.copy({ text: strip.command, surface: press.surface })}>
                copy command
              </Button>
            )}
          </Box>
        ))}
        <Text>
          In flight {v.inFlight} · Blocked {v.blocked} · Unclaimed {v.unclaimed}
        </Text>
        {v.sources !== '' && <Text dimColor>sources: {v.sources}</Text>}
        {v.generatedAt !== '' && (
          <Text dimColor={!v.isStale} color={v.isStale ? 'warning' : undefined}>
            generated {v.generatedAt}
            {v.isStale ? ' (stale)' : ''}
          </Text>
        )}
      </Box>
    )
  })
}
