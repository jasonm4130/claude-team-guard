# claude-team-guard

[![CI](https://github.com/jasonm4130/claude-team-guard/actions/workflows/ci.yml/badge.svg)](https://github.com/jasonm4130/claude-team-guard/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/jasonm4130/claude-team-guard?sort=semver)](https://github.com/jasonm4130/claude-team-guard/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/jasonm4130/claude-team-guard)](go.mod)
[![License](https://img.shields.io/github/license/jasonm4130/claude-team-guard)](LICENSE)

A [Claude Code](https://claude.com/claude-code) hook that stops [agent-team](https://code.claude.com/docs/en/agent-teams) work from silently going missing. It is a single Go binary with no dependencies and no configuration. It reads the hook payload on stdin and always fails open.

If you run agent teams and keep finding teammates sitting idle after saying "Waiting for CI", or getting back "ended without delivering a report" from a helper subagent, this hook is for you.

## The two bugs

### 1. Idle teammates aren't woken by their own background tasks

A teammate starts a background Bash command or a Monitor, such as a CI watch, and ends its turn. The tool result told it "You will be notified when it completes". It isn't. Claude Code queues the completion notice, but nothing starts a turn for an idle teammate. The notice is delivered only when something else, usually the lead, sends the teammate a message. The main session isn't affected.

Upstream: [anthropics/claude-code#77300](https://github.com/anthropics/claude-code/issues/77300)

**What the hook does:** on `TeammateIdle`, it reads the teammate's transcript and lists the background tasks the teammate has launched but never seen complete. If there are any, the hook exits 2, so the teammate keeps working, and the teammate is told:

```
Don't go idle yet: these background tasks of yours haven't reported back:
- bwait (output: /tmp/…/tasks/bwait.output)
You are a teammate, and an idle teammate is NOT woken when its background tasks finish …
Wait in the foreground instead: a Bash call with timeout 600000 that blocks until the work is done …
If the wait will run past about 30 minutes, SendMessage team-lead … then stop.
```

It blocks at most twice for the same set of tasks, then lets the teammate idle. It can't trap one.

It also lets the teammate idle whenever a message is waiting in its team inbox. A teammate only receives messages between turns. Without this check, a block sends it back into a foreground wait while the lead's message sits undelivered: in real transcripts, 11 lead messages waited through a block this way, some for hours. Letting the teammate idle delivers the message at once. A yield doesn't count against the two blocks.

### 2. A teammate's helper subagent loses its report

When a teammate spawns a subagent with the Agent tool, the subagent's `SubagentHandback` is refused with "Nothing was sent: the agent that spawned you is no longer running". This happens while the teammate is alive and waiting. The teammate gets back "The subagent ended without delivering a report … Its unsent text is not shown". If it asks the helper to resend with `SendMessage`, the report is lost again.

Upstream: [anthropics/claude-code#96225](https://github.com/anthropics/claude-code/issues/96225) (#99651 was closed as its duplicate)

**What the hook does:** on `PostToolUse` for `Agent` or `SendMessage`, when the result says the report was lost, the hook reads the report from the helper's own transcript. It uses the last `SubagentHandback` message, or else the last text reply. It hands that report to the teammate as `additionalContext`.

### 3. A teammate only reads the lead's messages between turns

A message sent to a busy teammate with `SendMessage` waits until the teammate's turn ends. In the transcripts, all 102 lead messages to teammates arrived only after a turn ended. In 35 of them, the teammate made a median of 39 tool calls before seeing the message, so corrections like "rescope" landed after the work was done. A background subagent with no team name gets the same message mid-turn, at its next tool call.

**What the hook does:** on `PostToolUse` for any tool, when the agent is an in-process teammate, it reads the teammate's team inbox. Any unread message the teammate hasn't been shown is handed over as `additionalContext`, so the teammate sees it after its current tool call. Each message is shown once. Structured protocol messages, such as shutdown requests, are left for Claude Code to deliver. The teammate is told that the message will arrive again when its turn ends, and that the second copy is a duplicate. A long foreground command still holds the message until it returns.

## Does it matter?

These numbers come from ten days of real agent-team transcripts on one machine, 2026-11-05 to 2026-11-15, Claude Code 2.1.316 to 2.1.325:

| | |
|---|---|
| Background tasks that finished while their teammate was idle | 57 |
| …delivered only when something else woke the teammate | 56, median 42 min late, worst 26 h |
| Foreground helper subagents spawned by teammates (2.1.323+) | 42 |
| …whose report was lost | 41 |
| Lost reports this hook recovers when replayed over those transcripts | 44 of 44 (41 `Agent`, 3 `SendMessage` retries) |

**Live check (Claude Code 2.1.325):**
- A teammate that started `sleep 40` in the background and tried to idle was held by the hook. It waited in the foreground and reported back unprompted.
- A teammate whose helper's handback was refused received the helper's exact report through the hook.

## Install

Download a binary from [the latest release](https://github.com/jasonm4130/claude-team-guard/releases/latest) and check it against `checksums.txt`:

```sh
tar -xzf claude-team-guard_*_darwin_arm64.tar.gz
install -m 755 claude-team-guard_*/claude-team-guard ~/.local/bin/
```

Or build it from source, which needs only a Go toolchain:

```sh
go install github.com/jasonm4130/claude-team-guard@latest
```

Then register both hooks in `~/.claude/settings.json`:

```json
"hooks": {
  "TeammateIdle": [
    { "hooks": [ { "type": "command", "command": "~/.local/bin/claude-team-guard" } ] }
  ],
  "PostToolUse": [
    { "matcher": "*",
      "hooks": [ { "type": "command", "command": "~/.local/bin/claude-team-guard" } ] }
  ]
}
```

Check the install:

```sh
claude-team-guard --version
echo '{"hook_event_name":"TeammateIdle"}' | claude-team-guard; echo "exit $?"   # exit 0, no output
```

## How it works

- **One binary for every event.** It dispatches on `hook_event_name` and ignores any event it doesn't handle.
- **It finds the transcripts by itself.** Teammates and their helpers write to `<session>/subagents/agent-*.jsonl` beside the lead's transcript. The hook locates the right file from `transcript_path`, using `agent_id` or `teammate_name` when it needs to.
- **Pending tasks are matched on the text Claude Code writes.** A task counts as launched when its tool result says `Command running in background with ID: …` or `Monitor started (task …`. It counts as seen once a `<task-id>` completion notice arrives, or once the teammate itself stops it with `TaskStop` or `KillShell`.
- **It defers to the lead.** Before blocking, it reads `<config>/teams/<team>/inboxes/<teammate>.json`. The team comes from the payload's `team_name`. Failing that, the hook reads it from the teammate's spawn result in the lead's transcript and caches it, because a resumed session keeps its original team. The last resort is `session-<first 8 of the session id>`. Names are sanitised the way Claude Code does it. If any message in the inbox is unread, the hook lets the teammate idle so the message is delivered. Claude Code empties that file on delivery (checked on 2.1.330).
- **It never waits.** The hook returns immediately, and the teammate does the waiting with a foreground call.
- **It fails open.** A parse error, a missing file or a panic exits 0 with no output. A bug here leaves Claude Code behaving as it would without the hook.
- **State is a small counter per teammate and task set,** kept under your user cache directory (`claude-team-guard/`) and written atomically. Deleting it is harmless.
- **A recovered report is capped at 40,000 bytes.**

## Limitations

- It depends on Claude Code's transcript wording, checked against 2.1.325. If a release changes that wording, the hook stops matching and falls back to doing nothing. It won't misfire.
- It only tracks background Bash and Monitor tasks. Agent calls made by a teammate already run in the foreground.
- It was tested with in-process teammates (`teammateMode: "in-process"`) on macOS. It should work on Linux. Windows is untested.

## Removing it

Remove the two hook entries from `settings.json` once #77300 and #96225 are fixed. Nothing else needs cleaning up.

## Development

```sh
gofmt -l .                  # must print nothing
go vet ./...
go test -race ./...
golangci-lint run ./...
```

The tests drive `run()` end to end over synthetic transcripts. Releases are built by GoReleaser from `vX.Y.Z` tags.

## License

[MIT](LICENSE)
