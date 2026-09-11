package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	openagent "github.com/yusheng-g/openagent-go"
)

// captureToolCallObserver records every ToolCallEvent it receives, for
// asserting that the mcp Server's tool handler emits tracking events.
// Mutex-guarded to match the ToolCallObserver concurrency contract
// (implementations MUST be safe for concurrent use) — even though the
// current tests are serial, this keeps the test double honest.
type captureToolCallObserver struct {
	mu     sync.Mutex
	events []openagent.ToolCallEvent
}

func (c *captureToolCallObserver) OnToolCall(_ context.Context, e openagent.ToolCallEvent) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

// snapshot returns a copy of the recorded events for lock-free assertion.
func (c *captureToolCallObserver) snapshot() []openagent.ToolCallEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]openagent.ToolCallEvent, len(c.events))
	copy(out, c.events)
	return out
}

// errorTool always returns a structured error, to verify the handler maps
// ToolError.Message into ToolCallEvent.Err.
type errorTool struct{}

func (t *errorTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "fail_tool",
		Description: "Always fails.",
		Parameters:  openagent.SchemaOf[struct{}](),
	}
}

func (t *errorTool) Execute(_ context.Context, _ json.RawMessage) *openagent.ToolResult {
	return openagent.ErrorResult(errToolCall("boom"), false, "E_FAIL")
}

// errToolCall is a tiny error type so ErrorResult can wrap it (ErrorResult
// takes an error, extracts .Error() into ToolError.Message).
type errToolCall string

func (e errToolCall) Error() string { return string(e) }

// TestToolCallObserver_AddToolPath verifies that a tool registered via
// AddTool emits a ToolCallEvent through the wired observer when invoked
// via the real client tools/call path.
func TestToolCallObserver_AddToolPath(t *testing.T) {
	obs := &captureToolCallObserver{}
	server := NewServer("test-server", "1.0.0", nil)
	server.SetToolCallObserver(obs)
	if err := server.AddTool(&echoTool{}); err != nil {
		t.Fatalf("AddTool: %v", err)
	}

	st, ct := mcpsdk.NewInMemoryTransports()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(context.Background(), st) }()
	defer func() { <-serverDone }()

	ctx := context.Background()
	client := NewClient("test-client", "1.0.0")
	session, err := client.ConnectTransport(ctx, ct)
	if err != nil {
		t.Fatalf("ConnectTransport: %v", err)
	}
	defer session.Close()

	tools, err := session.Tools(ctx)
	if err != nil || len(tools) == 0 {
		t.Fatalf("Tools: %v (n=%d)", err, len(tools))
	}
	args, _ := json.Marshal(map[string]string{"message": "hi"})
	res := tools[0].Execute(ctx, args)
	if res.Error != nil || res.Content != "echo: hi" {
		t.Fatalf("Execute = %+v, want echo: hi", res)
	}

	evts := obs.snapshot()
	if len(evts) != 1 {
		t.Fatalf("expected 1 ToolCallEvent, got %d", len(evts))
	}
	evt := evts[0]
	if evt.ToolName != "echo" {
		t.Errorf("ToolName = %q, want echo", evt.ToolName)
	}
	if evt.EntryPoint != openagent.EntryPointIAC {
		t.Errorf("EntryPoint = %q, want %q", evt.EntryPoint, openagent.EntryPointIAC)
	}
	if evt.DurationMs < 0 {
		t.Errorf("DurationMs = %d, want >= 0", evt.DurationMs)
	}
	if evt.Err != nil {
		t.Errorf("Err = %v, want nil (tool succeeded)", evt.Err)
	}
}

