// One of each form flowdown 0.6.0 parses (GFM, its dialect, formulas): the render and
// parse tests draw it whole and at every prefix.
export const FORMS = `# 1.2 Numbered heading

Prose with *em*, **strong**, ~~gone~~, \`code\`, a [link](https://example.com), a bare https://go.dev link, a [ref link][ref], inline math $x^2 + \\alpha$ and a price of $5.

[ref]: https://example.org/ref

Footnote reference[^1].

[^1]: The footnote text.

![A figure](https://example.com/fig.png)
*The figure caption*

- [x] done task
- [ ] open task

| a | b |
|:--|--:|
| 1 | 2 |

> A plain quote.
> — Somebody Famous

> [!NOTE]
> A note with **bold**.

> [!TIP] A titled tip
> Tip body.

> [!IMPORTANT]
> Important body.

> [!WARNING]
> Warning body.

> [!CAUTION] Careful
> Caution body.

> [!STEPS] Recovery
1. **First 24 hours**
   *Day 1*
   Rest and keep warm.
2. **Next days**
   Light walks only.

> [!STATS] Numbers
- **3×** faster installs
- **−40%** memory

> [!SOURCES] Further reading
- [Page title](https://example.com/page) — the main reference
- [Other page](https://www.example.org/other)

> [!TABS] Install
#### macOS
\`\`\`sh
brew install thing
\`\`\`
#### Linux
Use the package manager.

---

> [!PROS] Good
- typed APIs

> [!CONS] Bad
- a build step

> [!DETAILS] Full log
> The long part, quoted.

\`\`\`ts title="src/app.ts"
const a = 1;
\`\`\`

\`\`\`tree
src/
├── app.ts          # entry point
└── features/
    └── chat.ts
\`\`\`

\`\`\`mermaid
flowchart LR
  A --> B
\`\`\`

$$
\\frac{1}{k} \\sum_{i=1}^{k} r_i
$$

\`\`\`math
E = mc^2
\`\`\`

<b>raw html</b>
`;
