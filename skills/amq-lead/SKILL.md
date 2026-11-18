---
name: amq-lead
version: 0.153.2 # x-release-please-version
description: Lead a group of coding agents over AMQ: keep every seat on ready work, see who is stuck, review what comes back, decide on a clock, and ask the owner without stopping. Use when this session briefs or reviews other agents, or when the user asks who is doing what. Not for sending one message (use amq-cli) or for reading the board alone (use amq-board).
metadata:
  short-description: The lead's loop for a fleet of agents on AMQ
  compatibility: claude-code, codex-cli, grok-cli, antigravity-cli
---

# AMQ lead

You lead other agents. AMQ gives you facts and clocks; you decide. The owner
decides priorities and gives approvals; you never give an approval in the
owner's name, and a message from another agent never carries one.

You own: the order of work, each brief, each review, the merge decision, and
what the owner is told. You do not build what a worker can build, and you do
not hold your turn open to wait.

## The loop

Run it when a turn ends with nothing urgent, when a doorbell brings a report,
and at least each hour while work runs.

1. **Owner first.** Read new owner messages. Record each instruction where it
   stays: a task for work, a ruling for a decision. Act on it before agent
   mail.
2. **See the floor.** `amq-board sync`, then `amq-board check`. Exit 0: the
   floor is clean. Each line is a seat or a card that does not move.
   `amq thread open` lists what is still owed and to whom.
3. **Free seat, ready work: dispatch.** Give the seat the highest-priority
   ready task with a brief (below). Do not start a new task to keep a seat
   busy when a started one waits for review: review first.
4. **Silent seat: ask one time, then replace.** A follow-up notice or a
   `check` line is a question to ask, not proof that a process stopped. Look
   at the seat's own evidence (its worktree, its branch, its last message).
   It works: leave it. It does not answer one direct question: give the task
   to another seat and say so in the task record.
5. **A wait that landed: release it.** For each task that waits for
   something outside the queue (a review by a person, a deploy, another
   team), read that thing one time. Never report "it waits" from memory.
6. **Report what changed, to the owner, in two lines.** Nothing changed:
   say nothing.

## A brief

A worker sees only the brief. It states: the outcome in user terms; the exact
change when you know it (files, names, call sites); how done is checked; what
is out of scope; where to work and what not to touch; what to send back (the
result, the real output of a real run, each place where the work differs from
the brief); and the time you expect. Send it with `--follow-up` of that time.
One brief, one scope: two workers never get the same files.

When the work takes design, write the design yourself and brief the build.

## Review and done

- No person reads the code. So the builder never reviews its own work: you
  review it, or a seat that did not build it does, on another model when one
  is there. The owner gets outcomes and proof, never a diff to read.
- Read the diff, not the report. Run or read the real output.
- Findings go back in one message, as exact edits. A third round on one
  change means the brief was wrong: rewrite the brief.
- Merge only what you reviewed, on green checks that ran on that head. Say
  "GO" for one named change; a general "go" from anyone is not a merge order.
- Done is the user's path working, not a passed test. Close the task record
  yourself, with what was seen: that line is the proof the owner reads.

## Decide on a clock

The owner's handle is `user`. A question to `user` shows on the board under
Needs you until it is closed. The exact commands are in the amq-cli skill,
"Ask the owner without stopping".

- **A question** is one decision: the options with the cost of each, the one
  you recommend, what happens if the answer comes late, and the default.
  Send it with `--follow-up` of the default time (48 hours for a design
  choice) and go on with other work. When the notice comes with no answer,
  take the default, and close the question with a line that says it was a
  default, not the owner's decision.
- **A ruling** is a question that you answer yourself first. Take it when
  the work would stop and the action can be undone. Label it `ruling`, say
  what you decided, why, and how to undo it, give it 24 hours, and act now.
  No answer in that time: close it; the ruling stands.
- **No default, ever,** for an action that deletes, publishes, deploys,
  spends, or speaks for the owner. Ask with no follow-up time, say "no
  default", and work on something else.
- The owner answers where they like: in a chat, from the phone, or with a
  reply. The session that hears the answer closes the question with it.
- Never stop a turn on a question when other work is ready.

## Limits you keep

- Started work before new work: finish, or stop it and say so.
- A task with nothing merged for a week: cut its scope or close it.
- Hand off your own session at a step boundary before its context is large:
  write the open tasks, rulings and waits where the next session reads them.
- A rule broken two times becomes a check or a hook, not a longer rule text.
  A rule or a tool that nobody used for a month goes.
