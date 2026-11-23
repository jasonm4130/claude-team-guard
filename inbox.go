package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

type inboxMsg struct {
	From    string `json:"from"`
	Text    string `json:"text"`
	Summary string `json:"summary"`
	MsgID   string `json:"msg_id"`
	Time    string `json:"timestamp"`
	Read    *bool  `json:"read"`
}

func (m inboxMsg) unread() bool { return m.Read == nil || !*m.Read }

func (m inboxMsg) key() string {
	if m.MsgID != "" {
		return m.MsgID
	}
	h := sha256.Sum256([]byte(m.From + "\x00" + m.Time + "\x00" + m.Text))
	return hex.EncodeToString(h[:8])
}

// protocol reports a structured message (shutdown_request, plan approval and
// the like): those are for Claude Code to route, not for a teammate to read early.
func (m inboxMsg) protocol() bool {
	t := strings.TrimSpace(m.Text)
	if !strings.HasPrefix(t, "{") {
		return false
	}
	var v struct {
		Type string `json:"type"`
	}
	return json.Unmarshal([]byte(t), &v) == nil && v.Type != ""
}

// teammateName names the in-process teammate this payload comes from, or ""
// for the lead or a plain subagent.
func teammateName(p Payload) string {
	if p.TeammateName != "" {
		return p.TeammateName
	}
	path := ""
	if filepath.Base(filepath.Dir(p.TranscriptPath)) == "subagents" {
		path = p.TranscriptPath
	} else if p.AgentID != "" {
		id, _, _ := strings.Cut(p.AgentID, "@")
		if !safeID.MatchString(id) {
			return ""
		}
		path = filepath.Join(sessionDir(p.TranscriptPath), "subagents", "agent-"+id+".jsonl")
	}
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".meta.json")
	if err != nil {
		return ""
	}
	var meta struct {
		Name     string `json:"name"`
		TaskKind string `json:"taskKind"`
	}
	if json.Unmarshal(b, &meta) != nil || meta.TaskKind != "in_process_teammate" {
		return ""
	}
	return meta.Name
}

// maxInjected caps the text handed over in one tool call.
const maxInjected = 20000

// earlyMessages returns waiting messages the teammate hasn't been shown yet,
// as additionalContext text, so it sees them after its current tool call
// rather than when its turn ends. Each message is shown once.
func earlyMessages(p Payload, env Env) string {
	name := teammateName(p)
	path := inboxPath(p, name, env)
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var msgs []inboxMsg
	if json.Unmarshal(raw, &msgs) != nil || len(msgs) == 0 {
		return ""
	}
	stateFile := filepath.Join(env.StateDir, unsafeChars.ReplaceAllString("inbox__"+path, "_"))
	if len(stateFile) > 250 {
		h := sha256.Sum256([]byte(path))
		stateFile = filepath.Join(env.StateDir, "inbox__"+hex.EncodeToString(h[:12]))
	}
	seen := map[string]bool{}
	if b, err := os.ReadFile(stateFile); err == nil {
		for _, k := range strings.Fields(string(b)) {
			seen[k] = true
		}
	}
	var b strings.Builder
	var keys []string
	for _, m := range msgs {
		if !m.unread() || m.protocol() || strings.TrimSpace(m.Text) == "" || seen[m.key()] {
			continue
		}
		keys = append(keys, m.key())
		from := m.From
		if from == "" {
			from = "a teammate"
		}
		fmt.Fprintf(&b, "\n--- message from %s", from)
		if m.Summary != "" {
			fmt.Fprintf(&b, " (%s)", m.Summary)
		}
		b.WriteString(" ---\n")
		b.WriteString(strings.TrimSpace(m.Text))
		b.WriteString("\n")
	}
	if len(keys) == 0 {
		return ""
	}
	body := b.String()
	if len(body) > maxInjected {
		body = body[:maxInjected] + "\n[truncated by claude-team-guard; the full text arrives when your turn ends]"
	}
	// A lost write only means a message is shown twice, never hidden.
	all := make([]string, 0, len(seen)+len(keys))
	for k := range seen {
		all = append(all, k)
	}
	all = append(all, keys...)
	_ = writeAtomic(stateFile, []byte(strings.Join(all, "\n")))
	return "claude-team-guard: new message(s) arrived for you mid-turn. Claude Code would only " +
		"deliver them when your turn ends, so they are shown now. Read them before your next " +
		"step and change course if they ask you to. The same message(s) will arrive again " +
		"formally when your turn ends: that copy is a duplicate, not a new instruction.\n" + body
}
