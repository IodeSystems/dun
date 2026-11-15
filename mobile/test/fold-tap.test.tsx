/// <reference types="jest" />
// A fold opens below its head, tinted, and a tap on the head or the body closes it.
import { act } from 'react';
import { Platform } from 'react-native';
import { createRoot, type TestInstance } from 'test-renderer';

import { Thinking } from '../src/components/thinking/Thinking';
import { ToolRun } from '../src/components/tools/ToolRun';
import { dark, light } from '../src/lib/theme';
import type { Run } from '../src/lib/toolcalls';

const flat = (s: unknown): Record<string, unknown> =>
  Object.assign({}, ...([s].flat(5).filter(Boolean) as object[])) as Record<string, unknown>;
const text = (n: TestInstance | string): string => (typeof n === 'string' ? n : n.children.map(text).join(''));
const render = (el: React.ReactElement) => {
  const root = createRoot();
  act(() => {
    root.render(el);
  });
  return root;
};
const bodies = (root: ReturnType<typeof createRoot>) => root.container.queryAll((x) => x.props.testID === 'fold-body');
const tap = (n: TestInstance) => act(() => (n.props.onClick as (e: object) => void)({ nativeEvent: {} }));
const heads = (root: ReturnType<typeof createRoot>) =>
  root.container.queryAll((x) => x.props.accessibilityRole === 'button' && typeof x.props.onClick === 'function');

const thought = { text: 'headline\nsecond line', start: 0, end: 3000, live: false };
const call = (command: string) => ({
  tool: 'exec',
  args: { command },
  state: 'done' as const,
  result: 'first\nbody-' + command,
});
const run: Run = { key: 'r', kind: 'tools', calls: [call('a'), call('b')], thoughts: [{ ...thought, at: 1 }] };

test('a thought opens below, tinted, and a tap on its text closes it', () => {
  const root = render(<Thinking item={thought} />);
  expect(bodies(root)).toHaveLength(0);
  tap(heads(root)[0]!);
  expect(text(root.container)).toContain('second line');
  const [body] = bodies(root);
  tap(body!);
  expect(text(root.container)).not.toContain('second line');
});

test('a tap that ends a text selection does not close', () => {
  const root = render(<Thinking item={thought} />);
  tap(heads(root)[0]!);
  const old = Platform.OS;
  const g = globalThis as { getSelection?: unknown };
  Platform.OS = 'web';
  g.getSelection = () => ({ toString: () => 'deep' });
  try {
    tap(bodies(root)[0]!);
    expect(text(root.container)).toContain('second line');
    g.getSelection = () => ({ toString: () => '' });
    tap(bodies(root)[0]!);
    expect(text(root.container)).not.toContain('second line');
  } finally {
    Platform.OS = old;
    delete g.getSelection;
  }
});

test('a nested fold toggles only itself; the parent has the lighter tint', () => {
  const root = render(<ToolRun run={run} />);
  tap(heads(root)[0]!);
  expect(text(root.container)).toContain('$ a');
  // heads: run, call a, call b, thought
  const inner = heads(root).find((h) => text(h).includes('$ a'))!;
  tap(inner);
  expect(text(root.container)).toContain('body-a');
  const open = root.container.queryAll((x) => flat(x.props.style).borderLeftWidth === 3);
  const bgs = open.map((x) => flat(x.props.style).backgroundColor);
  expect(bgs).toContain(light.sunken);
  expect(bgs).toContain(light.fold);
  // Tap the inner body: it closes, the run stays open.
  const innerBody = bodies(root).findLast((b) => text(b).includes('body-a'))!;
  tap(innerBody);
  expect(text(root.container)).not.toContain('body-a');
  expect(text(root.container)).toContain('$ b');
  // Tap the run's body gap: the run closes.
  tap(bodies(root)[0]!);
  expect(text(root.container)).not.toContain('$ b');
});

test('the fold tint differs from the section tint in both themes', () => {
  expect(light.fold).not.toBe(light.sunken);
  expect(dark.fold).not.toBe(dark.sunken);
});
