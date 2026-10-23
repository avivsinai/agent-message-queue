# Public Launch API

AMQ exposes a versioned launch contract for tools that must plan and apply a
session without parsing human output. The Go package is `launchapi`. The JSON
contract is [schemas/launch-api-v1.schema.json](../schemas/launch-api-v1.schema.json).

## Negotiation

The build advertises its contract version in `launchapi.ContractSemverV1`. A
caller negotiates its required contract range, intent version, result version,
and features before it depends on them:

```go
negotiated, err := launchapi.Negotiate(launchapi.RequirementV1{
	ContractSemver: ">=0.61.0 <0.62.0", // a 0.61 contract
	IntentVersion:  launchapi.IntentVersionV1,
	ResultVersion:  launchapi.ResultVersionV1,
	Features:       []string{"prepare_apply_v1"},
})
if err != nil {
	return err
}
_ = negotiated
```

`ContractSemver` is either one exact version (`0.61.1`) or space-separated
terms, each an operator (`>=`, `<=`, `>`, `<`, `=`) followed by a full
`major.minor.patch` version. Negotiation fails when the build's
`ContractSemverV1` is outside the range.

A caller must require every feature it uses. Contract semver alone does not
claim that an unadvertised feature is available.

The build derives the advertised feature set from the platform
implementation that the build can call:

| Build platform | Advertised launch features |
| --- | --- |
| macOS, Linux (including WSL) | The full set: `launch_intent_v1`, `prepare_apply_v1`, `lifecycle_v1`, `managed_tmux_v1`, `plan_only_commands_v1`, `initial_input`, `placement`, `base_root`, `on_live`, `caller_context`, `executable_identity`, and `wrapper`. |
| Native Windows | `launch_intent_v1` and `plan_only_commands_v1` only. Negotiation rejects `prepare_apply_v1`, `lifecycle_v1`, and `managed_tmux_v1`. |

WSL uses the Linux binary and gets the Linux row. Negotiation fails closed for
a feature that is not in the current build's advertised set.

## Decoding

Public JSON decoders run a bounded structural pass before contract decoding.
A structural refusal is a `*launchapi.StrictJSONError`. Inspect its typed
`Code`, not its message:

- `launchapi.StrictJSONDuplicateKey` (`duplicate_json_key`): an object has an
  exact duplicate key.
- `launchapi.StrictJSONDepthExceeded` (`depth_exceeded`): nesting exceeds
  `launchapi.StrictJSONMaxDepth`.

Object keys compare after JSON string decoding, so keys that differ in case or
whitespace are different keys. The error carries the containing `Path` and,
for a duplicate, the `Key`.

The decoder accepts only the underscore placement target enum and the
`columns|rows|tiled` layout enum. Clients translate their own placement
vocabulary before they prepare a request, for example `current-window` to
`current_window`, `vertical` to `columns`, and `horizontal` to `rows`. These
are client mappings, not decoder aliases.

## Providers

`PreviewV1.capabilities` reports the static adapter grammar of the selected
providers without executing a caller-supplied provider.

- `grammar_version` is the adapter-owned version that consumers compare. It
  changes when the allowed argument forms, configuration overrides, or carrier
  support change. `allowed_argument_forms` is derived from the same table that
  validates committed arguments, so the advertised list changes only when what
  validation admits changes, and each such change bumps `grammar_version`
  once. The adapter table in `internal/launch/adapter.go` owns the values.
- `verified_provider_version` is informational. It names the newest provider
  release on which AMQ verified the grammar. It is neither an exact pin nor
  the installed version.

Provider argument grammar. Each form is one argument, followed by one value
when the form takes one. A "safe value" is 1 to 128 characters from
`[A-Za-z0-9_.@+:-]` and does not start with `-`. A "session label" is one or
two `/`-separated parts, each matching `[a-z0-9_][a-z0-9_-]*`.

