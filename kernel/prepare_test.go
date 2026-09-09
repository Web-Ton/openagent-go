package kernel

import (
	"strings"
	"testing"

	openagent "github.com/yusheng-g/openagent-go"
)

func TestEnsureValidWorkingSet_NonEmpty(t *testing.T) {
	// Non-empty working set (regardless of roles) — returned unchanged.
	// SafeCompressionBoundary guarantees it starts with a user, so a non-
	// empty set is always valid.
	msgs := []openagent.Message{
		{Role: openagent.RoleUser, Content: "hello"},
		{Role: openagent.RoleAssistant, Content: "hi"},
	}
	got := ensureValidWorkingSet(msgs)
	if len(got) != 2 {
		t.Fatalf("expected 2 msgs (unchanged), got %d", len(got))
	}
}

func TestEnsureValidWorkingSet_EmptyInjectsUser(t *testing.T) {
	// Empty working set — inject a synthetic <system-reminder> user.
	got := ensureValidWorkingSet(nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 (injected user), got %d", len(got))
	}
	if got[0].Role != openagent.RoleUser {
		t.Fatalf("expected RoleUser, got %s", got[0].Role)
	}
	if !strings.Contains(got[0].Content, "<system-reminder>") {
		t.Fatalf("expected <system-reminder> in content, got: %s", got[0].Content)
	}
	if !got[0].Transient {
		t.Fatalf("expected Transient=true (not persisted)")
	}
}
