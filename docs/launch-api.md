# Public Launch API

AMQ exposes a versioned launch contract for tools that must plan and apply a
session without parsing human output. The Go package is `launchapi`. The JSON
contract is [schemas/launch-api-v1.schema.json](../schemas/launch-api-v1.schema.json).

The current compatibility floor is `0.61.0`. A caller must negotiate its
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
version `2026.08.11-e8db854`.

The tier-1 provider smokes are opt-in. The Codex smoke first proves that an
unused launch stays pending and returns AMQ's typed stale-conversation action.
It then sends one fixed prompt through the managed pane, waits for notify-backed
identity publication, and performs one exact headless resume:

```bash
AMQ_CLAUDE_LIVE=1 go test ./internal/launch -run TestClaudeLiveManagedMintResumeAndCrashReuse -count=1 -v
AMQ_CODEX_LIVE=1 go test ./internal/launch -run TestCodexLiveManagedAcquireResumeAndCrashReuse -count=1 -v
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

Prepare is read-only. It returns the exact subject, plan, and trust digests, a
preview, current observations, and required actions. Apply accepts the original
Prepare request, the returned subject digest, and one explicit decision for
each required action. It recomputes the plan and fails closed if state changed.

```go
request := launchapi.PrepareRequestV1{
	RequestVersion: launchapi.RequestVersionV1,
	Target: launchapi.TargetV1{
		ProjectRoot: "/workspace/project",
		SessionRoot: "/workspace/project/.agent-mail/collab",
		Session:     "collab",
	},
	Launcher: "commands",
	Intent:   intent,
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
	SubjectDigest:  prepared.SubjectDigest,
	Decisions:      decisions,
})
```

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
