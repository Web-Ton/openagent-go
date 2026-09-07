package openagent

// ── Run result ──

// RunResult is the output of an Agent.Run call.
type RunResult struct {
	Messages      []Message // all messages from this run
	FinalOutput   string    // last assistant message content
	TurnCount     int
	Usage         Usage  // total tokens used
	ContextWindow int    // model's context window size (0 if unknown)
	StopReason    string // "end_turn", "refusal", "cancelled", etc. (ACP agents)
	RunID         string // trajectory grouping key — joins this result with its StageEvent/DecisionEvent stream
	ParentRunID   string // #1: the enclosing run's RunID (team/orchestrator); empty for solo runs
}

// ── Stream events ──

// StreamEventType categorizes events emitted by RunStream.
type StreamEventType string

const (
	StreamThought       StreamEventType = "thought" // reasoning content (o1, deepseek-r1)
	StreamTextDelta     StreamEventType = "text_delta"
	StreamToolCall      StreamEventType = "tool_call"
	StreamToolProgress  StreamEventType = "tool_progress" // streaming tool output chunk
	StreamToolResult    StreamEventType = "tool_result"
	StreamRetrying      StreamEventType = "retrying"
	StreamDone          StreamEventType = "done"
	StreamError         StreamEventType = "error"
	StreamAborted       StreamEventType = "aborted"        // context cancelled, deadline exceeded
	StreamSkillsUpdated StreamEventType = "skills_updated" // skill list changed (reload_skills)
	StreamCompacting    StreamEventType = "compacting"     // context compaction started
	StreamCompacted     StreamEventType = "compacted"      // context compaction finished
)

// StreamEvent is emitted by RunStream for real-time rendering.
type StreamEvent struct {
	Type       StreamEventType
	Text       string          // text_delta, tool_progress
	Message    Message         // tool_call, tool_result
	Result     *RunResult      // done
	Error      error           // error, retrying
	ToolCallID string          // tool_progress — disambiguates concurrent streaming tools
	Skills     []SkillInfo     // skills_updated — the new skill catalog
	Compaction *CompactionInfo // compacting/compacted — compaction state
	Retry      *RetryInfo      // retrying — retry state
}

// RetryInfo carries retry state for StreamRetrying events: which model is
// being retried, which attempt this is, how long the runtime will wait
// before the next attempt, and the error that triggered the retry.
type RetryInfo struct {
	Model          string  // model ID being retried (e.g. "gpt-4o")
	Attempt        int     // retry attempt number (1-based: 1st retry = 1)
	MaxRetries     int     // total retries allowed
	BackoffSeconds float64 // backoff delay before this retry
	Error          error   // the error that triggered the retry
}

// CompactionInfo carries compaction state for StreamCompacting/StreamCompacted
// events. On "compacting" it reports the overflow that triggered compaction;
// on "compacted" it reports the outcome (messages compressed, tokens freed).
type CompactionInfo struct {
	// OverflowTokens is how many tokens exceeded the budget (compacting only).
	OverflowTokens int
	// TotalMessages is the message count before compaction.
	TotalMessages int
	// CompressedMessages is how many messages were folded into the summary (compacted only).
	CompressedMessages int
	// FreedTokens is the approximate prompt tokens removed (compacted only).
	FreedTokens int
	// Error is non-empty when compaction failed (compacted only).
	Error string
}
