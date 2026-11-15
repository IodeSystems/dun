/// <reference types="jest" />
// Sub-agents open from every place that names one: an agent tool call, a
// notice, a plan node, the header badge's sheet. All of them through the
// one opener (components/agents/AgentOpen), with the engine's `agent` field.
import { act, type ReactElement } from 'react';
import { createRoot, type TestInstance } from 'test-renderer';

import { SafeAreaProvider } from 'react-native-safe-area-context';

import { AgentOpenProvider, type AgentOpen } from '../src/components/agents/AgentOpen';
import { NoticeRow } from '../src/components/notices/NoticeRow';
import { PlanSheet } from '../src/components/plan/PlanSheet';
import { ToolCallRow } from '../src/components/tools/ToolRun';
import { badge, type Agent } from '../src/lib/activity';
import { agentCallTitle } from '../src/lib/agentcall';
import { apply, empty, replaceWith, type View } from '../src/lib/events';
import type { ToolCall } from '../src/lib/toolcalls';

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const roots: ReturnType<typeof createRoot>[] = [];
// Unmounted inside the test: a Sheet's Modal left mounted runs after Jest's teardown.
afterEach(async () => {
  await act(() => {
    roots.splice(0).forEach((r) => r.unmount());
    return Promise.resolve();
  });
});

async function render(el: ReactElement) {
  const root = createRoot();
  roots.push(root);
  await act(() => {
    root.render(el);
    return Promise.resolve();
  });
  return root.container;
}

function text(n: TestInstance | string): string {
  return typeof n === 'string' ? n : n.children.map(text).join('');
}
const texts = (root: TestInstance) =>
  root
    .queryAll((n) => n.type === 'Text', { matchDeepestOnly: false })
    .filter((n) => n.parent?.type !== 'Text')
    .map(text);

async function pressWhere(root: TestInstance, pick: (n: TestInstance) => boolean, what: string) {
  const btn = root.queryAll((n) => n.props.accessibilityRole === 'button' && pick(n))[0];
  if (!btn) throw new Error(`no button ${what} in ${JSON.stringify(texts(root))}`);
  type F = { memoizedProps?: { onPress?: () => void }; return: F | null } | null;
  let f = btn.unstable_fiber as F;
  while (f && typeof f.memoizedProps?.onPress !== 'function') f = f.return;
  await act(() => {
    f!.memoizedProps!.onPress!();
    return Promise.resolve();
  });
}
const pressOpen = (root: TestInstance, n: number) =>
  pressWhere(root, (b) => b.props.accessibilityLabel === `Open agent #${n}`, `Open agent #${n}`);

const kid: Agent = { id: 2, model: 'kid', state: 'idle', status: 'reading', prompt: 'p', tokens: 0, seconds: 0, at: 0 };

function provider(opened: number[]): AgentOpen {
  return { agents: [kid], nodes: { '1': 'build' }, open: (n) => opened.push(n) };
}

const spawn: ToolCall = {
  id: 'c1',
  tool: 'agent',
  args: { node: '1' },
  result: 'Started agent #2.',
  state: 'done',
  agent: 2,
};

test('an agent call: its title and state from structure, Open opens it, the press still folds', async () => {
  const opened: number[] = [];
  const root = await render(
    <AgentOpenProvider value={provider(opened)}>
      <ToolCallRow call={spawn} />
    </AgentOpenProvider>,
  );
  expect(texts(root)).toEqual(['▸ ✓ ', 'start agent #2 on node 1 build', '  → idle', 'Open ›']);
  await pressOpen(root, 2);
  expect(opened).toEqual([2]);
  await pressWhere(root, (b) => text(b).includes('start agent #2'), 'the row');
  expect(texts(root)).toContain('agent #2 · kid · idle');
  expect(texts(root)).toContain('status: reading');
  await pressOpen(root, 2);
  expect(opened).toEqual([2, 2]);
});

test('an agent call with no child named (a refused spawn, an old log) has no Open', async () => {
  const root = await render(
    <AgentOpenProvider value={provider([])}>
      <ToolCallRow call={{ tool: 'agent', args: { node: '9' }, result: 'ERROR: no plan node 9', state: 'error' }} />
    </AgentOpenProvider>,
  );
  expect(root.queryAll((n) => n.props.testID === 'open-agent')).toHaveLength(0);
});

