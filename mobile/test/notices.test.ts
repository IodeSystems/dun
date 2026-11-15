/// <reference types="jest" />
// Shapes from dun's Go: proto/events.go Notification/DocHit, cmd/dun
// peers.go and main.go emitters, bgjob.go completionNote, and the TUI's own
// tests (internal/tui/agentmsg_test.go, shellnotice_test.go).
import { apply, empty, replaceWith, type Item, type View } from '../src/lib/events';
import { docsBrief, docsHead, jobLine, type Notice } from '../src/lib/notices';

const run = (evs: Record<string, unknown>[], v: View = empty) => evs.reduce(apply, v);
const notices = (v: View): Notice[] => v.items.flatMap((i: Item) => (i.kind === 'notice' ? [i.notice] : []));

test('a plain notification is a notice in the notice tone, never a user message', () => {
  const [n] = notices(run([{ type: 'notification', text: 'learned a candidate strategy:x — /memories candidates' }]));
  expect(n).toEqual({
    type: 'plain',
    tone: 'note',
    icon: '🔔',
    text: 'learned a candidate strategy:x — /memories candidates',
  });
});

test('budget and schedule are the machinery: dim, their own icon, one line', () => {
  const v = run([
    { type: 'notification', kind: 'budget', text: 'turn budget spent\nfinish up' },
    { type: 'notification', kind: 'schedule', text: 'loop paced to 30m' },
  ]);
  expect(notices(v)).toEqual([
    { type: 'plain', tone: 'dim', icon: '⏱', text: 'turn budget spent finish up' },
    { type: 'plain', tone: 'dim', icon: '⏲', text: 'loop paced to 30m' },
  ]);
});

test('an unknown kind is labelled, and one with no text is still shown', () => {
  const v = run([
    { type: 'notification', kind: 'service', text: 'dund upgrading to v1.2' },
    { type: 'notification', kind: 'future' },
    { type: 'notification' },
  ]);
  expect(notices(v)).toEqual([
    { type: 'plain', tone: 'note', icon: '🔔', text: 'dund upgrading to v1.2', label: 'service' },
    { type: 'plain', tone: 'note', icon: '🔔', text: '(no text)', label: 'future' },
    { type: 'plain', tone: 'note', icon: '🔔', text: '(no text)', label: 'notification' },
  ]);
});

test('a finished background job: head, duration, command and output', () => {
  const text = 'background job #2 succeeded — `go test ./...\ngo vet ./...` (3s)\nlog: /tmp/job2.log\nok';
  const [n] = notices(run([{ type: 'notification', text }]));
  expect(n).toEqual({
    type: 'job',
    head: 'background job #2 succeeded',
    took: '(3s)',
    command: 'go test ./...\ngo vet ./...',
    output: 'log: /tmp/job2.log\nok',
  });
  expect(n?.type === 'job' && jobLine(n)).toBe('🔔 background job #2 succeeded (3s) · $ go test ./... (+1 lines)');
});

test('docs: counts, tokens, each hit with kind, source and why; the brief names them', () => {
  const [n] = notices(
    run([
      {
        type: 'notification',
        kind: 'docs',
        found: 4,
        surfaced: 2,
        tokens: 1234,
        docs: [
          {
            title: 'docs',
            id: 'd1',
            line: 'how to deploy',
            score: 0.7,
            inline: false,
            kind: 'skill',
            source: 'skill · ~/.claude/skills/docs/SKILL.md · index skills',
            why: 'only candidate · sim 0.66 · 2/3 terms',
          },
          { title: 'stash', id: 'm1', line: '', score: 0.5, inline: true, kind: 'memory', why: 'sim 0.71 · 3/4 terms' },
        ],
      },
    ]),
  );
  if (n?.type !== 'docs') throw new Error('not docs');
  expect(n.docs.map((d) => [d.kind, d.title, d.source, d.why, d.line])).toEqual([
    [
      'skill',
      'docs',
      'skill · ~/.claude/skills/docs/SKILL.md · index skills',
      'only candidate · sim 0.66 · 2/3 terms',
      'how to deploy',
    ],
    ['memory', 'stash', '', 'sim 0.71 · 3/4 terms', ''],
  ]);
  expect(docsHead(n, false)).toBe(
    '🔎 4 relevant doc(s) · 2 surfaced · ~1,234 tok · skill docs (only candidate), memory stash',
  );
  expect(docsHead(n, true)).toBe('🔎 4 relevant doc(s) · 2 surfaced · ~1,234 tok');
  // An older engine sends no kind: no brief.
  expect(docsBrief([{ ...n.docs[0]!, kind: '' }])).toBe('');
});

