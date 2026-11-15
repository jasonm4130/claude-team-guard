package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Transcript records in the shape Claude Code 2.1.325 writes them.

func toolResult(text string) map[string]any {
	return map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "toolu_x", "content": []any{map[string]any{"type": "text", "text": text}}},
	}}}
}

func bashLaunch(id string) map[string]any {
	return toolResult("Command running in background with ID: " + id + ". Output is being written to: /tmp/s/tasks/" + id + ".output. You will be notified when it completes")
}

func monitorLaunch(id string) map[string]any {
	return toolResult("Monitor started (task " + id + ", expires in 10m unless the source ends first)")
}

func notice(id string) map[string]any {
	return map[string]any{"type": "attachment", "attachment": map[string]any{"type": "queued_command",
		"prompt": "<task-notification>\n<task-id>" + id + "</task-id>\n<status>completed</status>\n</task-notification>"}}
}

func toolUse(name string, input map[string]any) map[string]any {
	return map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "stop_reason": "tool_use",
		"content": []any{map[string]any{"type": "tool_use", "id": "toolu_y", "name": name, "input": input}}}}
}

func say(text string) map[string]any {
	return map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "stop_reason": "end_turn",
		"content": []any{map[string]any{"type": "text", "text": text}}}}
}

func writeJSONL(t *testing.T, path string, recs ...map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Claude Code writes < and > unescaped; match it.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// session builds <tmp>/sess.jsonl plus one named teammate under subagents/.
func session(t *testing.T, name string, recs ...map[string]any) (lead string) {
	t.Helper()
	dir := t.TempDir()
	lead = filepath.Join(dir, "sess.jsonl")
	writeJSONL(t, lead)
	writeJSONL(t, filepath.Join(dir, "sess", "subagents", "agent-a"+name+"-123.jsonl"), recs...)
	meta, _ := json.Marshal(map[string]any{"name": name, "taskKind": "in_process_teammate"})
	if err := os.WriteFile(filepath.Join(dir, "sess", "subagents", "agent-a"+name+"-123.meta.json"), meta, 0o644); err != nil {
		t.Fatal(err)
	}
	return lead
}

func runHook(t *testing.T, env Env, payload map[string]any) (int, string, string) {
	t.Helper()
	in, _ := json.Marshal(payload)
	var out, errb bytes.Buffer
	code := run(bytes.NewReader(in), &out, &errb, env)
	return code, out.String(), errb.String()
}

func idlePayload(lead, name string) map[string]any {
	return map[string]any{"hook_event_name": "TeammateIdle", "transcript_path": lead, "teammate_name": name}
}

func TestIdleBlocksOnUnseenBackgroundTask(t *testing.T) {
	lead := session(t, "ci-watch", say("starting"), bashLaunch("bdone"), notice("bdone"), monitorLaunch("bmon"), bashLaunch("bwait"), say("Waiting for CI."))
	env := Env{StateDir: t.TempDir()}
	code, _, msg := runHook(t, env, idlePayload(lead, "ci-watch"))
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	for _, want := range []string{"bmon", "bwait (output: /tmp/s/tasks/bwait.output)", "#77300"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "bdone") {
		t.Errorf("message lists a task whose notice was delivered:\n%s", msg)
	}
}

func TestIdleAllowsWhenNothingPending(t *testing.T) {
	cases := map[string][]map[string]any{
		"no background work": {say("done")},
		"notice delivered":   {bashLaunch("b1"), notice("b1"), say("done")},
		"stopped itself":     {monitorLaunch("m1"), toolUse("TaskStop", map[string]any{"task_id": "m1"}), say("done")},
		"killed shell":       {bashLaunch("b2"), toolUse("KillShell", map[string]any{"shell_id": "b2"}), say("done")},
	}
	for name, recs := range cases {
		t.Run(name, func(t *testing.T) {
			lead := session(t, "tm", recs...)
			if code, _, msg := runHook(t, Env{StateDir: t.TempDir()}, idlePayload(lead, "tm")); code != 0 || msg != "" {
				t.Fatalf("exit = %d, stderr = %q; want 0 and nothing", code, msg)
			}
		})
	}
}

func TestIdleLoopGuard(t *testing.T) {
	lead := session(t, "stuck", bashLaunch("bx"), say("Waiting."))
	env := Env{StateDir: t.TempDir()}
	var codes []int
	for range maxBlocks + 2 {
		c, _, _ := runHook(t, env, idlePayload(lead, "stuck"))
		codes = append(codes, c)
	}
	want := []int{2, 2, 0, 0}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("exit codes = %v, want %v", codes, want)
		}
	}
	// A new task set starts a fresh count.
	writeJSONL(t, teammateTranscript(Payload{TranscriptPath: lead, TeammateName: "stuck"}), bashLaunch("bx"), bashLaunch("by"))
	if c, _, _ := runHook(t, env, idlePayload(lead, "stuck")); c != 2 {
		t.Fatalf("new task set: exit = %d, want 2", c)
	}
}

func TestIdleFindsTranscriptByAgentIDOrDirectPath(t *testing.T) {
	lead := session(t, "named", bashLaunch("b1"))
	direct := teammateTranscript(Payload{TranscriptPath: lead, TeammateName: "named"})
	for name, p := range map[string]map[string]any{
		"agent_id":    {"hook_event_name": "TeammateIdle", "transcript_path": lead, "agent_id": "anamed-123@sess"},
		"direct path": {"hook_event_name": "TeammateIdle", "transcript_path": direct, "teammate_name": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			if c, _, _ := runHook(t, Env{StateDir: t.TempDir()}, p); c != 2 {
				t.Fatalf("exit = %d, want 2", c)
			}
		})
	}
}

