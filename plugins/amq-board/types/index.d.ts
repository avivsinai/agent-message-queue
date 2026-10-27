/** One Waiting on you strip, every string already stripped of control and format characters. */
export type BoardStrip = {
  taskID: string
  title: string
  reason: string
  age: string
  command: string
}

/** What the pane draws: the board model projected and cleaned at each refresh. */
export type BoardView = {
  /** Empty when the last refresh read a model; else the one line to show. */
  status: string
  strips: BoardStrip[]
  waiting: number
  recordedProgress: number
  requested: number
  total: number
  waitingOmitted: number
  sourcesOmitted: number
  blocked: number
  unclaimed: number
  sources: string
  project: string
  /** The snapshot's published_at as given; empty before the first model. */
  publishedAt: string
  /** True once the publication is more than 5 minutes older than the last refresh. */
  isStale: boolean
}

declare module 'claude-code' {
  interface PluginState {
    'amq-board': { view: BoardView }
  }
}