// TestToolCallObserver_AddToolWithSchemaPath verifies the SECOND
// registration path (AddToolWithSchema) also wires the observer — the
// shared buildToolHandler covers both.
func TestToolCallObserver_AddToolWithSchemaPath(t *testing.T) {
	obs := &captureToolCallObserver{}
	server := NewServer("test-server", "1.0.0", nil)
	// AddToolWithSchema takes a raw JSON schema; a minimal object schema
	// is enough to register — the test only cares that the handler is wired.
	schema := json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}}}`)
	if err := server.AddToolWithSchema(&echoTool{}, schema); err != nil {
		t.Fatalf("AddToolWithSchema: %v", err)
	}
	server.SetToolCallObserver(obs)

	st, ct := mcpsdk.NewInMemoryTransports()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(context.Background(), st) }()
	defer func() { <-serverDone }()

	ctx := context.Background()
	client := NewClient("test-client", "1.0.0")
	session, err := client.ConnectTransport(ctx, ct)
	if err != nil {
		t.Fatalf("ConnectTransport: %v", err)
	}
	defer session.Close()

	tools, err := session.Tools(ctx)
	if err != nil || len(tools) == 0 {
		t.Fatalf("Tools: %v (n=%d)", err, len(tools))
	}
	args, _ := json.Marshal(map[string]string{"message": "schema"})
	tools[0].Execute(ctx, args)

	evts := obs.snapshot()
	if len(evts) != 1 {
		t.Fatalf("expected 1 ToolCallEvent, got %d", len(evts))
	}
	if evts[0].ToolName != "echo" {
		t.Errorf("ToolName = %q, want echo", evts[0].ToolName)
	}
}

// TestToolCallObserver_ErrorResult verifies that when a tool returns a
// structured error, the ToolCallEvent.Err carries the error message.
func TestToolCallObserver_ErrorResult(t *testing.T) {
	obs := &captureToolCallObserver{}
	server := NewServer("test-server", "1.0.0", nil)
	server.SetToolCallObserver(obs)
	if err := server.AddTool(&errorTool{}); err != nil {
		t.Fatalf("AddTool: %v", err)
	}

	st, ct := mcpsdk.NewInMemoryTransports()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(context.Background(), st) }()
	defer func() { <-serverDone }()

	ctx := context.Background()
	client := NewClient("test-client", "1.0.0")
	session, err := client.ConnectTransport(ctx, ct)
	if err != nil {
		t.Fatalf("ConnectTransport: %v", err)
	}
	defer session.Close()

	tools, err := session.Tools(ctx)
	if err != nil || len(tools) == 0 {
		t.Fatalf("Tools: %v (n=%d)", err, len(tools))
	}
	tools[0].Execute(ctx, json.RawMessage(`{}`))

	evts := obs.snapshot()
	if len(evts) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evts))
	}
	evt := evts[0]
	if evt.ToolName != "fail_tool" {
		t.Errorf("ToolName = %q, want fail_tool", evt.ToolName)
	}
	if evt.Err == nil {
		t.Fatal("Err = nil, want non-nil (tool returned an error)")
	}
	if evt.Err.Error() != "boom" {
		t.Errorf("Err = %q, want 'boom'", evt.Err.Error())
	}
}

// TestToolCallObserver_NilObserverNoPanic verifies that with no observer
// wired (the dev/default case), tool invocation works normally and does
// not panic.
func TestToolCallObserver_NilObserverNoPanic(t *testing.T) {
	server := NewServer("test-server", "1.0.0", nil)
	// No SetToolCallObserver call — toolCallObserver is nil.
	if err := server.AddTool(&echoTool{}); err != nil {
		t.Fatalf("AddTool: %v", err)
	}

	st, ct := mcpsdk.NewInMemoryTransports()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(context.Background(), st) }()
	defer func() { <-serverDone }()

	ctx := context.Background()
	client := NewClient("test-client", "1.0.0")
	session, err := client.ConnectTransport(ctx, ct)
	if err != nil {
		t.Fatalf("ConnectTransport: %v", err)
	}
	defer session.Close()

	tools, err := session.Tools(ctx)
	if err != nil || len(tools) == 0 {
		t.Fatalf("Tools: %v (n=%d)", err, len(tools))
	}
	args, _ := json.Marshal(map[string]string{"message": "ok"})
	res := tools[0].Execute(ctx, args)
	if res.Error != nil || res.Content != "echo: ok" {
		t.Fatalf("Execute with nil observer = %+v, want echo: ok (no panic)", res)
	}
}