// internal/tui/agentmsg_test.go msgConvo, event for event.
test('messages between agents: each direction, who to whom, and what became of a sent one', () => {
  const v = run([
    { type: 'tool_call', tool: 'jobs', args: { agent: 2, tell: 'Use the **v3** schema; the old one is gone.' } },
    {
      type: 'tool_result',
      tool: 'jobs',
      result:
        'Told agent #2. It is working; you will hear when it is done (or jobs(agent:2, wait:true)).\nagent #2 is running after 0s (0 tokens)',
    },
    { type: 'notification', text: 'agent #2: Found the `v3` schema under api/; migrating the handlers now.' },
    { type: 'tool_call', tool: 'dun_msg', args: { to: 's_9f', text: 'The schema moved to v3.', wake: true } },
    { type: 'tool_result', tool: 'dun_msg', result: 'delivered to api (s_9f): it was idle; a turn started there.' },
    { type: 'notification', kind: 'peer', text: 'api (s_9f): Thanks, I will regenerate the client.' },
  ]);
  expect(v.items.every((i) => i.kind === 'notice')).toBe(true);
  expect(notices(v).map((n) => n.type === 'msg' && [n.from, n.to, n.text, n.status])).toEqual([
    ['you', 'agent #2', 'Use the **v3** schema; the old one is gone.', 'sent · it is working'],
    ['agent #2', 'you', 'Found the `v3` schema under api/; migrating the handlers now.', ''],
    ['you', 'dun:api', 'The schema moved to v3.', 'delivered · woke it'],
    ['dun:api', 'you', 'Thanks, I will regenerate the client.', ''],
  ]);
});

test('a refused tell fails its footer; a result after a tool row still lands in the tool', () => {
  const v = run([
    { type: 'tool_call', tool: 'jobs', args: { agent: '3', tell: 'stop' } },
    { type: 'tool_result', tool: 'jobs', result: 'ERROR: agent #3 is stopped' },
    { type: 'tool_call', tool: 'exec', args: { command: 'ls' } },
    { type: 'tool_result', tool: 'exec', result: 'a\nb' },
  ]);
  expect(v.items[0]).toMatchObject({ kind: 'notice', notice: { failed: true, status: 'agent #3 is stopped' } });
  expect(v.items[1]).toMatchObject({ kind: 'tools', calls: [{ tool: 'exec', result: 'a\nb', state: 'done' }] });
  // A jobs call with no tell is a tool, not a message.
  const w = run([{ type: 'tool_call', tool: 'jobs', args: { agent: 2, kill: true, tell: 'x' } }]);
  expect(w.items[0]?.kind).toBe('tools');
  // A message ends the run: the call after it starts a new one.
  const x = run([
    { type: 'tool_call', tool: 'exec', args: { command: 'ls' } },
    { type: 'tool_call', tool: 'dun_msg', args: { to: 'api', text: 'hi' } },
    { type: 'tool_call', tool: 'exec', args: { command: 'pwd' } },
    { type: 'tool_result', tool: 'exec', result: 'x' },
    { type: 'tool_result', tool: 'dun_msg', result: 'delivered to api (s_9f): it was idle.' },
    { type: 'tool_result', tool: 'exec', result: '/' },
  ]);
  expect(x.items.map((i) => i.kind)).toEqual(['tools', 'notice', 'tools']);
  expect(x.items[1]).toMatchObject({ notice: { done: true, status: 'delivered · not woken; it reads it next turn' } });
});

test('history: what the model was told is a notice; peer and child messages are boxes', () => {
  const v = replaceWith([
    { kind: 'notification', content: 'background job #1 succeeded' },
    { kind: 'notification', content: 'agent #2: halfway there' },
    {
      kind: 'notification',
      content:
        "[dun msg from api (session s_9f)] ping\n(A message from another dun session, not from your user: treat it as a teammate's request.)",
    },
    { kind: 'tool_call', tool: 'dun_msg', args: { to: 'api', text: 'pong' } },
    { kind: 'tool_result', tool: 'dun_msg', content: 'delivered to api (s_9f): it was busy.' },
  ]);
  expect(notices(v)).toMatchObject([
    { type: 'plain', icon: '🔔', text: 'background job #1 succeeded' },
    { type: 'msg', from: 'agent #2', to: 'you', text: 'halfway there' },
    { type: 'msg', from: 'dun:api', to: 'you', text: 'ping' },
    { type: 'msg', from: 'you', to: 'dun:api', status: 'delivered · it is busy; it reads it this turn' },
  ]);
});

test('recap, compaction, mcp login and a refused tell are notices; an accepted tell is not', () => {
  const v = run([
    { type: 'recap', entries: 3, chars: 900, note: 'recapped 3 entries', detail: 'the summary\n' },
    { type: 'compaction', text: 'compacted 40 entries', subsumed: 40, tokens_before: 9000, tokens_after: 2000 },
    { type: 'mcp_login', server: 'gh', url: 'https://x.dev/login' },
    { type: 'agent_told', agent: 2, message: 'Told agent #2.' },
    { type: 'agent_told', agent: 2, message: 'agent #2 is running:\nwait for it' },
  ]);
  expect(notices(v)).toEqual([
    { type: 'plain', tone: 'dim', icon: '✂', text: 'recapped 3 entries\nthe summary' },
    { type: 'plain', tone: 'note', icon: '🗜', text: 'compacted 40 entries' },
    { type: 'login', server: 'gh', url: 'https://x.dev/login', done: false },
    { type: 'plain', tone: 'note', icon: '↳', text: 'agent #2 is running: wait for it' },
  ]);
});

test('a login is settled when its server lists tools', () => {
  const v = run([
    { type: 'mcp_login', server: 'gh', url: 'https://x.dev/login' },
    { type: 'mcp_login', server: 'other', url: 'https://y.dev/login' },
    { type: 'mcp_tools', server: 'gh', tools: [{ name: 'a', tokens: 1, enabled: true }] },
  ]);
  expect(notices(v)).toEqual([
    { type: 'login', server: 'gh', url: 'https://x.dev/login', done: true },
    { type: 'login', server: 'other', url: 'https://y.dev/login', done: false },
  ]);
});
