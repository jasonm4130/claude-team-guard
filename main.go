// Command claude-team-guard is a Claude Code hook that works around two
// agent-team bugs where a teammate's work never reaches anyone.
//
// One binary serves every event: it reads the hook payload on stdin and
// dispatches on hook_event_name.
//
//   - TeammateIdle: keep a teammate from idling while its own background Bash
//     or Monitor tasks are unseen. An idle teammate is not woken when they
//     finish (anthropics/claude-code#77300).
//   - PostToolUse on Agent: when a teammate's helper subagent "ended without
//     delivering a report", recover the report from the helper's transcript
//     and hand it to the teammate as additional context. The same applies to
//     a SendMessage that resumes such a helper and loses its report again.
//   - PostToolUse on any tool: show a teammate the messages waiting in its
//     team inbox, which Claude Code delivers only between turns.
//
// Any failure exits 0 with no output, so a bug here never blocks an agent.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

var (
	version = "dev"
	commit  = ""
	date    = ""
)

// Payload holds the hook input fields this tool reads. Fields come and go
// between Claude Code versions; every one is optional.
type Payload struct {
	HookEventName  string          `json:"hook_event_name"`
	TranscriptPath string          `json:"transcript_path"`
	AgentID        string          `json:"agent_id"`
	TeammateName   string          `json:"teammate_name"`
	TeamName       string          `json:"team_name"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Printf("claude-team-guard %s %s %s\n", version, commit, date)
		return
	}
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr, defaultEnv()))
}

// run returns the process exit code: 2 blocks (TeammateIdle), 0 otherwise.
func run(stdin io.Reader, stdout, stderr io.Writer, env Env) (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	var p Payload
	if err := json.NewDecoder(stdin).Decode(&p); err != nil {
		return 0
	}
	switch p.HookEventName {
	case "TeammateIdle":
		return idle(p, stderr, env)
	case "PostToolUse":
		return postToolUse(p, stdout, env)
	}
	return 0
}

// postToolUse hands the agent any recovered report and any message waiting in
// its team inbox, as one additionalContext.
func postToolUse(p Payload, stdout io.Writer, env Env) int {
	var parts []string
	for _, s := range []string{recoverReport(p), earlyMessages(p, env)} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return 0
	}
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "PostToolUse",
			"additionalContext": strings.Join(parts, "\n\n"),
		},
	}
	_ = json.NewEncoder(stdout).Encode(out)
	return 0
}
