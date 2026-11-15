// Markdown for assistant replies: flowdown's parser (no DOM), our own RN renderer
// (src/components/markdown/). This file is the pure part: cut a reply into blocks
// that keep their identity while it streams, and parse each block once.
import { createDocument, type DocumentState, type MarkdownBlock } from '@localix-ai/flowdown/document';
import { defaultUrlTransform, parse, toText, type Node } from '@localix-ai/flowdown/engine';

export type { MarkdownBlock, Node };
export { toText };

/** One reply drawn more than once: update it with the text so far. */
export type Reply = { update(text: string, streaming: boolean): DocumentState };

export function createReply(): Reply {
  const doc = createDocument();
  return { update: (text, streaming) => doc.update(text, { streaming }) };
}

const parsed = new WeakMap<MarkdownBlock, Node>();

/**
 * The tree of one block. `closer` is the fence line a streaming reply still owes its last
 * block ('' otherwise); a block without one is parsed once and kept by identity.
 */
export function parseBlock(block: MarkdownBlock, closer = ''): Node {
  if (closer !== '') return parse(block.source + closer);
  let node = parsed.get(block);
  if (node === undefined) {
    node = parse(block.source);
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
