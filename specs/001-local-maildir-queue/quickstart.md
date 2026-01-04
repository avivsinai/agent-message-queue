# Quickstart

## Initialize

```
amq init --root .agent-mail --agents codex,claude
```

## Send

```
amq send --to claude --subject "Review notes" --thread p2p/claude__codex --body @notes.md
amq send --to claude --body "Quick ping"
```

## List / Read / Ack

```
amq list --me claude --new
amq read --me claude --id <msg_id>
amq ack  --me claude --id <msg_id>
```

## Thread

```
amq thread --me codex --id p2p/claude__codex --limit 50
```

## Presence (optional)

```
amq presence set --me codex --status busy
amq presence list
```

## Cleanup

```
amq cleanup --tmp-older-than 36h
amq cleanup --tmp-older-than 36h --dry-run
```
