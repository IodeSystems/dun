/// <reference types="jest" />
// The new-session picker: recent workspaces until something is typed, then
// the folders dund completes it with; a recent row starts there, a folder
// opens, Start sends what is typed, and dund's sentences show as they are.
import { act } from 'react';
import { createRoot, type TestInstance } from 'test-renderer';

import { NewSessionForm } from '../src/components/NewSessionForm';
import { baseName, canStart, descend, pickerView, resolvedLine, type DirListing } from '../src/lib/newsession';

const recent = [
  { path: '/home/u/src/aw4', lastActive: '2026-01-01T00:00:00Z', running: true },
  { path: '/home/u/src/dun', lastActive: null, running: false },
];
const listing: DirListing = {
  workspace: '/home/u/src/a',
  problem: 'no directory /home/u/src/a: it does not exist',
  dir: '/home/u/src',
  items: ['/home/u/src/aw4', '/home/u/src/apps'],
  truncated: true,
};

test('pickerView: recent when empty, folders once typed', () => {
  expect(pickerView('  ', recent, listing)).toEqual({ kind: 'recent', rows: recent });
  expect(pickerView('', undefined, undefined)).toEqual({ kind: 'recent', rows: [] });
  expect(pickerView('~/src/a', recent, listing)).toEqual({
    kind: 'dirs',
    dir: '/home/u/src',
    rows: listing.items,
    truncated: true,
  });
  expect(pickerView('~/src/a', recent, undefined)).toEqual({ kind: 'dirs', dir: '', rows: [], truncated: false });
});

test('resolvedLine shows dund: its reason, or where Start starts', () => {
  expect(resolvedLine('~/src/a', listing)).toEqual({ text: listing.problem, ok: false });
  expect(resolvedLine('~/src/aw4', { ...listing, workspace: '/home/u/src/aw4', problem: null })).toEqual({
    text: 'Starts in /home/u/src/aw4',
    ok: true,
  });
  expect(resolvedLine('', listing).text).toBe('');
  expect(resolvedLine('x', null).text).toBe('');
});

test('descend, baseName, canStart', () => {
  expect(descend('/home/u/src')).toBe('/home/u/src/');
  expect(descend('/home/u/src//')).toBe('/home/u/src/');
  expect(baseName('/home/u/src/aw4/')).toBe('aw4');
  expect(baseName('/')).toBe('/');
  expect(canStart(' ', false)).toBe(false);
  expect(canStart('aw4', true)).toBe(false);
  expect(canStart('aw4', false)).toBe(true);
});

const text = (n: TestInstance | string): string => (typeof n === 'string' ? n : n.children.map(text).join(''));
const render = (el: React.ReactElement) => {
  const root = createRoot();
  act(() => {
    root.render(el);
  });
  return root;
};
const buttons = (root: ReturnType<typeof createRoot>) =>
  root.container.queryAll((x) => x.props.accessibilityRole === 'button' && typeof x.props.onClick === 'function');
const press = (root: ReturnType<typeof createRoot>, label: string) => {
  const b = buttons(root).find((x) => text(x).includes(label));
  if (!b) throw new Error('no button ' + label + ' in ' + buttons(root).map(text).join(' | '));
  act(() => (b.props.onClick as (e: object) => void)({ nativeEvent: {} }));
};

function form(over: Partial<React.ComponentProps<typeof NewSessionForm>> = {}) {
  const started: string[] = [];
  const typed: string[] = [];
  const props: React.ComponentProps<typeof NewSessionForm> = {
    typed: '',
    onType: (s) => typed.push(s),
    view: pickerView('', recent, null),
    resolved: { text: '', ok: false },
    pending: false,
    onStart: (w) => started.push(w),
    ...over,
  };
  return { root: render(<NewSessionForm {...props} />), started, typed };
}

test('a recent workspace starts in one tap', () => {
  const { root, started } = form();
  const all = text(root.container);
  expect(all).toContain('Start in a recent workspace');
  expect(all).toContain('/home/u/src/aw4');
  press(root, '/home/u/src/dun');
  expect(started).toEqual(['/home/u/src/dun']);
});

test('a folder opens; Start sends what is typed; dund says why it would refuse', () => {
  const { root, started, typed } = form({
    typed: ' ~/src/a ',
    view: pickerView('~/src/a', recent, listing),
    resolved: resolvedLine('~/src/a', listing),
  });
  const all = text(root.container);
  expect(all).toContain('Folders in /home/u/src');
  expect(all).toContain('no directory /home/u/src/a: it does not exist');
  expect(all).toContain('More folders than shown');
  press(root, 'apps/');
  expect(typed).toEqual(['/home/u/src/apps/']);
  press(root, 'Start');
  expect(started).toEqual(['~/src/a']);
});

test("the create error is dund's sentence, shown in place of the line", () => {
  const { root } = form({
    typed: 'x',
    view: pickerView('x', recent, listing),
    resolved: { text: 'Starts in /home/u/x', ok: true },
    error: 'no directory /home/u/x: it does not exist',
  });
  const all = text(root.container);
  expect(all).toContain('no directory /home/u/x: it does not exist');
  expect(all).not.toContain('Starts in');
});

test('Start is off while empty or starting', () => {
  for (const over of [{ typed: '' }, { typed: 'x', pending: true }]) {
    const { root, started } = form(over);
    const start = buttons(root).find((b) => /Start(ing…)?$/.test(text(b)));
    const state = start?.props.accessibilityState as { disabled?: boolean } | undefined;
    expect(state?.disabled ?? start?.props['aria-disabled']).toBeTruthy();
    expect(started).toEqual([]);
  }
});
