// Markdown for assistant replies: flowdown's parser (no DOM), our own RN renderer
// (src/components/markdown/). This file is the pure part: cut a reply into blocks
// that keep their identity while it streams, and parse each block once.
//
// The parse reads flowdown's dialect (the forms dun's TUI draws through bubbleteandmd:
// callouts, steps, stats, sources, tabs, pros/cons, details, titled code, tree fences)
// and formulas. A form is a mark on an ordinary node (formOf); the renderer draws the
// marks it knows and the plain Markdown for the rest.
import {
  createDocument,
  flowdownDialect,
  formOf,
  type DocumentState,
  type MarkdownBlock,
} from '@localix-ai/flowdown/document';
import {
  defaultUrlTransform,
  parse,
  toText,
  type Extension,
  type InlineState,
  type Node,
} from '@localix-ai/flowdown/engine';
import { parseFileTree, type TreeRow } from '@localix-ai/flowdown/remark';
import { lineText, renderToLines } from '@localix-ai/flowdown/tui';

export type { MarkdownBlock, Node, TreeRow };
export { formOf, toText };

/** One reply drawn more than once: update it with the text so far. */
export type Reply = { update(text: string, streaming: boolean): DocumentState };

export function createReply(): Reply {
  // refs: a [text][label] finds a definition written in another block.
  const doc = createDocument({ refs: true });
  // math: a literal dollar ($5) is escaped, so it is not read as a formula.
  return { update: (text, streaming) => doc.update(text, { streaming, math: true }) };
}

const DOLLAR = 36;
const isSpace = (ch: string | undefined) => ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r';

/**
 * `$x$` as an inlineMath node, by pandoc's rule as flowdown reads it (its core/math-syntax,
 * which no entry point exports): a run of dollars opens, the next run of the same length
 * closes; one `$` opens before a non-space and closes after one, not before a digit.
 */
function inlineMath(state: InlineState): boolean {
  const { text, pos } = state;
  let open = pos;
  while (text.charCodeAt(open) === DOLLAR) open++;
  const size = open - pos;
  if (size === 1 && (text[open] === undefined || isSpace(text[open]))) return false;
  for (let at = text.indexOf('$', open); at !== -1; at = text.indexOf('$', at)) {
    let end = at;
    while (text.charCodeAt(end) === DOLLAR) end++;
    if (end - at === size && (size > 1 || closesInline(text, at))) {
      let value = text.slice(open, at).replace(/\n/g, ' ');
      if (value.length > 2 && value.startsWith(' ') && value.endsWith(' ') && value.trim() !== '') {
        value = value.slice(1, -1);
      }
      state.push({ type: 'inlineMath', value });
      state.pos = end;
      return true;
    }
    at = end;
  }
  return false;
}

function closesInline(text: string, at: number): boolean {
  const before = text[at - 1];
  const after = text[at + 1];
  if (before === undefined || isSpace(before) || before === '$') return false;
  if (after !== undefined && after >= '0' && after <= '9') return false;
  let slashes = 0;
  while (text[at - 1 - slashes] === '\\') slashes++;
  return slashes % 2 === 0;
}

/** Formulas left as their LaTeX: `$…$`, `$$` fences and ```math fences (as `math` nodes). */
const math: Extension = {
  name: 'math',
  mathFences: true,
  inline: [{ chars: '$', parse: inlineMath }],
  transform(root) {
    const walk = (n: Node) => {
      if (n.type === 'code' && n.lang === 'math') n.type = 'math';
      n.children?.forEach(walk);
    };
    walk(root);
  },
};

const EXTENSIONS = [flowdownDialect, math];

const read = (block: MarkdownBlock, source: string) => parse(source, { extensions: EXTENSIONS, refs: block.refs });

const parsed = new WeakMap<MarkdownBlock, Node>();

/**
 * The tree of one block. `closer` is the fence line a streaming reply still owes its last
 * block ('' otherwise); a block without one is parsed once and kept by identity.
 */
export function parseBlock(block: MarkdownBlock, closer = ''): Node {
  if (closer !== '') return read(block, block.source + closer);
  let node = parsed.get(block);
  if (node === undefined) {
    node = read(block, block.source);
    parsed.set(block, node);
  }
  return node;
}

/** The URL a link may open, or null: relative, http(s), mailto, irc(s), xmpp pass; javascript:, data: and the rest do not. */
export function safeUrl(url: string | undefined): string | null {
  if (!url) return null;
  return defaultUrlTransform(url) || null;
}

/** A code block's text without the line ending the fence leaves after it. */
export function codeText(node: Node): string {
  return (node.value ?? '').replace(/\n$/, '');
}

/** A table as columns of cells: each column is drawn as one View, so its cells share a width. */
export function tableColumns(table: Node): { align: Node['cellAlign']; cells: Node[] }[] {
  const rows = table.children ?? [];
  const width = Math.max(0, ...rows.map((r) => r.children?.length ?? 0));
  const cols: { align: Node['cellAlign']; cells: Node[] }[] = [];
  for (let i = 0; i < width; i++) {
    cols.push({
      align: table.align?.[i] ?? null,
      cells: rows.map((r) => r.children?.[i] ?? { type: 'tableCell', children: [] }),
    });
  }
  return cols;
}

const formulas = new Map<string, string[]>();

/**
 * A formula's LaTeX as Unicode lines (`\frac{1}{k} \sum_{i=1}^{k}` is `1/k ∑ᵢ₌₁ᵏ`), the way
 * flowdown's terminal target writes it; a command it does not know stays as written. Not
 * typeset: KaTeX is a quarter of a megabyte and needs HTML.
 */
export function mathLines(latex: string): string[] {
  let lines = formulas.get(latex);
  if (lines === undefined) {
    lines = renderToLines(`$$\n${latex}\n$$`, { width: 10_000 }).map((l) => lineText(l).replace(/^ {2}/, ''));
    if (lines.length === 0) lines = [latex];
    if (formulas.size > 500) formulas.clear();
    formulas.set(latex, lines);
  }
  return lines;
}

/** One ```tree row with the branch guides `tree` draws before its name. */
export type TreeLine = TreeRow & { guide: string };

/** A ```tree fence as rows with guides; [] when the text is not a tree. */
export function treeLines(source: string): TreeLine[] {
  const rows = parseFileTree(source);
  // last[i]: no later row at rows[i].depth before a shallower one (flowdown tui/render.js tree).
  const last: boolean[] = [];
  const after: boolean[] = [];
  for (let at = rows.length - 1; at >= 0; at--) {
    const { depth } = rows[at]!;
    last[at] = !after[depth];
    after.length = depth;
    after[depth] = true;
  }
  const open: boolean[] = [];
  return rows.map((row, at) => {
    open.length = row.depth;
    open[row.depth] = !last[at];
    let guide = '';
    for (let d = 1; d < row.depth; d++) guide += open[d] ? '│  ' : '   ';
    if (row.depth > 0) guide += last[at] ? '└─ ' : '├─ ';
    return { ...row, guide };
  });
}
