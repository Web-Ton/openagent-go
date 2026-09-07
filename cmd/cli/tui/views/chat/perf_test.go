package chat

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// mkMarkdown builds a markdown document that mixes styled elements
// (headings, bold/inline code, fenced code, lists, tables) repeated to
// roughly the requested size.
func mkMarkdown(size int) string {
	block := "# Heading\n\nSome **bold** and `inline code` with [a link](https://x.example).\n\n" +
		"```go\nfunc main() { fmt.Println(\"hi\") }\n```\n\n" +
		"- item one\n- item two\n\n" +
		"| col A | col B |\n| ----- | ----- |\n| 1 | 2 |\n\n"
	var sb strings.Builder
	for sb.Len() < size {
		sb.WriteString(block)
	}
	return sb.String()[:size]
}

// newBenchModel is a chat model sized like a real terminal.
func newBenchModel() *Model {
	m := newTestModel()
	m.width = 120
	m.height = 40
	return m
}

// ── single 1MB markdown message ──

// BenchmarkRenderMarkdownText1M measures one markdown render of a 1MB
// assistant message (the reply path), including the surface-background pass.
func BenchmarkRenderMarkdownText1M(b *testing.B) {
	doc := mkMarkdown(1_000_000)
	b.SetBytes(int64(len(doc)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if out := renderMarkdownText(doc, 94); out == "" {
			b.Fatal("empty render")
		}
	}
}

// BenchmarkMarkdownSurfaceBG1M isolates the SGR repaint of the 1MB render
// output (what glosses every span with the card surface background).
func BenchmarkMarkdownSurfaceBG1M(b *testing.B) {
	out := renderMarkdownText(mkMarkdown(1_000_000), 94)
	b.SetBytes(int64(len(out)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := markdownSurfaceBG(out); got == "" {
			b.Fatal("empty output")
		}
	}
}

// BenchmarkRenderMessagesSingle1M runs the full message pipeline for one
// 1MB assistant message (styleMessageBlock → markdown → messageCard).
func BenchmarkRenderMessagesSingle1M(b *testing.B) {
	m := newBenchModel()
	m.messages = []ChatMessage{{Role: "assistant", Content: mkMarkdown(1_000_000)}}
	b.SetBytes(1_000_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.renderCache = nil
		if out := m.renderMessages(); out == "" {
			b.Fatal("empty render")
		}
	}
}

// ── 1MB spread over many messages ──

// BenchmarkRenderMessages1M_1000x1K renders 1000 messages totalling ~1MB
// (cold cache: the budget-constrained full-document path).
func BenchmarkRenderMessages1M_1000x1K(b *testing.B) {
	doc := mkMarkdown(1_000)
	m := newBenchModel()
	m.messages = make([]ChatMessage, 1000)
	for i := range m.messages {
		m.messages[i] = ChatMessage{Role: "assistant", Content: doc}
	}
	b.SetBytes(1_000_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.renderCache = nil
		if out := m.renderMessages(); out == "" {
			b.Fatal("empty render")
		}
	}
}

// BenchmarkRenderMessages1M_1000x1K_Warm reuses the per-message style cache
// (steady-state: unchanged messages skip re-styling).
func BenchmarkRenderMessages1M_1000x1K_Warm(b *testing.B) {
	doc := mkMarkdown(1_000)
	m := newBenchModel()
	m.messages = make([]ChatMessage, 1000)
	for i := range m.messages {
		m.messages[i] = ChatMessage{Role: "assistant", Content: doc}
	}
	m.renderMessages() // prime the cache
	b.SetBytes(1_000_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if out := m.renderMessages(); out == "" {
			b.Fatal("empty render")
		}
	}
}

// ── realistic single-message sizes (cold cache) ──

func benchSingleSize(b *testing.B, size int) {
	m := newBenchModel()
	m.messages = []ChatMessage{{Role: "assistant", Content: mkMarkdown(size)}}
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.renderCache = nil
		if out := m.renderMessages(); out == "" {
			b.Fatal("empty render")
		}
	}
}

func BenchmarkRenderMessagesSingle100K(b *testing.B) { benchSingleSize(b, 100_000) }
func BenchmarkRenderMessagesSingle10K(b *testing.B)  { benchSingleSize(b, 10_000) }
func BenchmarkRenderMessagesSingle1K(b *testing.B)   { benchSingleSize(b, 1_000) }

// ── virtual-scroll window over 1MB ──

// BenchmarkVirtualWindow1M renders the visible 30-row window of a 1MB
// transcript (100 messages × 10KB) — the steady path while scrolling.
func BenchmarkVirtualWindow1M(b *testing.B) {
	doc := mkMarkdown(10_000)
	m := newBenchModel()
	m.messages = make([]ChatMessage, 100)
	for i := range m.messages {
		m.messages[i] = ChatMessage{Role: "assistant", Content: doc}
	}
	b.SetBytes(1_000_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.renderCache = nil
		if out := m.renderVirtualDocAt(30, 5_000); out == "" {
			b.Fatal("empty render")
		}
	}
}

// ── 1M-token context: interactive latency through the real Update path ──

// fillToContextCap stuffs the transcript to the in-memory cap
// (maxStoredChars ≈ 2MB ≈ a 1M-token context) with a realistic role mix:
// user prompts, markdown answers, thoughts, and tool rows with outputs.
func fillToContextCap(m *Model) {
	block := mkMarkdown(900)
	m.messages = m.messages[:0]
	total := 0
	for i := 0; total < maxStoredChars; i++ {
		switch i % 5 {
		case 0:
			m.messages = append(m.messages, ChatMessage{Role: "user", Content: "帮我分析一下这段代码的并发安全问题", TurnId: int64(i), CreatedAt: todayAt(10, i%60)})
		case 1:
			m.messages = append(m.messages, ChatMessage{Role: "thought", Content: block, TurnId: int64(i)})
		case 2:
			m.messages = append(m.messages, ChatMessage{Role: "tool", ToolName: "read src/async.go", ToolStatus: toolDone, ToolInput: `{"path":"src/async.go"}`, ToolOutput: block, TurnId: int64(i)})
		default:
			m.messages = append(m.messages, ChatMessage{Role: "assistant", Content: block, TurnId: int64(i), CreatedAt: todayAt(10, i%60)})
		}
		total += len(block)
	}
}

// latencyBudget fails the test when d exceeds limit; it always logs the
// measured value so runs document the actual headroom.
func latencyBudget(t *testing.T, what string, d time.Duration, limit time.Duration) {
	t.Helper()
	t.Logf("%-28s %8v (budget %v)", what, d.Round(time.Millisecond), limit)
	if d > limit {
		t.Errorf("%s took %v, budget %v", what, d, limit)
	}
}

// TestInteractiveLatencyAtContextCap drives the real Update/View path with
// the transcript stuffed to the in-memory cap and asserts every interactive
// path stays snappy: first paint, frame assembly, a streaming chunk plus
// its throttled flush, scrolling into uncached territory, and the worst
// streaming case — a long reply re-styling as it grows.
func TestInteractiveLatencyAtContextCap(t *testing.T) {
	m := newBenchModel()
	m.inChat = true
	fillToContextCap(m)

	start := time.Now()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	latencyBudget(t, "first paint (resize)", time.Since(start), 500*time.Millisecond)

	start = time.Now()
	if v := m.View(); v.Content == "" {
		t.Fatal("empty view")
	}
	latencyBudget(t, "frame assembly (View)", time.Since(start), 150*time.Millisecond)

	// A streaming chunk lands while pinned to the bottom, then the
	// throttled flush fires.
	m.needAutoScroll = true
	start = time.Now()
	m.Update(agentMessageMsg{text: mkMarkdown(4_000)})
	latencyBudget(t, "stream chunk (Update)", time.Since(start), 100*time.Millisecond)
	start = time.Now()
	m.Update(flushViewportMsg{})
	latencyBudget(t, "throttled flush", time.Since(start), 100*time.Millisecond)

	// PageUp into fresh (uncached) territory: the newly revealed rows style
	// on demand.
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	start = time.Now()
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	latencyBudget(t, "PageUp (uncached rows)", time.Since(start), 200*time.Millisecond)

	// Worst streaming case: one long reply re-styling as it grows —
	// ten 1KB chunks appended to an already-100KB assistant message; each
	// chunk restyles the whole growing block and refeeds the window.
	var long strings.Builder
	long.WriteString(mkMarkdown(100_000))
	m.messages = append(m.messages, ChatMessage{Role: "assistant", Content: long.String(), TurnId: 999999})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40}) // re-sync at the new tail
	worst := time.Duration(0)
	for i := 0; i < 10; i++ {
		s := time.Now()
		m.Update(agentMessageMsg{text: mkMarkdown(1_000)})
		if d := time.Since(s); d > worst {
			worst = d
		}
	}
	latencyBudget(t, "long-reply chunk (100KB)", worst, 200*time.Millisecond)
}

func BenchmarkStreamChunkAtCap(b *testing.B) {
	m := newBenchModel()
	m.inChat = true
	fillToContextCap(m)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.needAutoScroll = true
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Update(agentMessageMsg{text: " more streaming text arrives"})
	}
}
