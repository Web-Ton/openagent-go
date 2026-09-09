package openagent

// The legacy monolithic Memory interface has been split (P2, Context
// Architecture): short-term conversation storage lives in
// session.SessionStore, token-budget compression in session.Compressor,
// and durable knowledge in provider/memory.MemoryProvider. The root
// package keeps only the shared types below.

// CompressedContext bundles a summary with its coverage marker.
type CompressedContext struct {
	Summary      string `json:"summary"`
	ThroughIndex int    `json:"through_index"`
	// ThroughIndex marks how many messages have been covered by this summary.
	// The next compression pass only compresses messages after this index.
	// 0 means no compression has occurred (or the summary was produced by
	// an older version that didn't track this value).
}

// SafeCompressionBoundary adjusts the overflow index so compression doesn't
// break tool_call/tool_result pairs. If the last message in the compression
// range is an assistant with tool_calls, the boundary extends forward to
// include all consecutive tool results so the summary captures the complete
// tool exchange. all is in chronological order.
//
// It also guarantees an invariant: after compaction the retained working set
// (all[overflow:]) EITHER starts with a user message OR is empty. This is
// enforced by scanning forward from overflow to the next user message:
//   - found → overflow lands on that user (it stays in the working set)
//   - not found → overflow is pushed to len(all) (everything compressed,
//     working set is empty; the caller injects a <system-reminder> user
//     placeholder via ensureValidWorkingSet)
//
// Without this, an 80% compaction on a session with one user + a long
// assistant→tool chain compresses the only user into the summary, leaving a
// working set of pure assistant/tool messages that providers reject ("must
// contain at least one 'user' or 'tool' role") and that TrimOrphanToolCalls
// may delete to empty anyway.
//
// Returns the adjusted overflow index (may be larger than input).
func SafeCompressionBoundary(all []Message, overflow int) int {
	if overflow <= 0 || overflow >= len(all) {
		return overflow
	}

	lastCompressed := all[overflow-1]

	// If the last compressed message is an assistant with tool_calls,
	// its tool results (RoleTool) are in the working window. Extend
	// the boundary to include them so the summary captures the complete
	// tool exchange.
	if lastCompressed.Role == RoleAssistant && len(lastCompressed.ToolCalls) > 0 {
		for i := overflow; i < len(all); i++ {
			if all[i].Role == RoleTool {
				overflow = i + 1
			} else {
				break
			}
		}
	}

	// Invariant: working set starts with a user message or is empty.
	// Scan forward for the next user; if none exists, compress everything.
	for i := overflow; i < len(all); i++ {
		if all[i].Role == RoleUser {
			return i // user found — keep it as the first working message
		}
	}
	return len(all) // no user after overflow — compress all, working set is empty
}
