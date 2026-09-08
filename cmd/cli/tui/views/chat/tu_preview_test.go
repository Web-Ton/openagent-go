package chat

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	openacp "github.com/yusheng-g/openagent-go/acp/sdk"
)

// TestTUPreview is a headless preview harness used by tu acceptance: it
// prints the rendered chat page with one message of every role so the
// per-role card styling can be inspected in a virtual terminal. It is
// skipped unless TU_PREVIEW=1, so it never runs in normal test passes.
//
//	TU_PREVIEW=1 go test ./cmd/cli/tui/views/chat/ -run TestTUPreview -v
func TestTUPreview(t *testing.T) {
	if os.Getenv("TU_PREVIEW") != "1" {
		t.Skip("preview harness; set TU_PREVIEW=1")
	}
	m := newTestModel()
	// Messages must be set before the WindowSizeMsg Update: syncViewport
	// (which feeds the transcript viewport) runs at the end of Update.
	m.messages = []ChatMessage{
		{Role: "user", Content: "帮我看看这个项目的结构", TurnId: 1},
		{Role: "thought", Content: "让我先扫描一下仓库", TurnId: 1},
		{Role: "tool", ToolName: "settings list", ToolStatus: toolDone, ToolInput: `{"action":"list"}`,
			ToolOutput: "{\n  \"provider\": {\n    \"openai\": {\n      \"api_key\": \"sk-407fae...\",\n      \"model\": \"gpt-4o\"\n    }\n  }\n}", TurnId: 1},
		{Role: "tool", ToolName: "read_file", ToolStatus: toolDone, ToolInput: `{"path":"README.md","limit":80}`,
			ToolOutput: "# openagent-go\n\nA Go agent runtime with an ACP bridge.", TurnId: 1},
		{Role: "tool", ToolName: "bash", ToolStatus: toolRunning, ToolInput: `{"command":"go test ./..."}`, TurnId: 1},
		{Role: "tool", ToolName: "write_file", ToolStatus: toolFailed, ToolInput: `{"path":"docs/notes.md"}`,
			ToolOutput: "permission denied: docs/notes.md", TurnId: 1},
		{Role: "assistant", Content: "这是一个 Go 项目,结构如下:\n\n- `cmd/` 命令入口\n- `docs/` 文档\n\n我建议先看 `cmd/cli/tui`。", TurnId: 1},
		{Role: "error", Content: "连接 ACP 服务超时(timeout after 30s)", TurnId: 1, CreatedAt: todayAt(14, 30)},
	}
	for i := range m.messages {
		if m.messages[i].CreatedAt.IsZero() {
			m.messages[i].CreatedAt = todayAt(14, 25)
		}
	}
	m.messages = append(m.messages,
		ChatMessage{Role: "compact", TurnId: 1, CompactStart: todayAt(14, 26), CompactEnd: todayAt(14, 26).Add(8 * time.Second), CompactedMsgs: 24, FreedTokens: 3400},
		ChatMessage{Role: "assistant", Content: "压缩完成后的回答。", TurnId: 2, CreatedAt: todayAt(14, 27)},
		// The announced-but-unapproved call behind the permission panel: it
		// must NOT list in the transcript while the dialog is open.
		ChatMessage{Role: "tool", ToolName: "shell rm -rf build", ToolStatus: toolPending,
			ToolCallID: "tc-perm", TurnId: 2, CreatedAt: todayAt(14, 28)},
	)
	m.inChat = true
	// The sidebar prefers the session's human title over the raw id.
	m.activeSessionID = "acp_1788831515590962986_1"
	m.sessionTitle = "PPT 渲染链路调研"
	// Sidebar context numbers mirror the style reference: 16,110 tokens,
	// 2% used of a 1M window.
	m.usedTokens = 16110
	m.contextSize = 1000000
	// A pending permission request renders the approval panel in the input
	// slot (opencode-style chips, second option preselected, command
	// detail under the muted title).
	m.permissionReq = &openacp.RequestPermissionRequest{
		ToolCall: openacp.ToolCallUpdate{
			ToolCallID: "tc-perm",
			Title:      "shell Read go.mod and README top",
			Kind:       "execute",
			RawInput:   json.RawMessage(`{"command":"cat go.mod README.md | head -40"}`),
		},
		Options: []openacp.PermissionOption{
			{OptionID: "once", Name: "Allow once"},
			{OptionID: "always", Name: "Allow always"},
			{OptionID: "reject", Name: "Reject"},
		},
	}
	m.permissionSelectedIdx = 1
	// A mid-backoff retry renders the transient divider at the tail.
	m.retry = &retryState{attempt: 2, max: 5, delay: 4 * time.Second,
		startedAt: time.Now(), err: "429 too many requests"}
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m.needAutoScroll = false
	view := m.View().Content
	// Write the rendered screen to a file rather than stdout so a tu session
	// can display exactly the TUI frame without go-test log lines around it.
	if err := os.WriteFile("/tmp/tu_preview.ansi", []byte(view), 0o644); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view, "┃") {
		t.Error("preview missing message rails")
	}
	// The unapproved call must not list in the transcript while its
	// permission dialog is open.
	if strings.Contains(view, "rm -rf build") {
		t.Error("pending tool call listed in transcript during permission dialog")
	}
}
