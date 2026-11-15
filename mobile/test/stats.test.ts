/// <reference types="jest" />
import { fit, noStats, noteRequest, rate, statsLine, tokK, ttftText, type Stats } from '../src/lib/stats';

// The request events as the engine sends them (proto.Request, flat in the
// event frame), each with the session's figures (proto.RequestSession); the
// cases are internal/tui/reqbar_test.go's.
const stats = { ttft_ms: 800, pp: 1234, tg: 45.2, pp_avg: 1234, tg_avg: 45.2, cache_seen: true, cache_pct: 96 };
const reqDone = {
  v: 1,
  type: 'request',
  session: 's1',
  session_stats: stats,
  seq: 42,
  ts: '2026-11-14T10:00:00Z',
  phase: 'done',
  ttft_ms: 800,
  prompt: 5000,
  cached: 4800,
  cache_reported: true,
  pp: 1234,
  gen: 310,
  tg: 45.2,
  exact: true,
  ctx: 41300,
  window: 131072,
};

const fold = (evs: Record<string, unknown>[], s: Stats = noStats) => evs.reduce(noteRequest, s);

test('nothing before the first request', () => {
  expect(statsLine(noStats, 200)).toBe('');
  expect(fold([{ type: 'usage', total: 10 }]).seen).toBe(false);
});

test('the widths: pieces drop pp → ttft → cache → ctx, tg kept longest (TestReqBar_Widths)', () => {
  const s = fold([reqDone]);
  expect(statsLine(s, 200)).toBe('ctx 41.3k/131k · cache  96% · ttft  0.8s · pp  1.2k/s ~1.2k · tg   45/s   ~45');
  expect(statsLine(s, 56)).toBe('ctx 41.3k/131k · cache  96% · tg   45/s   ~45');
  expect(statsLine(s, 36)).toBe('ctx 41.3k/131k · tg   45/s   ~45');
  expect(statsLine(s, 20)).toBe('tg   45/s   ~45');
  expect(fit(s, 5)).toEqual([]);
});

test('the last event is the row: nothing is added up here (TestReqBar_LastEventIsTheBar)', () => {
  // A start's session keeps the last figures; the client shows them as sent.
  const start = {
    type: 'request',
    phase: 'start',
    ctx: 41700,
    window: 131072,
    session_stats: { ...stats, tg: 90, tg_avg: 36 },
  };
  const all = fold([reqDone, reqDone, start]);
  const last = fold([start]);
  expect(statsLine(last, 200)).toBe(statsLine(all, 200));
  expect(statsLine(last, 200)).toBe('ctx 41.7k/131k · cache  96% · ttft  0.8s · pp  1.2k/s ~1.2k · tg   90/s   ~36');
  // Widths do not move between events.
  const live = { ...start, phase: 'live', session_stats: { ...stats, ttft_ms: 12300, tg: 150 } };
  expect(statsLine(fold([live]), 200).length).toBe(statsLine(last, 200).length);
});

test('a cache never reported is not shown; one reported without a finished request is a dash', () => {
  const r = { ...reqDone, window: undefined, session_stats: { ...stats, cache_seen: undefined, cache_pct: undefined } };
  expect(statsLine(fold([r]), 200)).toBe('ctx 41.3k · ttft  0.8s · pp  1.2k/s ~1.2k · tg   45/s   ~45');
  const seen = { ...r, session_stats: { ...r.session_stats, cache_seen: true } };
  expect(statsLine(fold([seen]), 200)).toContain('cache    –');
  expect(statsLine(fold([{ ...seen, session_stats: { ...seen.session_stats, cache_pct: 0 } }]), 200)).toContain(
    'cache   0%',
  );
});

test("fallback, an engine from before session figures: the event's own, no averages", () => {
  const { session_stats: _, ...old } = reqDone;
  expect(statsLine(fold([old]), 200)).toBe(
    'ctx 41.3k/131k · cache    – · ttft  0.8s · pp  1.2k/s       · tg   45/s      ',
  );
});

test('numfmt: TokK, Rate and ttft as the Go prints them', () => {
  expect([940, 1000, 18_400, 99_950, 128_000, 131_072, 1_050_000, 2_000_000].map(tokK)).toEqual([
    '940',
    '1k',
    '18.4k',
    '100k',
    '128k',
    '131k',
    '1.05M',
    '2M',
  ]);
  expect(rate(45.9)).toBe('45/s');
  expect(rate(1234)).toBe('1.2k/s');
  // Go's %.1f takes an exact tie to the even digit: 1250 is 1.2k, not 1.3k.
  expect(tokK(1250)).toBe('1.2k');
  expect(tokK(1350)).toBe('1.4k');
  expect([250, 800, 12_300, 140_000].map(ttftText)).toEqual(['0.2s', '0.8s', '12.3s', '140s']);
});
