/// <reference types="jest" />
// Render tests for the tool renderers, on test-renderer (the renderer jest-expo uses).
import { act, type ReactElement } from 'react';
import { createRoot, type TestInstance } from 'test-renderer';

import { ToolCallRow, ToolRun } from '../src/components/tools/ToolRun';
import { CLIP_LINES } from '../src/components/tools/bodies';
import type { Run, ToolCall } from '../src/lib/toolcalls';

// React's act() wants to know it runs in a test.
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

async function render(el: ReactElement) {
  const root = createRoot();
  await act(() => {
    root.render(el);
    return Promise.resolve();
  });
  return root.container;
}

/** Every string under a node, joined. */
function text(n: TestInstance | string): string {
  return typeof n === 'string' ? n : n.children.map(text).join('');
}

/** The outermost Text nodes, each as its whole string. */
const lines = (root: TestInstance) =>
  root.queryAll((n) => n.type === 'Text', { matchDeepestOnly: false }).filter((n) => n.parent?.type !== 'Text');
const texts = (root: TestInstance) => lines(root).map(text);

/** Press the button whose text starts so: Pressable's onPress, found up the host node's fibers. */
async function press(root: TestInstance, startsWith: string) {
  const btn = root.queryAll((n) => n.props.accessibilityRole === 'button' && text(n).startsWith(startsWith))[0];
  if (!btn) throw new Error(`no button "${startsWith}" in ${JSON.stringify(texts(root))}`);
  type F = { memoizedProps?: { onPress?: () => void }; return: F | null } | null;
  let f = btn.unstable_fiber as F;
  while (f && typeof f.memoizedProps?.onPress !== 'function') f = f.return;
  const onPress = f?.memoizedProps?.onPress;
  if (!onPress) throw new Error(`"${startsWith}" has no onPress`);
  await act(() => {
    onPress();
    return Promise.resolve();
  });
}

const shell: ToolCall = {
  tool: 'exec',
  args: { command: 'go test ./...' },
  result: 'ok  pkg/a\nmain_test.go:3: want 1, got 2\nFAIL\n[exit: 1]',
  state: 'error',
};

test('shell: the call line with its verdict, the error kept under it, the output when opened', async () => {
  const root = await render(<ToolCallRow call={shell} />);
  expect(texts(root)).toEqual([
    '▸ ✗ ',
    '$ go test ./...',
    '  → ✗ 1 · main_test.go:3: want 1, got 2',
    'main_test.go:3: want 1, got 2',
    'FAIL',
  ]);
  await press(root, '▸');
  const open = texts(root);
  expect(open.slice(0, 2)).toEqual(['▾ ✗ ', '$ go test ./...']);
  expect(open).toContain('$ go test ./...');
  expect(open).toContain('ok  pkg/a\nmain_test.go:3: want 1, got 2\nFAIL');
  expect(open).toContain('exit: 1');
});

test('shell: a long output is cut, and shown whole on a press', async () => {
  const out = Array.from({ length: CLIP_LINES + 5 }, (_, i) => `line ${i}`).join('\n');
  const root = await render(
    <ToolCallRow call={{ tool: 'exec', args: { command: 'seq' }, result: out, state: 'done' }} />,
  );
  await press(root, '▸');
  expect(texts(root)).toContain(`show all ${CLIP_LINES + 5} lines`);
  await press(root, 'show all');
  expect(texts(root)).toContain(out);
});

test('edit: the diff, + and - lines in the theme colours', async () => {
  const call: ToolCall = {
    tool: 'file_edit',
    args: { path: 'internal/a.go' },
    result: 'edited internal/a.go: +2 -1\n@@ -2,4 +2,5 @@\n func A() int {\n-\treturn 1\n+\tx := 2\n+\treturn x\n }',
    state: 'done',
  };
  const root = await render(<ToolCallRow call={call} />);
  expect(texts(root)).toEqual(['▸ ✓ ', 'edit internal/a.go', ' +2 −1']);
  await press(root, '▸');
  const spans = root.queryAll((n) => n.type === 'Text' && n.parent?.type === 'Text');
  // A .go path: each line's code highlighted (code.test.tsx); + and - lines keep a tint.
  const bg = (s: string): unknown =>
    (spans.find((n) => text(n) === s)?.props.style as { backgroundColor?: string } | undefined)?.backgroundColor;
  const add = bg('+\tx := 2');
  const del = bg('-\treturn 1');
  expect(add).toBeTruthy();
  expect(del).toBeTruthy();
  expect(add).not.toBe(del);
  expect(bg(' func A() int {')).toBeUndefined();
});

test('edit of a file with no grammar: + and - lines in the theme colours', async () => {
  const call: ToolCall = {
    tool: 'file_edit',
    args: { path: 'notes.txt' },
    result: 'edited notes.txt: +1 -1\n@@ -1,1 +1,1 @@\n-a\n+b',
    state: 'done',
  };
  const root = await render(<ToolCallRow call={call} />);
  await press(root, '▸');
  const spans = root.queryAll((n) => n.type === 'Text' && n.parent?.type === 'Text');
  const colour = (s: string): unknown => (spans.find((n) => text(n) === s)?.props.style as { color?: string })?.color;
  expect(colour('+b')).toBeTruthy();
  expect(colour('-a')).toBeTruthy();
  expect(colour('+b')).not.toBe(colour('-a'));
});

test('a run of several calls is one section, its summary until opened', async () => {
  const run: Run = {
    key: '0',
    kind: 'tools',
    calls: [
      shell,
      { tool: 'file_read', args: { path: 'a.go' }, result: 'a.go: 5 lines\n1\tx', state: 'done' },
      { tool: 'exec', args: { command: 'ls' }, state: 'pending' },
    ],
  };
  const root = await render(<ToolRun run={run} />);
  expect(texts(root)).toEqual(['▸ ', '⚙ 3 steps — exec ×2, file read', ' · running… · 1 failed']);
  await press(root, '▸ ⚙');
  const open = texts(root);
  expect(open.slice(0, 3)).toEqual(['▾ ', '⚙ 3 steps — exec ×2, file read', ' · running… · 1 failed']);
  expect(open).toContain('read a.go');
  expect(open).toContain(' (5 lines)');
  expect(open).toContain('$ ls');
  expect(open).toContain('  → running…');
});

test('a run of one call is that call alone', async () => {
  const root = await render(<ToolRun run={{ key: '0', kind: 'tools', calls: [shell] }} />);
  expect(texts(root).slice(0, 3)).toEqual(['▸ ✗ ', '$ go test ./...', '  → ✗ 1 · main_test.go:3: want 1, got 2']);
});
