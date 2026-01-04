# Agent Message Queue (AMQ)

**The missing coordination layer for multi-agent development.**

When you're running Claude Code and Codex CLI in parallel—reviewing each other's work, dividing tasks, iterating on implementations—how do they talk to each other? AMQ solves this.

## Why AMQ?

Modern AI-assisted development often involves multiple agents working on the same codebase. But without coordination:
- Agents duplicate work or create conflicts
- Reviews require human intermediation
- Context switching kills productivity

AMQ enables **autonomous multi-agent collaboration**: agents message each other directly, request reviews, share status, and coordinate work—all without a server, daemon, or database.

### Key Features

- **Zero infrastructure** — Pure file-based. No server, no daemon, no database. Works anywhere files work.
- **Crash-safe** — Atomic Maildir delivery (tmp→new→cur). Messages are never partially written or lost.
- **Human-readable** — JSON frontmatter + Markdown body. Inspect with `cat`, debug with `grep`, version with `git`.
- **Real-time notifications** — `amq wake` injects terminal notifications when messages arrive (experimental).
- **Built for agents** — Priority levels, message kinds, threading, acknowledgments—all the primitives agents need.


<video src="docs/assets/demo.mp4" controls playsinline></video>


## Installation

### 1. Install Binary (macOS/Linux)

```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
```

Installs to `~/.local/bin` or `~/go/bin` (no sudo required). Verify: `amq --version`
Review the script before running; it verifies release checksums when possible.

### 2. Install Skill

**Claude Code** (via marketplace):
```
/plugin marketplace add avivsinai/skills-marketplace
/plugin install amq-cli@avivsinai-marketplace
```

**Codex CLI** (Codex chat command; not a shell command):
```
$skill-installer install https://github.com/avivsinai/agent-message-queue/tree/main/skills/amq-cli
```
Restart Codex after installing.

For manual download, build from source, or troubleshooting, see [INSTALL.md](INSTALL.md).

## Quick Start

### 1. Initialize Project

```bash
amq init --root .agent-mail --agents claude,codex
```

### 2. Start Agent Sessions

**Terminal 1 — Claude Code:**
```bash
export AM_ME=claude AM_ROOT=.agent-mail
amq wake &
claude
```

**Terminal 2 — Codex CLI:**
```bash
export AM_ME=codex AM_ROOT=.agent-mail
amq wake &
codex
```

### 3. Send & Receive

```bash
# Send a message
amq send --to codex --subject "Review needed" --kind review_request \
  --body "Please review internal/cli/send.go"

# Check inbox
amq list --new

# Filter by priority or sender
amq list --new --priority urgent
amq list --new --from codex --kind review_request

# Read all messages (one-shot, moves to cur, auto-acks)
amq drain --include-body

# Reply to a message
amq reply --id <msg_id> --kind review_response --body "LGTM with comments"
```

## Message Kinds & Priority

AMQ messages can include metadata for smart agent handling:

| Kind | Reply Kind | Use Case |
|------|------------|----------|
| `review_request` | `review_response` | Code review requests |
| `question` | `answer` | Questions needing answers |
| `decision` | — | Design decisions |
| `brainstorm` | — | Open-ended discussion |
| `status` | — | FYI updates |
| `todo` | — | Task assignments |

| Priority | Agent Behavior |
|----------|----------------|
| `urgent` | Interrupt current work, respond immediately |
| `normal` | Add to TODO list, respond after current task |
| `low` | Batch for end of session |

## Co-op Mode

For real-time Claude Code + Codex CLI collaboration, see [COOP.md](COOP.md).

**One-liner setup:**
```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/setup-coop.sh | bash
```

## How It Works

AMQ uses the battle-tested [Maildir](https://cr.yp.to/proto/maildir.html) format:

1. **Write** — Message written to `tmp/` directory
2. **Sync** — File fsynced to disk
3. **Deliver** — Atomic rename to `new/` (never partial)
4. **Process** — Reader moves to `cur/` after reading

This guarantees crash-safety: if the process dies mid-write, no corrupt message appears in the inbox.

```
.agent-mail/
├── agents/
│   ├── claude/
│   │   ├── inbox/{tmp,new,cur}/   # Incoming messages
│   │   ├── outbox/sent/           # Sent copies (audit trail)
│   │   ├── acks/{received,sent}/  # Acknowledgments
│   │   └── dlq/{tmp,new,cur}/     # Dead letter queue
│   └── codex/
│       └── ...
└── meta/config.json               # Agent registry
```

## Documentation

- [INSTALL.md](INSTALL.md) — Alternative installation methods
- [COOP.md](COOP.md) — Co-op mode protocol & best practices
- [CLAUDE.md](CLAUDE.md) — Agent instructions, CLI reference, architecture

## Development

```bash
git clone https://github.com/avivsinai/agent-message-queue.git
cd agent-message-queue
make build   # Build binary
make test    # Run tests
make ci      # Full CI: vet + lint + test + smoke
```

## FAQ

**Why not just use a database?**
Files are universal, debuggable, and work everywhere. No connection strings, no migrations, no ORM. Just files.

**Why not Redis/RabbitMQ/etc?**
Those require infrastructure. AMQ is for local inter-process communication where agents share a filesystem. No server to configure or keep running.

**What about Windows?**
The core queue works on Windows. The `amq wake` notification feature requires WSL.

**Is this production-ready?**
For local development workflows, yes. AMQ is intentionally simple—it's not trying to be a distributed message broker.

**How does AMQ compare to MCP Agent Mail or Gas Town?**

All three solve agent coordination, but for different use cases:

- **[MCP Agent Mail](https://github.com/Dicklesworthstone/mcp_agent_mail)** is a server-based coordination stack with shared inboxes, file-reservation leases, Git-backed archives, and SQLite/FTS search. It also has a commercial iOS companion for remote oversight. Best when you want centralized coordination, search, and mobile monitoring.

- **[Gas Town](https://github.com/steveyegge/gastown)** is a larger-scale orchestration system built around tmux and the Beads data plane, with multiple agent roles (Mayor, Witness, Refinery). Aimed at managing many parallel agents with richer orchestration primitives.

- **AMQ** is intentionally minimal: a single binary, local file queue, Maildir-style atomic delivery, no server/database/daemon. Best for 2–3 agents on one machine when you want something you can understand and debug in minutes.

Other multi-agent orchestration frameworks exist (e.g., [Claude-Flow](https://github.com/ruvnet/claude-flow), [ccswarm](https://github.com/nwiizo/ccswarm)) with broader automation and agent-pool features. AMQ stays intentionally small.

## License

MIT
