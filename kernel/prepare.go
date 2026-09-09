package kernel

import (
	"context"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
)

// compactionInfo carries compaction observability back to the loop.
type compactionInfo struct {
	err         error
	count       int                          // number of messages newly compressed
	freedTokens int                          // prompt tokens the new summary removed (approx, can be 0)
	from, to    int                          // global index range covered (for observability)
	compressed  *openagent.CompressedContext // summary after this pass (nil if none)
	// attempted is true when Compact was actually called (and the
	// "context compacting..." pre-thought was sent to ch). The caller
	// uses this to decide whether to emit a matching follow-up: when
	// false, no compaction ran and there is nothing to close.
	attempted bool
	// summaryTokens is the token cost of the new summary. The manual /compact
	// path (CompressAll) sets it from CompressStats; the auto path
	// (prepareMemory) leaves 0 — compactionOutcome still emits the key so
	// manual and auto detail share the same shape (a consumer sees 0 and
	// knows summary cost was not measured for that pass).
	summaryTokens int
	// throughIndex is the backend's post-compaction ThroughIndex marker. The
	// manual path sets it from cc.ThroughIndex; the auto path leaves 0 and
	// relies on from/to for the range (cc.ThroughIndex == ci.to there, so the
	// marker adds no information beyond what `to` already carries).
	throughIndex int
}

// workingTokenBudget returns the token budget for the working message set.
// Explicit MaxWorkingTokens wins; otherwise 70% of the model context
// window; falls back to 20000.
func (rt *Runtime) workingTokenBudget() int {
	if rt.cfg.MaxWorkingTokens > 0 {
		return rt.cfg.MaxWorkingTokens
	}
	if cw := rt.runModel.ContextWindow(); cw > 0 {
		return cw * 7 / 10 // 70%
	}
	return 20000
}

