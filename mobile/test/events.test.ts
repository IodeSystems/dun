/// <reference types="jest" />
import {
  apply,
  applySnapshot,
  empty,
  filterModels,
  interruptedNote,
  isKillAll,
  parseLine,
  replaceWith,
  suggest,
  type View,
} from '../src/lib/events';

const run = (evs: Record<string, unknown>[], v: View = empty) => evs.reduce(apply, v);

test('a turn: echo, streamed tokens, the settled message, done', () => {
  const v = run([
    { type: 'user', content: 'hi' },
    { type: 'turn', kind: 'user' },
    { type: 'turn_state', phase: 'working', queued: [] },
    { type: 'token', text: 'hel' },
    { type: 'token', text: 'lo' },
  ]);
  expect(v.busy).toBe(true);
  expect(v.streaming).toBe('hello');
  const w = run(
    [
      { type: 'message', role: 'assistant', content: 'hello' },
      { type: 'done' },
      { type: 'turn_state', phase: 'idle', queued: [] },
    ],
    v,
  );
  expect(w.items.map((i) => i.kind)).toEqual(['user', 'assistant']);
  expect(w.streaming).toBeNull();
  expect(w.busy).toBe(false);
});

test('a tool call settles the text streamed before it; the result lands in its row', () => {
  const v = run([
    { type: 'token', text: 'let me look' },
    { type: 'tool_call', tool: 'exec', args: { cmd: 'ls' } },
    { type: 'tool_result', tool: 'exec', result: 'a b' },
  ]);
  expect(v.items.map((i) => i.kind)).toEqual(['assistant', 'tools']);
  expect(v.items[1]).toMatchObject({ kind: 'tools', calls: [{ tool: 'exec', state: 'done', result: 'a b' }] });
  expect(v.streaming).toBeNull();
});

test('history replaces what is shown, never adds', () => {
  const v = run([{ type: 'user', content: 'old' }]);
  const w = apply(v, {
    type: 'history',
    items: [
      { kind: 'user', content: 'a' },
      { kind: 'assistant', content: 'b' },
    ],
  });
  expect(w.items.map((i) => (i.kind === 'tools' || i.kind === 'notice' ? '' : i.text))).toEqual(['a', 'b']);
});

test('an ask stays open until answered, from any client; its row comes from the ask', () => {
  const v = run([
    { type: 'tool_call', tool: 'ask_user', call_id: 'c1', args: { question: 'colour?', options: ['red', 'blue'] } },
    {
      type: 'ask',
      ask_id: 'a1',
      call_id: 'c1',
      question: 'colour?',
      options: ['red', 'blue', 'green'],
      multi: true,
      recommended_all: ['blue', 'green'],
      no_custom: true,
    },
  ]);
  expect(v.ask).toEqual({
    askId: 'a1',
    question: 'colour?',
    options: ['red', 'blue', 'green'],
    recommended: 'blue',
    recommendedAll: ['blue', 'green'],
    multi: true,
    noCustom: true,
  });
  expect(v.items).toEqual([expect.objectContaining({ kind: 'question', multi: true, callId: 'c1' })]);
  const w = run(
    [
      { type: 'answered', ask_id: 'old', value: 'stale' }, // another question's: nothing
      { type: 'answered', ask_id: 'a1', value: 'blue, green', by: 'unix:1000' },
      { type: 'tool_result', tool: 'ask_user', call_id: 'c1', result: 'blue, green' },
    ],
    v,
  );
  expect(w.ask).toBeNull();
  expect(w.items.map((i) => i.kind)).toEqual(['question', 'user']);
  expect(w.items[1]).toMatchObject({ text: 'blue, green' });
});