| Provider | Form | Value |
| --- | --- | --- |
| Claude | `--allowedTools` | comma-separated list, at most 512 bytes, of `name["("spec")"]` entries |
| Claude | `--effort` | `low`, `medium`, `high`, `xhigh`, or `max` |
| Claude | `--model` | safe value |
| Claude | `-n`, `--name` | session label; `--name=<label>` is also accepted |
| Claude | `--no-chrome`, `--safe-mode`, `--verbose` | none |
| Claude | `--permission-mode` | `acceptEdits`, `auto`, `manual`, `dontAsk`, or `plan` |
| Codex | `-c` | `model_reasoning_effort=<value>` with `minimal`, `low`, `medium`, `high`, or `xhigh`, or `approvals_reviewer=<value>` with `user`, `auto_review`, or `guardian_subagent`; the value may carry one surrounding pair of double quotes; duplicate or unknown keys are rejected |
| Codex | `--ask-for-approval` | `on-request` only; codex-cli refuses `untrusted` at start, and `never` is not admitted |
| Codex | `--model` | safe value |
| Codex | `--sandbox` | `read-only` or `workspace-write` |
| Codex | `--no-alt-screen`, `--search` | none |
| Grok Build | `--tools`, `--disallowed-tools` | opaque Grok tool names, 1 to 256 UTF-8 bytes, no C0 control characters or DEL, no surrounding whitespace, not starting with `-`; the Claude `--allowedTools` validator does not check them |
| Grok Build | `--effort` | `low`, `medium`, `high`, `xhigh`, or `max` |
| Grok Build | `--model`, `--sandbox` | safe value |
| Grok Build | `--no-alt-screen`, `--no-auto-update` | none |
| Cursor CLI | `--model` | safe value |

- Every `env_overlay` key matches the POSIX environment grammar
  `[A-Za-z_][A-Za-z0-9_]*`. A key with shell syntax, such as `a;b c`, is
  refused.

### Provider identity

Subject schema 2 binds runtime provider identity before Apply executes it.
Prepare records each runnable provider's consulted path (PATH lookup or
absolute) and its on-disk identity tuple, including symlink hops. Subject
schema 1 has no such binding.

- A change to the inode, mtime, symlink hops, or PATH resolution of the same
  path changes `subject_digest`. Apply then returns `subject_changed` with no
  launch mutation.
- If the provider, wrapper, or cwd changes after Apply authorization,
  Reconcile refuses with the typed result `authorized_identity_changed`. It
  refuses before capability probing or ticket creation.
- Plan and trust digests stay stable across these replacements.

### Initial input

The initial-input carrier is typed. Claude, Codex, and Grok advertise
`argument`: AMQ appends the exact text as the final provider argv element. The
`stdin` and `file` carriers are typed but not advertised, and return
`initial_input_unsupported`, including when a wrapper is present.

- The content is at most `launchapi.MaxInitialInputBytes` of UTF-8, has no C0
  controls, DEL, CR, or LF, and does not begin with `-`.
- Invalid text returns the validation code `initial_input_control` or
  `initial_input_leading_dash` before Prepare inspects the target.
- The content changes the plan and subject digests, not the trust digest.
- It is a one-time bootstrap carrier: a resume plan never appends it again.
  Bootstrap text belongs here, not in a reconstructed provider command.
- The normalized execution and wake policy enters both the plan and trust
  digests. A non-default execution, wake, or injector policy therefore needs
  fresh trust for that exact policy.

### Wrapper

An optional participant `wrapper` has a clean absolute `executable` path and
static `args`. The path resolves to one regular file; a resolvable symlink is
accepted.

AMQ executes this argv, without a shell:

```text
wrapper executable + wrapper args + resolved provider executable + provider args
```

The wrapper path and arguments enter the plan, trust, subject, ticket, and
command preview. An argument-carrier initial input stays the final
inner-provider argument.

### Executable containment

AMQ checks both the raw path and its resolved target before it plans or
tickets an executable. A path presented from the project returns a typed code:

| Code | Path |
| --- | --- |
| `provider_project_contained` | provider |
| `wrapper_project_contained` | wrapper |
| `amq_launcher_project_contained` | AMQ launcher |

