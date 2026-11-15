/// <reference types="jest" />
import { fit, noStats, noteRequest, rate, statsLine, tokK, ttftText, type Stats } from '../src/lib/stats';

// The request events as dund sends them (proto.Request, flat in the event
// frame); the cases are internal/tui/reqbar_test.go's.
const reqDone = {
  v: 1,
  type: 'request',
  session: 's1',
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

test('no bounce: start keeps the last figures, live replaces what it measured (TestReqBar_NoBounce)', () => {
  const a = fold([reqDone]);
  const b = fold([{ type: 'request', phase: 'start', ctx: 41700, window: 131072 }], a);
  const c = fold([{ type: 'request', phase: 'live', ttft_ms: 12300, gen: 9, tg: 150, ctx: 41709, window: 131072 }], b);
  const [la, lb, lc] = [a, b, c].map((s) => statsLine(s, 200));
  expect(lb).toBe('ctx 41.7k/131k · cache  96% · ttft  0.8s · pp  1.2k/s ~1.2k · tg   45/s   ~45');
  expect(lc?.endsWith('ttft 12.3s · pp  1.2k/s ~1.2k · tg  150/s   ~45')).toBe(true);
  expect(new Set([la, lb, lc].map((l) => l?.length)).size).toBe(1);
  // After the request ends the figures stay, the turn's end changes nothing.
  const d = fold([reqDone, { type: 'done' }], c);
  expect(statsLine(d, 200).endsWith('tg   45/s   ~45')).toBe(true);
});

test('the average is weighted by tokens (TestReqBar_AverageIsTokenWeighted)', () => {
  const s = fold([
    { ...reqDone, tg: 30, gen: 900 },
    { ...reqDone, tg: 90, gen: 100 },
  ]);
  expect(statsLine(s, 200).endsWith('tg   90/s   ~36')).toBe(true);
});

test('a cache never reported is not shown; a window not stated is the count alone (TestReqBar_CacheNeverReported)', () => {
  const r = { ...reqDone, cached: undefined, cache_reported: undefined, window: undefined };
  const s = fold([r]);
  expect(statsLine(s, 200)).toBe('ctx 41.3k · ttft  0.8s · pp  1.2k/s ~1.2k · tg   45/s   ~45');
  // Reported once, 0% is a figure.
  expect(statsLine(fold([{ ...r, cache_reported: true }], s), 200)).toContain('cache   0%');
});

test('cache rate is cached prompt tokens over prompt tokens, from the done phase only', () => {
  const live = fold([{ type: 'request', phase: 'live', prompt: 1000, cached: 10, cache_reported: true, ctx: 1 }]);
  expect(statsLine(live, 200)).toContain('cache    –');
  expect(fold([{ ...reqDone, prompt: 3, cached: 2 }]).cachePct).toBe(67);
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

test('a real round from a throwaway dund: live, then done (the endpoint states no window)', () => {
  const head = { epoch: '315f0e263c901ffb', session: '20261114-175419', type: 'request', v: 1 };
  const live = {
    ...head,
    ctx: 5939,
    gen: 5,
    phase: 'live',
    seq: 21,
    tg: 13.867705314157652,
    ts: '2026-11-14T17:54:24.569036872-08:00',
    ttft_ms: 1,
  };
  const done = {
    ...head,
    cache_reported: true,
    cached: 4900,
    ctx: 5320,
    exact: true,
    gen: 120,
    phase: 'done',
    pp: 300000,
    prompt: 5200,
    seq: 96,
    tg: 18.826602722503786,
    ts: '2026-11-14T17:54:30.643319069-08:00',
    ttft_ms: 1,
  };
  expect(statsLine(fold([live]), 200)).toBe('ctx  5.9k · ttft  0.0s · pp       –       · tg   13/s      ');
  expect(statsLine(fold([live, done]), 200)).toBe(
    'ctx  5.3k · cache  94% · ttft  0.0s · pp  300k/s ~300k · tg   18/s   ~18',
  );
});
