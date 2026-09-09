package openagent

import "testing"

func TestSafeCompressionBoundary_AssistantToolPair(t *testing.T) {
	// Boundary lands on an assistant-with-tool_calls: extend to include
	// the trailing tool results, then forward-scan finds user at [3].
	msgs := []Message{
		{Role: RoleUser, Content: "u1"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "t1"}}},
		{Role: RoleTool, ToolCallID: "t1"},
		{Role: RoleUser, Content: "u2"},
	}
	// overflow=2 → extend past tool result → 3 → forward-scan: all[3] is user → 3
	got := SafeCompressionBoundary(msgs, 2)
	if got != 3 {
		t.Fatalf("expected 3, got %d", got)
	}
}

func TestSafeCompressionBoundary_ExtendsToNextUser(t *testing.T) {
	// overflow lands between two users, on an assistant. Forward-scan
	// moves overflow to the next user.
	msgs := []Message{
		{Role: RoleUser, Content: "u1"},
		{Role: RoleAssistant, Content: "a1"},
		{Role: RoleAssistant, Content: "a2"},
		{Role: RoleAssistant, Content: "a3"}, // overflow points here
		{Role: RoleUser, Content: "u2"},      // next user — boundary lands here
		{Role: RoleAssistant, Content: "a4"},
	}
	got := SafeCompressionBoundary(msgs, 3)
	if got != 4 {
		t.Fatalf("expected 4 (next user), got %d", got)
	}
}

func TestSafeCompressionBoundary_NoUserCompressesAll(t *testing.T) {
	// No user message after overflow — invariant says compress everything
	// (working set is empty; caller injects a placeholder).
	msgs := []Message{
		{Role: RoleUser, Content: "u1"},
		{Role: RoleAssistant, Content: "a1"},
		{Role: RoleAssistant, Content: "a2"},
	}
	// overflow=2 → forward-scan from 2 → no user → len(all)=3
	got := SafeCompressionBoundary(msgs, 2)
	if got != 3 {
		t.Fatalf("expected 3 (compress all, no user after), got %d", got)
	}
}

func TestSafeCompressionBoundary_SingleUserCompressesAll(t *testing.T) {
	// The production regression: one user + long assistant→tool chain.
	// 80% scan sets overflow past the only user. Forward-scan finds no
	// more users → compress all → working set empty → placeholder injected.
	msgs := []Message{
		{Role: RoleUser, Content: "the only user"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "t1"}}},
		{Role: RoleTool, ToolCallID: "t1"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "t2"}}},
		{Role: RoleTool, ToolCallID: "t2"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "t3"}}},
		{Role: RoleTool, ToolCallID: "t3"},
	}
	// overflow=6 → forward-scan from 6 → no user → len(all)=7
	got := SafeCompressionBoundary(msgs, 6)
	if got != 7 {
		t.Fatalf("expected 7 (compress all), got %d — working set must be empty", got)
	}
}

func TestSafeCompressionBoundary_UserAlreadyAtOverflow(t *testing.T) {
	// A user message is already at the overflow position — no change.
	msgs := []Message{
		{Role: RoleUser, Content: "u1"},
		{Role: RoleAssistant, Content: "a1"},
		{Role: RoleUser, Content: "u2"},
		{Role: RoleAssistant, Content: "a2"},
	}
	got := SafeCompressionBoundary(msgs, 2)
	if got != 2 {
		t.Fatalf("expected 2 (user already there), got %d", got)
	}
}
