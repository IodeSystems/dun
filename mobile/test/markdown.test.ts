/// <reference types="jest" />
// The parser half: no DOM. RN's jest environment is node-based, with no document.

import { codeText, createReply, mathLines, parseBlock, safeUrl, tableColumns, type Node } from '../src/lib/markdown';

import { FORMS } from './fixtures/flowdown-forms';

const tree = (md: string): Node[] => {
  const { blocks, closer } = createReply().update(md, false);
  return blocks.flatMap((b, i) => parseBlock(b, i === blocks.length - 1 ? closer : '').children ?? []);
};

test('no DOM globals in this environment', () => {
  expect(typeof (globalThis as Record<string, unknown>).document).toBe('undefined');
});

test('each block kind parses', () => {
  const md = [
    '# Title',
    'Some *em*, **strong**, ~~gone~~ and `code` with [a link](https://x.dev).',
    '- one\n- two\n  1. nested',
    '* [ ] todo\n* [x] done',
    '> quoted',
    '```ts\nconst a = 1;\n```',
    '| a | b |\n|:--|--:|\n| 1 | 2 |',
    '---',
  ].join('\n\n');
  expect(tree(md).map((n) => n.type)).toEqual([
    'heading',
    'paragraph',
    'list',
    'list',
    'blockquote',
    'code',
    'table',
    'thematicBreak',
  ]);
  const [, para, list, tasks, , code, table] = tree(md);
  expect(para!.children!.map((n) => n.type)).toEqual([
    'text',
    'emphasis',
    'text',
    'strong',
    'text',
    'delete',
    'text',
    'inlineCode',
    'text',
    'link',
    'text',
  ]);
  expect(list!.children![1]!.children!.map((n) => n.type)).toEqual(['paragraph', 'list']);
  expect(tasks!.children!.map((n) => n.checked)).toEqual([false, true]);
  expect(code!.lang).toBe('ts');
  expect(codeText(code!)).toBe('const a = 1;');
  const cols = tableColumns(table!);
  expect(cols.map((c) => c.align)).toEqual(['left', 'right']);
  expect(cols[0]!.cells.map((c) => c.header)).toEqual([true, false]);
});

test('safeUrl lets web links through and refuses scripts', () => {
  expect(safeUrl('https://x.dev')).toBe('https://x.dev');
  expect(safeUrl('mailto:a@b.c')).toBe('mailto:a@b.c');
  expect(safeUrl('javascript:alert(1)')).toBeNull();
  expect(safeUrl('data:text/html,x')).toBeNull();
  expect(safeUrl(undefined)).toBeNull();
});

test('streaming token by token: open fence, list and emphasis read sanely', () => {
  const md = 'Intro **bold** text.\n\n- a\n- b\n\n```py\nprint(1)\nprint(2)\n```\n\nDone *now*.';
  const reply = createReply();
  for (let i = 1; i <= md.length; i++) {
    const { blocks, closer } = reply.update(md.slice(0, i), true);
    // Every prefix parses, and the last block parses with the closer it is owed.
    blocks.forEach((b, j) => parseBlock(b, j === blocks.length - 1 ? closer : ''));
  }
  // Mid fence: the code block has its lines so far, not the rest of the reply as prose.
  const at = md.indexOf('print(2)') + 3;
  const mid = reply.update(md.slice(0, at), true);
  expect(mid.growing).toBe(true);
  const last = parseBlock(mid.blocks.at(-1)!, mid.closer).children![0]!;
  expect(last.type).toBe('code');
  expect(codeText(last)).toBe('print(1)\npri');
  // Mid emphasis: `**bol` is completed to strong, not shown as stars.
  const open = reply.update('Intro **bol', true);
  expect(parseBlock(open.blocks[0]!, open.closer).children![0]!.children!.map((n) => n.type)).toEqual([
    'text',
    'strong',
  ]);
  // Mid list: two items so far.
  const list = reply.update('- a\n- b', true);
  expect(parseBlock(list.blocks[0]!).children![0]!.children).toHaveLength(2);
});

test('unchanged blocks keep identity across updates; their parse is reused', () => {
  const reply = createReply();
  const a = reply.update('# Head\n\nfirst para\n\nsec', true);
  const head = a.blocks[0]!;
  const tree0 = parseBlock(head);
  const b = reply.update('# Head\n\nfirst para\n\nsecond para', true);
  expect(b.blocks[0]).toBe(head);
  expect(b.blocks[1]).toBe(a.blocks[1]);
  expect(b.blocks[2]).not.toBe(a.blocks[2]);
  expect(parseBlock(b.blocks[0]!)).toBe(tree0);
});

test('the dialect fixture parses at every prefix while streaming', () => {
  const reply = createReply();
  for (let i = 1; i <= FORMS.length; i++) {
    const { blocks, closer } = reply.update(FORMS.slice(0, i), true);
    blocks.forEach((b, j) => parseBlock(b, j === blocks.length - 1 ? closer : ''));
  }
});

test('math: $…$ and $$ fences are formulas, a price is text', () => {
  const [p, display] = tree('Area $\\pi r^2$ costs $5 or $6.\n\n$$\nx^2\n$$');
  expect(p!.children!.map((n) => n.type)).toEqual(['text', 'inlineMath', 'text']);
  expect(display!.type).toBe('math');
  expect(mathLines('\\pi r^2')).toEqual(['π r²']);
});
