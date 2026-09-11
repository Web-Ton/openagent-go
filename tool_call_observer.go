package openagent

import "context"

// ToolCallEvent carries metadata about a single tool invocation.
// It is emitted by the MCP tool dispatch layer (mcp.Server's tool handler)
// at the completion of each tool.Execute, and consumed by
// ToolCallObserver implementations (e.g. track.ToolCallObserverImpl for
// HTTP event reporting).
//
// This mirrors the SessionObserver / SessionLifecycleEvent pattern but
// operates at tool-call granularity: SessionObserver observes the session
// lifecycle *around* runs; ToolCallObserver observes individual tool
// invocations *inside* a run. iac-server (a stateless MCP server with no
// session concept) uses this instead of SessionObserver to report tool
// usage counts.
type ToolCallEvent struct {
	ToolName   string // tool Definition().Name
	EntryPoint string // one of EntryPointIAC (session entry points live in session_observer.go)
	DurationMs int64  // handler wall-clock duration in milliseconds
	Err        error  // non-nil if tool.Execute returned an error
}

// ToolCallObserver receives tool-call events.  nil ToolCallObserver =
// events are silently dropped — callers MUST nil-check before invoking.
//
// This is a plain Go interface (no WASM, no plugin loader), compiled
// statically — same pattern as SessionObserver.  Implementations include
// track.ToolCallObserverImpl (HTTP event reporting); future
// implementations may cover OpenTelemetry spans or usage metering.
//
// Concurrency contract: implementations MUST be safe for concurrent use.
// Tool handlers run in parallel goroutines (e.g. concurrent MCP
// tools/call requests), so OnToolCall may be invoked from many
// goroutines at once.
type ToolCallObserver interface {
	OnToolCall(ctx context.Context, event ToolCallEvent)
}

// EntryPointIAC is the entry_point for iac-server tool-call events
// (ToolCallEvent.EntryPoint).  Session entry points (EntryPointACP /
// EntryPointCLI / EntryPointREST) live in session_observer.go; this one
// is tool-call-specific and kept here next to the ToolCallEvent it
// pertains to.
const EntryPointIAC = "iac"

// MultiToolCallObserver combines multiple ToolCallObservers into one.
// Each observer is called in order; nil observers are skipped.  Returns
// nil when no observers remain after filtering, so the caller can store
// the result directly and nil-check once.  Same combiner pattern as
// MultiSessionObserver.
func MultiToolCallObserver(observers ...ToolCallObserver) ToolCallObserver {
	var filtered []ToolCallObserver
	for _, o := range observers {
		if o != nil {
			filtered = append(filtered, o)
		}
	}
	switch len(filtered) {
	case 0:
		return nil
	case 1:
		return filtered[0]
	default:
		return &multiToolCallObserver{list: filtered}
	}
}

type multiToolCallObserver struct {
	list []ToolCallObserver
}

func (m *multiToolCallObserver) OnToolCall(ctx context.Context, event ToolCallEvent) {
	for _, o := range m.list {
		o.OnToolCall(ctx, event)
	}
}
