---
title: Tutorial 04 - Agent-to-Agent Communication
sidebarTitle: 04 - Communication
description: How agents coordinate through mail, slung work, and hooks — without direct connections.
---

Earlier tutorials had _you_ talking to agents — peeking at output, nudging
sessions. This one covers how agents talk to _each other_. To follow along you
need `my-city` running with `my-project` rigged and agents for `mayor` and
`reviewer` (the setup from [Tutorial 03](/tutorials/03-sessions)).

## Agents coordinate only through the store

Agents never reference each other. No function calls, no shared memory, no
handles — each session is its own process with its own terminal, history, and
provider. They coordinate through two indirect channels: **mail** (messages) and
**slung work** — `gc sling` (delegated tasks). In both, the sender names a
destination and Gas City routes it; the sender never holds a reference to who
receives it.

![Three agents — mayor, reviewer, worker — drawn as disconnected boxes, each connected only to a central bead store. No arrow links two agents directly; mail is a bead and an inbox is a query for it.](/diagrams/excalidraw-rendered/coordination-through-store.svg)

That indirection is the point. The mayor slings work to `my-project/reviewer`
without knowing whether one reviewer session exists or five, whether it runs on
Claude or Codex, or whether it's active or idle. Work and messages persist in the
store; sessions come and go independently — they can run, idle, restart, and
scale without breaking anyone's reference, because there are none.

## Mail is a persistent, tracked message

Mail creates a message the recipient picks up on its next turn. It contrasts
with nudge from Tutorial 03:

| | Mail | Nudge |
| --- | --- | --- |
| Carrier | A bead in the store | Terminal input |
| Survives a crash | Yes | No |
| Subject line | Yes | No |
| Wakes the recipient | On request from the sender or the recipient's configuration | Yes |
| State | Stays unread until processed | Fire-and-forget |

Send mail to the mayor:

```shell
~/my-city
$ gc mail send mayor -s "Review needed" -m "Please look at the auth module changes in my-project"
Sent message mc-msg-8t8 to mayor
```

`gc mail send` takes the recipient as a positional argument and the subject/body
via `-s`/`-m` flags. (You can also pass just `<to> <body>` with no subject.)

Mail does not create wake demand by itself. Add `--notify` to request a turn for
the recipient even when earlier mail is still unread. In a managed city, that
request can wake a non-running recipient; an unmanaged city queues the nudge for
later delivery; without a city store the nudge is skipped.

To request a turn when selected agents send or reply locally, set
`mail_wake_from` on the receiving agent. It defaults to an empty list. Use
qualified agent names, as in `depends_on`, or `human` for local commands sent
as human. The next mail command reads any changes. Processes that can edit
the city's configuration can change the list. A match identifies the agent
name used by a local command. Any process inside the city can send under that name.

| Recipient of listed mail | What happens |
| --- | --- |
| Human | The mail goes to the inbox. |
| Closed, closing, or archived | The record keeps its current state. |
| Held, quarantined, suspended, drained, waiting, or preparing to sleep | The mail stays in the inbox. The block stays in effect, even if the session is still running. |
| Running and unblocked | An idle session receives a reminder. A busy session receives it when idle if the mail is still unread. |
| Stopped with `mail_wake_start = false` (the default) | Store the mail and leave the session stopped. |
| Stopped outside a configured named session | Store the mail and leave the session stopped. |
| Stopped in an unmanaged city | Store the mail and leave the session stopped. |
| Stopped with an existing record outside ordinary sleep | Preserve the current state and sleep reason. |
| Eligible for a start, in ordinary sleep | Queue the reminder and ask the orchestrator to start the session. Ordinary sleep has an empty reason, `idle`, `idle-timeout`, or `no-wake-reason`. |
| Eligible for a start, with no existing or closed record | Create the named session through its usual start path, record the turn, and queue the reminder. |

A start requires `mail_wake_start = true` on the recipient, a configured
named session as the target, and a managed orchestrator.

