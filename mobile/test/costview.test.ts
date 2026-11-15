/// <reference types="jest" />
import { costBlock, costBlocks, ledgerBlocks, money, parseSpend, until } from '../src/lib/costview';

// The cases are internal/tui/spend_test.go's; the spend as the engine sends
// it (proto.Spend).
const now = Date.parse('2026-10-22T12:00:00Z');
const full = {
  currency: 'USD',
  session_cost: 1234.5,
  calls: 14,
  costed_calls: 12,
  last_turn_cost: 0.0042,
  last_turn_calls: 3,
  last_turn_costed: 3,
  key: {
    spend: 45.5,
    limit: 50,
    remaining: 4.5,
    reset_at: '2026-11-01T00:00:00Z',
    interval: '30d',
    currency: 'USD',
    source: 'litellm',
    as_of: '2026-10-22T11:58:00Z',
  },
  limits: [
    { name: 'requests', limit: 60, remaining: 59, reset_at: '2026-10-22T12:00:01Z' },
    { name: 'tokens', limit: 150000, remaining: 149984, reset_at: '2026-10-22T11:59:00Z' },
  ],
};

const flat = (b: ReturnType<typeof costBlock>) =>
  b.rows.map((r) => `${r.label.trim()} ${r.value} ${r.note ?? ''}`.trim());

test('cost & limits: session, last turn, key budget, rate limits (TestCostBlockAlignedAndGrouped)', () => {
  const b = costBlock(parseSpend(full), now);
  expect(b.title).toBe('cost & limits');
  expect(flat(b)).toEqual([
    'session spend 1,234.50 USD · 12 of 14 calls priced',
    'last turn 0.00 USD (0.0042)',
    'key spend 45.50 USD · this budget period · as of 2m ago',
    'key limit 50.00 USD',
    'key remaining 4.50 USD (9%)',
    'key resets in 9d 12h 2026-11-01 00:00 UTC · every 30d',
    'requests left 59 of 60 (98%) · resets in 1s',
    'tokens left 149,984 of 150,000 (100%) · reset 1m ago (stale)',
  ]);
  // Under a tenth left is a warning; a stale window is not.
  expect(b.rows.filter((r) => r.warn).map((r) => r.label)).toEqual(['key remaining']);
});

test('what the endpoint did not report reads "not reported", never 0 (TestCostBlockNotReported)', () => {
  for (const sp of [null, { calls: 4 }, { calls: 4, key_unsupported: true }, { key_error: 'status 500' }]) {
    const rows = costBlock(parseSpend(sp), now).rows;
    for (const label of ['session spend', 'last turn', 'rate limits']) {
      const r = rows.find((x) => x.label === label);
      expect(r?.value).toBe('—');
      expect(r?.note).toMatch(/^not reported/);
    }
  }
  const unsupported = costBlock(parseSpend({ key_unsupported: true }), now).rows;
  expect(unsupported.find((r) => r.label === 'key budget')?.note).toContain('the endpoint has none');
  const noLimit = costBlock(parseSpend({ key: { spend: 2, currency: 'USD' } }), now).rows;
  expect(noLimit.find((r) => r.label === 'key limit')).toMatchObject({ value: '—' });
  expect(noLimit.find((r) => r.label === 'key spend')?.note).toContain('all time (no reset stated)');
});

test('calls with no cost say why, by gateway (TestCostBlock_NoCostHint)', () => {
  const why = (src: string) =>
    costBlock(parseSpend({ calls: 3, key: { spend: 1, source: src } }), now).rows.find((r) => r.label === '  why')
      ?.note;
  expect(why('litellm')).toContain('include_cost_in_streaming_usage');
  expect(why('openrouter')).toContain('openrouter.ai');
  expect(why('')).toContain('no price table');
});

test('rollups and this machine (TestCostBlockRollups)', () => {
  const sp = parseSpend({
    ...full,
    session_hour: 0.25,
    session_today: 1.5,
    session_interval: 1234.5,
    interval: '30d',
    interval_of_key: true,
    ledger: {
      entries: true,
      hour: { USD: 1 },
      today: { USD: 2 },
      week: { USD: 3 },
      month: { USD: 4 },
      year: { USD: 10 },
      interval: { USD: 5 },
      total: { USD: 26.43, credits: 3 },
      by_model: { 'or/b': { USD: 1 }, 'or/a': { USD: 3 } },
    },
  });
  expect(flat(costBlock(sp, now)).slice(1, 4)).toEqual([
    'this hour 0.25 USD',
    'today 1.50 USD',
    'key period (30d) 1,234.50 USD',
  ]);
  const [machine, byModel] = ledgerBlocks(sp);
  expect(flat(machine!)).toEqual([
    'machine spend 26.43 USD + 3 credits · all time, every session on this machine',
    'this hour 1.00 USD',
    'today 2.00 USD',
    'this week 3.00 USD',
    'this month 4.00 USD',
    'this year 10.00 USD',
    'key period (30d) 5.00 USD',
  ]);
  expect(byModel!.rows.map((r) => r.label.trim())).toEqual(['or/a', 'or/b']);
  expect(costBlocks(sp, now).map((b) => b.title)).toEqual(['cost & limits', 'this machine', 'this month by model']);
  expect(ledgerBlocks(parseSpend({}))[0]!.rows[0]!.label).toBe('nothing spent yet');
});

test('money and until as the Go prints them', () => {
  expect([0, 0.004, 1.005, 1234.5, -0.5].map(money)).toEqual(['0.00', '0.00', '1.00', '1,234.50', '-0.50']);
  expect(until(now + 61_000, now)).toBe('in 1m');
  expect(until(now - 3 * 86400_000 - 3600_000, now)).toBe('3d 1h ago');
});
