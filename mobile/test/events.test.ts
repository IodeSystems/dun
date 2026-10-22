/// <reference types="jest" />
import { apply, applySnapshot, empty, replaceWith, type View } from '../src/lib/events';

const run = (evs: Record<string, unknown>[], v: View = empty) => evs.reduce(apply, v);

test('a turn: echo, streamed tokens, the settled message, done', () => {
  const v = run([
    { type: 'user', content: 'hi' },
    { type: 'turn', kind: 'user' },
    { type: 'token', text: 'hel' },
    { type: 'token', text: 'lo' },
  ]);
  expect(v.busy).toBe(true);
  expect(v.streaming).toBe('hello');
  const w = run([{ type: 'message', role: 'assistant', content: 'hello' }, { type: 'done' }], v);
  expect(w.items.map((i) => i.kind)).toEqual(['user', 'assistant']);
  expect(w.streaming).toBeNull();
  expect(w.busy).toBe(false);
});

test('a tool call settles the text streamed before it', () => {
  const v = run([
    { type: 'token', text: 'let me look' },
    { type: 'tool_call', tool: 'exec', args: { cmd: 'ls' } },
    { type: 'tool_result', tool: 'exec', result: 'a b' },
  ]);
  expect(v.items.map((i) => i.kind)).toEqual(['assistant', 'tool', 'tool']);
  expect(v.streaming).toBeNull();
});

test('history replaces what is shown, never adds', () => {
  const v = run([{ type: 'user', content: 'old' }]);
  const w = apply(v, { type: 'history', items: [{ kind: 'user', content: 'a' }, { kind: 'assistant', content: 'b' }] });
  expect(w.items.map((i) => (i.kind === 'tool' ? i.title : i.text))).toEqual(['a', 'b']);
});

test('an ask stays open until answered, from any client', () => {
  const v = run([{ type: 'ask', question: 'colour?', options: ['red', 'blue'] }]);
  expect(v.ask).toEqual({ question: 'colour?', options: ['red', 'blue'] });
  const w = apply(v, { type: 'answered', value: 'blue' });
  expect(w.ask).toBeNull();
  expect(w.items.at(-1)).toMatchObject({ kind: 'user', text: 'blue' });
});

test('a snapshot renders the conversation then applies its state events', () => {
  const v = applySnapshot({ after: 40, items: [{ kind: 'user', content: 'q' }], state: [{ type: 'ask', question: 'ok?', options: [] }] });
  expect(v.items).toHaveLength(1);
  expect(v.ask?.question).toBe('ok?');
});

test('unknown events change nothing', () => {
  expect(apply(empty, { type: 'from_the_future', x: 1 })).toBe(empty);
  expect(replaceWith([null, 3, { kind: 'mystery' }]).items).toHaveLength(0);
});