Each sender session can request three reminders per recipient in 15 minutes.
Sends as `human` share one allowance. During that window, a session's reply
to the sender that woke it goes to the inbox without requesting another turn.
In a longer loop the fourth reminder from the same sender is suppressed.
An explicit `--notify` takes precedence and uses none of this allowance.

Check for unread mail:

```shell
~/my-city
$ gc mail check mayor
1 unread message(s) for mayor
```

See the inbox:

```shell
~/my-city
$ gc mail inbox mayor
ID          FROM   SUBJECT        BODY
mc-msg-8t8  human  Review needed  Please look at the auth module changes in my-project
```

`gc mail inbox` lists only unread messages, so there's no STATE column —
everything listed is unread by definition.

If you want to see the mayor react right away in `peek` or `logs`, give it a
turn:

```shell
~/my-city
$ gc session nudge mayor "Check mail and hook status, then act accordingly"
Nudged mayor
```

(As in Tutorial 03, you may see `Queued nudge for mayor` instead — both
confirm the nudge is on its way.)

The nudge doesn't deliver the mail — it only starts a new turn. The delivery
happens via a hook: on each turn, `gc mail check --inject` runs and any unread
mail appears as a system reminder in the agent's context. So the mayor never
checks its inbox manually; the hook surfaces mail, and the nudge tells it to act
on what it finds.

## Slung work delegates a task without naming a session

Once the mayor takes a turn, it reads your mail, decides the reviewer should
handle it, and slings the work:

```shell
~/my-city
$ gc session peek mayor --lines 6
[mayor] Got mail: "Review needed" — auth module changes in my-project
[mayor] Routing to my-project/reviewer...
[mayor] Running: gc sling my-project/reviewer "Review the auth module changes"
```

(The above is illustrative — `peek` returns the actual terminal contents of the
session, so you'll see whatever the agent has rendered, not Gas City–formatted
lines.)

The mayor slung a bead to the rig-scoped `my-project/reviewer` agent and Gas
City resolved the rest: it woke the reviewer if asleep, or routed to an
available session if several existed. The mayor describes the work and slings
it; routing is not its problem.

This is the pattern that scales: a human mails the mayor, the mayor plans and
slings tasks to agents, agents do the work and close their beads — every hop
through the store.

## Hooks wire a bare provider into Gas City

Without hooks, a session is just a provider process — Claude in a terminal, with
no awareness of Gas City. Hooks wire the provider's event system in so agents
receive mail, pick up slung work, and drain queued nudges automatically.

`gc init` wires Claude's hooks for you: it writes a managed `.gc/settings.json`
that Claude reads on every session start. No TOML in `pack.toml` or `city.toml`
is needed for the default behavior — `grep install_agent_hooks` in a fresh city
turns up nothing.

Claude is the only provider wired automatically. To run an agent on a different
provider — say you moved the mayor to Codex — list that provider in
`install_agent_hooks` on the agent, and Gas City installs its hook files into the
agent's working directory:

```toml
# agents/mayor/agent.toml — install hook files for this agent's provider
install_agent_hooks = ["codex"]
```

Agent-local overrides live in `agents/<name>/agent.toml`. (You can also set
`install_agent_hooks` under `[workspace]` in `city.toml` as a city-wide
default, but that spelling is deprecated — config load warns and points you
at the per-agent form — and an agent-level list replaces the workspace one
rather than adding to it.)

Either way, once a session starts Gas City installs the hook settings the
provider reads. For Claude that's `.gc/settings.json`, which fires Gas City
commands at key moments — session start, before each turn, and right before the
provider compacts its context. Those commands surface pending work, deliver
mail, drain queued nudges, and save a handoff before a context cycle. Without
them you'd run `gc mail check` and `gc prime` by hand on every agent.

## What's next

You've seen the two coordination mechanisms — mail for messages and slung beads
for work — and the hook infrastructure that wires it all together. From here:

- **[The six primitives](/getting-started/how-gas-city-works)** — the canonical model mail,
  slung work, and hooks build on
- **[Formulas](/tutorials/05-formulas)** — how multi-step work should be
  done: steps, dependencies, and variables
- **[Beads](/tutorials/06-beads)** — the work tracking system underneath it all