## Intent

The intent owns the desired participants. Discovery owns the project root,
default session, session root, and local launcher preference. A public intent
does not replace committed project configuration.

Intent example:

```json
{
  "intent_version": 1,
  "participants": [
    {
      "handle": "claude",
      "runnable": true,
      "executable": "claude",
      "wrapper": {"executable": "/opt/company/bin/seat-wrapper", "args": ["--profile", "lead"]},
      "cwd": {"kind": "relative", "path": "."},
      "resume_policy": "resume",
      "execution": {
        "require_wake": true,
        "no_gitignore": false,
        "wake": {"mode": "enabled"}
      }
    },
    {"handle": "operator", "runnable": false}
  ]
}
```

### Project root

`amq setup --project-root <dir>` overrides discovery:

- The named directory, with parent components canonicalized, is the project
  root. AMQ writes `.amqrc`, `.amq/launch.json`, and `.gitignore` there.
- Upward base discovery does not redirect writes into a parent repo's live
  base root.
- AMQ refuses a symlinked leaf and follows a symlinked parent component.

Without the flag:

- An existing `.amq/launch.json` in cwd is authority, so a nested directory
  that owns its config is the project root.
- A nested or linked Git worktree is the same ceiling: discovery uses that
  worktree's own config or fails closed, and never adopts the parent live base
  root.
- A parent `.amqrc` above an explicit project root, or above a cwd that owns
  its config, is ignored, so the parent tree stays byte-identical.
- Otherwise AMQ refuses an upward-discovered parent `.amqrc` and names the
  parent path.

### Participants

`resume_policy` accepts exactly `resume`, `fresh`, or `disabled`.

The adapter set supports Claude Code, Codex CLI, Cursor CLI, and Grok Build,
plus adapter `acp` for any Agent Client Protocol agent. Pi stays direct-only.

An `acp` seat in committed `.amq/launch.json` names the agent's ACP command,
for example `{"handle": "grok", "adapter": "acp", "resume_policy": "fresh",
"command": ["grok", "agent", "--no-leader", "stdio"]}`. Its `command` is any
argv, because the plan trust digest binds it byte for byte. Its `env` takes
the common keys only. The plan runs `amq-acp host -- <agent argv>`, using the
`amq-acp` beside `amq` (else from PATH) and the agent executable resolved to
an absolute path outside the project. Launch checks `amq-acp` against the
project before it runs anything. In the plan and the execution ticket the
agent is the provider and `amq-acp` its wrapper, so both identities are bound
and checked again before exec; an acp seat takes no other `wrapper`. The seat runs with wake disabled
(audit reason `amq-acp host consumes its own inbox`), because the host takes
its inbox itself. Launch owns no conversation identity for it: the host loads
its own ACP session again on every start, so `resume_policy` is `fresh` or
`disabled`, and `resume` is refused. `amq setup` never adds an `acp` seat and
keeps one written by hand. A public intent cannot select `acp`.

Each provider owns its conversation identity:

- Claude Code and Grok Build mint session IDs from the launch nonce.
- Codex CLI reports its thread ID after a completed turn through its `notify`
  hook. Capture accepts codex-cli `codexCaptureMinimumVersion` (`0.147.0`) and
  newer, verified through `codexCaptureVerifiedVersion`
  (`internal/launch/adapter_codex.go`). An older or non-release version is
  refused with `capture_version_unsupported`. Prepare reports that refusal on
  the Codex participant, and Apply refuses only that seat with Code 6; the
  refusal does not block other seats. AMQ adds one static hook override bound to
  the session root, handle, launch nonce, and AMQ executable. The private hook
  validates the exact ticket and payload, persists immutable evidence, and only
  then publishes the conversation identity. Apply leaves the seat `pending`
  with Code 0, because no backend observes the thread at create. Codex also
  sends notify for side threads, such as title generation, in any order, so a
  notify binds only a thread whose own rollout
  (`$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*-<thread-id>.jsonl`, found by the
  exact thread ID) starts with a `session_meta` for the seat's cwd with
  `source` `cli` and, when present, `thread_source` `user`. Any other notify binds nothing
  and leaves the seat as it is. A resumed seat's notify for its captured
  thread is already captured. The hook then forwards the identical
  payload to the operator's configured Codex notify command; a missing command
  is a no-op, and a forwarding failure cannot undo or fail the evidence path. An
  unused Codex launch stays pending and cannot be resumed.
