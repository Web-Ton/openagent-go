package acp

import (
	"context"
	"encoding/json"
	"testing"

	openagent "github.com/yusheng-g/openagent-go"
	openacp "github.com/yusheng-g/openagent-go/acp/sdk"
	"github.com/yusheng-g/openagent-go/governance"
)

// fakeClientRequester is a minimal ClientRequester that only answers
// RequestPermission; every other method returns a stub (unused by the
// approver path). The outcome it returns is scripted per-test.
type fakeClientRequester struct {
	outcome openacp.RequestPermissionOutcome
	err     error
}

func (f *fakeClientRequester) RequestPermission(ctx context.Context, req openacp.RequestPermissionRequest) (*openacp.RequestPermissionResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &openacp.RequestPermissionResponse{Outcome: f.outcome}, nil
}
func (f *fakeClientRequester) ReadTextFile(context.Context, openacp.ReadTextFileRequest) (*openacp.ReadTextFileResponse, error) {
	return nil, nil
}
func (f *fakeClientRequester) WriteTextFile(context.Context, openacp.WriteTextFileRequest) (*openacp.WriteTextFileResponse, error) {
	return nil, nil
}
func (f *fakeClientRequester) CreateTerminal(context.Context, openacp.CreateTerminalRequest) (*openacp.CreateTerminalResponse, error) {
	return nil, nil
}
func (f *fakeClientRequester) TerminalOutput(context.Context, openacp.TerminalOutputRequest) (*openacp.TerminalOutputResponse, error) {
	return nil, nil
}
func (f *fakeClientRequester) WaitForTerminalExit(context.Context, openacp.WaitForTerminalExitRequest) (*openacp.WaitForTerminalExitResponse, error) {
	return nil, nil
}
func (f *fakeClientRequester) KillTerminal(context.Context, openacp.KillTerminalRequest) (*openacp.KillTerminalResponse, error) {
	return nil, nil
}
func (f *fakeClientRequester) ReleaseTerminal(context.Context, openacp.ReleaseTerminalRequest) (*openacp.ReleaseTerminalResponse, error) {
	return nil, nil
}

func strPtr(s string) *openacp.PermissionOptionId { v := openacp.PermissionOptionId(s); return &v }

// sampleCall is a tool call with a real name + args so ApprovalKey and
// MemoryKeys produce stable, recallable keys.
func sampleCall() openagent.ToolCall {
	return openagent.ToolCall{
		ID:   "c1",
		Type: "function",
		Function: openagent.ToolCallFunction{
			Name:      "read_file",
			Arguments: `{"path":"/tmp/a.txt"}`,
		},
	}
}

func approverAsk(outcome openacp.RequestPermissionOutcome, err error) (governance.Decision, error) {
	a := &acpApprover{
		client: &fakeClientRequester{outcome: outcome, err: err},
	}
	return a.Ask(context.Background(), sampleCall(), openagent.FunctionDefinition{Name: "read_file"}, openagent.Session{})
}

