package openagent

import (
	"context"
	"sync"
	"testing"
)

// captureToolCallObserver records every ToolCallEvent it receives.
type captureToolCallObserver struct {
	mu     sync.Mutex
	events []ToolCallEvent
}

func (c *captureToolCallObserver) OnToolCall(_ context.Context, e ToolCallEvent) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

func (c *captureToolCallObserver) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

func (c *captureToolCallObserver) snapshot() []ToolCallEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ToolCallEvent, len(c.events))
	copy(out, c.events)
	return out
}

func TestMultiToolCallObserver_NilFiltering(t *testing.T) {
	// All nil → returns nil (caller can nil-check once).
	obs := MultiToolCallObserver(nil, nil)
	if obs != nil {
		t.Fatal("MultiToolCallObserver(nil, nil) should return nil")
	}
}

func TestMultiToolCallObserver_SingleFlatten(t *testing.T) {
	// A single non-nil observer should be returned directly (not wrapped).
	c := &captureToolCallObserver{}
	obs := MultiToolCallObserver(nil, c, nil)
	if obs != c {
		t.Fatal("MultiToolCallObserver with one non-nil should return it directly")
	}
}

func TestMultiToolCallObserver_FanOut(t *testing.T) {
	a := &captureToolCallObserver{}
	b := &captureToolCallObserver{}
	obs := MultiToolCallObserver(a, b)

	evt := ToolCallEvent{ToolName: "list_deployments", EntryPoint: EntryPointIAC}
	obs.OnToolCall(context.Background(), evt)

	if a.count() != 1 || b.count() != 1 {
		t.Fatalf("expected both observers to receive the event; got a=%d b=%d",
			a.count(), b.count())
	}
}

func TestMultiToolCallObserver_PreservesFields(t *testing.T) {
	c := &captureToolCallObserver{}
	obs := MultiToolCallObserver(c)

	evt := ToolCallEvent{
		ToolName:   "apply_deployment",
		EntryPoint: EntryPointIAC,
		DurationMs: 999,
	}
	obs.OnToolCall(context.Background(), evt)

	got := c.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].ToolName != "apply_deployment" {
		t.Errorf("ToolName = %q, want apply_deployment", got[0].ToolName)
	}
	if got[0].EntryPoint != EntryPointIAC {
		t.Errorf("EntryPoint = %q, want %q", got[0].EntryPoint, EntryPointIAC)
	}
	if got[0].DurationMs != 999 {
		t.Errorf("DurationMs = %d, want 999", got[0].DurationMs)
	}
}

func TestEntryPointIAC_Value(t *testing.T) {
	if EntryPointIAC != "iac" {
		t.Errorf("EntryPointIAC = %q, want \"iac\"", EntryPointIAC)
	}
}
