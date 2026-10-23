/// <reference types="jest" />
import { apply, applySnapshot, empty, filterModels, interruptedNote, interrupting, isKillAll, parseLine, replaceWith, suggest, type View } from '../src/lib/events';

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
  const w = run([
    { type: 'answered', value: 'blue' },
    { type: 'tool_call', tool: 'ask_user', args: { question: 'colour?', options: ['red', 'blue'], recommended: 'blue' } },
    { type: 'tool_result', tool: 'ask_user', result: 'blue' },
  ], v);
  expect(w.ask).toBeNull();
  expect(w.items.map((i) => i.kind)).toEqual(['question', 'user']);
  expect(w.items[0]).toMatchObject({ text: 'colour?', recommended: 'blue' });
});

test('an ask_user in history renders as the question and its answer, not JSON', () => {
  const v = apply(empty, {
    type: 'history',
    items: [
      { kind: 'tool_call', tool: 'ask_user', args: { question: 'Did you mean to switch models?' } },
      { kind: 'tool_result', tool: 'ask_user', content: 'yes' },
    ],
  });
  expect(v.items.map((i) => i.kind)).toEqual(['question', 'user']);
  expect(v.items[0]).toMatchObject({ text: 'Did you mean to switch models?', options: [] });
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
  { name: 'raglit', description: 'docs', group: 'tools', choices: [{ value: 'on' }, { value: 'off' }], state: 'off' },
  { name: 'remember', description: 'save', group: 'memory', text: true },
];

test('commands and skills are kept, through a history replace', () => {
  let v = run([{ type: 'commands', items: cmds }, { type: 'catalog', skills: [{ name: 'tdd', description: 'test first', source: 'user' }] }]);
  v = apply(v, { type: 'history', items: [] });
  expect(v.commands.map((c) => c.name)).toEqual(['raglit', 'remember']);
  expect(v.skills[0]?.name).toBe('tdd');
});

test('a line is a message, a slash command, a skill, or unknown', () => {
  const skills = [{ name: 'tdd', description: '' }];
  expect(parseLine('hello', skills)).toEqual({ kind: 'message', text: 'hello' });
  expect(parseLine(' /raglit on ', skills)).toEqual({ kind: 'slash', line: 'raglit on' });
  expect(parseLine('/skill tdd fix it', skills)).toEqual({ kind: 'skill', text: 'Use the skill tdd (call skill name=tdd first): fix it' });
  expect(parseLine('/skill nope', skills).kind).toBe('unknown');
});

test('the palette completes names, then choices, then skills', () => {
  expect(suggest('/r', cmds, []).map((s) => s.insert)).toEqual(['/raglit ', '/remember ']);
  expect(suggest('/raglit o', cmds, []).map((s) => s.insert)).toEqual(['/raglit on', '/raglit off']);
  expect(suggest('/skill t', cmds, [{ name: 'tdd', description: 'x' }]).map((s) => s.insert)).toEqual(['/skill tdd ']);
});

test('a confirmed close marks the session closed; a reset clears it', () => {
  const v = run([{ type: 'user', content: 'x' }, { type: 'control', id: 'close', message: 'closed: forgot session s1' }]);
  expect(v.closed).toBe(true);
  const w = apply(run([{ type: 'commands', items: cmds }, { type: 'user', content: 'x' }]), { type: 'reset' });
  expect(w.items).toHaveLength(0);
  expect(w.commands).toHaveLength(2);
});

test('/model is the client picker; models and settings are kept', () => {
  expect(parseLine('/model qwen 27', [])).toEqual({ kind: 'provider', cmd: 'model', args: ['qwen', '27'] });
  expect(parseLine('/login corp', [])).toEqual({ kind: 'provider', cmd: 'login', args: ['corp'] });
  const v = run([
    { type: 'settings', model: 'm1', provider: 'local', url: 'http://l' },
    { type: 'models', providers: [{ name: 'local', url: 'http://l', ids: ['qwen-27b', 'llama'] }, { name: 'or', url: 'http://o', ids: ['qwen-7b'] }] },
  ]);
  expect(v.settings?.model).toBe('m1');
  expect(filterModels(v.models!, 'qwen').map((r) => r.id)).toEqual(['qwen-27b', 'qwen-7b']);
  expect(filterModels(v.models!, 'or qwen').map((r) => r.provider)).toEqual(['or']);
});

test('a queued message loses its mark when the turn that read it ends', () => {
  const v = run([{ type: 'turn' }, { type: 'user', content: 'also this', queued: true }]);
  expect(v.items[0]).toMatchObject({ queued: true });
  expect(apply(v, { type: 'done' }).items[0]).toMatchObject({ queued: false });
});

test('a provider that failed to list (ids null) does not break the picker', () => {
  const ps = [
    { name: 'dead', url: 'https://dead', ids: null, error: 'HTTP 401' },
    { name: 'ok', url: 'https://ok', ids: ['m1'] },
  ];
  expect(filterModels(ps, '').map((r) => r.id)).toEqual(['m1']);
});

test('an interrupted turn keeps its partial reply, ends with a note, and keeps queued marks', () => {
  const v = run([
    { type: 'user', content: 'go' },
    { type: 'turn', kind: 'user' },
    { type: 'token', text: 'Starting with' },
    { type: 'user', content: 'no, the other one', queued: true },
  ]);
  const s = interrupting(v);
  expect(s.status).toBe('interrupting…');
  const w = apply(s, { type: 'done', reason: 'interrupted' });
  expect(w.busy).toBe(false);
  expect(w.status).toBe('');
  expect(w.streaming).toBeNull();
  expect(w.items.map((i) => i.kind)).toEqual(['user', 'user', 'assistant', 'note']);
  expect(w.items[2]).toMatchObject({ text: 'Starting with' });
  expect(w.items[3]).toMatchObject({ text: interruptedNote });
  expect(w.items[1]).toMatchObject({ queued: true });
});

test('an interrupted turn names the jobs it left running', () => {
  const v = run([{ type: 'user', content: 'go' }, { type: 'turn', kind: 'user' }]);
  const detail = 'still running: #3 go test ./... (2m) — /jobs kill 3 to stop it';
  const w = apply(v, { type: 'done', reason: 'interrupted', detail });
  expect(w.items.slice(-2)).toMatchObject([{ text: interruptedNote }, { kind: 'note', text: detail }]);
  expect(apply(v, { type: 'done', reason: 'interrupted' }).items.at(-1)).toMatchObject({ text: interruptedNote });
});

test('only /jobs kill all asks first', () => {
  expect(isKillAll('jobs kill all')).toBe(true);
  expect(isKillAll('/jobs KILL all ')).toBe(true);
  expect(isKillAll('jobs kill 3')).toBe(false);
  expect(isKillAll('jobs')).toBe(false);
});

test('interrupting is a no-op when idle, and an idle answer clears it', () => {
  expect(interrupting(empty)).toBe(empty);
  const v = interrupting(run([{ type: 'turn' }]));
  const w = apply(v, { type: 'control', id: 'interrupt', message: 'nothing to interrupt: no turn is running' });
  expect(w.status).toBe('');
});
