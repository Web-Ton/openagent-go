package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	openacp "github.com/yusheng-g/openagent-go/acp/sdk"
)

// acpEventHandler implements both openacp.EventHandler and
// openacp.ClientRequestHandler. The ACP SDK calls these methods from a
// background reader goroutine.
//
// Streaming events (EventHandler) are forwarded to the bubbletea model via
// Program.Send (one-way, fire-and-forget).
//
// Permission requests (ClientRequestHandler.HandleRequestPermission) need a
// response — a reply channel bridges the goroutine → TUI → goroutine round
// trip: the handler sends a msg with a channel, blocks on the channel; the
// TUI shows a dialog, the user picks an option, Update writes the response
// to the channel, the handler returns it to the server.
//
// Other ClientRequestHandler methods (fs, terminal) are not yet implemented.

// NewAcpEventHandler creates a handler that implements both EventHandler
// and ClientRequestHandler.
func NewAcpEventHandler(p *tea.Program) *acpEventHandler {
	return &acpEventHandler{program: p}
}

type acpEventHandler struct {
	program *tea.Program
}

// ── EventHandler ──

// acpMetaTime extracts the stored message wall-clock from a sessionUpdate's
// _meta (loadSession replay stamps "created_at" per message; live events
// carry no meta). Zero time when absent or unparsable.
func acpMetaTime(meta map[string]any) time.Time {
	if meta == nil {
		return time.Time{}
	}
	s, ok := meta["created_at"].(string)
	if !ok || s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (h *acpEventHandler) OnAgentMessage(text string, meta map[string]any) {
	h.program.Send(agentMessageMsg{text: text, createdAt: acpMetaTime(meta)})
}

func (h *acpEventHandler) OnAgentThought(text string, meta map[string]any) {
	h.program.Send(agentThoughtMsg{text: text, createdAt: acpMetaTime(meta)})
}

func (h *acpEventHandler) OnUserMessage(text string, meta map[string]any) {
	h.program.Send(userMessageMsg{text: text, createdAt: acpMetaTime(meta)})
}

// acpMetaInt reads an integer field from a sessionUpdate's _meta (JSON
// numbers unmarshal as float64).
func acpMetaInt(meta map[string]any, key string) int {
	if meta == nil {
		return 0
	}
	if f, ok := meta[key].(float64); ok {
		return int(f)
	}
	return 0
}

func acpMetaStr(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	s, _ := meta[key].(string)
	return s
}

func (h *acpEventHandler) OnContextCompacting(meta map[string]any) {
	h.program.Send(contextCompactingMsg{totalMessages: acpMetaInt(meta, "total_messages")})
}

func (h *acpEventHandler) OnContextCompacted(meta map[string]any) {
	h.program.Send(contextCompactedMsg{
		compressed: acpMetaInt(meta, "compressed_messages"),
		freed:      acpMetaInt(meta, "freed_tokens"),
		errStr:     acpMetaStr(meta, "error"),
	})
}

func (h *acpEventHandler) OnToolCall(tc openacp.ToolCallUpdate) {
	msg := toolCallMsg{id: tc.ToolCallID, title: tc.Title, status: toolRunning, createdAt: acpMetaTime(tc.Meta)}
	switch tc.Status {
	case "completed":
		msg.status = toolDone
	case "failed":
		msg.status = toolFailed
	}
	if b, err := json.Marshal(tc.RawInput); err == nil && string(b) != "null" {
		msg.input = string(b)
	}
	if b, err := json.Marshal(tc.RawOutput); err == nil && string(b) != "null" {
		msg.output = string(b)
	}
	h.program.Send(msg)
}

func (h *acpEventHandler) OnPlan(plan openacp.Plan) {
	h.program.Send(planMsg{entries: plan.Entries})
}

func (h *acpEventHandler) OnAvailableCommandsUpdate(cmds []openacp.AvailableCommand) {
}

func (h *acpEventHandler) OnModeUpdate(modeID openacp.SessionModeId) {
	h.program.Send(modeUpdateMsg{mode: string(modeID)})
}

func (h *acpEventHandler) OnConfigOptionUpdate(opts []openacp.SessionConfigOption) {
	h.program.Send(configOptionsMsg{opts: opts})
}

func (h *acpEventHandler) OnUsageUpdate(used, total int, cost *openacp.Cost) {
	h.program.Send(usageUpdateMsg{used: used, total: total})
}

func (h *acpEventHandler) OnSessionInfo(title string, metadata map[string]any) {}

// ── ClientRequestHandler ──

// HandleRequestPermission sends the permission request to the TUI via
// Program.Send and blocks on a reply channel until the user selects an
// option. If the context is cancelled (e.g. user quits), returns the error.
func (h *acpEventHandler) HandleRequestPermission(ctx context.Context, req openacp.RequestPermissionRequest) (*openacp.RequestPermissionResponse, error) {
	replyCh := make(chan openacp.RequestPermissionResponse, 1)
	h.program.Send(permissionRequestMsg{req: req, replyCh: replyCh})
	select {
	case resp := <-replyCh:
		return &resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *acpEventHandler) HandleReadTextFile(ctx context.Context, req openacp.ReadTextFileRequest) (*openacp.ReadTextFileResponse, error) {
	return nil, fmt.Errorf("fs/read_text_file not implemented")
}

func (h *acpEventHandler) HandleWriteTextFile(ctx context.Context, req openacp.WriteTextFileRequest) (*openacp.WriteTextFileResponse, error) {
	return nil, fmt.Errorf("fs/write_text_file not implemented")
}

func (h *acpEventHandler) HandleCreateTerminal(ctx context.Context, req openacp.CreateTerminalRequest) (*openacp.CreateTerminalResponse, error) {
	return nil, fmt.Errorf("terminal/create not implemented")
}

func (h *acpEventHandler) HandleTerminalOutput(ctx context.Context, req openacp.TerminalOutputRequest) (*openacp.TerminalOutputResponse, error) {
	return nil, fmt.Errorf("terminal/output not implemented")
}

func (h *acpEventHandler) HandleWaitForTerminalExit(ctx context.Context, req openacp.WaitForTerminalExitRequest) (*openacp.WaitForTerminalExitResponse, error) {
	return nil, fmt.Errorf("terminal/wait not implemented")
}

func (h *acpEventHandler) HandleKillTerminal(ctx context.Context, req openacp.KillTerminalRequest) (*openacp.KillTerminalResponse, error) {
	return nil, fmt.Errorf("terminal/kill not implemented")
}

func (h *acpEventHandler) HandleReleaseTerminal(ctx context.Context, req openacp.ReleaseTerminalRequest) (*openacp.ReleaseTerminalResponse, error) {
	return nil, fmt.Errorf("terminal/release not implemented")
}
