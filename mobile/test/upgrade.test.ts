/// <reference types="jest" />
import { apply, applySnapshot, empty } from '../src/lib/events';
import { shouldReload, upgradeLines, waitsOnTurnHere } from '../src/lib/upgrade';

const pending = {
  type: 'upgrade',
  state: 'pending',
  version: 'd80fc66',
  waiting: [
    { session: 'here', what: 'turn' },
    { session: 'here', what: 'job', id: 3, detail: 'go test ./...' },
    { session: 'other', what: 'turn' },
  ],
};

test('the upgrade event sets the banner; the lines name each wait and how to end it', () => {
  const v = apply(empty, pending);
  expect(v.upgrade?.state).toBe('pending');
  expect(upgradeLines(v.upgrade, 'here')).toEqual([
    '⟳ dund is upgrading to d80fc66 — waiting for:',
    '· the turn in this session (Stop interrupts it)',
    '· job #3 go test ./... (/jobs kill 3)',
    '· a turn in session other (another client)',
  ]);
  expect(waitsOnTurnHere(v.upgrade, 'here')).toBe(true);
  expect(waitsOnTurnHere(v.upgrade, 'other2')).toBe(false);
});

test('restarting replaces the list; a snapshot from the new dund clears the banner', () => {
  const v = apply(apply(empty, pending), { type: 'upgrade', state: 'restarting', version: 'd80fc66' });
  expect(upgradeLines(v.upgrade, 'here')).toEqual(['⟳ dund is restarting into d80fc66… this reconnects']);
  expect(waitsOnTurnHere(v.upgrade, 'here')).toBe(false);
  expect(applySnapshot({ type: 'snapshot', items: [] }, v).upgrade).toBeNull();
});

test('history and reset keep the banner', () => {
  const v = apply(empty, pending);
  expect(apply(v, { type: 'history', items: [] }).upgrade).not.toBeNull();
  expect(apply(v, { type: 'reset' }).upgrade).not.toBeNull();
});

test("dund's automatic resume shows as a note, live and from history", () => {
  const v = apply(empty, { type: 'user', content: '[automatic] dund restarted into build b while you were working.' });
  expect(v.items[0]).toMatchObject({
    kind: 'note',
    text: '↻ resumed after the restart — dund restarted into build b while you were working.',
  });
  const w = apply(empty, { type: 'history', items: [{ kind: 'user', content: '[automatic] x' }] });
  expect(w.items[0]?.kind).toBe('note');
});

test('reload only when the build changed, never for dev or unknown', () => {
  expect(shouldReload('a', 'b')).toBe(true);
  expect(shouldReload('a', 'a')).toBe(false);
  expect(shouldReload(null, 'b')).toBe(false);
  expect(shouldReload('a', '')).toBe(false);
  expect(shouldReload('dev', 'b')).toBe(false);
  expect(shouldReload('a', 'dev')).toBe(false);
});
