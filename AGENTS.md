# AGENTS.md

A Claude Code hook for agent teams: one Go binary that reads a hook payload on stdin. On `TeammateIdle` it keeps a teammate from idling over unseen background tasks. On `PostToolUse` (`Agent`, `SendMessage`) it recovers a helper subagent's dropped report. README.md explains both bugs and the design.

## Rules that are easy to break

- **Go standard library only.** No modules in `go.mod` beyond the language version.
- **Fail open.** Every error path exits 0 with nothing on stdout or stderr. Only a confirmed pending task may exit 2, and only through the loop guard (`maxBlocks`).
- **Never sleep or wait in the hook.** It runs inline in the teammate's turn.
- **Match Claude Code's real transcript text.** Launch and notice markers are copied from real 2.1.325 transcripts. When changing one, check it against a real `~/.claude/projects/*/*/subagents/agent-*.jsonl` and say which version in the PR. Test fixtures are synthetic. Never commit real transcript content, which holds private data.
- **Lines can be megabytes.** Read transcripts with `eachLine`, never `bufio.Scanner` with its default limit.
- **State files** live under `os.UserCacheDir()/claude-team-guard`, written atomically. Losing one must be harmless.

## Layout

- `main.go`: the payload and dispatch.
- `idle.go`: TeammateIdle and the loop guard.
- `recover.go`: lost-report recovery.
- `transcript.go`: transcript location and scanning.
- `main_test.go`: tests, run end to end through `run()`.

## Checks

```sh
gofmt -l .                  # must print nothing
go vet ./...
go test -race ./...
golangci-lint run ./...
```

## Releasing

Releases are tag-driven, as in claude-statusline. Push an annotated `vX.Y.Z` tag on `main`, and the Release workflow builds darwin/linux × amd64/arm64 with checksums and SBOMs. To install on Jason's machine: download the darwin_arm64 archive, check it against `checksums.txt`, then `install -m 755` into `~/.local/bin/`. The hooks that call it live in chezmoi (`private_dot_claude/settings.json` in the dotfiles repo), not here.

## Retiring

When anthropics/claude-code#77300 and the teammate handback bug are fixed, remove the hooks from settings and archive this repo.
