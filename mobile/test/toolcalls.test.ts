/// <reference types="jest" />
import { bodies, bodyFor } from '../src/components/tools/registry';
import { DiffBody, GenericBody, SearchBody, ShellBody } from '../src/components/tools/bodies';
import { apply, applySnapshot, empty, type Item, type View } from '../src/lib/events';
import { failed, type Run } from '../src/lib/toolcalls';
import { callText, preview, runSummary, toolLabel } from '../src/lib/tooltext';

const run = (evs: Record<string, unknown>[], v: View = empty) => evs.reduce(apply, v);
const runs = (v: View) => v.items.filter((i): i is Run => i.kind === 'tools');
const states = (v: View) => runs(v).map((r) => r.calls.map((c) => `${c.tool}:${c.state}`));

test('a call is pending until its result, then done or error, in one item', () => {
  const v = run([{ type: 'tool_call', tool: 'exec', args: { command: 'ls' } }]);
  expect(states(v)).toEqual([['exec:pending']]);
  const w = run([{ type: 'tool_result', tool: 'exec', result: 'a\nb' }], v);
  expect(w.items).toHaveLength(1);
  expect(w.items[0]!.key).toBe(v.items[0]!.key);
  expect(states(w)).toEqual([['exec:done']]);
  const x = run([
    { type: 'tool_call', tool: 'exec', args: { command: 'false' } },
    { type: 'tool_result', tool: 'exec', result: 'boom\n[exit: 1]' },
  ]);
  expect(states(x)).toEqual([['exec:error']]);
});

test('consecutive calls are one run; a reply or a note ends it (toolrun.go)', () => {
  const v = run([
    { type: 'tool_call', tool: 'exec', args: { command: 'ls' } },
    { type: 'tool_result', tool: 'exec', result: 'a' },
    { type: 'tool_call', tool: 'file_read', args: { path: 'a' } },
    { type: 'tool_result', tool: 'file_read', result: 'a: 1 line\n1\tx' },
    { type: 'token', text: 'so' },
    { type: 'tool_call', tool: 'exec', args: { command: 'pwd' } },
    { type: 'tool_result', tool: 'exec', result: '/' },
    { type: 'message', content: 'done' },
  ]);
  expect(v.items.map((i) => i.kind)).toEqual(['tools', 'assistant', 'tools', 'assistant']);
  expect(states(v)).toEqual([['exec:done', 'file_read:done'], ['exec:done']]);
  expect(new Set(v.items.map((i) => i.key)).size).toBe(4);
});

test('results pair by call_id when they come out of order', () => {
  const v = applySnapshot({
    items: [
      { kind: 'tool_call', tool: 'exec', call_id: 'a', args: { command: 'one' } },
      { kind: 'tool_call', tool: 'exec', call_id: 'b', args: { command: 'two' } },
      { kind: 'tool_result', tool: 'exec', call_id: 'b', content: 'second' },
      { kind: 'tool_result', tool: 'exec', call_id: 'a', content: 'first' },
    ],
    state: [],
  });
  const [r] = runs(v);
  expect(r!.calls.map((c) => [c.args?.command, c.result])).toEqual([
    ['one', 'first'],
    ['two', 'second'],
  ]);
});

test('live: a call shows pending from its start; its result pairs by call_id, is_error decides', () => {
  const v = run([
    { type: 'tool_call', tool: 'exec', call_id: 'a', args: { command: 'one' } },
    { type: 'tool_call', tool: 'exec', call_id: 'b', args: { command: 'two' } },
  ]);
  expect(states(v)).toEqual([['exec:pending', 'exec:pending']]);
  const w = run(
    [
      { type: 'tool_result', tool: 'exec', call_id: 'a', result: 'looks like ERROR but is not' },
      { type: 'tool_result', tool: 'exec', call_id: 'b', result: 'ok', is_error: true },
    ],
    v,
  );
  expect(runs(w)[0]!.calls.map((c) => [c.args?.command, c.state])).toEqual([
    ['one', 'done'],
    ['two', 'error'],
  ]);
});

// FALLBACK: an event log from before call ids.
test('without ids a result takes the newest pending call of its tool', () => {
  const v = run([
    { type: 'tool_call', tool: 'exec', args: { command: 'one' } },
    { type: 'tool_call', tool: 'file_read', args: { path: 'x' } },
    { type: 'tool_result', tool: 'exec', result: 'r1' },
    { type: 'tool_result', tool: 'file_read', result: 'r2' },
  ]);
  expect(runs(v)[0]!.calls.map((c) => c.result)).toEqual(['r1', 'r2']);
});

test('a late result finds its call in an earlier run', () => {
  const v = run([
    { type: 'tool_call', tool: 'agent', args: { task: 't' } },
    { type: 'message', content: 'waiting' },
    { type: 'tool_result', tool: 'agent', result: 'ok' },
  ]);
  expect(v.items.map((i) => i.kind)).toEqual(['tools', 'assistant']);
  expect(states(v)).toEqual([['agent:done']]);
});

