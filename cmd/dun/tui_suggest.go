package main

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// suggestion is one predicted next user message (--suggest).
type suggestion struct {
	text string
	prob float64
}

func parseSuggestionItems(v any) []suggestion {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]suggestion, 0, len(arr))
	for _, it := range arr {
		m, _ := it.(map[string]any)
		if t := strings.TrimSpace(str(m["text"])); t != "" {
			out = append(out, suggestion{text: t, prob: floatOf(m["prob"])})
		}
	}
	return out
}

// idleSuggestDelay is how long the input must sit still, and empty, before the
// UI spends a model call guessing what you were going to type.
//
// It exists because the engine used to volunteer suggestions at the end of
// EVERY turn — including the autonomous ones a heartbeat provoked — so a
// session nobody was typing into still billed a request per turn, and one
// landed between a tool result and the model's next move. Three seconds is long
// enough that finishing a thought does not trigger it and short enough that the
// picker is there when you look up.
const idleSuggestDelay = 3 * time.Second

// idleSuggestMsg fires when the debounce may have elapsed.
type idleSuggestMsg struct{}

func idleSuggestTick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return idleSuggestMsg{} })
}

// armIdleSuggest restarts the idle clock. Called from the keystroke funnel and
// at the end of a turn — the two things that mean "the human's turn now".
//
// It arms nothing when a suggestion could not fire anyway (suggestions off, or
// an empty conversation with nothing to predict from). Otherwise every
// keystroke of a session that will never ask schedules a timer, and a keypress
// that the UI deliberately ignores stops looking ignored to its caller.
func (m *tuiModel) armIdleSuggest() {
	if !m.idleSuggestPossible() {
		return
	}
	m.lastKeyAt = time.Now()
	m.suggestedThisIdle = false
	m.idleWantTick = true
}

// idleSuggestPossible is the cheap precondition: could this session ever ask?
// Not before a turn has finished in this TUI: opening or resuming a session
// and pressing a key is not "the human's turn now", and must not spend a
// model call (USER 2026-10-24).
func (m tuiModel) idleSuggestPossible() bool {
	return m.suggestMode != "off" && !m.quitting && m.liveTurnDone && m.asked()
}

// asked reports whether the conversation on screen holds a user message.
// Before one there is nothing to predict a next message from: a new session
// (or one just cleared) asks for no suggestions and shows none.
func (m tuiModel) asked() bool {
	for _, e := range m.convo {
		if e.userText != "" {
			return true
		}
	}
	return false
}

// idleSuggestReady is every condition that must hold to spend the call.
//
// The `len(m.suggestions) == 0` clause is what makes "once per idle" hold even
// though a keystroke restarts the clock: with a picker already on screen the
// answer is already there, so fiddling with the input cannot re-ask. Typing
// something and clearing it back to empty starts a genuinely new idle, and only
// then — with the old suggestions cleared by the turn that consumed them — can
// another request go out.
func (m tuiModel) idleSuggestReady() bool {
	if m.suggestMode == "off" || m.suggestedThisIdle || len(m.suggestions) > 0 {
		return false
	}
	// Never mid-turn, never over a modal: a suggestion is a question for a
	// person who is free to answer it.
	if m.busy || m.starting || m.asking || m.inspecting || m.searching || m.quitting {
		return false
	}
	if !m.asked() || !m.liveTurnDone { // nothing to predict from, or nothing new
		return false
	}
	return strings.TrimSpace(m.input.Value()) == "" &&
		time.Since(m.lastKeyAt) >= idleSuggestDelay
}

// suggestActive: the next-message picker shows only when idle with an empty
// input, so it never fights typing or a running turn. In "auto" mode it also
// requires that the input is empty (the default). In "on" mode it shows even
// when the user is typing (so suggestions are always visible).
func (m tuiModel) suggestActive() bool {
	// The root's predictions are not a child's: inside a child's view
	// there are none to pick.
	if m.suggestMode == "off" || !m.asked() || m.scopeAgent != 0 {
		return false
	}
	return m.focus == focusInput && !m.busy && !m.starting && !m.asking &&
		len(m.suggestions) > 0 &&
		(m.suggestMode == "on" || strings.TrimSpace(m.input.Value()) == "")
}