test('a snapshot that holds the asking call shows one question row', () => {
  const v = applySnapshot({
    items: [{ kind: 'tool_call', tool: 'ask_user', call_id: 'c1', args: { question: 'ok?', options: ['y', 'n'] } }],
    state: [],
  });
  const w = apply(v, { type: 'ask', ask_id: 'a', call_id: 'c1', question: 'ok?', options: ['y', 'n'] });
  expect(w.items.filter((i) => i.kind === 'question')).toHaveLength(1);
  expect(w.ask?.askId).toBe('a');
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
  const v = applySnapshot({
    after: 40,
    items: [{ kind: 'user', content: 'q' }],
    state: [{ type: 'ask', question: 'ok?', options: [] }],
  });
  // The restored question (dund's, not in the history) is its own row.
  expect(v.items.map((i) => i.kind)).toEqual(['user', 'question']);
  expect(v.ask?.question).toBe('ok?');
});

test('unknown events change nothing', () => {
  expect(apply(empty, { type: 'from_the_future', x: 1 })).toBe(empty);
  expect(replaceWith([null, 3, { kind: 'mystery' }]).items).toHaveLength(0);
});

test('a snapshot event in the stream replaces what is shown', () => {
  const v = run([{ type: 'user', content: 'stale' }]);
  const w = apply(v, { type: 'snapshot', after: 9, items: [{ kind: 'assistant', content: 'fresh' }], state: [] });
  expect(w.items.map((i) => (i.kind === 'tools' || i.kind === 'notice' ? '' : i.text))).toEqual(['fresh']);
});

const cmds = [
  { name: 'raglit', description: 'docs', group: 'tools', choices: [{ value: 'on' }, { value: 'off' }], state: 'off' },
  { name: 'remember', description: 'save', group: 'memory', text: true },
];

test('commands and skills are kept, through a history replace', () => {
  let v = run([
    { type: 'commands', items: cmds },
    { type: 'catalog', skills: [{ name: 'tdd', description: 'test first', source: 'user' }] },
  ]);
  v = apply(v, { type: 'history', items: [] });
  expect(v.commands.map((c) => c.name)).toEqual(['raglit', 'remember']);
  expect(v.skills[0]?.name).toBe('tdd');
});

test('a line is a message, a slash command, a skill, or unknown', () => {
  const skills = [{ name: 'tdd', description: '' }];
  expect(parseLine('hello', skills)).toEqual({ kind: 'message', text: 'hello' });
  expect(parseLine(' /raglit on ', skills)).toEqual({ kind: 'slash', line: 'raglit on' });
  expect(parseLine('/skill tdd fix it', skills)).toEqual({
    kind: 'skill',
    text: 'Use the skill tdd (call skill name=tdd first): fix it',
  });
  expect(parseLine('/skill nope', skills).kind).toBe('unknown');
});

test('the palette completes names, then choices, then skills', () => {
  expect(suggest('/r', cmds, []).map((s) => s.insert)).toEqual(['/raglit ', '/remember ']);
  expect(suggest('/raglit o', cmds, []).map((s) => s.insert)).toEqual(['/raglit on', '/raglit off']);
  expect(suggest('/skill t', cmds, [{ name: 'tdd', description: 'x' }]).map((s) => s.insert)).toEqual(['/skill tdd ']);
});

test('a confirmed close marks the session closed; a reset clears it', () => {
  const v = run([
    { type: 'user', content: 'x' },
    { type: 'control', id: 'close', message: 'closed: forgot session s1' },
  ]);
  expect(v.closed).toBe(true);
  const w = apply(
    run([
      { type: 'commands', items: cmds },
      { type: 'user', content: 'x' },
    ]),
    { type: 'reset' },
  );
  expect(w.items).toHaveLength(0);
  expect(w.commands).toHaveLength(2);
});

test('/model is the client picker; models and settings are kept', () => {
  expect(parseLine('/model qwen 27', [])).toEqual({ kind: 'provider', cmd: 'model', args: ['qwen', '27'] });
  expect(parseLine('/login corp', [])).toEqual({ kind: 'provider', cmd: 'login', args: ['corp'] });
  const v = run([
    { type: 'settings', model: 'm1', provider: 'local', url: 'http://l' },
    {
      type: 'models',
      providers: [
        { name: 'local', url: 'http://l', ids: ['qwen-27b', 'llama'] },
        { name: 'or', url: 'http://o', ids: ['qwen-7b'] },
      ],
    },
  ]);
  expect(v.settings?.model).toBe('m1');
  expect(filterModels(v.models!, 'qwen').map((r) => r.id)).toEqual(['qwen-27b', 'qwen-7b']);
  expect(filterModels(v.models!, 'or qwen').map((r) => r.provider)).toEqual(['or']);
});

