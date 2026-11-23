package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// inboxPending reports whether the teammate has a lead or peer message waiting.
// A teammate sees messages only between turns, so blocking its idle while one
// waits sends it back into a foreground wait and the message sits until that
// ends (measured: up to 13 h late). Letting it idle delivers the message at
// once. The inbox is <config>/teams/<team>/inboxes/<name>.json, a JSON array
// that Claude Code empties on delivery (checked on 2.1.330).
func inboxPending(p Payload) bool {
	if p.TeammateName == "" || p.TeammateName != filepath.Base(p.TeammateName) {
		return false
	}
	sess := sessionDir(p.TranscriptPath)
	if sess == "" {
		return false
	}
	team := p.TeamName
	if team == "" {
		id := filepath.Base(sess)
		if len(id) < 8 {
			return false
		}
		team = "session-" + id[:8]
	}
	if team != filepath.Base(team) {
		return false
	}
	config := filepath.Dir(filepath.Dir(filepath.Dir(sess)))
	b, err := os.ReadFile(filepath.Join(config, "teams", team, "inboxes", p.TeammateName+".json"))
	if err != nil {
		return false
	}
	var msgs []struct {
		Read *bool `json:"read"`
	}
	if json.Unmarshal(b, &msgs) != nil {
		return false
	}
	for _, m := range msgs {
		if m.Read == nil || !*m.Read {
			return true
		}
	}
	return false
}
