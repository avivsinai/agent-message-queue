# Public Launch API

AMQ exposes a versioned launch contract for tools that must plan and apply a
session without parsing human output. The Go package is `launchapi`. The JSON
contract is [schemas/launch-api-v1.schema.json](../schemas/launch-api-v1.schema.json).

The current contract is `0.61.1`. A caller must negotiate its
required contract range, intent version, result version, and features before it
depends on them:

```go
negotiated, err := launchapi.Negotiate(launchapi.RequirementV1{
	ContractSemver: ">=0.61.0 <0.62.0",
	IntentVersion:  launchapi.IntentVersionV1,
	ResultVersion:  launchapi.ResultVersionV1,
	Features:       []string{"prepare_apply_v1"},
})
if err != nil {
	return err
}
_ = negotiated
```

A `0.61.1` binary advertises `placement`, `initial_input`, `base_root`,
`on_live`, `caller_context`, and `executable_identity`. It can still omit later
Wave B3 feature IDs while those beads are incomplete. A caller must require
every feature it uses. Contract semver alone does not claim that an unadvertised
feature is available.

`PreviewV1.capabilities` reports the selected providers' static adapter grammar
without executing a caller-supplied provider. `grammar_version` is the
adapter-owned version that consumers compare. It changes when the allowed
argument forms, configuration overrides, or carrier support changes.
`verified_provider_version` is informational and names the provider release on
which that grammar was verified; it does not claim the installed version.
Runtime provider identity is bound separately before Apply executes it.

The initial-input carrier is typed. Claude and Codex currently advertise
`argument`; AMQ appends its exact text as the final provider argv element.
`stdin` and `file` remain typed but unadvertised and return
`initial_input_unsupported`. Content is limited to 262,144 UTF-8 bytes without
NUL. Content changes the plan and subject digests but not the trust digest.
Claude admits one `--allowedTools <comma-separated-list>` pair. Codex admits
ordered `-c model_reasoning_effort=<value>` pairs with values `minimal`, `low`,
`medium`, `high`, or `xhigh`; duplicate keys and unknown keys or values reject.

## Intent

The intent owns the desired participants. Discovery owns the project root,
default session, session root, and local launcher preference. A public intent
does not replace committed project configuration.

