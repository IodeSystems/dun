/// <reference types="jest" />
import { askAge, askClosed, askFoldLine, askTag, intakeLine, parsePlan } from '../src/lib/plan';

// The cases are internal/tui/askfold_test.go's: the plan event (proto.Plan)
// with the asks ledger beside the rows.
const now = Date.parse('2026-11-18T12:00:00Z');
const at = now / 1000 - 300;
const plan = parsePlan({
  total: 1,
  done: 0,
  intake: {
    messages: [
      { at, text: 'the tools and stubs dir is not what I expect\nmore' },
      { at, text: 'continue' },
      { at, text: 'rety' },
    ],
  },
  asks: [
    { n: 1, title: 'fix the stubs dir', state: 'open', at, working: true },
    { n: 2, title: 'vehicle telemetry', state: 'open', at },
    { n: 3, title: 'add plan nodes', state: 'done', at },
    { n: 5, title: 'locate tow yards', state: 'filed', at, nodes: ['1'] },
    { n: 6, title: 'deploy it', state: 'blocked', why: 'which region?', at },
  ],
  rows: [{ id: '1', title: 'write the tow yard list', status: 'todo', ask: 5 }],
});

test('the intake is one row', () => {
  expect(intakeLine(plan.intake!)).toBe('▸ new · 3 messages · "the tools and stubs dir is not…" +2');
  expect(parsePlan({ rows: [] }).intake).toBeUndefined();
});

test('only the worked ask shows working; the others open with their age', () => {
  const open = plan.asks!.filter((a) => !askClosed(a));
  expect(open.map((a) => askTag(a, true, now))).toEqual(['working', 'open · 5m', 'blocked: which region?']);
  expect(askTag(open[0]!, false, now)).toBe('open · 5m');
});

test('done and filed asks fold into one row; a node names its ask', () => {
  const closed = plan.asks!.filter(askClosed);
  expect(closed.map((a) => a.n)).toEqual([3, 5]);
  expect(askFoldLine(closed)).toBe('▸ 1 done · 1 filed');
  expect(plan.rows[0]!.ask).toBe(5);
});

test('ages', () => {
  expect(askAge(now, now / 1000)).toBe('now');
  expect(askAge(now, now / 1000 - 3 * 3600)).toBe('3h');
  expect(askAge(now, now / 1000 - 3 * 86400)).toBe('3d');
});
