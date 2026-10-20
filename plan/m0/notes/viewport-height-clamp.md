---
question: Why must the conversation pane's height go through convoPane.setHeight?
source: plan/plan.md item 8 (2026-08-26)
---
Closing a phone's soft keyboard GROWS the pane, which SHRINKS maxYOffset
(len(lines) - Height). Assigning vp.Height directly does not clamp (SetYOffset
does), so the offset was left past the end — measured yoff=44 max=32. AtBottom
is `YOffset >= maxYOffset`, true from outside the content, so nothing pulled it
back. Only mid-stream, because applyScroll's GotoBottom rescues the idle case.
