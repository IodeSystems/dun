package main

import (
	"unicode"

	tea "charm.land/bubbletea/v2"
)

// Key and mouse messages as Bubble Tea v2 decodes them, for tests.

// runesKey is typed text: one grapheme per key, as the decoder sends it (an
// uppercase letter carries shift, as it does from a real terminal).
func runesKey(r []rune) tea.KeyPressMsg {
	k := tea.KeyPressMsg{Text: string(r), Code: tea.KeyExtended}
	if len(r) == 1 {
		k.Code = r[0]
		if unicode.IsUpper(r[0]) {
			k.Code, k.ShiftedCode, k.Mod = unicode.ToLower(r[0]), r[0], tea.ModShift
		}
	}
	return k
}

// mouseAction picks which v2 mouse message mouseMsg builds.
type mouseAction int

const (
	mousePress mouseAction = iota
	mouseRelease
	mouseMotion
)

// mouseMsg builds the message a terminal sends for action a with button b at
// x,y. A wheel "press" is a MouseWheelMsg, as it is from a real terminal.
func mouseMsg(a mouseAction, b tea.MouseButton, x, y int) tea.MouseMsg {
	mo := tea.Mouse{X: x, Y: y, Button: b}
	switch {
	case b == tea.MouseWheelUp || b == tea.MouseWheelDown || b == tea.MouseWheelLeft || b == tea.MouseWheelRight:
		return tea.MouseWheelMsg(mo)
	case a == mouseRelease:
		return tea.MouseReleaseMsg(mo)
	case a == mouseMotion:
		return tea.MouseMotionMsg(mo)
	}
	return tea.MouseClickMsg(mo)
}
