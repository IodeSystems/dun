---
question: Why does loopguard only flag CONSECUTIVE identical tool calls?
source: plan/plan.md item 4 (2026-08-24)
---
Measured on a stuck session: near-identical arguments scored 0.01–0.29
similarity (each attempt was a genuinely different edit) — rejected. Recurring
result lines: 3/8 healthy vs 4/8 stuck, no threshold fits — rejected. Identical
arguments back-to-back found 12 byte-identical `recap` calls nobody noticed,
with zero false positives above 2 (ship 8× and git log 3× were all legitimate,
separated by 3–48 calls). It refuses rather than warns; polling tools are exempt.