// prepareMemory fetches the working message set, triggers token-based
// compaction if needed, and trims to the token budget. Messages are NEVER
// deleted — compaction only updates the summary.
//
// The returned compactionInfo.err carries a compaction failure if one
// occurred (observability only; the working set is still usable).
//
// ch receives a StreamThought ("context compacting...") immediately before
// the Compact call so ACP clients can surface the stall to the user (a
// compaction attempt can take tens of seconds against a slow gateway).
// The matching "compacted/failed" follow-up is emitted by the caller
// (run.go) after inspecting the returned compactionInfo. ch may be nil.
func (rt *Runtime) prepareMemory(ctx context.Context, session openagent.Session, ch chan<- openagent.StreamEvent) ([]openagent.Message, compactionInfo, error) {
	var ci compactionInfo
	if rt.deps.SessionStore == nil {
		return nil, ci, nil
	}
	// enter/leave pair for the memory-fetch stage (leave duration covers
	// compaction + working-set trim; the detail reports the fetch window —
	// total stored messages vs the post-summary increment actually read —
	// plus the compaction outcome of this pass).
	var (
		totalCount int
		from       int
		msgs       []openagent.Message
		err        error
	)
	start := time.Now()
	rt.observe(ctx, openagent.StageMemoryFetch, "enter", nil, time.Time{}, nil)
	defer func() {
		rt.observe(ctx, openagent.StageMemoryFetch, "leave", map[string]any{
			"total":     totalCount,
			"from":      from,
			"fetched":   len(msgs),
			"compacted": ci.count, // messages newly covered by the summary
			"comp_from": ci.from,  // global index range of the new summary
			"comp_to":   ci.to,    // (exclusive end of the range)
		}, start, err)
	}()

	// ── Load any existing summary unconditionally ──
	// Manual compaction (/compact) can leave a session whose remaining
	// messages fit the budget — without this load the summary would not
	// be injected and history would silently vanish from the prompt.
	// Auto-compaction below overwrites ci.compressed with the new summary.
	if rt.deps.Compressor != nil {
		if cc, err := rt.deps.Compressor.Compressed(ctx, session.ID); err == nil && cc != nil && cc.Summary != "" {
			ci.compressed = cc
		}
	}
	// Make the summary visible to the prompt-overhead estimate below
	// (estimatePromptOverhead reads rt.compressed, which the loop assigns
	// only AFTER prepareMemory returns). A freshly loaded summary must
	// count against this turn's budget, or a small-window model plus a big
	// summary overflows and hard-fails every run after /compact.
	if ci.compressed != nil {
		rt.compressed = ci.compressed
	}

	budget := rt.workingTokenBudget()

	// ── Subtract fixed overhead that the prompt adds ──
	modelID := openagent.TokenizerModelID(rt.runModel)
	overhead := rt.estimatePromptOverhead(ctx, session, modelID)
	budget -= overhead
	if budget < 500 {
		budget = 500 // keep a minimal working window
	}

	// Fetch total count and the post-summary increment. Messages are never
	// deleted, so RecentAfter skips everything the summary already covers
	// (no 5000 cap: the overflow scan below must see every un-summarized
	// message to decide the boundary, and the head can no longer fall
	// outside the fetch window).
	totalCount, err = rt.deps.SessionStore.Count(ctx, session.ID)
	if err != nil {
		return nil, ci, err
	}
	if totalCount == 0 {
		return nil, ci, nil
	}
	from = 0
	if ci.compressed != nil {
		from = ci.compressed.ThroughIndex
	}
	msgs, err = rt.deps.SessionStore.RecentAfter(ctx, session.ID, from, totalCount-from)
	if err != nil || len(msgs) == 0 {
		return nil, ci, err
	}
	// msgs starts at global index `from` (RecentAfter does no trimming),
	// so local index == global index - from.

	// ── Compaction pass: compress overflow messages ──
	// The token scan walks only the post-summary increment (already
	// compressed messages are not fetched at all).
	//
	// Two-step logic:
	//   1. Trigger check: compute the TOTAL token count of the working set.
	//      Only when it exceeds the budget (real overflow) does compaction
	//      fire — not at 20%, not at 50%, at 100%.
	//   2. Retain target: once triggered, compress CompactRatio (default 80%)
	//      of the budget and keep the most recent retainFraction (20%) as
	//      headroom. The scan walks from the tail, and overflow lands where
	//      cumulative tokens first exceed retainTarget.
	//
	// The previous code used retainTarget as the TRIGGER threshold (break at
	// 20% → compaction fired at 20% of budget, far before real overflow).
	// That caused premature compaction on small conversations, unnecessary
	// summary growth, and a faster positive-feedback loop — the opposite of
	// what CompactRatio was meant to fix.
	retainFraction := 1.0 - rt.cfg.CompactRatio
	if retainFraction <= 0 || retainFraction >= 1 {
		retainFraction = 0.2 // default 0.8 ratio → retain 20%
	}
	retainTarget := int(float64(budget) * retainFraction)

	// Single tail-to-head scan serves both purposes:
	//   1. Trigger: after the full scan, `tokens` is the total working-set
	//      size — compaction fires only when total > budget (real overflow).
	//   2. Retain: if total > budget, the scan already recorded where
	//      cumulative tokens first exceeded retainTarget — that's the
	//      overflow point (compress everything before it, keep the tail).
	// Each message's tokens are counted exactly once.
	// modelID already declared above (L110); reused here.
	overflow := len(msgs) // default: no compaction (budget fits)
	tokens := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		tokens += openagent.CountMessageTokens(modelID, msgs[i])
		if tokens > retainTarget && overflow == len(msgs) {
			overflow = i + 1 // first (from tail) index past retainTarget
		}
	}
	// Trigger check: only compact on real overflow, not at retainTarget.
	if tokens <= budget {
		overflow = len(msgs) // total fits budget — no compaction
	}
	if overflow < len(msgs) {
		overflow = openagent.SafeCompressionBoundary(msgs, overflow)
		oldTI := 0
		if rt.deps.Compressor != nil {
			if cc, err := rt.deps.Compressor.Compressed(ctx, session.ID); err == nil && cc != nil {
				oldTI = cc.ThroughIndex
			}
		}
		globalCutoff := from + overflow
		if rt.deps.Compressor != nil {
			// Surface the compaction attempt to the user before the
			// (potentially slow) Compact call — a gateway 504 can stall
			// here for up to the summarizer's per-call timeout. Without
			// this hint the ACP client just shows a spinning agent with
			// no explanation. The follow-up ("compacted N tokens" /
			// "failed") is emitted by run() after this returns.
			chSend(ctx, ch, openagent.StreamEvent{
				Type: openagent.StreamCompacting,
				Compaction: &openagent.CompactionInfo{
					OverflowTokens: overflow,
					TotalMessages:  len(msgs),
				},
			})
			ci.attempted = true
			openagent.ObserveDecision(ctx, rt.deps.Observer, openagent.DecisionEvent{
				Layer: openagent.DecisionCompactionAuto, Outcome: openagent.OutcomeAttempted,
				Subject: session.ID, Detail: map[string]any{
					"overflow": overflow,
					"total":    len(msgs),
					"cutoff":   globalCutoff,
					"prev_ti":  oldTI,
					"budget":   budget,
				},
			})
			// messages=nil: the backend re-fetches from the session head.
			// globalCutoff is a GLOBAL message index, but msgs is the
			// post-summary window — the backend's prefetch branch only
			// trusts a slice that starts at the session head, so passing
			// msgs here would misalign.
			ci.err = rt.deps.Compressor.Compact(ctx, session.ID, globalCutoff, nil)
		}
		if rt.deps.Compressor != nil {
			if ci.err == nil {
				if cc, err := rt.deps.Compressor.Compressed(ctx, session.ID); err == nil && cc != nil {
					ci.compressed = cc
					if cc.ThroughIndex > oldTI {
						ci.count = cc.ThroughIndex - oldTI
						ci.from = oldTI
						ci.to = cc.ThroughIndex
						// freedTokens = tokens freed by compaction: the
						// compressed messages (msgs[:overflow]) minus the
						// summary that replaces them. This mirrors
						// CompressAll's accounting so the ACP "Compacted N
						// messages → summary (freed ~K tokens)" thought is
						// consistent with the /compact slash echo.
						modelID := openagent.TokenizerModelID(rt.runModel)
						freed := openagent.CountMessages(modelID, msgs[:overflow])
						freed -= openagent.CountMessageTokens(modelID, openagent.Message{
							Role:    openagent.RoleSystem,
							Content: cc.Summary,
						})
						if freed < 0 {
							freed = 0
						}
						ci.freedTokens = freed
						// The backend may advance ThroughIndex past the
						// overflow point (SafeCompressionBoundary
						// adjustments). The working set must not re-inject
						// what the NEW summary already covers — the summary
						// and the raw messages would double-count against
						// this turn's budget.
						if ti := cc.ThroughIndex - from; ti > overflow {
							overflow = ti
							if overflow > len(msgs) {
								overflow = len(msgs)
							}
						}
					} else {
						// Compact was a silent no-op (no summarizer
						// configured, or nothing new to compress): trimming
						// here would drop the head with no summary to cover
						// it — those messages would vanish from the prompt
						// forever. Keep the whole working set instead; the
						// prompt hard window check errors rather than
						// silently forgetting (fail-loud).
						overflow = len(msgs)
					}
				} else {
					// No summary came back (read failed / backend has no
					// marker): same no-advance treatment.
					overflow = len(msgs)
				}
			} else {
				// Compaction failed: the summary does NOT cover the head.
				// The original fail-loud design kept the whole working set
				// and let the hard window check error. But when the summarizer
				// fails persistently (429, timeout, network), this makes every
				// turn return the FULL un-trimmed history — across sessions the
				// prompt grows ~2× per run (370K → 730K) and every run crashes.
				// Degrade instead: trim to budget from the tail (keep the most
				// recent messages), losing the oldest un-summarized messages.
				// They remain in the store (never deleted); a later successful
				// compaction can still fold them into a summary. Losing history
				// is better than crashing every run.
				overflow = openagent.SafeCompressionBoundary(msgs, overflow)
			}
		}
		// Emit the auto-compaction outcome — mirrors CompressAll's
		// Attempted/Failed/Freed/Skipped vocabulary so a consumer treats
		// manual and auto compaction uniformly. ci.attempted gates this:
		// no Compact call means no event (the Attempted above didn't fire).
		if ci.attempted {
			_, outcome, detail := compactionOutcome(ci)
			openagent.ObserveDecision(ctx, rt.deps.Observer, openagent.DecisionEvent{
				Layer: openagent.DecisionCompactionAuto, Outcome: outcome,
				Subject: session.ID, Detail: detail,
			})
		}
	}

	// ── Working set: trim to token budget ──
	// No compressor = trimming drops the head with no way to recover it
	// (fail-loud philosophy: the prompt hard window check errors instead
	// of silently forgetting). With a compressor, msgs already excludes
	// the OLD summary's coverage; overflow carries either the token-trim
	// point or the new summary's coverage (whichever is later — the
	// compaction pass above reconciles them).
	//
	// ci.count > 0 is the discriminator for the overflow == len(msgs)
	// cases: budget fits / compaction did NOT advance (keep everything —
	// trimming would drop the head with no summary to cover it) vs.
	// compaction advanced and the summary covers everything (working set
	// must be empty, or the summary and the raw messages double-count).
	if rt.deps.Compressor == nil {
		return msgs, ci, nil
	}
	if ci.count == 0 && ci.err == nil {
		// No compaction advanced AND no failure — budget fit or silent no-op.
		// Keep everything (trimming would drop the head with no summary).
		return msgs, ci, nil
	}
	if ci.count == 0 && ci.err != nil {
		// Compaction failed: trim to budget from the tail (degrade gracefully
		// — see the comment at the failure site above). overflow was set to
		// the SafeCompressionBoundary of the token-trim point.
		if overflow < len(msgs) {
			return msgs[overflow:], ci, nil
		}
		return msgs, ci, nil
	}
	return msgs[overflow:], ci, nil
}