test('a result with no call is shown on its own; its call, when it comes, fills the args', () => {
  const v = run([{ type: 'tool_result', tool: 'exec', result: 'orphan' }]);
  expect(runs(v)[0]!.calls[0]).toMatchObject({ tool: 'exec', args: null, state: 'done', result: 'orphan' });
  const w = applySnapshot({
    items: [
      { kind: 'tool_result', tool: 'exec', call_id: 'z', content: 'early' },
      { kind: 'tool_call', tool: 'exec', call_id: 'z', args: { command: 'late' } },
    ],
    state: [],
  });
  expect(runs(w)).toHaveLength(1);
  expect(runs(w)[0]!.calls).toEqual([
    { id: 'z', tool: 'exec', args: { command: 'late' }, result: 'early', state: 'done' },
  ]);
});

test('a snapshot after a live call replaces it, not adds to it', () => {
  const v = run([{ type: 'tool_call', tool: 'exec', args: { command: 'ls' } }]);
  const w = apply(v, {
    type: 'snapshot',
    items: [
      { kind: 'tool_call', tool: 'exec', call_id: '1', args: { command: 'ls' } },
      { kind: 'tool_result', tool: 'exec', call_id: '1', content: 'a' },
    ],
    state: [],
  });
  expect(states(w)).toEqual([['exec:done']]);
});

test('ask_user stays a question and its answer', () => {
  const v = run([
    { type: 'tool_call', tool: 'ask_user', args: { question: 'q?' } },
    { type: 'tool_result', tool: 'ask_user', result: 'yes' },
  ]);
  expect(v.items.map((i: Item) => i.kind)).toEqual(['question', 'user']);
});

test('failed reads exec exit markers and ERROR results (toolrun.go toolFailed)', () => {
  expect(failed('exec', 'ok\n[exit: 2]')).toBe(true);
  expect(failed('exec', 'printed [exit: 2] mid-line\nok')).toBe(false);
  expect(failed('file_edit', 'ERROR: "x" does not appear')).toBe(true);
  expect(failed('mcp__x', 'fine')).toBe(false);
});

test('the TUI words: labels, call lines, previews, summary', () => {
  expect(toolLabel('notes__project_notes')).toBe('notes · project notes');
  expect(toolLabel('node_query')).toBe('node query');
  expect(callText('exec', { command: 'go test ./...', timeout: 60 })).toBe('$ go test ./... (timeout=60)');
  expect(callText('file_read', { path: '.', grep: 'needle' })).toBe('grep /needle/');
  expect(callText('mcp__fetch', { url: 'https://x' })).toBe('mcp · fetch  url=https://x');
  const done = (tool: string, args: Record<string, unknown>, result: string) =>
    preview({ tool, args, result, state: failed(tool, result) ? 'error' : 'done' }).text;
  // Real agentkit/files output (as internal/tui/filerender_test.go makes it).
  expect(done('file_read', { path: 'internal/a.go' }, 'internal/a.go: lines 2-4 of 5\n2\t\n3\tfunc A() int {')).toBe(
    ':2-4 (3 lines of 5)',
  );
  expect(done('file_edit', { path: 'a.go' }, 'edited internal/a.go: +2 -1\n@@ -2,4 +2,5 @@\n-x\n+y\n+z')).toBe('+2 −1');
  expect(done('file_write', { path: 'n.md' }, 'wrote docs/new.md (3 lines, new)')).toBe('(3 lines, new)');
  expect(done('file_read', { path: '.', grep: 'needle' }, '1 match in 1 file\ninternal/b.go:2:// needle')).toBe(
    '1 match in 1 file',
  );
  expect(done('exec', { command: 'make' }, 'a.go:3: unused x\nFAIL\n[exit: 1]')).toBe('✗ 1 · a.go:3: unused x');
  expect(done('exec', { command: 'ls' }, 'a\nb')).toBe('✓ 2 lines · a');
  expect(preview({ tool: 'exec', args: {}, state: 'pending' }).text).toBe('running…');
  expect(
    runSummary([
      { tool: 'exec', args: {}, state: 'done' },
      { tool: 'exec', args: {}, state: 'error' },
      { tool: 'file_read', args: {}, state: 'pending' },
    ]),
  ).toEqual({ head: '⚙ 3 steps — exec ×2, file read', thought: '', failed: ' · 1 failed', running: ' · running…' });
});

test('the renderer registry: the TUI tools, a grep read, and the generic fallback', () => {
  expect(bodyFor({ tool: 'exec', args: {} })).toBe(ShellBody);
  expect(bodyFor({ tool: 'file_edit', args: {} })).toBe(DiffBody);
  expect(bodyFor({ tool: 'node_edit', args: {} })).toBe(DiffBody);
  expect(bodyFor({ tool: 'file_read', args: { path: '.', grep: 'x' } })).toBe(SearchBody);
  expect(bodyFor({ tool: 'notes__project_notes', args: {} })).toBe(GenericBody);
  expect(bodyFor({ tool: 'never_heard_of_it', args: null })).toBe(GenericBody);
  expect(Object.keys(bodies)).toEqual(expect.arrayContaining(['exec', 'file_read', 'file_write', 'search']));
});

