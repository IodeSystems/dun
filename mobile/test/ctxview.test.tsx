/// <reference types="jest" />
import { act } from 'react';
import { createRoot, type TestInstance } from 'test-renderer';

import { ContextSection } from '../src/components/header/ContextSection';
import { contextBlocks, noCtxView, noteCtxView } from '../src/lib/ctxview';

const fold = (evs: Record<string, unknown>[]) => evs.reduce(noteCtxView, noCtxView);
const rows = (evs: Record<string, unknown>[], blocks = 0) =>
  Object.fromEntries(contextBlocks(fold(evs), blocks).map((b) => [b.title, b.rows]));

test('window room, ratio and cuts come from usage / context_cost', () => {
  const r = rows([
    {
      type: 'context_cost',
      window: 32000,
      window_reserved: 4000,
      prompt_tokens: 30000,
      chars_per_token: 3.456,
      ratio_measured: true,
      ratio_rounds: 7,
      window_cuts: 2,
      window_folds: 1,
    },
  ]);
  expect(r['window room']).toEqual([
    { label: 'room now', value: '2,000', note: '← below the reservation' },
    { label: 'ratio', value: '3.46 chars/token', note: '(measured over 7 rounds)' },
    { label: 'cut short', value: '2 (1 folded)', note: 'replies stopped for room' },
  ]);
  expect(rows([{ type: 'usage' }])['window room']).toBeUndefined();
});

test('shaping, tool results, rules and queued messages are counted from their events', () => {
  const r = rows(
    [
      { type: 'ready', tools: ['a', 'b', 'c'] },
      { type: 'recap', entries: 3, chars: 900 },
      { type: 'compaction', tokens_before: 9000, tokens_after: 2000 },
      { type: 'tool_result', content: 'ok' },
      { type: 'tool_result', content: 'x [12 characters elided] y' },
      { type: 'user', queued: true, content: 'hi' },
      { type: 'user', content: 'plain' },
      { type: 'usage', system_tokens: 10, system_rules: 400, system_rules_mode: 'auto', forced_calls: 2 },
    ],
    12,
  );
  expect(r.conversation).toEqual([
    { label: 'blocks', value: '12' },
    { label: 'tools', value: '3' },
  ]);
  expect(r['context shaping']).toEqual([
    { label: 'compactions', value: '1', note: 'saved 7,000 tokens' },
    { label: 'recaps', value: '1', note: '3 entries, 900 chars → disk' },
  ]);
  expect(r['tool results']).toEqual([
    { label: 'total', value: '2' },
    { label: 'truncated', value: '1', note: '(LOD-capped at 4000 chars)' },
  ]);
  expect(r['project rules']).toEqual([{ label: 'rules', value: '400', note: 'tokens (/rules auto)' }]);
  expect(r['out-of-band']).toEqual([
    { label: 'queued msgs', value: '1', note: '(delivered mid-turn)' },
    { label: 'forced calls', value: '2', note: '(host-injected)' },
  ]);
});

const text = (n: TestInstance | string): string => (typeof n === 'string' ? n : n.children.map(text).join(''));

test('the section renders a title and its rows', () => {
  const root = createRoot();
  act(() => {
    root.render(<ContextSection ctxv={fold([{ type: 'recap', entries: 1, chars: 2 }])} blocks={5} />);
  });
  const t = text(root.container);
  expect(t).toContain('conversation');
  expect(t).toContain('recaps');
  expect(t).toContain('1 entries, 2 chars → disk');
});
