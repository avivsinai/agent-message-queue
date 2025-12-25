# Quickstart

## Initialize

```
amq init --root .agent-mail --agents codex,cloudcode
```

## Send

```
amq send --to cloudcode --subject "Review notes" --thread p2p/codex__cloudcode --body @notes.md
amq send --to cloudcode --body "Quick ping"
```

## List / Read / Ack

```
amq list --me cloudcode --new
amq read --me cloudcode --id <msg_id>
amq ack  --me cloudcode --id <msg_id>
```

## Thread

```
amq thread --me codex --id p2p/codex__cloudcode --limit 50
```

## Presence (optional)

```
amq presence set --me codex --status busy
amq presence list
```

## Cleanup

```
amq cleanup --tmp-older-than 36h
```
