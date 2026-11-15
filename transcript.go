package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// sessionDir is the lead session's directory, which holds subagents/. The
// payload's transcript_path may point at the lead's transcript
// (<dir>.jsonl) or at a teammate's own (<dir>/subagents/agent-*.jsonl).
func sessionDir(transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}
	if d := filepath.Dir(transcriptPath); filepath.Base(d) == "subagents" {
		return filepath.Dir(d)
	}
	return strings.TrimSuffix(transcriptPath, ".jsonl")
}

// teammateTranscript finds the idling teammate's own transcript.
func teammateTranscript(p Payload) string {
	if filepath.Base(filepath.Dir(p.TranscriptPath)) == "subagents" {
		return p.TranscriptPath
	}
	sub := filepath.Join(sessionDir(p.TranscriptPath), "subagents")
	if p.AgentID != "" {
		id, _, _ := strings.Cut(p.AgentID, "@")
		if f := filepath.Join(sub, "agent-"+id+".jsonl"); exists(f) {
			return f
		}
	}
	if p.TeammateName == "" {
		return ""
	}
	metas, _ := filepath.Glob(filepath.Join(sub, "*.meta.json"))
	for _, m := range metas {
		var meta struct {
			Name     string `json:"name"`
			TaskKind string `json:"taskKind"`
		}
		b, err := os.ReadFile(m)
		if err != nil || json.Unmarshal(b, &meta) != nil {
			continue
		}
		if meta.TaskKind == "in_process_teammate" && meta.Name == p.TeammateName {
			return strings.TrimSuffix(m, ".meta.json") + ".jsonl"
		}
	}
	return ""
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// eachLine calls fn for every line of a JSONL file. Lines can be megabytes
// (tool results), so it reads with no line-length limit.
func eachLine(path string, fn func([]byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			fn(line)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// entry is the subset of a transcript record this tool reads.
type entry struct {
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type  string          `json:"type"`
	Name  string          `json:"name"`
	Text  string          `json:"text"`
	Input json.RawMessage `json:"input"`
}

func (e entry) blocks() []block {
	var bs []block
	_ = json.Unmarshal(e.Message.Content, &bs) // string content leaves bs empty
	return bs
}

// Task is a background task the teammate launched.
type Task struct {
	ID     string
	Output string // output file, when the launch result names one
}

var (
	reBashLaunch = regexp.MustCompile(`Command running in background with ID: (\w+)\. Output is being written to: (\S+?\.output)`)
	reMonLaunch  = regexp.MustCompile(`Monitor started \(task (\w+)`)
	reNotified   = regexp.MustCompile(`(?:<|\\u003c)task-id(?:>|\\u003e)(\w+)`)
)

var (
	mBash    = []byte("Command running in background with ID: ")
	mMonitor = []byte("Monitor started (task ")
	mTaskID  = []byte("task-id")
	mStop    = []byte(`"name":"TaskStop"`)
	mKill    = []byte(`"name":"KillShell"`)
)

// pendingTasks lists background Bash and Monitor tasks the transcript
// launched whose completion notice never reached it and that it did not stop
// itself. A task that finished but whose notice is still queued counts as
// pending: the teammate has not seen its result.
func pendingTasks(path string) ([]Task, error) {
	var order []string
	launched := map[string]string{}
	seen := map[string]bool{}
	err := eachLine(path, func(line []byte) {
		launch := bytes.Contains(line, mBash) || bytes.Contains(line, mMonitor)
		notice := bytes.Contains(line, mTaskID)
		stop := bytes.Contains(line, mStop) || bytes.Contains(line, mKill)
		if !launch && !notice && !stop {
			return
		}
		var e entry
		if json.Unmarshal(line, &e) != nil {
			return
		}
		switch e.Type {
		case "user":
			if !launch {
				return
			}
			for _, m := range reBashLaunch.FindAllSubmatch(line, -1) {
				add(launched, &order, string(m[1]), string(m[2]))
			}
			for _, m := range reMonLaunch.FindAllSubmatch(line, -1) {
				add(launched, &order, string(m[1]), "")
			}
		case "attachment", "queue-operation":
			for _, m := range reNotified.FindAllSubmatch(line, -1) {
				seen[string(m[1])] = true
			}
		case "assistant":
			for _, b := range e.blocks() {
				if b.Type != "tool_use" || (b.Name != "TaskStop" && b.Name != "KillShell") {
					continue
				}
				var in struct {
					TaskID  string `json:"task_id"`
					ShellID string `json:"shell_id"`
				}
				_ = json.Unmarshal(b.Input, &in)
				seen[in.TaskID] = true
				seen[in.ShellID] = true
			}
		}
	})
	if err != nil {
		return nil, err
	}
	var out []Task
	for _, id := range order {
		if !seen[id] {
			out = append(out, Task{ID: id, Output: launched[id]})
		}
	}
	return out, nil
}

func add(m map[string]string, order *[]string, id, output string) {
	if _, ok := m[id]; !ok {
		*order = append(*order, id)
	}
	if output != "" || m[id] == "" {
		m[id] = output
	}
}

// lastReport returns a subagent's report from its transcript: the message
// of its last SubagentHandback call, or failing that its last text reply.
func lastReport(path string) (string, error) {
	var handback, text string
	err := eachLine(path, func(line []byte) {
		if !bytes.Contains(line, []byte(`"type":"assistant"`)) {
			return
		}
		var e entry
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" {
			return
		}
		for _, b := range e.blocks() {
			switch {
			case b.Type == "tool_use" && b.Name == "SubagentHandback":
				var in struct {
					Message string `json:"message"`
				}
				if json.Unmarshal(b.Input, &in) == nil && strings.TrimSpace(in.Message) != "" {
					handback = in.Message
				}
			case b.Type == "text" && strings.TrimSpace(b.Text) != "":
				text = b.Text
			}
		}
	})
	if handback != "" {
		return handback, err
	}
	return text, err
}
