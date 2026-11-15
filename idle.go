package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// maxBlocks is how many times one teammate is kept from idling over the same
// set of pending tasks. After that it may idle, so the hook can never trap it.
const maxBlocks = 2

// Env carries what tests replace.
type Env struct {
	StateDir string
}

func defaultEnv() Env {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return Env{StateDir: filepath.Join(dir, "claude-team-guard")}
}

func idle(p Payload, stderr io.Writer, env Env) int {
	path := teammateTranscript(p)
	if path == "" {
		return 0
	}
	tasks, err := pendingTasks(path)
	if err != nil || len(tasks) == 0 {
		return 0
	}
	who := p.TeammateName
	if who == "" {
		who = p.AgentID
	}
	if !takeBlock(env.StateDir, who, tasks) {
		return 0
	}
	_, _ = fmt.Fprint(stderr, idleMessage(tasks))
	return 2
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// takeBlock counts a block against this teammate and task set and reports
// whether one is still allowed.
func takeBlock(stateDir, teammate string, tasks []Task) bool {
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	sort.Strings(ids)
	name := unsafeChars.ReplaceAllString(teammate+"__"+strings.Join(ids, "-"), "_")
	if len(name) > 200 {
		name = name[:200]
	}
	file := filepath.Join(stateDir, name)
	n := 0
	if b, err := os.ReadFile(file); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	if n >= maxBlocks {
		return false
	}
	// A lost write only means one more block than intended, never a trap:
	// the counter still rises on the next successful write.
	_ = writeAtomic(file, []byte(strconv.Itoa(n+1)))
	return true
}

func writeAtomic(file string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), file)
}

func idleMessage(tasks []Task) string {
	var b strings.Builder
	b.WriteString("Don't go idle yet: these background tasks of yours haven't reported back:\n")
	for _, t := range tasks {
		if t.Output != "" {
			fmt.Fprintf(&b, "- %s (output: %s)\n", t.ID, t.Output)
		} else {
			fmt.Fprintf(&b, "- %s\n", t.ID)
		}
	}
	b.WriteString("You are a teammate, and an idle teammate is NOT woken when its background tasks " +
		"finish, whatever the tool result said (Claude Code bug #77300). If you go idle now you " +
		"stall until someone messages you.\n" +
		"Wait in the foreground instead: a Bash call with timeout 600000 that blocks until the work " +
		"is done (`gh pr checks N --watch`, `gh run watch ID --exit-status`, or an " +
		"`until ...; do sleep 15; done` loop), repeated as needed, then carry on.\n" +
		"If the wait will run past about 30 minutes, SendMessage team-lead with exactly what you " +
		"are waiting on and ask it to message you when it's done, then stop.\n")
	return b.String()
}