describe('thinking inside a run (toolrun.go: thoughts are steps of the open run)', () => {
  const call = (n: string) => [
    { type: 'tool_call', tool: 'exec', args: { command: n } },
    { type: 'tool_result', tool: 'exec', result: n },
  ];
  const think = (text: string, a: string, b: string) => [
    { type: 'reasoning', text, ts: a },
    { type: 'tool_call', tool: 'read', args: { path: text }, ts: b },
    { type: 'tool_result', tool: 'read', result: 'x' },
  ];
  const evs = [...call('a'), { type: 'reasoning', text: '# Thinking: next', ts: '2026-11-14T10:00:00Z' }, ...call('b')];

  test('call, thinking, call is one run holding both calls and the thought, in order', () => {
    const v = run(evs);
    expect(v.items.map((i) => i.kind)).toEqual(['tools']);
    const r = runs(v)[0]!;
    expect(r.calls.map((c) => c.args?.command)).toEqual(['a', 'b']);
    expect(r.thoughts).toHaveLength(1);
    expect(r.thoughts![0]).toMatchObject({ at: 1, live: false, text: '# Thinking: next' });
  });

  test('a live thought grows inside the run and the next call closes it', () => {
    let v = run([...call('a'), { type: 'reasoning', text: 'ab', ts: '2026-11-14T10:00:00Z' }]);
    v = run([{ type: 'reasoning', text: 'cd' }], v);
    expect(v.items).toHaveLength(1);
    expect(runs(v)[0]!.thoughts![0]).toMatchObject({ text: 'abcd', live: true });
    v = run([{ type: 'tool_call', tool: 'exec', args: {}, ts: '2026-11-14T10:00:04Z' }], v);
    expect(runs(v)[0]!.thoughts![0]).toMatchObject({ live: false, end: Date.parse('2026-11-14T10:00:04Z') });
  });

  test('a thought before the first call is taken into the run, as the TUI does', () => {
    const v = run([{ type: 'reasoning', text: 'first' }, ...call('a')]);
    expect(v.items.map((i) => i.kind)).toEqual(['tools']);
    expect(runs(v)[0]!.thoughts![0]).toMatchObject({ at: 0, text: 'first' });
  });

  test('thinking then a reply: the run ends and the next call starts a new run', () => {
    const v = run([...call('a'), { type: 'reasoning', text: 't' }, { type: 'token', text: 'so' }, ...call('b')]);
    expect(v.items.map((i) => i.kind)).toEqual(['tools', 'assistant', 'tools']);
    expect(runs(v)[0]!.thoughts).toHaveLength(1);
    expect(runs(v)[1]!.thoughts).toBeUndefined();
  });

  test('a user message or a note ends the run too', () => {
    const v = run([...call('a'), { type: 'user', content: 'hm' }, { type: 'reasoning', text: 't' }, ...call('b')]);
    expect(v.items.map((i) => i.kind)).toEqual(['tools', 'user', 'tools']);
  });

  test('history replay gives the same run as live', () => {
    const live = run(evs);
    const hist = applySnapshot({
      type: 'snapshot',
      items: [
        { kind: 'tool_call', tool: 'exec', args: { command: 'a' }, call_id: '1' },
        { kind: 'tool_result', tool: 'exec', content: 'a', call_id: '1' },
        { kind: 'reasoning', content: '# Thinking: next' },
        { kind: 'tool_call', tool: 'exec', args: { command: 'b' }, call_id: '2' },
        { kind: 'tool_result', tool: 'exec', content: 'b', call_id: '2' },
      ],
    });
    expect(hist.items.map((i) => i.kind)).toEqual(['tools']);
    const [a, b] = [runs(live)[0]!, runs(hist)[0]!];
    expect(b.calls.map((c) => [c.args, c.result, c.state])).toEqual(a.calls.map((c) => [c.args, c.result, c.state]));
    expect(b.thoughts!.map((t) => [t.at, t.text])).toEqual(a.thoughts!.map((t) => [t.at, t.text]));
  });

  test('the summary counts thoughts as steps and adds the time thought (toolrun.go summary)', () => {
    const calls = [
      { tool: 'exec', args: {}, state: 'done' as const },
      { tool: 'exec', args: {}, state: 'done' as const },
    ];
    expect(runSummary(calls, [{ start: 1000, end: 13000, live: false }])).toMatchObject({
      head: '⚙ 3 steps — exec ×2',
      thought: ' · thought 12s',
    });
    expect(runSummary(calls, [{ start: 0, end: 0, live: false }]).thought).toBe('');
    expect(run(think('t', '2026-11-14T10:00:00Z', '2026-11-14T10:00:05Z')).items).toHaveLength(1);
  });
});
