/// <reference types="jest" />
import { apply, applySnapshot, empty, parseLine, replaceWith, suggest, type View } from '../src/lib/events';

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

test('a snapshot event in the stream replaces what is shown', () => {
  const v = run([{ type: 'user', content: 'stale' }]);
  const w = apply(v, { type: 'snapshot', after: 9, items: [{ kind: 'assistant', content: 'fresh' }], state: [] });
  expect(w.items.map((i) => (i.kind === 'tool' ? i.title : i.text))).toEqual(['fresh']);
});

const cmds = [
  { name: 'rag', description: 'docs', group: 'tools', choices: [{ value: 'on' }, { value: 'off' }], state: 'off' },
  { name: 'remember', description: 'save', group: 'memory', text: true },
];

test('commands and skills are kept, through a history replace', () => {
  let v = run([{ type: 'commands', items: cmds }, { type: 'catalog', skills: [{ name: 'tdd', description: 'test first', source: 'user' }] }]);
  v = apply(v, { type: 'history', items: [] });
  expect(v.commands.map((c) => c.name)).toEqual(['rag', 'remember']);
  expect(v.skills[0]?.name).toBe('tdd');
});

test('a line is a message, a slash command, a skill, or unknown', () => {
  const skills = [{ name: 'tdd', description: '' }];
  expect(parseLine('hello', skills)).toEqual({ kind: 'message', text: 'hello' });
  expect(parseLine(' /rag on ', skills)).toEqual({ kind: 'slash', line: 'rag on' });
  expect(parseLine('/skill tdd fix it', skills)).toEqual({ kind: 'skill', text: 'Use the skill tdd (call skill name=tdd first): fix it' });
  expect(parseLine('/skill nope', skills).kind).toBe('unknown');
});

test('the palette completes names, then choices, then skills', () => {
  expect(suggest('/r', cmds, []).map((s) => s.insert)).toEqual(['/rag ', '/remember ']);
  expect(suggest('/rag o', cmds, []).map((s) => s.insert)).toEqual(['/rag on', '/rag off']);
  expect(suggest('/skill t', cmds, [{ name: 'tdd', description: 'x' }]).map((s) => s.insert)).toEqual(['/skill tdd ']);
});

test('a confirmed close marks the session closed; a reset clears it', () => {
  const v = run([{ type: 'user', content: 'x' }, { type: 'control', id: 'close', message: 'closed: forgot session s1' }]);
  expect(v.closed).toBe(true);
  const w = apply(run([{ type: 'commands', items: cmds }, { type: 'user', content: 'x' }]), { type: 'reset' });
  expect(w.items).toHaveLength(0);
  expect(w.commands).toHaveLength(2);
});
