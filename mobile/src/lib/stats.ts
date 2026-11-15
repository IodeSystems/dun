// The request stats: the TUI's top-bar figures (internal/tui/reqbar.go),
// ported. dund measures them (reqstats.go) and sends a `request` event at a
// round's start, live while it streams, and at its end:
//
//   ctx 41.3k/131k · cache 96% · ttft 0.8s · pp 1.2k/s ~1.1k · tg 45/s ~41
//
// ctx is what the conversation fills of the window; cache the round's cached
// prompt tokens over its prompt tokens (only once the provider has reported
// the cache field; an unreported cache is not 0%); ttft the wait for the
// first streamed token; pp prompt tokens a second; tg generated tokens a
// second, live while the reply streams. pp and tg also show the session's
// average weighted by tokens, after a "~". Each figure keeps its last value
// until a new one arrives. Every value is padded to a fixed width, so a
// number changing never moves the others; when the row is too narrow the
// pieces go in the order pp, ttft, cache, ctx, tg.

/** What the screen keeps of the request events (reqBar). */
export type Stats = {
  seen: boolean;
  ctx: number;
  window: number;
  cacheSeen: boolean;
  ttft: number;
  pp: number;
  tg: number;
  cachePct: number;
  cacheHas: boolean;
  ppSum: number;
  ppN: number;
  tgSum: number;
  tgN: number;
};

export const noStats: Stats = {
  seen: false,
  ctx: 0,
  window: 0,
  cacheSeen: false,
  ttft: 0,
  pp: 0,
  tg: 0,
  cachePct: 0,
  cacheHas: false,
  ppSum: 0,
  ppN: 0,
  tgSum: 0,
  tgN: 0,
};

const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);

/** Folds one event in; anything but a request event leaves s as it is (noteRequest). */
export function noteRequest(s: Stats, ev: Record<string, unknown>): Stats {
  if (ev.type !== 'request') return s;
  const prompt = num(ev.prompt);
  const cached = num(ev.cached);
  const gen = num(ev.gen);
  const pps = num(ev.pp);
  const tgs = num(ev.tg);
  const ttft = num(ev.ttft_ms);
  const reported = ev.cache_reported === true;
  const n: Stats = {
    ...s,
    seen: true,
    ctx: num(ev.ctx),
    window: num(ev.window),
    cacheSeen: s.cacheSeen || reported,
    ttft: ttft > 0 ? ttft : s.ttft,
    pp: pps > 0 ? pps : s.pp,
    tg: tgs > 0 ? tgs : s.tg,
  };
  if (ev.phase !== 'done') return n;
  if (reported && prompt > 0) {
    n.cachePct = Math.round((cached * 100) / prompt);
    n.cacheHas = true;
  }
  if (pps > 0 && prompt - cached > 0) {
    n.ppSum += pps * (prompt - cached);
    n.ppN += prompt - cached;
  }
  if (tgs > 0 && gen > 0) {
    n.tgSum += tgs * gen;
    n.tgN += gen;
  }
  return n;
}

/** Go's %.<p>f: an exact tie goes to the even digit (toFixed rounds it up). */
function fixed(x: number, p: number): string {
  const y = x * 10 ** p;
  if (Math.abs(y - Math.trunc(y)) !== 0.5) return x.toFixed(p);
  const t = Math.trunc(y);
  return ((t % 2 === 0 ? t : t + Math.sign(y)) / 10 ** p).toFixed(p);
}

/** A size the short way: 940, 18.4k, 128k, 1.05M (numfmt.TokK). */
export function tokK(n: number): string {
  n = Math.trunc(n);
  if (n < 1000) return String(n);
  if (n < 100_000) return fixed(n / 1000, 1).replace(/\.0$/, '') + 'k';
  if (n < 1_000_000) return Math.round(n / 1000) + 'k';
  return fixed(n / 1e6, 2).replace(/\.00$/, '') + 'M';
}

/** Tokens a second (numfmt.Rate). */
export const rate = (tps: number) => tokK(Math.trunc(tps)) + '/s';

/** A wait in seconds, its width steady: 0.8s, 12.3s, 140s (ttftText). */
export function ttftText(ms: number): string {
  const s = ms / 1000;
  return s >= 100 ? `${Math.trunc(s)}s` : `${fixed(s, 1)}s`;
}

/** A token-weighted average, "" before there is one. */
const avg = (sum: number, n: number) => (n > 0 ? '~' + tokK(Math.trunc(sum / n)) : '');

const pad = (s: string, w: number) => s.padStart(w);
const dash = (ok: boolean, s: string) => (ok ? s : '–');

/** One stat: its label and its value padded to a fixed width; lower drop goes first. */
export type Piece = { label: string; value: string; drop: number };

/** The stats in display order (pieces). */
export function pieces(s: Stats): Piece[] {
  let ctx: Piece = { label: 'ctx', value: pad(tokK(s.ctx), 5), drop: 3 };
  if (s.window > 0) {
    const w = tokK(s.window);
    ctx = { ...ctx, value: pad(`${pad(tokK(s.ctx), 5)}/${w}`, 6 + w.length) };
  }
  const out = [ctx];
  if (s.cacheSeen) out.push({ label: 'cache', value: pad(dash(s.cacheHas, `${s.cachePct}%`), 4), drop: 2 });
  const withAvg = (v: number, a: string, w: number) => `${pad(dash(v > 0, rate(v)), w)} ${pad(a, 5)}`;
  out.push(
    { label: 'ttft', value: pad(dash(s.ttft > 0, ttftText(s.ttft)), 5), drop: 1 },
    { label: 'pp', value: pad(withAvg(s.pp, avg(s.ppSum, s.ppN), 7), 13), drop: 0 },
    { label: 'tg', value: pad(withAvg(s.tg, avg(s.tgSum, s.tgN), 6), 12), drop: 4 },
  );
  return out;
}

const SEP = ' · ';

/** A piece's width in characters (every character here is one UTF-16 unit). */
const cells = (p: Piece) => p.label.length + 1 + p.value.length;

/** The pieces that fit in room characters, least first out (view); [] before any request. */
export function fit(s: Stats, room: number): Piece[] {
  if (!s.seen) return [];
  let ps = pieces(s);
  const width = (xs: Piece[]) => xs.reduce((n, p, i) => n + (i > 0 ? SEP.length : 0) + cells(p), 0);
  while (ps.length > 0 && width(ps) > room) {
    let low = 0;
    ps.forEach((p, i) => {
      if (p.drop < (ps[low]?.drop ?? 0)) low = i;
    });
    ps = ps.filter((_, i) => i !== low);
  }
  return ps;
}

/** The row as text: what fits in room characters. */
export const statsLine = (s: Stats, room: number) =>
  fit(s, room)
    .map((p) => `${p.label} ${p.value}`)
    .join(SEP);