```json
{
  "intent_version": 1,
  "participants": [
    {
      "handle": "claude",
      "runnable": true,
      "executable": "claude",
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

`resume_policy` accepts exactly `resume`, `fresh`, or `disabled`.

The v1 adapter set supports Claude Code, Codex CLI, and Cursor CLI. Claude Code
mints its session ID from the launch nonce. Codex CLI `0.147.0` reports its
provider-owned thread ID after a completed turn through its legacy `notify`
hook. AMQ adds one static hook override that is bound to the session root,
handle, launch nonce, and AMQ executable. The private hook validates the exact
ticket and payload, persists immutable evidence, and only then publishes the
conversation identity. It forwards the identical payload to the operator's
configured Codex notify command after publication; a missing command is a
no-op, and a forwarding failure cannot undo or fail the evidence path. An
unused Codex launch remains pending and cannot be resumed. Cursor CLI acquires
its provider-owned chat ID before process start through `create-chat` at exact
version `2026.08.11-e8db854`. Cursor's provider identity is `cursor-agent`.

The registered launcher backends are `commands`, `tmux`, `cmux`, and
`ghostty`. `--launcher auto` walks the local preference; an explicit
`--launcher <name>` wins. When `CMUX_SURFACE_ID` is set, auto prepends `cmux`
ahead of `ghostty`; otherwise `TERM_PROGRAM=ghostty` prepends `ghostty`.
Selection still requires Detect Available. Cmux Create uses `--focus false`
and restores prior selection; see [Managed launch recovery](launch-recovery.md).

The tier-1 provider smokes are opt-in and skip unless the env is `1`. The Codex
smoke first proves that an unused launch stays pending and returns AMQ's typed
stale-conversation action. It then sends one fixed prompt through the managed
pane, waits for notify-backed identity publication, and performs one exact
headless resume:

```bash
AMQ_CLAUDE_LIVE=1 go test ./internal/launch -run TestClaudeLiveManagedMintResumeAndCrashReuse -count=1 -v
AMQ_CODEX_LIVE=1 go test ./internal/launch -run TestCodexLiveManagedAcquireResumeAndCrashReuse -count=1 -v
AMQ_CURSOR_LIVE=1 go test ./internal/launch -run TestCursorLiveResumeManagedExecutionAndCrashReuse -count=1 -v
```

Managed launcher live proofs skip unless the env is `1`. Run them from a shell
inside the matching surface:

```bash
AMQ_CMUX_LIVE=1 go test ./internal/launch -run TestCmuxLive -count=1 -v
AMQ_GHOSTTY_LIVE=1 go test ./internal/launch -run TestGhosttyLive -count=1 -v
```

The smoke harness disables Claude tools with the CLI-equivalent single argument
`--tools=` and uses
`--permission-mode plan`; it runs Codex with `--sandbox read-only`. These are
harness controls, not additions to the adapter's committed option contract.
The smokes record the exact managed process arguments and provider IDs, require
headless resume to return the requested ID, and stop at the first failure.
Claude persists a minted `--session-id` only after its first turn. The live
smoke proves that an unused mint returns AMQ's typed stale-conversation action,
then bootstraps the same ID with a no-tools turn before it proves exact resume.
It uses an already-trusted checkout as the Claude cwd and keeps its AMQ session
root in a temporary directory; it never changes Claude trust state.

Grok and Pi are excluded because they have no provider CLI; Gemini CLI and
OpenCode also remain outside this adapter set.

## Prepare and Apply

Prepare is read-only. It returns the exact subject schema, subject, plan, and
trust digests, a preview, current observations, and required actions. Apply
accepts the original Prepare request, the returned subject schema and digest,
and one explicit decision for each required action. It recomputes the subject
under that schema and fails closed if state changed.

Omitted `placement` keeps the selected backend's v0.61 layout and still appears
on `PreviewV1.placement.effective`. An explicit tuple the backend cannot
realize returns `outcome: unsupported` and `reason: placement_unsupported`
with zero planned backend mutation. Subject schema 2 binds that preview into
`subject_digest`; schema 1 rejects an explicit placement field.

Omitted `TargetV1.base_root` preserves the v0.61 exact-root behavior. When it
is present, only the exact `project_root/.amqrc` authorizes it: the value must
be the configured root or one direct child, and `session_root` must be the
direct child named by `session`. Prepare does not create directories. A missing
authorized base appears as a deterministic `create_base_root` entry in
`planned_writes`; Apply revalidates the config and parent identity, then creates
the base and session exclusively with mode `0700`. Siblings, nested roots,
symlinks, alternate spellings, or changed authority return a typed refusal with
no launch mutation.

`on_live` is `keep` or `refuse`. Omission and explicit `refuse` keep the v0.61
whole-binding refusal. Explicit `keep` on a proven-owned live seat keeps that
process and lets Apply create missing seats in the same tmux omitted-placement
session. Hostile live resources refuse even with `keep`. Schema 1 rejects
`on_live: keep`; omit and `refuse` stay digest-stable.

`caller_context` is an opaque correlation map. It is limited to 32 entries;
keys are 1 through 64 UTF-8 bytes, values are at most 1,024 UTF-8 bytes, and
all keys plus values are at most 16 KiB. NUL, invalid UTF-8, and duplicate JSON
keys reject. Canonical key ordering enters subject schema 2 and immutable
evidence, but never the plan or trust digest. Apply echoes the request map.
Inspect, Focus, and Close load it from the proven-owned binding; lifecycle
requests cannot replace it.

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

New Prepare calls use subject schema `2`. Apply input serialized by a `0.61.0`
caller has no `subject_schema`; `0.61.1` interprets that omission as schema `1`
and reports `reprepare_recommended` in the result hints. A new caller must copy
the returned schema. It must not omit the field to select legacy behavior.

`reviewedChoiceFor` is intentionally caller-owned. AMQ does not choose trust,
stale-conversation, rebind, or degraded-capability decisions for the caller.

The equivalent CLI split is:

```bash
amq launch --plan intent.json --prepare --json --launcher commands
amq launch --apply apply-request.json --json
```

Both commands accept `-` for standard input. The Apply document contains the
complete target and launcher, so `--session`, `--root`, and `--launcher` are not
valid with `--apply`. Exit code `6` means the JSON result requires an operator
action. JSON on stdout remains the machine contract; stderr is for people and
must not be parsed.