test('a queued message with no id (an old event log) loses its mark when the turn ends', () => {
  const v = run([{ type: 'turn' }, { type: 'user', content: 'also this', queued: true }]);
  expect(v.items[0]).toMatchObject({ queued: true });
  expect(apply(v, { type: 'done' }).items[0]).toMatchObject({ queued: false });
});

test('a queued message with an id waits for delivered, not for the turn to end', () => {
  const v = run([{ type: 'turn' }, { type: 'user', id: 'm1', content: 'also this', queued: true }, { type: 'done' }]);
  expect(v.items[0]).toMatchObject({ queued: true, id: 'm1' });
  const w = apply(v, { type: 'delivered', ids: ['m1'] });
  expect(w.items).toHaveLength(1);
  expect(w.items[0]).toMatchObject({ kind: 'user', id: 'm1', text: 'also this' });
  expect(w.items[0]).not.toMatchObject({ queued: true });
});

test('withdrawn drops the rows it names, by id, not by text', () => {
  const v = run([
    { type: 'user', id: 'a', content: 'same', queued: true },
    { type: 'user', id: 'b', content: 'same', queued: true },
    { type: 'withdrawn', ids: ['b'] },
  ]);
  expect(v.items).toHaveLength(1);
  expect(v.items[0]).toMatchObject({ id: 'a', queued: true });
});

test('a provider that failed to list (ids null) does not break the picker', () => {
  const ps = [
    { name: 'dead', url: 'https://dead', ids: null, error: 'HTTP 401' },
    { name: 'ok', url: 'https://ok', ids: ['m1'] },
  ];
  expect(filterModels(ps, '').map((r) => r.id)).toEqual(['m1']);
});

test('a delivered message lands after the tool result that carried it, live as on replay', () => {
  const v = run([
    { type: 'turn' },
    { type: 'tool_call', tool: 'exec', args: { command: 'ls' } },
    { type: 'user', id: 'm1', content: 'also this', queued: true },
    { type: 'turn_state', phase: 'working', queued: [{ id: 'm1', text: 'also this' }] },
  ]);
  expect(v.status).toBe('1 message(s) queued for the running turn');
  let w = apply(v, { type: 'tool_result', tool: 'exec', result: 'ok' });
  expect(w.items.find((i) => i.kind === 'user')).toMatchObject({ queued: true });
  w = run(
    [
      { type: 'delivered', ids: ['m1'] },
      { type: 'turn_state', phase: 'working', queued: [] },
    ],
    w,
  );
  expect(w.items.map((i) => i.kind)).toEqual(['tools', 'user']);
  expect(w.status).toBe('working…');
  const replayed = replaceWith([
    { kind: 'tool_call', tool: 'exec', args: { command: 'ls' } },
    { kind: 'tool_result', tool: 'exec', content: 'ok' },
    { kind: 'user', id: 'm1', queued: true, content: 'also this' },
  ]);
  expect(replayed.items.map((i) => i.kind)).toEqual(['tools', 'user']);
});

test('an interrupted turn keeps its partial reply, ends with a note, and keeps queued marks', () => {
  const v = run([
    { type: 'user', content: 'go' },
    { type: 'turn', kind: 'user' },
    { type: 'token', text: 'Starting with' },
    { type: 'user', id: 'q', content: 'no, the other one', queued: true },
  ]);
  const s = apply(v, { type: 'turn_state', phase: 'interrupting', queued: [{ id: 'q', text: 'no, the other one' }] });
  expect(s.status).toBe('interrupting…');
  // The queue outlives the cut turn: the next turn takes it.
  const w = run(
    [
      { type: 'done', reason: 'interrupted' },
      { type: 'turn_state', phase: 'idle', queued: [{ id: 'q', text: 'no, the other one' }] },
    ],
    s,
  );
  expect(w.busy).toBe(false);
  expect(w.status).toBe('1 message(s) queued for the running turn');
  expect(w.streaming).toBeNull();
  expect(w.items.map((i) => i.kind)).toEqual(['user', 'user', 'assistant', 'note']);
  expect(w.items[2]).toMatchObject({ text: 'Starting with' });
  expect(w.items[3]).toMatchObject({ text: interruptedNote });
  expect(w.items[1]).toMatchObject({ queued: true });
});