test('agentCallTitle says each op once', () => {
  expect(agentCallTitle({ args: { node: '1' } })).toBe('start agent on node 1');
  expect(agentCallTitle({ args: { id: 2, tell: 'go\non' }, agent: 2 })).toBe('tell agent #2: go on');
  expect(agentCallTitle({ args: { id: 2, dismiss: true }, agent: 2 })).toBe('dismiss agent #2');
  expect(agentCallTitle({ args: { id: 2, node: '3' }, agent: 2 }, { '3': 'test' })).toBe('give agent #2 node 3 test');
  expect(agentCallTitle({ args: { id: 2 }, agent: 2 })).toBe('agent #2 report');
});

test('a notice from an agent opens it; one from no agent has no link', async () => {
  const opened: number[] = [];
  const msg = {
    type: 'msg' as const,
    from: 'agent #3',
    to: 'you',
    text: 'found it',
    carried: false,
    done: true,
    failed: false,
    status: '',
  };
  const root = await render(
    <AgentOpenProvider value={provider(opened)}>
      <NoticeRow notice={msg} agent={3} />
      <NoticeRow notice={{ type: 'plain', tone: 'dim', icon: '⏱', text: 'budget' }} />
    </AgentOpenProvider>,
  );
  expect(root.queryAll((n) => n.props.testID === 'open-agent')).toHaveLength(1);
  await pressOpen(root, 3);
  expect(opened).toEqual([3]);
});

test('a plan node an agent owns opens that agent', async () => {
  const opened: number[] = [];
  const row = (id: string, owner: string) => ({
    id,
    title: 'n' + id,
    details: '',
    status: 'doing',
    owner,
    why: '',
    waiting: [],
    depth: 0,
    repeat: false,
  });
  const root = await render(
    <SafeAreaProvider
      initialMetrics={{
        frame: { x: 0, y: 0, width: 390, height: 800 },
        insets: { top: 0, left: 0, right: 0, bottom: 0 },
      }}
    >
      <AgentOpenProvider value={provider(opened)}>
        <PlanSheet
          plan={{ mode: false, approved: true, total: 2, done: 0, rows: [row('1', 'agent:4'), row('2', '')] as never }}
          live={{ busy: false, asking: false, agents: { 4: 'running' } }}
          run={() => undefined}
          onClose={() => undefined}
        />
      </AgentOpenProvider>
    </SafeAreaProvider>,
  );
  expect(root.queryAll((n) => n.props.testID === 'open-agent')).toHaveLength(1);
  await pressOpen(root, 4);
  expect(opened).toEqual([4]);
});

test('the badge says what the sheet lists', () => {
  const job = { id: 1, command: 'x', state: 'running', bytes: 0, started: 0, ended: 0, code: 0, muted: false };
  expect(badge([kid, { ...kid, id: 3 }], [])?.label).toBe('2 agents');
  expect(badge([kid], [job])?.label).toBe('1 agent · 1 job');
  expect(badge([], [job, { ...job, id: 2 }])?.label).toBe('2 jobs');
  expect(badge([], [])).toBeNull();
});

const callsOf = (v: View) => v.items.flatMap((i) => (i.kind === 'tools' ? i.calls : []));

test('the engine’s agent field lands on the call and the notice, live and replayed', () => {
  const live = [
    { type: 'tool_call', call_id: 'c1', tool: 'agent', args: { node: '1' } },
    { type: 'tool_result', call_id: 'c1', tool: 'agent', result: 'Started agent #2.', agent: 2 },
    { type: 'tool_call', call_id: 'c2', tool: 'agent', args: { id: 2, tell: 'go' }, agent: 2 },
    { type: 'tool_result', call_id: 'c2', tool: 'agent', result: 'Told agent #2.', agent: 2 },
    { type: 'notification', text: 'agent #2: done', agent: 2 },
  ].reduce(apply, empty);
  expect(callsOf(live).map((c) => c.agent)).toEqual([2, 2]);
  expect(live.items.find((i) => i.kind === 'notice')).toMatchObject({ agent: 2 });

  const replay = replaceWith([
    { kind: 'tool_call', call_id: 'c1', tool: 'agent', args: { node: '1' }, agent: 2 },
    { kind: 'tool_result', call_id: 'c1', tool: 'agent', content: 'Started agent #2.', agent: 2 },
    { kind: 'notification', content: 'agent #2: done', agent: 2 },
    { kind: 'notification', content: 'plan: 0 of 1 done' },
  ]);
  expect(callsOf(replay).map((c) => c.agent)).toEqual([2]);
  const ns = replay.items.filter((i) => i.kind === 'notice');
  expect(ns.map((n) => (n as { agent?: number }).agent)).toEqual([2, undefined]);
});
