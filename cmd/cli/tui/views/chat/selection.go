package chat

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/yusheng-g/openagent-go/cmd/cli/tui/theme"
	"github.com/yusheng-g/openagent-go/cmd/cli/tui/utils"
)

// ── transcript box selection ──
//
// CellMotion tracking hands button drags to the app, so the transcript
// draws its own selection: a left press anchors, held motion extends, and
// release copies via OSC 52 (tea.SetClipboard). Coordinates are document
// cells — the viewport's YOffset row plus a cell column inside the
// transcript width — so the highlight stays glued to the text while the
// window scrolls.

// selCell is one endpoint of the selection in document coordinates.
type selCell struct {
	row int // document row (the viewport's coordinate space, top pad included)
	col int // cell column inside the transcript viewport (0-based)
}

// selectionFields are the box-selection state embedded in Model.
type selectionFields struct {
	// active is true between the anchoring press and its release; the box
	// exists only inside that window (release drops it — a copy is
	// confirmed by the toast, a blank box has nothing worth keeping).
	active bool
	// anchor is where the press landed; focus trails the cursor.
	anchor, focus selCell
}

// clearSelection drops the highlight (next press, changed content).
func (m *Model) clearSelection() {
	m.selection = selectionFields{}
}

// selectionSet reports whether a highlight is on screen.
func (m *Model) selectionSet() bool {
	return m.selection.anchor != m.selection.focus
}

// selRange returns the endpoints ordered.
func (m *Model) selRange() (r0, c0, r1, c1 int) {
	a, f := m.selection.anchor, m.selection.focus
	if a.row < f.row || (a.row == f.row && a.col <= f.col) {
		return a.row, a.col, f.row, f.col
	}
	return f.row, f.col, a.row, a.col
}

// selCellAt maps a terminal cell to a document cell for the selection.
// X clamps into the transcript width so dragging into the gap or past the
// bar selects to end-of-line; Y clamps into the visible window rows.
func (m *Model) selCellAt(x, y int) selCell {
	vpW := m.chatViewport.Width()
	col := min(max(x-1, 0), vpW-1) // viewport starts at screen column 1
	h := m.chatViewport.Height()
	row := m.chatViewport.YOffset() + min(max(y, 0), h-1)
	return selCell{row: row, col: col}
}

// startSelection anchors a box selection on a transcript press.
func (m *Model) startSelection(x, y int) {
	m.clearSelection()
	c := m.selCellAt(x, y)
	m.selection.anchor = c
	m.selection.focus = c
	m.selection.active = true
}

// extendSelection moves the focus endpoint to the cursor.
func (m *Model) extendSelection(x, y int) {
	m.selection.focus = m.selCellAt(x, y)
}

// finishSelection ends the drag: the box always drops on release. A
// non-empty box is copied via OSC 52 (tea.SetClipboard) and confirmed by
// the toast, which doubles as the "what happened" feedback in place of the
// dropped highlight; a zero-size (plain click) or blank box copies nothing
// and simply clears.
func (m *Model) finishSelection() (tea.Model, tea.Cmd) {
	has := m.selectionSet()
	text := ""
	if has {
		text = m.selectedText()
	}
	m.clearSelection()
	if !has || text == "" {
		return m, nil
	}
	return m, tea.Batch(tea.SetClipboard(text), m.notify("Copied to clipboard"))
}

// selectedText extracts the boxed cells from the visible window and joins
// them per line. The transcript is a windowed document: rows outside the
// fed window contribute nothing, so a drag that outran the viewport copies
// what was actually boxed on screen. Card chrome falls inside the box like
// any other character; blank edge lines are dropped so boxing a paragraph
// yields the paragraph, not its padding.
func (m *Model) selectedText() string {
	r0, c0, r1, c1 := m.selRange()
	yOff := m.chatViewport.YOffset()
	lines := strings.Split(m.chatViewport.View(), "\n")
	var out []string
	for li, line := range lines {
		dr := yOff + li
		if dr < r0 || dr > r1 {
			continue
		}
		from, to := 0, m.chatViewport.Width()
		if dr == r0 {
			from = c0
		}
		if dr == r1 {
			to = c1 + 1
		}
		if from >= to {
			continue
		}
		// Trailing spaces are card padding, not content — drop them so a
		// full-width middle row doesn't copy a run of blanks.
		out = append(out, strings.TrimRight(utils.PlainCells(line, from, to), " "))
	}
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// applySelectionOverlay repaints the selected cells of the visible window
// with the selection background. It runs on the fed viewport string, whose
// rows are exactly transcript-width, so the overlay can address cells
// directly; only visible rows exist here by construction (the virtual
// window keeps the rest as placeholders, and their coordinates simply
// never intersect the selection while off screen).
func (m *Model) applySelectionOverlay(vp string) string {
	if !m.selectionSet() {
		return vp
	}
	r0, c0, r1, c1 := m.selRange()
	yOff := m.chatViewport.YOffset()
	lines := strings.Split(vp, "\n")
	for li := range lines {
		dr := yOff + li
		if dr < r0 || dr > r1 {
			continue
		}
		from, to := 0, m.chatViewport.Width()
		if dr == r0 {
			from = c0
		}
		if dr == r1 {
			to = c1 + 1
		}
		if from < to {
			lines[li] = utils.OverlayBackground(lines[li], from, to, theme.ColorBgCode(theme.SelectionBg))
		}
	}
	return strings.Join(lines, "\n")
}

// viewportView is the transcript viewport's rendered string with the
// selection highlight applied. All three renderLeft scroll-container paths
// go through this so the highlight covers the permission/split layouts too.
func (m *Model) viewportView() string {
	return m.applySelectionOverlay(m.chatViewport.View())
}
