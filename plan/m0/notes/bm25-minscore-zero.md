---
question: Why does proactive RAG use MinScore 0?
source: servers_runtime.go:426
---
raglit's search is BM25, whose scores have no fixed range — tiny on a small
index (a correct hit scored 0.0000047 in the 2026-10-20 live check). But a
MATCH only returns matching rows, so any hit is a real lexical hit. MaxHits (2)
caps what is surfaced, not a score threshold.
