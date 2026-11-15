# claude-team-guard

A Claude Code hook that stops agent-team work from silently going missing. It is one Go binary with no dependencies. It reads the hook payload on stdin and handles two events.

## What it fixes

**A teammate waits on background work and never wakes up.** A teammate starts a background Bash command or a Monitor, such as a CI watch, says "Waiting for CI." and ends its turn. Claude Code does not wake an idle teammate when that task finishes. The notice sits in a queue until something else messages the teammate ([anthropics/claude-code#77300](https://github.com/anthropics/claude-code/issues/77300)). The main session is not affected.

- **What the hook does:** on `TeammateIdle`, it lists the teammate's background tasks whose completion it hasn't seen. If there are any, it exits 2, so the teammate keeps working. It also tells the teammate to wait in the foreground, or to ask the lead to wake it if the wait is long.
- **Loop guard:** it blocks at most twice for the same set of tasks, then lets the teammate idle.

**A teammate's helper subagent loses its report.** Since Claude Code 2.1.323, a subagent started by a teammate has its `SubagentHandback` refused with "the agent that spawned you is no longer running", even though the teammate is still active. The teammate gets back "ended without delivering a report … Its unsent text is not shown". A `SendMessage` asking the helper to resend loses it again.

- **What the hook does:** on `PostToolUse` for `Agent` or `SendMessage`, it reads the report from the helper's transcript and returns it to the teammate as `additionalContext`. It uses the last `SubagentHandback` message, or failing that the last text reply.

### How often this happened

Measured over one machine's transcripts, 2026-11-05 to 2026-11-15, Claude Code 2.1.316 to 2.1.325:

- 56 of 57 background tasks that finished while their teammate was idle were delivered late: a median of 42 minutes, and at most 26 hours.
- 41 of 42 foreground helper subagents started by teammates lost their report. This was on 2.1.323 and 2.1.325.

Replayed against those transcripts, the binary recovers all 44 lost reports: 41 from `Agent` and 3 from `SendMessage`. It would have blocked the idle of 12 of the 173 teammates.

## Install

Download the archive for your platform from [Releases](https://github.com/jasonm4130/claude-team-guard/releases) and check it against `checksums.txt`. Then:

```sh
install -m 755 claude-team-guard_*/claude-team-guard ~/.local/bin/
```

Add both hooks to `~/.claude/settings.json`:

```json
"hooks": {
  "TeammateIdle": [
    { "hooks": [ { "type": "command", "command": "~/.local/bin/claude-team-guard" } ] }
  ],
  "PostToolUse": [
    { "matcher": "Agent|SendMessage",
      "hooks": [ { "type": "command", "command": "~/.local/bin/claude-team-guard" } ] }
  ]
}
```

`claude-team-guard --version` prints the build.

## Design notes

- **Fail open.** Any parse error, missing file or panic exits 0 with no output. A bug here must never block an agent.
- **The hook doesn't wait.** It returns at once, and the teammate does the waiting with a foreground call. Sleeping inside the hook would hold the teammate inside a hook timeout instead.
- **The transcript is the source of truth.** Tasks are matched by the exact text Claude Code writes when it launches them (`Command running in background with ID:`, `Monitor started (task`) and by the `<task-id>` in completion notices. If a Claude Code release changes that text, the hook stops matching and fails open: it lets the teammate idle as before.
- **State** is a per-teammate, per-task-set counter under `os.UserCacheDir()/claude-team-guard`, written atomically. Losing it is harmless.
- **The recovered report is capped at 40,000 bytes.**

Remove it once both upstream bugs are fixed.