func TestFailsOpen(t *testing.T) {
	env := Env{StateDir: t.TempDir()}
	for name, in := range map[string]string{
		"not json":         "{",
		"unknown event":    `{"hook_event_name":"Stop"}`,
		"missing teammate": `{"hook_event_name":"TeammateIdle","transcript_path":"/nonexistent/x.jsonl","teammate_name":"nobody"}`,
		"empty payload":    `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if c := run(strings.NewReader(in), &out, &errb, env); c != 0 || out.Len() != 0 || errb.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q; want silent 0", c, out.String(), errb.String())
			}
		})
	}
}

const lostResult = "  The subagent ended without delivering a report through SubagentHandback, so no report was delivered. Its unsent text is not shown.\n\nagentId: ahelper99 (use SendMessage with to: 'ahelper99' to continue this agent)\n<usage>subagent_tokens: 1</usage>"

func TestRecoverReport(t *testing.T) {
	dir := t.TempDir()
	lead := filepath.Join(dir, "sess.jsonl")
	writeJSONL(t, filepath.Join(dir, "sess", "subagents", "agent-ahelper99.jsonl"),
		say("working"),
		toolUse("SubagentHandback", map[string]any{"message": "PR #12 is open; tests pass."}),
		toolResult(`{"success":false,"message":"Nothing was sent: the agent that spawned you is no longer running."}`),
		say("I couldn't hand this back."),
	)
	teammate := filepath.Join(dir, "sess", "subagents", "agent-ateam-1.jsonl")
	for name, tp := range map[string]string{"lead path": lead, "teammate path": teammate} {
		t.Run(name, func(t *testing.T) {
			code, out, _ := runHook(t, Env{}, map[string]any{
				"hook_event_name": "PostToolUse", "tool_name": "Agent", "transcript_path": tp,
				"tool_response": map[string]any{"content": []any{map[string]any{"type": "text", "text": lostResult}}},
			})
			var got struct {
				HookSpecificOutput struct {
					HookEventName     string `json:"hookEventName"`
					AdditionalContext string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if code != 0 || json.Unmarshal([]byte(out), &got) != nil {
				t.Fatalf("exit=%d out=%q", code, out)
			}
			if got.HookSpecificOutput.HookEventName != "PostToolUse" || !strings.Contains(got.HookSpecificOutput.AdditionalContext, "PR #12 is open; tests pass.") {
				t.Fatalf("unexpected output: %s", out)
			}
		})
	}
}

func TestRecoverReadsStructuredAgentID(t *testing.T) {
	dir := t.TempDir()
	writeJSONL(t, filepath.Join(dir, "sess", "subagents", "agent-a516.jsonl"), say("Fetched: version 2.3."))
	_, out, _ := runHook(t, Env{}, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Agent",
		"transcript_path": filepath.Join(dir, "sess.jsonl"),
		"tool_response": map[string]any{"agentId": "a516", "handback": "withheld", "content": []any{
			map[string]any{"type": "text", "text": "The subagent ended without delivering a report through SubagentHandback."}}}})
	if !strings.Contains(out, "Fetched: version 2.3.") {
		t.Fatalf("out = %q", out)
	}
}

func TestRecoverOnSendMessageResume(t *testing.T) {
	dir := t.TempDir()
	writeJSONL(t, filepath.Join(dir, "sess", "subagents", "agent-a67fef9abc.jsonl"), say("Review: no blocking defects."))
	_, out, _ := runHook(t, Env{}, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "SendMessage",
		"transcript_path": filepath.Join(dir, "sess.jsonl"), "tool_input": map[string]any{"to": "a67fef9abc", "message": "deliver it"},
		"tool_response": map[string]any{"success": true, "message": "Resumed agent. Its final report is not in this message.",
			"inlineHandback": map[string]any{"displayName": "a67fef9", "content": []any{
				map[string]any{"type": "text", "text": "The subagent ended without delivering a report through SubagentHandback."}}}}})
	if !strings.Contains(out, "Review: no blocking defects.") {
		t.Fatalf("out = %q", out)
	}
}

func TestRecoverFallsBackToLastText(t *testing.T) {
	dir := t.TempDir()
	writeJSONL(t, filepath.Join(dir, "sess", "subagents", "agent-ahelper99.jsonl"), say("first"), say("Final: all green."))
	_, out, _ := runHook(t, Env{}, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Agent",
		"transcript_path": filepath.Join(dir, "sess.jsonl"), "tool_response": lostResult})
	if !strings.Contains(out, "Final: all green.") {
		t.Fatalf("out = %q", out)
	}
}

func TestRecoverIgnoresNormalResults(t *testing.T) {
	for name, p := range map[string]map[string]any{
		"delivered report": {"hook_event_name": "PostToolUse", "tool_name": "Agent", "transcript_path": "/x/s.jsonl", "tool_response": "Here is the report. agentId: a1"},
		"other tool":       {"hook_event_name": "PostToolUse", "tool_name": "Bash", "transcript_path": "/x/s.jsonl", "tool_response": lostResult},
		"helper missing":   {"hook_event_name": "PostToolUse", "tool_name": "Agent", "transcript_path": "/x/s.jsonl", "tool_response": lostResult},
	} {
		t.Run(name, func(t *testing.T) {
			if code, out, _ := runHook(t, Env{}, p); code != 0 || out != "" {
				t.Fatalf("exit=%d out=%q; want silent 0", code, out)
			}
		})
	}
}

func TestLongLines(t *testing.T) {
	big := strings.Repeat("x", 3<<20)
	lead := session(t, "big", toolResult(big), bashLaunch("b1"), toolResult(big))
	if c, _, _ := runHook(t, Env{StateDir: t.TempDir()}, idlePayload(lead, "big")); c != 2 {
		t.Fatalf("exit = %d, want 2", c)
	}
}
