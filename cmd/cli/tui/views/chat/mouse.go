package chat

import (
	tea "charm.land/bubbletea/v2"

	"github.com/yusheng-g/openagent-go/cmd/cli/tui/layout"
)

// ── mouse interaction ──
//
// CellMotion tracking (see createView) delivers clicks, wheel, drags and
// motion to the app. This file owns the routing: clicks reach the scrollbar
// drag and the transcript selection; motion extends whichever drag is
// active; release ends it.

// scrollbarX returns the terminal column of the scrollbar track. The left
// area paints with 1-column page padding, so inside it the viewport spans
// columns 1..leftW-4, one gap column follows, and the bar sits at leftW-2
// (renderLeft joins viewport + gap + scrollbar in that order).
func (m *Model) scrollbarX() int {
	return layout.GetLeftWidth(m.width) - 2
}

// scrollbarMetrics mirrors renderScrollbar's thumb math so hit-testing and
// drag mapping stay consistent with what is drawn. ok=false when the
// document fits the viewport (no thumb, nothing to drag).
func (m *Model) scrollbarMetrics() (thumbH, maxOffset int, ok bool) {
	total := m.chatViewport.TotalLineCount()
	h := m.chatViewport.Height()
	if h <= 0 || total <= h {
		return 0, 0, false
	}
	return max(1, h*h/total), total - h, true
}

// handleMouseClick routes a left press. The scrollbar claim comes first:
// the bar column sits right beside the transcript, so without the check a
// press there would fall through to selection. Other presses are inert for
// now — panel surfaces have no click targets of their own.
func (m *Model) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	if msg.Button == tea.MouseLeft && m.inChat && !m.panelOpen && m.permissionReq == nil {
		if m.startScrollbarDrag(msg.X, msg.Y) {
			return m, nil
		}
		if msg.Y < m.chatViewport.Height() {
			m.startSelection(msg.X, msg.Y)
		}
	}
	return m, nil
}

// startScrollbarDrag begins a scrollbar drag from a left press at the bar
// column. A press on the track first jumps the thumb so it centers under
// the cursor (standard scrollbar behavior), then drags from there; a press
// on the thumb grabs it in place. Returns false when the press misses the
// bar or there is nothing to scroll.
func (m *Model) startScrollbarDrag(x, y int) bool {
	if x != m.scrollbarX() || y < 0 || y >= m.chatViewport.Height() {
		return false
	}
	thumbH, maxOffset, ok := m.scrollbarMetrics()
	if !ok {
		return false
	}
	span := m.chatViewport.Height() - thumbH
	thumbY := m.chatViewport.YOffset() * span / maxOffset
	if y >= thumbY && y < thumbY+thumbH {
		m.sbarGrab = y - thumbY
	} else {
		// Track press: jump so the cursor sits mid-thumb, then drag.
		want := min(max(y-thumbH/2, 0), span)
		m.chatViewport.SetYOffset(want * maxOffset / span)
		m.sbarGrab = y - want
		m.needAutoScroll = m.isNearBottom()
	}
	m.sbarDrag = true
	return true
}

// handleMouseMotion extends the active drag. CellMotion reports motion only
// while a button is held, and the held button arrives in the message, so a
// left motion outside the bar column still scrolls — scrollbars keep
// dragging when the cursor strays sideways.
func (m *Model) handleMouseMotion(msg tea.MouseMotionMsg) (tea.Model, tea.Cmd) {
	if m.sbarDrag && msg.Button == tea.MouseLeft {
		m.dragScrollbarTo(msg.Y)
		return m, nil
	}
	if m.selection.active && msg.Button == tea.MouseLeft {
		m.extendSelection(msg.X, msg.Y)
	}
	return m, nil
}

// dragScrollbarTo maps the cursor row back to a scroll offset — the exact
// inverse of the renderScrollbar thumb mapping — and applies it via
// SetYOffset; feedViewport rebuilds the styled window around the new offset
// at the end of the Update. Dragging is user scroll intent: auto-scroll
// stays off unless the drag parks the window back at the bottom (same rule
// as wheel-down).
func (m *Model) dragScrollbarTo(y int) {
	thumbH, maxOffset, ok := m.scrollbarMetrics()
	if !ok {
		return
	}
	span := m.chatViewport.Height() - thumbH
	thumbY := min(max(y-m.sbarGrab, 0), span)
	off := thumbY * maxOffset / span
	if off != m.chatViewport.YOffset() {
		m.chatViewport.SetYOffset(off)
		m.needAutoScroll = m.isNearBottom()
	}
}

// handleMouseRelease ends the active drag. A released box selection copies
// its text via OSC 52.
func (m *Model) handleMouseRelease(msg tea.MouseReleaseMsg) (tea.Model, tea.Cmd) {
	if msg.Button != tea.MouseLeft {
		return m, nil
	}
	if m.sbarDrag {
		m.sbarDrag = false
	}
	if m.selection.active {
		return m.finishSelection()
	}
	return m, nil
}