- Cursor CLI acquires its chat ID before process start through `create-chat`
  at the exact version `cursorCaptureVersion`
  (`internal/launch/adapter_cursor.go`). Its provider identity is
  `cursor-agent`. The executable is `agent`, and `cursor-agent` is a supported
  alias, including `.exe` variants.

In committed `.amq/launch.json`, `agents[].named` overrides the top-level
`named` setting, and naming is on by default. A managed Claude fresh plan
carries `--name <session>/<handle>` in the ticket and trust digest. A resume
plan never adds or renames a session. Codex, Cursor, and Grok are unnamed
under managed launch.

### Launchers

The registered launcher backends are `commands`, `tmux`, `cmux`, and
`ghostty`.

- `--launcher <name>` wins over `--launcher auto`, which walks the local
  preference.
- Under `auto`, `CMUX_SURFACE_ID` prepends `cmux` ahead of `ghostty`;
  otherwise `TERM_PROGRAM=ghostty` prepends `ghostty`.
- Selection still requires Detect Available.
- Cmux Create uses `--focus false` and restores the prior selection. See
  [Managed launch recovery](launch-recovery.md).

## Prepare and Apply

Prepare is read-only. It returns the exact subject schema, subject, plan, and
trust digests, a preview, current observations, and required actions. Apply
accepts the original Prepare request, the returned subject schema and digest,
and one explicit decision for each required action. It recomputes the subject
under that schema and fails closed if state changed.

For a capture-mode participant planned fresh, Prepare runs the provider
identity probe after the executable-containment check, and otherwise writes
nothing. Each provider probes with its own arguments: Codex runs `--version`,
`--help`, and `resume --help`; Cursor runs `--version`, `--help`, and
`create-chat --help`. Prepare refuses only that seat, with `planned_outcome`
`unsupported`, when the provider is project-contained
(`provider_project_contained`, and the probe never runs) or its version is
outside the capture range (`capture_version_unsupported`: Codex below its
floor, Cursor other than its exact version). The other seats still plan, and a
`stale_conversation` action for the refused seat stays.

`SubjectSchemaV2` is the schema for new Prepare calls. A caller copies the
returned schema and never omits the field to select legacy behavior. An Apply
request with no `subject_schema` is read as `SubjectSchemaV1` and reports
`reprepare_recommended` in the result hints. Both constants live in
`internal/launch/prepare.go`.

Schema 1 rules:

- A participant-only Apply stays provisionable.
- If any participant is runnable, Apply returns `action_required` with
  `reason_code=reprepare_required` before launch mutation. Prepare and Apply
  again with schema 2.
- Schema 1 rejects an explicit `placement` and `on_live: keep`.

### Request fields

`placement`: omission keeps the selected backend's default layout, which
still appears in `PreviewV1.placement.effective`. An explicit tuple that the
backend cannot realize returns `outcome: unsupported` and
`reason: placement_unsupported`, with no planned backend mutation. Schema 2
binds the preview into `subject_digest`. One refused seat cohort-blocks the
creation of every missing seat.

`TargetV1.base_root`: omission keeps the exact-root behavior. When present,
only the exact `project_root/.amqrc` authorizes it.

- The value is the configured root or one direct child, and `session_root` is
  the direct child named by `session`.
- Prepare creates no directories. A missing authorized base appears as a
  deterministic `create_base_root` entry in `planned_writes`.
- Apply revalidates the config and parent identity, then creates the base and
  session exclusively with mode `0700`.
- A sibling, nested root, symlink, alternate spelling, or changed authority
  returns a typed refusal with no launch mutation.

