import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'

import type { BoardStrip, BoardView } from '../types'

// The pane and its refresh period while it is open.
const PANE = 'amq-board'
const EVERY_MS = 60_000
const RUN_TIMEOUT_MS = 90_000
// The dedicated pane DTO and both transports share this encoded-byte ceiling.
const MAX_OUTPUT = 4 * 1024 * 1024
const STALE_MS = 5 * 60_000
// The first Claude Code whose plugin API this module is written against.
const MIN_VERSION = [2, 1, 308] as const

const EMPTY: BoardView = {
  status: 'loading the board…',
  strips: [],
  waiting: 0,
  recordedProgress: 0,
  blocked: 0,
  unclaimed: 0,
  sources: '',
  project: '',
  publishedAt: '',
  isStale: false,
  requested: 0,
  total: 0,
  waitingOmitted: 0,
  sourcesOmitted: 0,
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

// Only the dedicated bounded projection is accepted; full model JSON is not a pane payload.
type BoardModel = {
  schema: 'amq/board-pane/v1'
  generation: number
  generated_at: string
  published_at: string
  dir: string
  project: string
  columns: { id: string; total: number }[]
  current_total: number
  waiting: { task_id: string; title: string; reason: string; since: string; command: string }[]
  waiting_total: number
  waiting_omitted: number
  sources: { name: string; status: string; partial: boolean }[]
  sources_omitted: number
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

function count(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
}

// Copy commands are either exact and safe, or absent. Never edit shell text.
function validCommand(value: unknown): value is string {
  if (typeof value !== 'string' || Array.from(value).length > 4096 || /[\p{Cc}\p{Cf}\u2028\u2029]/u.test(value)) return false
  for (const char of value) {
    const point = char.codePointAt(0)!
    if (point >= 0xd800 && point <= 0xdfff) return false
  }
  return true
}

function utf8Bytes(text: string): number {
  let bytes = 0
  for (const char of text) {
    const n = char.codePointAt(0)!
    bytes += n < 0x80 ? 1 : n < 0x800 ? 2 : n < 0x10000 ? 3 : 4
  }
  return bytes
}

function parseModel(text: string): BoardModel | undefined {
  if (utf8Bytes(text) > MAX_OUTPUT) return undefined
  let raw: unknown
  try { raw = JSON.parse(text) } catch { return undefined }
  if (field(raw, 'schema') !== 'amq/board-pane/v1') return undefined
  const model = raw as BoardModel
  if (!count(model.generation) || model.generation === 0 || typeof model.generated_at !== 'string' || typeof model.published_at !== 'string' || !Number.isFinite(Date.parse(model.generated_at)) || !Number.isFinite(Date.parse(model.published_at)) || typeof model.dir !== 'string' || model.dir === '' || typeof model.project !== 'string') return undefined
  if (!Array.isArray(model.columns) || !Array.isArray(model.waiting) || !Array.isArray(model.sources) || model.waiting.length > 100 || model.sources.length > 32 || !count(model.current_total) || !count(model.waiting_total) || !count(model.waiting_omitted) || !count(model.sources_omitted)) return undefined
  let total = 0
  let waiting = 0
  const ids = new Set<string>()
  for (const column of model.columns) {
    if (!column || typeof column.id !== 'string' || column.id === '' || ids.has(column.id) || !count(column.total)) return undefined
    ids.add(column.id)
    total += column.total
    if (column.id === 'waiting_on_you') waiting = column.total
  }
  if (!count(total) || total !== model.current_total || waiting !== model.waiting_total || model.waiting.length + model.waiting_omitted !== waiting) return undefined
  for (const strip of model.waiting) {
    if (!strip || typeof strip.task_id !== 'string' || strip.task_id === '' || Array.from(strip.task_id).length > 256 || /[\p{Cc}\p{Cf}\u2028\u2029]/u.test(strip.task_id) || typeof strip.title !== 'string' || typeof strip.reason !== 'string' || typeof strip.since !== 'string' || !Number.isFinite(Date.parse(strip.since)) || !validCommand(strip.command)) return undefined
  }
  for (const source of model.sources) {
    if (!source || typeof source.name !== 'string' || typeof source.status !== 'string' || typeof source.partial !== 'boolean') return undefined
  }
  return model
}

/** Ages use the refresh clock; freshness belongs to the published snapshot. */
export function project(model: BoardModel, now: number): BoardView {
  const published = time(model.published_at)
  const column = (id: string) => model.columns.find(c => c.id === id)?.total ?? 0
  const strips: BoardStrip[] = model.waiting.map(strip => {
    const since = time(strip.since)
    return {
      taskID: strip.task_id,
      title: clean(strip.title),
      reason: clean(strip.reason),
      age: since !== undefined ? `${age(Math.max(0, now - since))} ago` : 'since unknown',
      command: strip.command,
    }
  })
  return {
    status: '', strips,
    waiting: model.waiting_total,
    waitingOmitted: model.waiting_omitted,
    recordedProgress: column('in_flight'),
    blocked: column('blocked'),
    unclaimed: column('unclaimed'),
    requested: column('requests'),
    total: model.current_total,
    sources: model.sources.map(s => `${clean(s.name, 40)} ${clean(s.status, 20)}${s.partial ? ' (partial)' : ''}`).join(' · '),
    sourcesOmitted: model.sources_omitted,
    project: clean(model.project) || clean(model.dir),
    publishedAt: clean(model.published_at, 40),
    isStale: published === undefined || now - published > STALE_MS,
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
    const res = await $.http.fetch(`${url}/${token}/pane.json`)
    if (!res.ok) return undefined
    const model = parseModel(res.text)
    if (!model) return undefined
    // serve reads one project; another project's board is not this one's.
    return within(cwd, model.dir) ? model : undefined
  } catch {
    return undefined
  }
}

/**
 * Runs amq-board --pane-json. stop() or the 90 s deadline ends the refresh at
 * once and asks the engine to kill the run; the refresh never waits on a
 * child that does not answer.
 */
async function fromRun($: EngineInterface, cwd: string): Promise<BoardModel | string> {
  const run = $.process.spawn({ argv: ['amq-board', '--pane-json'], cwd })
  let abort: (why: string) => void = () => {}
  const aborted = new Promise<never>((_, reject) => {
    abort = why => reject(new Error(why))
  })
  aborted.catch(() => {})
  const kill = (why: string) => {
    abort(why)
    void run.return({ code: null, signal: 'SIGTERM' }).catch(() => {})
  }
  const deadline = $.clock.after(RUN_TIMEOUT_MS, () => kill('amq-board: no answer in 90 s'))
  const stopped = () => kill('amq-board: stopped')
  killRun = stopped
  let stdout = ''
  let outputBytes = 0
  let trailingHighSurrogate = false
  let stderr = ''
  try {
    let step = await Promise.race([run.next(), aborted])
    while (!step.done) {
      if (step.value.stream === 'stdout') {
        const text = step.value.text
        outputBytes += utf8Bytes(text)
        if (text.length > 0) {
          const first = text.charCodeAt(0)
          if (trailingHighSurrogate && first >= 0xdc00 && first <= 0xdfff) outputBytes -= 2
          const last = text.charCodeAt(text.length - 1)
          trailingHighSurrogate = last >= 0xd800 && last <= 0xdbff
        }
        stdout += text
      }
      else if (stderr.length < 4096) stderr += step.value.text
      if (outputBytes > MAX_OUTPUT) {
        kill('amq-board: --pane-json printed more than 4 MiB')
        return 'amq-board: --pane-json printed more than 4 MiB'
      }
      step = await Promise.race([run.next(), aborted])
    }
    const code = step.value?.code
    if (code !== 0) {
      const first = stderr.split('\n').find(line => line.trim() !== '') ?? `exit ${code ?? 'by signal'}`
      return `amq-board: ${clean(first)}`
    }
  } catch (err) {
    const text = err instanceof Error ? err.message : String(err)
    if (text.startsWith('amq-board: ')) return text
    return /ENOENT|not found|no such file/i.test(text) ? 'amq-board not found' : `amq-board: ${clean(text)}`
  } finally {
    deadline.cancel()
    if (killRun === stopped) killRun = undefined
  }
  return parseModel(stdout) ?? 'amq-board: --pane-json printed no valid amq/board-pane/v1 JSON'
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
    const now = await $.clock.now()
    // Every await is a point where the pane may have closed: check before
    // the update and again before the status, with nothing awaited between
    // the last check and the call it guards.
    if (!(await isCurrent($, gen))) return
    let next: BoardView
    if (typeof got === 'string') {
      // Keep the last board on screen, marked by the failure.
      next = last ? { ...project(last, now), status: got } : { ...EMPTY, status: got }
    } else {
      last = got
      next = project(got, now)
    }
    await update($, view, () => next)
    if (!(await isCurrent($, gen))) return
    $.ui.status(`board: ${next.waiting} waiting · ${next.recordedProgress} marked in progress`)
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
        await $.command.register({ name: 'board', description: 'Open or close the AMQ fleet board panel' })
        // Only after a reload can the pane already be open: resume its refresh.
        if ((await $.ui.panes()).some(pane => pane.id === PANE)) start($)
      }
    } catch {
      // An engine without these calls keeps loading everything else.
    }
    return next(e)
  }).catch(($, e, next) => next(e))

  on('command.run', { command: 'board' }, async ($, e) => {
    if ((await $.ui.panes()).some(pane => pane.id === PANE)) {
      await $.ui.close({ id: PANE })
      return (await $.ui.panes()).some(pane => pane.id === PANE)
        ? { text: 'AMQ board is still open.' }
        : { text: 'AMQ board closed.' }
    }
    const opened = await $.ui.open({ id: PANE, title: 'AMQ board' })
    start($)
    return opened.isPlaced
      ? { text: 'AMQ board opened; run /board again to close. It refreshes every minute while open.' }
      : { text: `AMQ board opened; it waits for a surface that can show it (${opened.reason}).` }
  }).catch(() => ({ text: 'AMQ board could not open or close.' }))

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
        {v.publishedAt !== '' && v.strips.length === 0 && <Text dimColor>nothing waits on you</Text>}
        {v.strips.map((strip, i) => (
          <Box flexDirection="column" marginBottom={1}>
            <Text>{strip.title}</Text>
            <Text dimColor>Task ID: {strip.taskID}</Text>
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
        {v.waitingOmitted > 0 && <Text dimColor>{v.waitingOmitted} more waiting tasks omitted from this pane</Text>}
        <Text>
          Recorded progress {v.recordedProgress} · Blocked {v.blocked} · Unclaimed {v.unclaimed} · Reply due {v.requested}
        </Text>
        <Text dimColor>Current tasks {v.total}</Text>
        {v.sources !== '' && <Text dimColor>sources: {v.sources}</Text>}
        {v.sourcesOmitted > 0 && <Text dimColor>{v.sourcesOmitted} more source summaries omitted</Text>}
        <Text dimColor>Published data only. Run amq-board sync --watch for updates.</Text>
        {v.publishedAt !== '' && (
          <Text dimColor={!v.isStale} color={v.isStale ? 'warning' : undefined}>
            published {v.publishedAt}
            {v.isStale ? ' (stale)' : ''}
          </Text>
        )}
      </Box>
    )
  })
}
