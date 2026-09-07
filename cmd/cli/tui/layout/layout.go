package layout

const (
	MinWidth     = 60
	MinHeight    = 18
	BarWidth     = 1
	ScrollbarGap = 1
	// TranscriptTopPad is the blank page row above the first message block
	// in the transcript document, so the list never touches the viewport's
	// top edge.
	TranscriptTopPad = 1
	InputHeight  = 2
	Footerheight = 1
	Padding      = 1
	StatusHeight = 1
	SpaceHeight  = 1
)

// Chat
func GetRightWidth(width int) int {
	if width < 120 {
		return 0
	}
	return 40
}

func GetLeftWidth(width int) int {
	return max(1, width-GetRightWidth(width))
}

func GetContentWidth(width int) int {
	return max(1, GetLeftWidth(width)-2)
}

func GetPropmptWidth(width int) int {
	return max(1, GetLeftWidth(width)-1)
}

func GetViewWidth(width int) int {
	return max(1, GetContentWidth(width)-BarWidth)
}

// GetTranscriptWidth returns the message-list viewport width: the view width
// minus the one-column gap kept between the transcript and the scrollbar, so
// message blocks never touch the bar.
func GetTranscriptWidth(width int) int {
	return max(1, GetViewWidth(width)-ScrollbarGap)
}

func GetInputAreaHeight() int {
	return InputHeight + StatusHeight + Footerheight + Padding*2
}

func GetViewHeight(height int) int {
	return max(1, height-SpaceHeight-GetInputAreaHeight())
}

// Welcome
func GetWelcomeWidth(width int) int {
	if width > 80 {
		return 72
	}
	return int(float32(width) * 0.9)
}
