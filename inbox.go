package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

// teammateMeta is the <agent>.meta.json Claude Code writes beside each
// subagent transcript. In-process teammates carry their name and team.
type teammateMeta struct {
	Name     string `json:"name"`
	TeamName string `json:"teamName"`
	TaskKind string `json:"taskKind"`
}

func readMeta(path string) (teammateMeta, bool) {
	var m teammateMeta
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &m) != nil || m.TaskKind != "in_process_teammate" || m.Name == "" {
		return teammateMeta{}, false
	}
	return m, true
}

// metaByName finds an in-process teammate's meta by its name.
func metaByName(sess, name string) (teammateMeta, bool) {
	metas, _ := filepath.Glob(filepath.Join(sess, "subagents", "*.meta.json"))
	for _, f := range metas {
		if m, ok := readMeta(f); ok && m.Name == name {
			return m, true
		}
	}
	return teammateMeta{}, false
}

// identify names the in-process teammate a payload comes from. ok is false
// for the lead and for plain subagents. When the payload names an agent, its
// meta decides; teammate_name alone is trusted only without one.
func identify(p Payload) (teammateMeta, bool) {
	sess := sessionDir(p.TranscriptPath)
	if sess == "" {
		return teammateMeta{}, false
	}
	if filepath.Base(filepath.Dir(p.TranscriptPath)) == "subagents" {
		return readMeta(strings.TrimSuffix(p.TranscriptPath, ".jsonl") + ".meta.json")
	}
	if p.AgentID != "" {
		id, team, at := strings.Cut(p.AgentID, "@")
		if at {
			// name@team, the form team config uses.
			m, ok := metaByName(sess, id)
			if ok && m.TeamName == "" {
				m.TeamName = team
			}
			return m, ok
		}
		if !safeID.MatchString(id) {
			return teammateMeta{}, false
		}
		return readMeta(filepath.Join(sess, "subagents", "agent-"+id+".meta.json"))
	}
	if p.TeammateName != "" {
		if m, ok := metaByName(sess, p.TeammateName); ok {
			return m, true
		}
		return teammateMeta{Name: p.TeammateName}, true
	}
	return teammateMeta{}, false
}

// teamName finds the team a teammate belongs to: the payload's team_name,
// then the teammate's meta, then its spawn result in the lead's transcript,
// and last "session-<first 8 of session id>", which is wrong for a resumed
// session. Transcript lookups are cached, misses included, because the lead's
// transcript can run to tens of megabytes.
func teamName(p Payload, m teammateMeta, env Env) string {
	if p.TeamName != "" {
		return p.TeamName
	}
	if m.TeamName != "" {
		return m.TeamName
	}
	sess := sessionDir(p.TranscriptPath)
	id := filepath.Base(sess)
	if sess == "" || len(id) < 8 {
		return ""
	}
	cache := filepath.Join(env.StateDir, unsafeChars.ReplaceAllString("team__"+id+"__"+m.Name, "_"))
	if b, err := os.ReadFile(cache); err == nil && len(b) > 0 {
		return string(b)
	}
	team := spawnedTeam(sess+".jsonl", m.Name)
	if team == "" {
		team = "session-" + id[:8]
	}
	_ = writeAtomic(cache, []byte(team))
	return team
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
		walk(v, func(o map[string]any) {
			if n, _ := o["name"].(string); n == name {
				if t, _ := o["team_name"].(string); t != "" {
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

func inboxPath(p Payload, m teammateMeta, env Env) string {
	sess := sessionDir(p.TranscriptPath)
	if m.Name == "" || sess == "" {
		return ""
	}
	team := teamName(p, m, env)
	if team == "" {
		return ""
	}
	config := filepath.Dir(filepath.Dir(filepath.Dir(sess)))
	return filepath.Join(config, "teams", inboxSafe(team), "inboxes", inboxSafe(m.Name)+".json")
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
	s := m.MsgID
	if s == "" {
		s = m.From + "\x00" + m.Time + "\x00" + m.Text
	}
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:12])
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

func readInbox(path string) []inboxMsg {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var msgs []inboxMsg
	if json.Unmarshal(b, &msgs) != nil {
		return nil
	}
	return msgs
}

// inboxPending reports whether the idling teammate has a message waiting.
// Blocking its idle then would send it back into a foreground wait while the
// message sits (measured: up to 13 h late); letting it idle delivers it at once.
func inboxPending(p Payload, env Env) bool {
	m, ok := identify(p)
	if !ok {
		return false
	}
	for _, msg := range readInbox(inboxPath(p, m, env)) {
		if msg.unread() {
			return true
		}
	}
	return false
}

// claim marks a message as shown, exactly once across concurrent hooks: the
// O_EXCL create succeeds for one caller only. If it can't be written, the
// message is not shown early; it still arrives when the turn ends.
func claim(dir, key string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.OpenFile(filepath.Join(dir, key), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	return f.Close() == nil
}

// claimed reports whether a message was already shown.
func claimed(dir, key string) bool {
	_, err := os.Stat(filepath.Join(dir, key))
	return !errors.Is(err, fs.ErrNotExist)
}

// maxInjected caps the text handed over in one tool call.
const maxInjected = 20000

// earlyMessages returns waiting messages the teammate hasn't been shown yet,
// as additionalContext text, so it sees them after its current tool call
// rather than when its turn ends. Each message is shown once.
func earlyMessages(p Payload, env Env) string {
	m, ok := identify(p)
	if !ok {
		return ""
	}
	path := inboxPath(p, m, env)
	msgs := readInbox(path)
	if len(msgs) == 0 {
		return ""
	}
	h := sha256.Sum256([]byte(path))
	dir := filepath.Join(env.StateDir, "shown", hex.EncodeToString(h[:12]))
	var b strings.Builder
	for _, msg := range msgs {
		text := strings.TrimSpace(msg.Text)
		if !msg.unread() || msg.protocol() || text == "" || claimed(dir, msg.key()) {
			continue
		}
		from := msg.From
		if from == "" {
			from = "a teammate"
		}
		head := "\n--- message from " + from
		if msg.Summary != "" {
			head += " (" + msg.Summary + ")"
		}
		head += " ---\n"
		room := maxInjected - b.Len() - len(head)
		if b.Len() > 0 && len(text) > room {
			break // left for a later tool call
		}
		if !claim(dir, msg.key()) {
			continue
		}
		b.WriteString(head)
		if len(text) > room {
			text = text[:max(room, 0)] + "\n[truncated by claude-team-guard; the full text arrives when your turn ends]"
		}
		b.WriteString(text)
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return fmt.Sprintf("claude-team-guard: new message(s) arrived for you mid-turn. Claude Code would only "+
		"deliver them when your turn ends, so they are shown now. Read them before your next "+
		"step and change course if they ask you to. The same message(s) will arrive again "+
		"formally when your turn ends: that copy is a duplicate, not a new instruction.\n%s", b.String())
}