// TestApproverSelectedAllowOnce: the spec path — outcome:"selected" +
// optionId:"allow_once" → Allow, NOT remembered (once semantics).
func TestApproverSelectedAllowOnce(t *testing.T) {
	mem := governance.NewSessionApprovalMemory()
	a := &acpApprover{
		client: &fakeClientRequester{outcome: openacp.RequestPermissionOutcome{
			Outcome:  openacp.PermissionOutcomeSelected,
			OptionID: strPtr("allow_once"),
		}},
		memory: mem,
	}
	d, err := a.Ask(context.Background(), sampleCall(), openagent.FunctionDefinition{Name: "read_file"}, openagent.Session{ID: "s1"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d.Action != governance.Allow {
		t.Fatalf("action = %s, want Allow", d.Action)
	}
	// allow_once must NOT persist — Recall misses.
	key := governance.ApprovalKey("read_file", json.RawMessage(`{"path":"/tmp/a.txt"}`))
	if _, ok := mem.Recall(context.Background(), "s1", key); ok {
		t.Fatalf("allow_once must not be remembered")
	}
}

// TestApproverSelectedAllowAlways: outcome:"selected" + allow_always →
// Allow, and the grant IS remembered (verifiable via Recall).
func TestApproverSelectedAllowAlways(t *testing.T) {
	mem := governance.NewSessionApprovalMemory()
	a := &acpApprover{
		client: &fakeClientRequester{outcome: openacp.RequestPermissionOutcome{
			Outcome:  openacp.PermissionOutcomeSelected,
			OptionID: strPtr("allow_always"),
		}},
		memory: mem,
	}
	d, err := a.Ask(context.Background(), sampleCall(), openagent.FunctionDefinition{Name: "read_file"}, openagent.Session{ID: "s1"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d.Action != governance.Allow {
		t.Fatalf("action = %s, want Allow", d.Action)
	}
	key := governance.ApprovalKey("read_file", json.RawMessage(`{"path":"/tmp/a.txt"}`))
	if got, ok := mem.Recall(context.Background(), "s1", key); !ok || got.Action != governance.Allow {
		t.Fatalf("allow_always not remembered: ok=%v got=%+v", ok, got)
	}
}

// TestApproverSelectedRejectOnce: outcome:"selected" + reject_once → Deny.
func TestApproverSelectedRejectOnce(t *testing.T) {
	d, err := approverAsk(openacp.RequestPermissionOutcome{
		Outcome:  openacp.PermissionOutcomeSelected,
		OptionID: strPtr("reject_once"),
	}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d.Action != governance.Deny {
		t.Fatalf("action = %s, want Deny", d.Action)
	}
}

// TestApproverCancelled: explicit outcome:"cancelled" → Deny "cancelled",
// regardless of whether an optionId is present (a dismiss must not be
// read as an approval even if the client redundantly echoes one).
func TestApproverCancelled(t *testing.T) {
	for name, oc := range map[string]openacp.RequestPermissionOutcome{
		"cancelled only":     {Outcome: openacp.PermissionOutcomeCancelled},
		"cancelled+optionId": {Outcome: openacp.PermissionOutcomeCancelled, OptionID: strPtr("allow_once")},
	} {
		t.Run(name, func(t *testing.T) {
			d, err := approverAsk(oc, nil)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if d.Action != governance.Deny || d.Reason != "cancelled" {
				t.Fatalf("got %s/%q, want Deny/cancelled", d.Action, d.Reason)
			}
		})
	}
}

// TestApproverLegacyLenient: a legacy client that predates the outcome
// discriminant sends only {"optionId":"allow_once"} (Outcome == ""). The
// approver must accept it leniently as "selected" — this is the live
// opencode-bug prevention: approvals never get read as rejections.
func TestApproverLegacyLenient(t *testing.T) {
	d, err := approverAsk(openacp.RequestPermissionOutcome{
		Outcome:  "", // missing discriminant (legacy)
		OptionID: strPtr("allow_once"),
	}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d.Action != governance.Allow {
		t.Fatalf("action = %s, want Allow (legacy lenient)", d.Action)
	}
}

// TestApproverEmptyDenies: a response with neither outcome nor optionId
// is ambiguous — fail closed.
func TestApproverEmptyDenies(t *testing.T) {
	d, err := approverAsk(openacp.RequestPermissionOutcome{}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d.Action != governance.Deny {
		t.Fatalf("action = %s, want Deny (fail closed)", d.Action)
	}
}

// TestApproverSelectedNoOptionIdDenies: outcome:"selected" without an
// optionId is malformed (spec requires optionId when selected) — deny.
func TestApproverSelectedNoOptionIdDenies(t *testing.T) {
	d, err := approverAsk(openacp.RequestPermissionOutcome{
		Outcome: openacp.PermissionOutcomeSelected,
	}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d.Action != governance.Deny {
		t.Fatalf("action = %s, want Deny (malformed selected)", d.Action)
	}
}

// TestApproverUnknownOutcomeDenies: an unrecognized outcome value (e.g.
// a future/typo'd discriminant) is denied rather than guessed.
func TestApproverUnknownOutcomeDenies(t *testing.T) {
	d, err := approverAsk(openacp.RequestPermissionOutcome{
		Outcome:  "maybe",
		OptionID: strPtr("allow_once"),
	}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d.Action != governance.Deny {
		t.Fatalf("action = %s, want Deny (unknown outcome)", d.Action)
	}
}

// TestApproverNoClientDenies: nil client → fail closed.
func TestApproverNoClientDenies(t *testing.T) {
	a := &acpApprover{client: nil}
	d, err := a.Ask(context.Background(), openagent.ToolCall{}, openagent.FunctionDefinition{}, openagent.Session{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d.Action != governance.Deny {
		t.Fatalf("action = %s, want Deny (no client)", d.Action)
	}
}

// riskCall is a tool call carrying a non-empty risk_note (destructive).
func riskCall() openagent.ToolCall {
	return openagent.ToolCall{
		ID:   "c2",
		Type: "function",
		Function: openagent.ToolCallFunction{
			Name:      "shell",
			Arguments: `{"command":"rm -rf /tmp/build","risk_note":"deleting build artifacts"}`,
		},
	}
}

func modeFn(m string) func() string { return func() string { return m } }

// TestApproverAutoModeAllowsRisk: auto mode is fully automatic — even a
// risk_note (destructive) call must Allow without prompting the client.
func TestApproverAutoModeAllowsRisk(t *testing.T) {
	a := &acpApprover{
		client: &fakeClientRequester{}, // would error if prompted, but auto must not prompt
		modeFn: modeFn("auto"),
	}
	d, err := a.Ask(context.Background(), riskCall(), openagent.FunctionDefinition{Name: "shell"}, openagent.Session{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d.Action != governance.Allow {
		t.Fatalf("auto+risk: action = %s, want Allow (fully automatic)", d.Action)
	}
}

// TestApproverSemiAutoAllowsSafe: semi-auto auto-allows calls WITHOUT a
// risk_note — no client prompt.
func TestApproverSemiAutoAllowsSafe(t *testing.T) {
	a := &acpApprover{
		client: &fakeClientRequester{},
		modeFn: modeFn("semi-auto"),
	}
	d, err := a.Ask(context.Background(), sampleCall(), openagent.FunctionDefinition{Name: "read_file"}, openagent.Session{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d.Action != governance.Allow {
		t.Fatalf("semi-auto safe: action = %s, want Allow", d.Action)
	}
}

// TestApproverSemiAutoPromptsRisk: semi-auto must prompt for a risk_note
// call — the client receives a permission request (not auto-allowed).
func TestApproverSemiAutoPromptsRisk(t *testing.T) {
	a := &acpApprover{
		client: &fakeClientRequester{outcome: openacp.RequestPermissionOutcome{
			Outcome:  openacp.PermissionOutcomeSelected,
			OptionID: strPtr("allow_once"),
		}},
		modeFn: modeFn("semi-auto"),
	}
	d, err := a.Ask(context.Background(), riskCall(), openagent.FunctionDefinition{Name: "shell"}, openagent.Session{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d.Action != governance.Allow {
		t.Fatalf("semi-auto risk: action = %s, want Allow (client approved)", d.Action)
	}
}

// TestApproverAutoAllowsSafe: auto mode auto-allows safe calls too (the
// common path — no risk_note, no prompt).
func TestApproverAutoAllowsSafe(t *testing.T) {
	a := &acpApprover{
		client: &fakeClientRequester{},
		modeFn: modeFn("auto"),
	}
	d, err := a.Ask(context.Background(), sampleCall(), openagent.FunctionDefinition{Name: "read_file"}, openagent.Session{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d.Action != governance.Allow {
		t.Fatalf("auto safe: action = %s, want Allow", d.Action)
	}
}
