# M0 probes — does the right note surface unasked?

Draft, 2026-10-20. Notes are in `notes/`. For each probe, send the message as
the user's first message in a fresh session with `/rag` on and the notes
ingested, then read `/context` → surfacing.

Record per probe: which note(s) surfaced (and their score/mode), whether the
right one was among them, how many wrong ones surfaced, bytes/tokens appended,
pass latency. Run each probe in bm25 and in hybrid mode.

- **L** = lexical: shares words with the note. BM25 should find it.
- **P** = paraphrase: same need, different words. BM25 likely misses; this is
  what tells us whether vec/hybrid is needed for memories.
- **N** = negative: no note applies. Anything surfaced is noise.

| note | L probe | P probe |
|---|---|---|
| untracked-files-lost | "should I stash before cherry-picking these commits?" | "is it ok to leave the new files uncommitted while I try something?" |
| exec-every-command-is-a-job | "why did exec return a job number instead of output?" | "the command I ran came back as a background thing, why?" |
| gh-device-flow-hang | "run gh auth login so you can open the PR" | "just log in to github and create the pull request yourself" |
| viewport-height-clamp | "the conversation pane is not at the bottom after resizing" | "on my phone the chat stops scrolling to the end when the keyboard closes" |
| loop-detection-consecutive | "loopguard refused my tool call, is that a loop?" | "why won't it let me run the same command again?" |
| model-invents-ids | "should memories be addressed by id?" | "how should the model point at a note it wants to change?" |
| recap-numbers-wrong | "can we store the approved recap as memory?" | "the summary looked right to me, can we keep it for next time?" |
| bm25-minscore-zero | "what MinScore should the docs preparer use?" | "why are tiny relevance scores still shown?" |
| meter-first-round-ratio | "why is the token estimate so high on the first round?" | "the context numbers look inflated right after starting" |
| mcp-stderr-flake | "TestStart_FailingAutostartIsNotFatal failed again" | "a server startup test fails now and then with broken pipe" |
| hermes-fork-cache-parity | "will the memory fork hit the prompt cache?" | "how do we make the background review cheap?" |

Negative probes (expect nothing surfaced):
- "rename the variable cfg to config in main.go"
- "what does lipgloss do?"
- "add a test for plural()"

What M0 decides (from `plan/plan.md` item 9): if the right note does not arrive
unasked on the L probes, stop — the write side has no benefit. If L works and P
fails in bm25 but works in hybrid, memories need hybrid search. Noise on the N
probes, and the carried tokens, set the S caps.
