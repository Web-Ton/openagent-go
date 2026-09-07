package kernel

import (
	"context"
	"fmt"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/eventbus"
	"github.com/yusheng-g/openagent-go/execution"
	"github.com/yusheng-g/openagent-go/governance"
)

// executeTools orchestrates the ⑥ policy/approval + ⑦ execution nodes.
// Approval happens sequentially first (the user clicks through dialogs
// quickly), then approved tools execute concurrently. Results preserve
// the call order.
func (rt *Runtime) executeTools(ctx context.Context, session openagent.Session, calls []openagent.ToolCall, ch chan<- openagent.StreamEvent) []openagent.Message {
	if len(calls) == 0 {
		return nil
	}

	results := make([]openagent.Message, len(calls))

	policy := rt.policy()
	// Custom (non-Engine) Policy impls can't be instrumented per-layer, so
	// wrap them with observingPolicy to capture ONE aggregate final-outcome
	// event. The default *governance.Engine already emits deep per-layer
	// events via its DecObserver field, so it is NOT wrapped (double-counting).
	if _, isEngine := policy.(*governance.Engine); !isEngine {
		if obs := rt.decisionObserver(); obs != nil {
			policy = observingPolicy{inner: policy, obs: obs}
		}
	}

	// Phase 1: evaluate all tools sequentially through the policy chain.
	approved := make([]bool, len(calls))
	for i, call := range calls {
		rc := rt.execution.Resolve(call.Function.Name)
		if rc == nil {
			results[i] = openagent.Message{
				Role:       openagent.RoleTool,
				ToolCallID: call.ID,
				Content:    fmt.Sprintf("tool %q not found", call.Function.Name),
			}
			continue
		}
		rt.logEvent(ctx, session.ID, eventbus.EventApprovalRequest, call.Function.Name, map[string]string{"call_id": call.ID})
		decision, err := policy.Evaluate(ctx, call, rc.Def, session)
		if err != nil {
			results[i] = openagent.Message{
				Role:       openagent.RoleTool,
				ToolCallID: call.ID,
				Content:    fmt.Sprintf("policy error: %v", err),
			}
			continue
		}
		rt.state.RecordApproval(call, call.Function.Name, decision.Action, decision.Reason)
		rt.logEvent(ctx, session.ID, eventbus.EventApprovalResult, decision, map[string]string{"call_id": call.ID})
		if decision.Action == governance.Deny {
			results[i] = openagent.Message{
				Role:       openagent.RoleTool,
				ToolCallID: call.ID,
				Content:    fmt.Sprintf("this call rejected, reason: %s", decision.Reason),
			}
			continue
		}
		if decision.ModifiedArgs != nil {
			// Human edited the arguments — execute with the edited args.
			// Mutate the slice element: `call` is a range copy, and Phase 2
			// reads calls[i] again.
			calls[i].Function.Arguments = string(decision.ModifiedArgs)
		}
		approved[i] = true
	}

	// Phase 2: execute approved tools concurrently as jobs, then collect
	// in call order (ordering preserved, execution parallel).
	handles := make([]execution.ExecutionHandle, len(calls))
	for i, call := range calls {
		if !approved[i] {
			continue
		}
		rt.state.RecordExecution(call.ID, call.Function.Name, "running", time.Now())
		rt.logEvent(ctx, session.ID, eventbus.EventToolCall, call.Function.Name, map[string]string{"call_id": call.ID})
		handles[i] = rt.execution.Start(ctx, session, call, ch)
	}
	for i, h := range handles {
		if h == nil {
			continue
		}
		if err := h.Wait(ctx); err != nil && ctx.Err() != nil {
			// Run cancelled mid-execution: cancel this job and the rest,
			// then wait up to 5s for each to actually finish so Output()
			// doesn't block forever on a tool that ignores cancellation.
			h.Cancel()
			for _, rest := range handles[i+1:] {
				if rest != nil {
					rest.Cancel()
				}
			}
			// If the job doesn't finish within the grace period, synthesize
			// a cancelled result instead of blocking indefinitely — a tool
			// that ignores context (e.g. a long shell command with no
			// cancellation check) would otherwise hang executeTools forever.
			results[i] = h.OutputWithTimeout(5 * time.Second)
			if results[i].Content == "" {
				results[i] = openagent.Message{
					Role:       openagent.RoleTool,
					ToolCallID: calls[i].ID,
					Content:    "[tool execution cancelled — did not terminate within 5s]",
				}
			}
			rt.state.RecordExecution(calls[i].ID, calls[i].Function.Name, "cancelled", time.Time{})
			rt.logEvent(ctx, session.ID, eventbus.EventToolResult, results[i].Content, map[string]string{"call_id": calls[i].ID})
			continue
		}
		results[i] = h.Output()
		rt.state.RecordExecution(calls[i].ID, calls[i].Function.Name, "done", time.Time{})
		rt.logEvent(ctx, session.ID, eventbus.EventToolResult, results[i].Content, map[string]string{"call_id": calls[i].ID})
	}
	return results
}

// toolDef resolves a tool name to its FunctionDefinition (built-in or
// registered). Used by the loop's handoff check.
func (rt *Runtime) toolDef(name string) *openagent.FunctionDefinition {
	rc := rt.execution.Resolve(name)
	if rc == nil {
		return nil
	}
	d := rc.Def
	return &d
}

// toolDefinitions maps tools to their function definitions.
func toolDefinitions(tools []openagent.Tool) []openagent.FunctionDefinition {
	if len(tools) == 0 {
		return nil
	}
	defs := make([]openagent.FunctionDefinition, len(tools))
	for i, t := range tools {
		defs[i] = t.Definition()
	}
	return defs
}
