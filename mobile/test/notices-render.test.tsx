/// <reference types="jest" />
import { act } from 'react';
import { createRoot, type TestInstance } from 'test-renderer';

import { NoticeRow } from '../src/components/notices/NoticeRow';
import type { Notice } from '../src/lib/notices';

const render = (n: Notice) => {
  const root = createRoot();
  act(() => {
    root.render(<NoticeRow notice={n} />);
  });
  return root;
};

const text = (n: TestInstance | string): string => (typeof n === 'string' ? n : n.children.map(text).join(''));
const all = (root: ReturnType<typeof createRoot>) => text(root.container);
// A Pressable's host View takes the press as onClick (Pressability).
const press = (root: ReturnType<typeof createRoot>, i = 0) => {
  const p = root.container.queryAll((x) => typeof x.props.onClick === 'function')[i];
  if (!p) throw new Error('no pressable ' + i);
  act(() => (p.props.onClick as (e: object) => void)({ nativeEvent: {} }));
};

test('a plain notice shows its first line; pressing opens the rest', () => {
  const root = render({ type: 'plain', tone: 'note', icon: '🔔', text: 'one\ntwo', label: 'service' });
  expect(all(root)).toBe('▸ service🔔 one');
  press(root);
  expect(all(root)).toBe('▾ service🔔 one\ntwo');
});

test('docs open to each document with why and source, and a doc opens to its snippet', () => {
  const root = render({
    type: 'docs',
    found: 2,
    surfaced: 1,
    tokens: 0,
    docs: [
      { title: 'README', line: 'install with make', score: 1, kind: 'doc', source: 'doc · README.md', why: 'sim 0.8' },
    ],
  });
  expect(all(root)).toBe('▸ 🔎 2 relevant doc(s) · 1 surfaced · doc README');
  press(root);
  expect(all(root)).toBe('▾ 🔎 2 relevant doc(s) · 1 surfaced▸ README (sim 0.8)doc · README.md');
  press(root, 2);
  expect(all(root)).toContain('install with make');
});

test('a message names sender and receiver; a long one folds', () => {
  const root = render({
    type: 'msg',
    from: 'you',
    to: 'agent #2',
    text: Array.from({ length: 12 }, (_, i) => `line ${i}`).join('\n'),
    carried: true,
    tool: 'jobs',
    done: true,
    failed: false,
    status: 'sent · it is working',
  });
  expect(all(root)).toMatch(/^✉ you → agent #2/);
  expect(all(root)).toContain('more…');
  expect(all(root)).toContain('sent · it is working');
  press(root);
  expect(all(root)).toContain('less');
});

test('a login notice shows the TUI line and a button that opens the URL; settled, it is only a line', () => {
  const { Linking } = jest.requireActual<typeof import('react-native')>('react-native');
  const open = jest.spyOn(Linking, 'openURL').mockResolvedValue();
  const n: Notice = { type: 'login', server: 'gh', url: 'https://x.dev/login', done: false };
  const root = render(n);
  expect(all(root)).toBe('🔑 open to log in to gh: https://x.dev/loginLog in to gh');
  press(root);
  expect(open).toHaveBeenCalledWith('https://x.dev/login');
  expect(all(render({ ...n, done: true }))).toBe('🔑 gh: logged in');
  expect(all(render({ ...n, url: '' }))).toBe('🔑 gh: waiting for a login to it that another session started');
});
