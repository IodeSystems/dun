---
question: Why are token estimates high at the start of a session?
source: plan/plan.md item 9, S finding (2026-10-20)
---
On the first round the prompt meter measured 4910 chars → 4830 tokens =
1.02 chars/token, because the provider's count includes the tool schemas and the
char count does not. promptMeter only splits that fixed overhead from the
per-char slope after several rounds (Overhead() is 0 until then). Early token
numbers — shaping and surfacing stats — run ~4× high; byte counts are exact.
