/// <reference types="jest" />
import { act } from 'react';
import { createRoot } from 'test-renderer';

import { NoticeRow } from '../src/components/notices/NoticeRow';
import { StyleSheet } from 'react-native';
import { light as lightColors } from '../src/lib/theme';
import { Thinking } from '../src/components/thinking/Thinking';
import { ToolRun } from '../src/components/tools/ToolRun';
import type { Notice } from '../src/lib/notices';
import type { Run } from '../src/lib/toolcalls';

const long = 'a very long title '.repeat(20);

/** Every folded row's title node: fold-title, or a plain notice's text. */
const titles = (el: React.ReactElement) => {
  const root = createRoot();
  act(() => {
    root.render(el);
  });
  return root.container.queryAll((x) => x.props.testID === 'fold-title' || x.props.testID === 'notice-text');
};

const call = (tool: string, args: Record<string, unknown>) => ({
  tool,
  args,
  state: 'done' as const,
  result: 'ok',
});

const oneLine = (el: React.ReactElement) => {
  const t = titles(el);
  expect(t.length).toBeGreaterThanOrEqual(1);
  for (const x of t) {
    expect(x.props.numberOfLines).toBe(1);
  }
};

test('a folded thought stays one line', () => {
  oneLine(<Thinking item={{ text: long, start: 0, end: 0, live: false }} />);
});

test('tool run summary and call lines stay one line', () => {
  const run: Run = {
    key: 'r',
    kind: 'tools',
    calls: [call('exec', { command: long }), call('read', { path: '/x/' + long })],
    thoughts: [{ text: 'hm', start: 1, end: 5001, live: false, at: 1 }],
  };
  oneLine(<ToolRun run={run} />);
  oneLine(<ToolRun run={{ key: 's', kind: 'tools', calls: [call('exec', { command: long })] }} />);
});

test.each<Notice>([
  { type: 'plain', tone: 'note', icon: '🔔', text: long },
  { type: 'job', head: 'background job #2 succeeded', took: '(3s)', command: long, output: '' },
  { type: 'docs', found: 4, surfaced: 2, tokens: 10, docs: [] },
  { type: 'msg', from: long, to: 'api', text: 'hi', carried: false, done: true, failed: false, status: '' },
])('a folded notice row stays one line: %#', (n) => {
  oneLine(<NoticeRow notice={n} />);
});

test('thinking is always the muted colour: folded title, meta, open header and body', () => {
  const muted = lightColors.muted;
  const colourOf = (x: { props: { style?: unknown } }) =>
    (StyleSheet.flatten(x.props.style as never) as { color?: string }).color;
  const root = createRoot();
  act(() => {
    root.render(<Thinking item={{ text: long, start: 0, end: 0, live: false }} />);
  });
  const folded = root.container.queryAll((x) => x.type === 'Text');
  expect(folded.length).toBeGreaterThanOrEqual(2);
  for (const x of folded) expect(colourOf(x)).toBe(muted);
  const btn = root.container.queryAll((x) => typeof x.props.onClick === 'function')[0]!;
  act(() => (btn.props.onClick as (e: object) => void)({ nativeEvent: {} }));
  const open = root.container.queryAll((x) => x.type === 'Text');
  expect(open.length).toBeGreaterThanOrEqual(2);
  for (const x of open) expect(colourOf(x)).toBe(muted);
});
