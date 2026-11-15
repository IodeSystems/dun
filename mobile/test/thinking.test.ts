/// <reference types="jest" />
import { apply, applySnapshot, empty, type View } from '../src/lib/events';
import { thinkDur, thinkHeader, thinkHeadline, thoughtHead, thoughtMeta, type Thought } from '../src/lib/thinking';

// internal/tui/thinkfold_test.go TestThinkHeadline, case for case.
test.each([
  ['empty', '', true, ''],
  ['blank', ' \n\n', false, ''],
  ['live: the latest step', '# Thinking: read the config\nmore\n# Thinking: find the bug\nbody', true, 'find the bug'],
  ['live: a step still arriving', '# Thinking: read the config\n# Thinking: find the b', true, 'find the b'],
  ['live: half a keyword is not a heading yet', '# Thinking: read the config\n# Thin', true, 'read the config'],
  ['live: a bare marker is not a heading', '# Thinking: read the config\n# Thinking:', true, 'read the config'],
  ['done: the last summary', '# Thinking: a\n# Thinking: b\n# Summary: found it\n', false, 'found it'],
  ['done: the last of two summaries', '# Summary: first\n# Thinking: c\n# Summary: second', false, 'second'],
  ['done, no summary: the last step', '# Thinking: a\nbody\n# Thinking: b\nbody', false, 'b'],
  ['live, only a summary so far: the first line', '# Summary: done\n', true, 'Summary: done'],
  ['no headings: the first line', '\n\n  Let me look at this.\nThen more.', false, 'Let me look at this.'],
  ['no headings, live', 'Let me look', true, 'Let me look'],
  ['case and spaces', '   #   THINKING :  Loud  \n#summary:quiet', false, 'quiet'],
  ['case and spaces, live', '   #thinking:  Loud  ', true, 'Loud'],
  ['more hashes', '## Thinking: deeper', true, 'deeper'],
  ['a heading mid-line is not one', 'see # Thinking: x\nfirst', true, 'see # Thinking: x'],
  ['crlf', '# Thinking: a\r\n# Summary: b\r\n', false, 'b'],
  ['inside a fence it still counts', '```sh\n# Thinking: in a script\n```\nafter', true, 'in a script'],
  ['a markdown title as the fallback', '# Plan\nbody', false, 'Plan'],
  ['inner whitespace collapses', '# Thinking:  two\tspaced   words', true, 'two spaced words'],
])('thinkHeadline: %s', (_name, text, live, want) => {
  expect(thinkHeadline(text, live)).toBe(want);
});

test('thinkDur', () => {
  expect([0, 999, 1000, 59_999, 60_000, 125_000].map(thinkDur)).toEqual(['<1s', '<1s', '1s', '59s', '1m00s', '2m05s']);
});

const t = (o: Partial<Thought>): Thought => ({ text: '', start: 0, end: 0, live: false, ...o });

test('the folded line: headline and meta, as thoughtLine', () => {
  const live = t({ text: '# Thinking: plan it\nsome words here', start: 1000, live: true });
  expect(thoughtHead(live)).toBe('plan it');
  expect(thoughtMeta(live, 13_500)).toBe('(12s…, 7 words)');
  expect(thoughtMeta(live, 13_500, true)).toBe('(12s…, 7w)');
  const done = t({ text: 'a b c', start: 1000, end: 4000 });
  expect(thoughtMeta(done, 0)).toBe('(3s, 3 words)');
  // From history: no times.
  expect(thoughtMeta(t({ text: 'a b' }), 0)).toBe('(–, 2 words)');
  // No text (Claude sends none by default): what it is.
  expect(thoughtHead(t({ live: true }))).toBe('thinking');
  expect(thoughtHead(t({ cut: true }))).toBe('interrupted');
  expect(thoughtHead(t({}))).toBe('thought');
  expect(thoughtMeta(t({ text: 'x', start: 1000, end: 1500, cut: true }), 0)).toBe('(⏹ <1s, 1 words)');
});

test('the open header, as thinkHeader', () => {
  expect(thinkHeader(t({ text: 'a b', start: 0, live: true }), 5000)).toBe('✻ thinking… 5s · 2 words');
  expect(thinkHeader(t({ text: 'a b', start: 1, end: 3001 }), 0)).toBe('✻ thought for 3s · 2 words');
  expect(thinkHeader(t({ text: 'a', start: 1, end: 2001, cut: true }), 0)).toBe('✻ interrupted after 2s · 1 words');
  expect(thinkHeader(t({ text: 'a' }), 0)).toBe('✻ thought · 1 words');
});

const run = (evs: Record<string, unknown>[], v: View = empty) => evs.reduce(apply, v);
const thoughts = (v: View) => v.items.filter((i) => i.kind === 'thinking');

test('one block per round: reasoning grows it, the first token closes it', () => {
  const v = run([
    { type: 'user', content: 'hi' },
    { type: 'turn' },
    { type: 'reasoning', text: '# Thinking: read', ts: '2026-11-14T10:00:00Z' },
    { type: 'reasoning', text: ' the file\nhmm', ts: '2026-11-14T10:00:02Z' },
  ]);
  expect(thoughts(v)).toHaveLength(1);
  expect(thoughts(v)[0]).toMatchObject({ text: '# Thinking: read the file\nhmm', live: true });
  expect(v.busy).toBe(true);
  const w = run([{ type: 'token', text: 'ok', ts: '2026-11-14T10:00:07Z' }], v);
  const th = thoughts(w)[0] as Thought;
  expect(th.live).toBe(false);
  expect(th.end - th.start).toBe(7000);
  // The next round's reasoning, after a tool call, is a block of its own.
  const x = run(
    [
      { type: 'tool_call', tool: 'read', args: {} },
      { type: 'tool_result', tool: 'read', result: 'x' },
      { type: 'reasoning', text: 'again' },
      { type: 'done', reason: 'interrupted' },
    ],
    w,
  );
  expect(thoughts(x)).toHaveLength(2);
  expect(thoughts(x)[1]).toMatchObject({ live: false, cut: true });
});

test('history: a finished round comes back as a reasoning item', () => {
  const v = applySnapshot({
    type: 'snapshot',
    items: [
      { kind: 'user', content: 'q' },
      { kind: 'reasoning', content: '# Thinking: x\n# Summary: y' },
      { kind: 'assistant', content: 'a' },
    ],
  });
  expect(v.items.map((i) => i.kind)).toEqual(['user', 'thinking', 'assistant']);
  expect(thoughtHead(thoughts(v)[0] as Thought)).toBe('y');
});
