package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
)

// A teammate receives SendMessage only between turns. Claude Code holds each
// waiting message in <config>/teams/<team>/inboxes/<name>.json, a JSON array
// it empties on delivery (checked on 2.1.330).

// inboxUnsafe mirrors Claude Code's getInboxPath, which replaces every other
// character with "-" in both the team and the agent name.
var inboxUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func inboxSafe(s string) string { return inboxUnsafe.ReplaceAllString(s, "-") }

// teamName finds the team a teammate belongs to. The payload's team_name
// comes first. A resumed session keeps its original team, so the
// "session-<first 8 of session id>" rule alone is wrong there; the lead's
// transcript records the team in each teammate's spawn result, and the answer
// is cached because that transcript can run to tens of megabytes.
func teamName(p Payload, name string, env Env) string {
	if p.TeamName != "" {
		return p.TeamName
	}
	sess := sessionDir(p.TranscriptPath)
	id := filepath.Base(sess)
	if sess == "" || len(id) < 8 {
		return ""
	}
	cache := filepath.Join(env.StateDir, unsafeChars.ReplaceAllString("team__"+id+"__"+name, "_"))
	if b, err := os.ReadFile(cache); err == nil && len(b) > 0 {
		return string(b)
	}
	if team := spawnedTeam(sess+".jsonl", name); team != "" {
		_ = writeAtomic(cache, []byte(team))
		return team
	}
	return "session-" + id[:8]
}

var teamMarker = []byte(`"team_name":"`)

// spawnedTeam reads the team from the last spawn result for this teammate in
// the lead's transcript: an object carrying both "name" and "team_name".
func spawnedTeam(leadPath, name string) string {
	team := ""
	_ = eachLine(leadPath, func(line []byte) {
		if !bytes.Contains(line, teamMarker) || !bytes.Contains(line, []byte(`"`+name+`"`)) {
			return
		}
		var v any
		if json.Unmarshal(line, &v) != nil {
			return
		}
		walk(v, func(m map[string]any) {
			if n, _ := m["name"].(string); n == name {
				if t, _ := m["team_name"].(string); t != "" {
					team = t
				}
			}
		})
	})
	return team
}

func walk(v any, fn func(map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		fn(x)
		for _, c := range x {
			walk(c, fn)
		}
	case []any:
		for _, c := range x {
			walk(c, fn)
		}
	}
}

func inboxPath(p Payload, name string, env Env) string {
	if name == "" {
		return ""
	}
	team := teamName(p, name, env)
	sess := sessionDir(p.TranscriptPath)
	if team == "" || sess == "" {
		return ""
	}
	config := filepath.Dir(filepath.Dir(filepath.Dir(sess)))
	return filepath.Join(config, "teams", inboxSafe(team), "inboxes", inboxSafe(name)+".json")
}

// inboxPending reports whether the teammate has a message waiting. A teammate
// sees messages only between turns, so blocking its idle while one waits
// sends it back into a foreground wait and the message sits until that ends
// (measured: up to 13 h late). Letting it idle delivers the message at once.
func inboxPending(p Payload, env Env) bool {
	path := inboxPath(p, p.TeammateName, env)
	if path == "" {
		return false
	}
	b, err := os.ReadFile(path)
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
