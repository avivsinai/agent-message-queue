# CodexBar

Parts of AMQ's usage collection are translated from CodexBar, under the MIT
license in [LICENSE](LICENSE).

- Upstream: https://github.com/steipete/CodexBar
- Commit: `d34fbb47afe73f81dd591bae28a60f72c39a9fe4`

Translated rules and the AMQ files that carry them (each marked with
`SPDX-License-Identifier: MIT`):

| AMQ file | Upstream source | What was translated |
| --- | --- | --- |
| `internal/usageformat/codexrpc/codexrpc.go` | `Sources/CodexBarCore/UsageFetcher.swift` | The `codex -s read-only -a never app-server` invocation, the `initialize`, `initialized`, `account/read` and `account/rateLimits/read` sequence, and the reply fields read. |
| `internal/usagenormalize/codex/codex.go` | `Sources/CodexBarCore/Providers/Codex/CodexRateWindowNormalizer.swift`, `Sources/CodexBarCore/UsageFetcher.swift` | Window meaning by duration (300 minutes is the 5 h window, 10080 minutes the weekly one), not by slot; null windows stay null. |
