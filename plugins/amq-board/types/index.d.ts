/** One Waiting on you strip, every string already stripped of control and format characters. */
export type BoardStrip = {
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
  inFlight: number
  blocked: number
  unclaimed: number
  sources: string
  project: string
  /** The model's generated_at as given; empty before the first model. */
  generatedAt: string
  /** True once the model is more than 5 minutes older than the last refresh. */
  isStale: boolean
}

declare module 'claude-code' {
  interface PluginState {
    'amq-board': { view: BoardView }
  }
}
