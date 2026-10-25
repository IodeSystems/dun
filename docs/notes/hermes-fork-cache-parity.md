---
question: How does a background memory fork get prompt-cache hits?
source: plan/plan.md item 9; hermes-agent @ 0b10b9f8 agent/background_review.py:920-1017
---
Hermes sends the fork the parent's system prompt verbatim and a BYTE-IDENTICAL
tools[] (all the parent's tools), limiting what the fork may do with a
dispatch-side whitelist, and appends the review prompt as one user message. The
first request is then a warm cache read. Dropping tools (as dun's suggest.go
does with tools=nil) changes the rendered prompt and likely misses the cache.