test('an interrupted turn names the jobs it left running', () => {
  const v = run([
    { type: 'user', content: 'go' },
    { type: 'turn', kind: 'user' },
  ]);
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

test('turn_state is busy, the status, the question and the queue; nothing is inferred', () => {
  const ts = (phase: string, more: Record<string, unknown> = {}) => ({
    type: 'turn_state',
    phase,
    queued: [],
    ...more,
  });
  let v = run([{ type: 'turn' }, { type: 'token', text: 'x' }]);
  expect(v.busy).toBe(false); // a token is not a state
  v = run([ts('working', { since: 5 })], v);
  expect(v.busy).toBe(true);
  expect(v.status).toBe('working…');
  v = run([ts('interrupting', { since: 5 })], v);
  expect(v.status).toBe('interrupting…');
  // A client that arrived late: the question and the queue from the state alone.
  const late = run([
    ts('waiting_answer', { ask: { question: 'which?', options: ['a', 'b'] }, queued: [{ id: 'q1', text: 'later' }] }),
  ]);
  expect(late.ask).toMatchObject({ question: 'which?', options: ['a', 'b'] });
  expect(late.items).toEqual([expect.objectContaining({ kind: 'user', id: 'q1', queued: true })]);
  expect(late.status).toBe('1 message(s) queued for the running turn');
  // A provider wait whose round is redone drops the half-streamed reply.
  const r = run([
    ts('working'),
    { type: 'token', text: 'half a sen' },
    ts('retrying', { discard: true, retry: { text: 'wait' } }),
  ]);
  expect(r.streaming).toBeNull();
  expect(r.status).toBe('wait');
  // The engine stopped with a message queued: idle, an empty queue, the row says it was lost.
  const lost = run([
    { type: 'user', id: 'm', content: 'x', queued: true },
    ts('working', { queued: [{ id: 'm', text: 'x' }] }),
    ts('idle'),
  ]);
  expect(lost.busy).toBe(false);
  expect(lost.items[0]).toMatchObject({ queued: false, lost: true });
});

test('a docs notification is a docs notice: each document, why it won and where it came from', () => {
  const v = run([
    {
      type: 'notification',
      kind: 'docs',
      surfaced: 1,
      docs: [
        {
          title: 'skill docs',
          score: 1.2e-5,
          why: 'only candidate · score 1.2e-05',
          source: 'skill · ~/.claude/skills/synced/b1/docs/SKILL.md · index skills',
        },
      ],
    },
  ]);
  const it = v.items[0];
  expect(it?.kind === 'notice' && it.notice.type === 'docs' && it.notice.docs[0]).toMatchObject({
    title: 'skill docs',
    why: 'only candidate · score 1.2e-05',
    source: 'skill · ~/.claude/skills/synced/b1/docs/SKILL.md · index skills',
  });
});

test('a reconnect (snapshot, twice) repeats no note: replayed state adds no rows', () => {
  const snap = {
    type: 'snapshot',
    items: [{ kind: 'user', id: 'u1', content: 'hi' }],
    state: [
      { type: 'server', id: 'rag', action: 'on', message: 'rag: on', replayed: true },
      { type: 'engine', state: 'restarting', reason: 'crash', replayed: true },
      { type: 'turn_state', phase: 'working', queued: [{ id: 'q', text: 'later' }], replayed: true },
    ],
  };
  const once = apply(empty, snap);
  const twice = apply(once, snap);
  for (const v of [once, twice]) {
    expect(v.items.map((i) => i.kind)).toEqual(['user', 'user']);
    expect(v.items[1]).toMatchObject({ id: 'q', queued: true });
    expect(v.busy).toBe(true);
  }
});
