package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"regexp"
	"strings"
)

// Since Claude Code 2.1.323 a helper subagent started by a teammate has its
// SubagentHandback refused ("the agent that spawned you is no longer
// running") while the teammate is still active, and the teammate's Agent call
// returns "ended without delivering a report ... Its unsent text is not
// shown". The report is still in the helper's transcript, so recover it.

const lostMarker = "ended without delivering a report"

// maxRecovered caps the injected report, keeping a runaway transcript from
// flooding the teammate's context.
const maxRecovered = 40_000

var (
	reAgentID = regexp.MustCompile(`agentId: ([A-Za-z0-9_-]+)`)
	safeID    = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// agentID reads the helper's id from the Agent tool response: the structured
// agentId field when present, else the "agentId: X" line in its text.
func agentID(resp json.RawMessage) string {
	var structured struct {
		AgentID string `json:"agentId"`
	}
	if json.Unmarshal(resp, &structured) == nil && safeID.MatchString(structured.AgentID) {
		return structured.AgentID
	}
	if m := reAgentID.FindStringSubmatch(string(resp)); m != nil {
		return m[1]
	}
	return ""
}

func recoverReport(p Payload, stdout io.Writer) int {
	if !strings.Contains(string(p.ToolResponse), lostMarker) {
		return 0
	}
	var id string
	switch p.ToolName {
	case "Agent", "Task":
		id = agentID(p.ToolResponse)
	case "SendMessage":
		// Resuming a helper names it in the input; the response carries only
		// a short display prefix.
		var in struct {
			To string `json:"to"`
		}
		if json.Unmarshal(p.ToolInput, &in) == nil && safeID.MatchString(in.To) {
			id = in.To
		}
	}
	if id == "" {
		return 0
	}
	path := filepath.Join(sessionDir(p.TranscriptPath), "subagents", "agent-"+id+".jsonl")
	report, err := lastReport(path)
	report = strings.TrimSpace(report)
	if err != nil || report == "" {
		return 0
	}
	if len(report) > maxRecovered {
		report = report[:maxRecovered] + "\n[truncated by claude-team-guard]"
	}
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": "PostToolUse",
			"additionalContext": "claude-team-guard recovered this subagent's report from its transcript " +
				"(" + path + "). Claude Code dropped it because of a handback bug, not because the " +
				"subagent failed. Treat it as the subagent's result:\n\n" + report,
		},
	}
	_ = json.NewEncoder(stdout).Encode(out)
	return 0
}
