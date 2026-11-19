/// <reference types="jest" />
import { askAge, askClosed, askFoldLine, intakeLine, motion, parsePlan } from '../src/lib/plan';

// The cases are internal/tui/askfold_test.go's: the plan as the engine sends
// it (proto.Plan), with the intake and each ask node's ask.
const now = Date.parse('2026-11-18T12:00:00Z');
const at = now / 1000 - 300;
const plan = parsePlan({
  total: 4,
  done: 2,
  intake: {
    messages: [
      { at, text: 'the tools and stubs dir is not what I expect\nmore' },
      { at, text: 'continue' },
      { at, text: 'rety' },
    ],
  },
  rows: [
    { id: '73', title: 'ask: fix the stubs dir', status: 'doing', ask: { n: 1, state: 'open', at, working: true } },
    { id: '75', title: 'ask: vehicle telemetry', status: 'doing', ask: { n: 2, state: 'open', at } },
    { id: '76', title: 'ask: add plan nodes', status: 'done', ask: { n: 3, state: 'done', at } },
    { id: '80', title: 'ask: locate tow yards', status: 'done', ask: { n: 4, state: 'filed', at, filed: ['90'] } },
  ],
});
const live = { busy: true, asking: false, agents: {}, phase: 0 };

test('the intake is one row', () => {
  expect(intakeLine(plan.intake!)).toBe('▸ new · 3 messages · "the tools and stubs dir is not…" +2');
  expect(parsePlan({ rows: [] }).intake).toBeUndefined();
});

test('only the worked ask shows working; the others open with their age', () => {
  const tags = plan.rows.filter((r) => !askClosed(r)).map((r) => motion(r, live, now).tag);
  expect(tags).toEqual(['working', 'open · 5m']);
  expect(motion(plan.rows[0]!, { ...live, busy: false }, now).tag).toBe('open · 5m');
});

test('done and filed asks fold into one row', () => {
  const closed = plan.rows.filter(askClosed);
  expect(closed.map((r) => r.id)).toEqual(['76', '80']);
  expect(askFoldLine(closed)).toBe('▸ 1 done · 1 filed');
  expect(plan.rows[3]!.ask!.filed).toEqual(['90']);
});

test('ages', () => {
  expect(askAge(now, now / 1000)).toBe('now');
  expect(askAge(now, now / 1000 - 3 * 3600)).toBe('3h');
  expect(askAge(now, now / 1000 - 3 * 86400)).toBe('3d');
});