`on_live` is `keep` or `refuse`. Omission and `refuse` refuse the whole
binding when a seat is live and stay digest-stable. `keep` on a proven-owned
live seat keeps that process and lets Apply create missing seats in the same
tmux omitted-placement session. Hostile live resources refuse even with
`keep`.

`caller_context` is an opaque correlation map.

- Limits are the `MaxCallerContext*` constants in
  `internal/launch/caller_context.go`: entries, key bytes (at least 1),
  value bytes, and total bytes for all keys plus values.
- NUL, invalid UTF-8, and duplicate JSON keys are rejected.
- Canonical key ordering enters subject schema 2 and immutable evidence,
  never the plan or trust digest.
- Apply echoes the request map. Inspect, Focus, and Close load it from the
  proven-owned binding; a lifecycle request cannot replace it.

### Go example

```go
request := launchapi.PrepareRequestV1{
	RequestVersion: launchapi.RequestVersionV1,
	Target: launchapi.TargetV1{
		ProjectRoot: "/workspace/project",
		BaseRoot:    "/workspace/project/.agent-mail/profile-a",
		SessionRoot: "/workspace/project/.agent-mail/profile-a/collab",
		Session:     "collab",
	},
	Launcher: "tmux",
	Placement: &launchapi.PlacementV1{
		Target: launchapi.PlacementCurrentWindow, Layout: launchapi.PlacementColumns, LauncherPane: "%17",
	},
	CallerContext: map[string]string{
		"run_id": "run-42", "task_generation": "3",
	},
	Intent: intent,
}

prepared, err := launchapi.Prepare(ctx, request)
if err != nil {
	return err
}

decisions := make([]launchapi.DecisionV1, 0, len(prepared.RequiredActions))
for _, action := range prepared.RequiredActions {
	choice, ok := reviewedChoiceFor(action)
	if !ok {
		return fmt.Errorf("no reviewed decision for %s", action.ActionID)
	}
	decisions = append(decisions, launchapi.DecisionV1{
		ActionID: action.ActionID,
		Choice:   choice,
	})
}

result, err := launchapi.Apply(ctx, launchapi.ApplyRequestV1{
	RequestVersion: launchapi.RequestVersionV1,
	Prepare:        request,
	SubjectSchema:  prepared.SubjectSchema,
	SubjectDigest:  prepared.SubjectDigest,
	Decisions:      decisions,
})
```

`reviewedChoiceFor` is caller-owned. AMQ does not choose trust,
stale-conversation, rebind, or degraded-capability decisions for the caller.

### Results

Apply and lifecycle results carry a typed mutation disposition
(`not_applied`, `committed`, or `uncertain`) and, after a launch commit, its
binding generation. These fields describe the launch backend binding only, not
session or roster durability. A post-commit failure is an action-required
result with exit code `6`, not a bare error.

## CLI

The CLI runs the same flow:

```bash
amq launch --plan intent.json --prepare --json --launcher commands
amq launch --apply apply-request.json --json
amq launch --request prepare-request.json --json
```

All three commands accept `-` for standard input.

- `--plan` is the intent-only convenience. Discovery resolves the target
  (project root, default session, session root, and local launcher
  preference) from the committed project config, and `--plan` carries only
  the `LaunchIntentV1`.
- `--apply` and `--request` carry the complete target and launcher in the
  document, so `--session`, `--root`, `--launcher`, and `--placement` are not
  valid with either.
- `--request` decodes a full `PrepareRequestV1` through
  `DecodePrepareRequestV1` and runs the same Prepare and Apply path as
  `--plan`. Every field a package caller can set is reachable from the CLI.
  It is mutually exclusive with `--plan`, `--apply`, and `--placement`.
- `--prepare` pairs with `--plan` or `--request` to run Prepare only, with no
  mutation, so a consumer can read the subject digest before Apply.

Exit code `6` means the JSON result requires an operator action. JSON on
stdout is the machine contract; stderr is for people and is not parseable.
