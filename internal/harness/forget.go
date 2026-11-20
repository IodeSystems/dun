package harness

import "strings"

// forget.go — hints given once, forgotten when their text leaves the context.
//
// Some model-facing hints are said once (or every Nth time) per session, and
// a flag on the Harness remembers that they were said. A fold or a recap can
// summarize the hint's text away; the flag then keeps the model from ever
// seeing it again. forgetHints clears those flags. A reset means "allowed
// again", not "say it now": each hint fires again only when its own trigger
// recurs.
//
// Two call sites, one per way the history shrinks:
//   - noteCompactionCause (compaction.go): every fold — the Shaper's, the
//     rescue fold, the boundary fold;
//   - applyRecapText (recap.go): every recap.
//
// Session flags that are NOT here say why at their declarations: setupSaid,
// indexAsked (said to the person, once per session), rulesDocs (a classifier,
// not a said-flag), learnState.calls (a cost cap).

// forgetters is the registry: one reset per hint whose text a fold can drop.
var forgetters = []func(*Harness){
	// recap.go: a recap cue can be raised again.
	(*Harness).forgetRecapCues,
}

// forgetHints runs every reset in forgetters.
func (h *Harness) forgetHints() {
	for _, f := range forgetters {
		f(h)
	}
}

// forgetRecapCues clears the recap cues already raised, except the
// large-result ones. A big:<id> key names one result: a fold or recap that
// drops the result means the cue cannot be raised again, and one that keeps
// the result keeps the cue's aside too (the aside came after the result, and
// a fold drops a prefix, a recap the span after an anchor). Clearing it would
// only repeat a cue the model can still see.
func (h *Harness) forgetRecapCues() {
	h.recapMu.Lock()
	defer h.recapMu.Unlock()
	for k := range h.recapSeen {
		if !strings.HasPrefix(k, bigCueKey("")) {
			delete(h.recapSeen, k)
		}
	}
}
