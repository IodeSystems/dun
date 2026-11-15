/// <reference types="jest" />
// Syntax highlighting: tokenizer → spans, the language rules (the TUI's chroma
// lookup), and the RN tree it draws.
import { act } from 'react';
import { createRoot, type TestInstance } from 'test-renderer';

import { Code, splitNumbered } from '../src/components/code/Code';
import { highlight, plainLines } from '../src/components/code/highlight';
import { langForPath, langOf } from '../src/components/code/languages';
import { Markdown } from '../src/components/markdown/Markdown';
import { bodyFor } from '../src/components/tools/registry';
import type { ToolCall } from '../src/lib/toolcalls';

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const text = (n: TestInstance | string): string => (typeof n === 'string' ? n : n.children.map(text).join(''));
/** Every nested Text with a colour, as [text, colour]. */
const coloured = (root: TestInstance) =>
  root
    .queryAll((n) => n.type === 'Text' && n.parent?.type === 'Text')
    .map((n) => [text(n), (n.props.style as { color?: string } | undefined)?.color] as const);

function render(el: React.ReactElement) {
  const root = createRoot();
  act(() => root.render(el));
  return root.container;
}

test('tokens fold to roles, split into lines, and keep every character', () => {
  const src = 'func main() {\n\t// hi\n\treturn "x" + 1\n}';
  const lines = highlight(src, 'go');
  expect(lines).toHaveLength(4);
  expect(lines.map((l) => l.map((s) => s.text).join('')).join('\n')).toBe(src);
  expect(lines[0]).toContainEqual({ text: 'func', role: 'keyword' });
  expect(lines[0]).toContainEqual({ text: 'main', role: 'function' });
  expect(lines[1]).toContainEqual({ text: '// hi', role: 'comment' });
  expect(lines[2]).toContainEqual({ text: '"x"', role: 'string' });
  expect(lines[2]).toContainEqual({ text: '1', role: 'number' });
});

test('a multi-line token is cut at each newline, its role on both halves', () => {
  const lines = highlight('/* a\nb */ x', 'javascript');
  expect(lines[0]).toEqual([{ text: '/* a', role: 'comment' }]);
  expect(lines[1]![0]).toEqual({ text: 'b */', role: 'comment' });
});

test('no grammar is plain lines', () => {
  expect(highlight('a\n\nb', null)).toEqual([[{ text: 'a' }], [], [{ text: 'b' }]]);
  expect(plainLines('x')).toEqual([[{ text: 'x' }]]);
});

test('fence languages: name, alias, extension, file name, any case; unknown is plain', () => {
  expect(langOf('go')).toBe('go');
  expect(langOf('TypeScript')).toBe('typescript');
  expect(langOf('sh')).toBe('bash');
  expect(langOf('rs')).toBe('rust');
  expect(langOf('yml title="x"')).toBe('yaml');
  expect(langOf('Dockerfile')).toBe('docker');
  expect(langOf('main.py')).toBe('python');
  expect(langOf('brainfuck')).toBeNull();
  expect(langOf('')).toBeNull();
  expect(langOf(undefined)).toBeNull();
});

test('file paths: the extension, else the base name', () => {
  expect(langForPath('internal/tui/render.go')).toBe('go');
  expect(langForPath('src/App.TSX')).toBe('tsx');
  expect(langForPath('C:\\x\\y.json')).toBe('json');
  expect(langForPath('deploy/Dockerfile')).toBe('docker');
  expect(langForPath('Dockerfile.dev')).toBe('docker');
  expect(langForPath('Makefile')).toBe('makefile');
  expect(langForPath('BUILD.bazel')).toBe('python');
  expect(langForPath('notes.txt')).toBeNull();
  expect(langForPath('LICENSE')).toBeNull();
  expect(langForPath('')).toBeNull();
});

test('Code draws spans in the theme colours; an unknown language has none', () => {
  const hl = render(<Code code={'const x = 1;'} lang="ts" />);
  expect(coloured(hl)).toContainEqual(['const', '#036fc4']);
  expect(text(hl)).toBe('const x = 1;');
  const plain = render(<Code code={'const x = 1;'} lang="nope" />);
  expect(coloured(plain)).toEqual([]);
  expect(text(plain)).toBe('const x = 1;');
  // Selectable, in a sideways scroll.
  expect(hl.queryAll((n) => n.type === 'Text' && n.props.selectable === true)).toHaveLength(1);
  expect(hl.queryAll((n) => n.type === 'RCTScrollView' && n.props.horizontal === true)).toHaveLength(1);
});

test('a streaming fence tokenizes only its settled lines', () => {
  const root = render(<Markdown text={'```go\nreturn 1\nreturn'} streaming />);
  const spans = coloured(root);
  expect(spans.filter(([t]) => t === 'return')).toHaveLength(1);
  expect(text(root)).toContain('return 1\nreturn');
});

test('file_read: numbers muted, code highlighted as one text', () => {
  expect(splitNumbered(['a.go: 2 lines', '1\tpackage x', '2\t'])).toEqual({
    head: ['a.go: 2 lines'],
    nums: ['1', '2'],
    code: ['package x', ''],
    tail: [],
  });
  expect(splitNumbered(['no numbers'])).toBeNull();
  const call: ToolCall = {
    id: 'r',
    tool: 'file_read',
    args: { path: 'a.go' },
    result: 'a.go: 2 lines\n1\tpackage x\n2\tvar s = "q"',
    state: 'done',
  };
  const Body = bodyFor(call);
  const root = render(<Body call={call} />);
  const spans = coloured(root);
  expect(spans).toContainEqual(['package', '#036fc4']);
  expect(spans).toContainEqual(['"q"', '#7e5b38']);
  expect(spans).toContainEqual(['1', '#6b6b66']);
});

test('file_edit: code inside +/- lines highlighted, the marker and tint kept', () => {
  const call: ToolCall = {
    id: 'e',
    tool: 'file_edit',
    args: { path: 'x.py' },
    result: 'edited x.py: +1 -1\n@@ -1,1 +1,1 @@\n-def a(): pass\n+def b(): pass',
    state: 'done',
  };
  const Body = bodyFor(call);
  const root = render(<Body call={call} />);
  const spans = coloured(root);
  expect(spans).toContainEqual(['-', '#b3261e']);
  expect(spans).toContainEqual(['+', '#2f6fdb']);
  expect(spans.filter(([t, col]) => t === 'def' && col === '#036fc4')).toHaveLength(2);
  const tinted = root.queryAll(
    (n) =>
      n.type === 'Text' && (n.props.style as { backgroundColor?: string } | undefined)?.backgroundColor === '#e3f4ea',
  );
  expect(tinted.map(text)).toEqual(['+def b(): pass']);
});
