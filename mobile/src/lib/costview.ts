// Cost and limits: the TUI's /usage "cost & limits" and "this machine"
// sections (internal/tui/spend.go costBlock, ledgerBlock), ported. From the
// spend the engine sends (proto.Spend): on its own `spend` event between
// turns, and inside `usage` at a turn's end. A value the endpoint did not
// report is absent on the wire and "not reported" here, never 0, which would
// read as free (or as exhausted).

/** 188,160 (DES-7: thousands separators). */
const commas = (n: number) => Math.trunc(n).toLocaleString('en-US');

type Ev = Record<string, unknown>;

export type KeyBudget = {
  spend: number;
  limit: number | null;
  remaining: number | null;
  resetAt: string;
  interval: string;
  currency: string;
  source: string;
  asOf: string;
};

export type RateLimit = { name: string; limit: number | null; remaining: number | null; resetAt: string };

/** Money by currency ("" = not stated). */
export type ByCur = Record<string, number>;

export type Ledger = {
  entries: boolean;
  total: ByCur;
  hour: ByCur;
  today: ByCur;
  week: ByCur;
  month: ByCur;
  year: ByCur;
  interval: ByCur;
  last24h: ByCur;
  byModel: Record<string, ByCur>;
};

export type Spend = {
  currency: string;
  mixed: boolean;
  session: number | null;
  hour: number | null;
  today: number | null;
  interval: number | null;
  intervalName: string;
  intervalOfKey: boolean;
  calls: number;
  costedCalls: number;
  lastTurn: number | null;
  lastTurnCalls: number;
  lastTurnCosted: number;
  key: KeyBudget | null;
  keyUnsupported: boolean;
  keyError: string;
  limits: RateLimit[];
  ledger: Ledger | null;
};

const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
const opt = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : null);
const str = (v: unknown) => (typeof v === 'string' ? v : '');
const obj = (v: unknown): Ev | null => (v && typeof v === 'object' && !Array.isArray(v) ? (v as Ev) : null);

function byCur(v: unknown): ByCur {
  const o = obj(v);
  const out: ByCur = {};
  if (o) for (const [k, x] of Object.entries(o)) if (typeof x === 'number' && Number.isFinite(x)) out[k] = x;
  return out;
}

function ledgerOf(v: unknown): Ledger | null {
  const l = obj(v);
  if (!l) return null;
  const models: Record<string, ByCur> = {};
  for (const [k, x] of Object.entries(obj(l.by_model) ?? {})) models[k] = byCur(x);
  return {
    entries: l.entries === true,
    total: byCur(l.total),
    hour: byCur(l.hour),
    today: byCur(l.today),
    week: byCur(l.week),
    month: byCur(l.month),
    year: byCur(l.year),
    interval: byCur(l.interval),
    last24h: byCur(l.last_24h),
    byModel: models,
  };
}

/** A proto.Spend off the wire. */
export function parseSpend(src: unknown): Spend | null {
  const s = obj(src);
  if (!s) return null;
  const k = obj(s.key);
  return {
    currency: str(s.currency),
    mixed: s.mixed === true,
    session: opt(s.session_cost),
    hour: opt(s.session_hour),
    today: opt(s.session_today),
    interval: opt(s.session_interval),
    intervalName: str(s.interval),
    intervalOfKey: s.interval_of_key === true,
    calls: num(s.calls),
    costedCalls: num(s.costed_calls),
    lastTurn: opt(s.last_turn_cost),
    lastTurnCalls: num(s.last_turn_calls),
    lastTurnCosted: num(s.last_turn_costed),
    key: k
      ? {
          spend: num(k.spend),
          limit: opt(k.limit),
          remaining: opt(k.remaining),
          resetAt: str(k.reset_at),
          interval: str(k.interval),
          currency: str(k.currency),
          source: str(k.source),
          asOf: str(k.as_of),
        }
      : null,
    keyUnsupported: s.key_unsupported === true,
    keyError: str(s.key_error),
    limits: Array.isArray(s.limits)
      ? (s.limits as unknown[]).map((x) => {
          const r = obj(x) ?? {};
          return { name: str(r.name), limit: opt(r.limit), remaining: opt(r.remaining), resetAt: str(r.reset_at) };
        })
      : [],
    ledger: ledgerOf(s.ledger),
  };
}

/** Go's %.2f with thousands separators (numfmt.Money). */
export function money(f: number): string {
  const cents = Math.round(f * 100);
  const a = Math.abs(cents);
  return `${cents < 0 ? '-' : ''}${commas(Math.trunc(a / 100))}.${String(a % 100).padStart(2, '0')}`;
}

/** The currency after an amount, and what a sub-cent amount rounds away (curNote). */
export function curNote(f: number, cur: string): string {
  let n = cur || '(currency not stated)';
  if (f > 0 && f < 0.005) n += ` (${f.toFixed(4)})`;
  return n;
}

/** An amount in a currency, the short way (numfmt.MoneyOf). */
export function moneyOf(v: number, cur: string): string {
  switch (cur) {
    case 'USD':
      return '$' + money(v);
    case 'USD estimate':
      return '~$' + money(v) + ' est';
    case 'credits':
      return v.toFixed(1).replace(/\.0$/, '') + ' credits';
    case '':
      return money(v);
  }
  return money(v) + ' ' + cur;
}

/** How far off t is, coarsely: "in 3m", "in 2d 4h", "2m ago" (until). */
export function until(t: number, now: number): string {
  let d = (t - now) / 1000;
  const ago = d < 0;
  if (ago) d = -d;
  let s: string;
  if (d < 60) s = `${Math.trunc(d)}s`;
  else if (d < 3600) s = `${Math.trunc(d / 60)}m`;
  else if (d < 48 * 3600) s = `${Math.trunc(d / 3600)}h ${Math.trunc(d / 60) % 60}m`;
  else s = `${Math.trunc(d / 86400)}d ${Math.trunc(d / 3600) % 24}h`;
  return ago ? s + ' ago' : 'in ' + s;
}

const when = (s: string) => {
  const t = s ? Date.parse(s) : NaN;
  return Number.isNaN(t) ? null : t;
};

/** The interval rollup's name: the key budget's period when stated, else 7 days (intervalLabel). */
export function intervalLabel(sp: Spend): string {
  if (sp.intervalOfKey) return `key period (${sp.intervalName})`;
  if (sp.intervalName) return `last ${sp.intervalName}`;
  return 'last 7d';
}

/** Why calls came back with no cost, and what would change that (noCostHint). */
export function noCostHint(sp: Spend): string {
  const src = sp.key?.source ?? '';
  if (src.startsWith('litellm'))
    return "LiteLLM sends a cost only on non-streaming responses, and dun streams: the gateway's operator can set include_cost_in_streaming_usage to put it in the stream (key spend above still counts it)";
  if (src === 'openrouter')
    return "OpenRouter normally puts a cost in every response's usage; none arrived — check the URL points at openrouter.ai";
  return 'this endpoint sends no cost with its responses (LiteLLM and OpenRouter do); dun has no price table to estimate one';
}

export type Row = { label: string; value: string; note?: string; warn?: boolean };
export type Block = { title: string; note?: string; rows: Row[] };

const nr = (label: string, why = ''): Row => ({ label, value: '—', note: 'not reported' + why });

/** The warning thresholds of the TUI's status line (spendWarning): under 10% left, under 2% loud. */
const warnFraction = 0.1;

/** The "cost & limits" section (costBlock). */
export function costBlock(src: Spend | null, now: number): Block {
  const sp = src ?? parseSpend({})!;
  const rows: Row[] = [];
  if (sp.session !== null) {
    let note = curNote(sp.session, sp.currency);
    if (sp.costedCalls < sp.calls) note += ` · ${commas(sp.costedCalls)} of ${commas(sp.calls)} calls priced`;
    if (sp.mixed) note += ' · mixed currencies';
    rows.push({ label: 'session spend', value: money(sp.session), note });
    const cur = curNote(0, sp.currency);
    for (const [label, v] of [
      ['this hour', sp.hour],
      ['today', sp.today],
      [intervalLabel(sp), sp.interval],
    ] as const)
      if (v !== null) rows.push({ label: '  ' + label, value: money(v), note: cur });
  } else rows.push(nr('session spend', ` (${commas(sp.calls)} calls, none priced)`));
  if (sp.session === null && sp.calls > 0) rows.push({ label: '  why', value: '', note: noCostHint(sp) });
  if (sp.lastTurn !== null) {
    let note = curNote(sp.lastTurn, sp.currency);
    if (sp.lastTurnCosted < sp.lastTurnCalls)
      note += ` · ${commas(sp.lastTurnCosted)} of ${commas(sp.lastTurnCalls)} calls priced`;
    rows.push({ label: 'last turn', value: money(sp.lastTurn), note });
  } else rows.push(nr('last turn'));

  const k = sp.key;
  if (k) {
    const asOf = when(k.asOf);
    const src = asOf !== null ? ' · as of ' + until(asOf, now) : '';
    const period = !k.resetAt && !k.interval ? ' · all time (no reset stated)' : ' · this budget period';
    rows.push({ label: 'key spend', value: money(k.spend), note: curNote(k.spend, k.currency) + period + src });
    if (k.limit !== null) rows.push({ label: 'key limit', value: money(k.limit), note: curNote(k.limit, k.currency) });
    else rows.push(nr('key limit', ' (none set, or not stated)'));
    if (k.remaining !== null) {
      let note = curNote(k.remaining, k.currency);
      const frac = k.limit !== null && k.limit > 0 ? k.remaining / k.limit : null;
      if (frac !== null) note += ` (${Math.round(100 * frac)}%)`;
      rows.push({
        label: 'key remaining',
        value: money(k.remaining),
        note,
        warn: frac !== null && frac < warnFraction,
      });
    } else rows.push(nr('key remaining'));
    const reset = when(k.resetAt);
    if (reset !== null)
      rows.push({
        label: 'key resets',
        value: until(reset, now),
        note:
          new Date(reset).toISOString().slice(0, 16).replace('T', ' ') +
          ' UTC' +
          (k.interval ? ' · every ' + k.interval : ''),
      });
    else if (k.interval) rows.push({ label: 'key resets', value: k.interval, note: '(no date stated)' });
    else rows.push(nr('key resets'));
  } else if (sp.keyUnsupported) rows.push(nr('key budget', ' (the endpoint has none)'));
  else if (sp.keyError) rows.push(nr('key budget', ` (read failed: ${sp.keyError.slice(0, 60)})`));
  else rows.push(nr('key budget'));

  if (sp.limits.length === 0) rows.push(nr('rate limits'));
  for (const r of sp.limits) {
    const label = r.name + ' left';
    if (r.remaining === null) {
      rows.push(nr(label));
      continue;
    }
    let note = '';
    let frac: number | null = null;
    if (r.limit !== null) {
      note = `of ${commas(r.limit)}`;
      if (r.limit > 0) {
        frac = r.remaining / r.limit;
        note += ` (${Math.round(100 * frac)}%)`;
      }
    }
    const t = when(r.resetAt);
    const stale = t !== null && t < now;
    if (t !== null) note += stale ? ` · reset ${until(t, now)} (stale)` : ` · resets ${until(t, now)}`;
    rows.push({
      label,
      value: commas(r.remaining),
      note: note.trim(),
      warn: !stale && frac !== null && frac < warnFraction,
    });
  }
  return { title: 'cost & limits', note: 'what the gateway reported', rows };
}

/** One ledger total: the first currency in the value column, any others after it (ledgerRow). */
function ledgerRow(label: string, by: ByCur, tail = ''): Row {
  const curs = Object.keys(by).sort();
  const first = curs[0];
  if (first === undefined) return { label, value: money(0), note: curNote(0, '') + tail };
  let note = curNote(by[first] ?? 0, first);
  for (const c of curs.slice(1)) note += ' + ' + moneyOf(by[c] ?? 0, c);
  return { label, value: money(by[first] ?? 0), note: note + tail };
}

/** Every session's and sub-agent's spend on this machine, and this month by model (ledgerBlock). */
export function ledgerBlocks(sp: Spend | null): Block[] {
  const l = sp?.ledger;
  const head = { title: 'this machine', note: 'every session and sub-agent, from the ledger' };
  if (!sp || !l || !l.entries)
    return [
      { ...head, rows: [{ label: 'nothing spent yet', value: '', note: 'no response reported a cost this year' }] },
    ];
  const rows: Row[] = [ledgerRow('machine spend', l.total, ' · all time, every session on this machine')];
  for (const [label, by] of [
    ['this hour', l.hour],
    ['today', l.today],
    ['this week', l.week],
    ['this month', l.month],
    ['this year', l.year],
    [intervalLabel(sp), l.interval],
  ] as const)
    rows.push(ledgerRow('  ' + label, by));
  if (Object.keys(l.last24h).length)
    rows.push(ledgerRow('this key, 24h', l.last24h, ' · the rate its budget is measured against'));
  const out: Block[] = [{ ...head, rows }];
  const sum = (by: ByCur) => Object.values(by).reduce((a, b) => a + b, 0);
  const names = Object.keys(l.byModel).sort(
    (a, b) => sum(l.byModel[b] ?? {}) - sum(l.byModel[a] ?? {}) || (a < b ? -1 : 1),
  );
  if (names.length)
    out.push({ title: 'this month by model', rows: names.map((n) => ledgerRow('  ' + n, l.byModel[n] ?? {})) });
  return out;
}

/** The whole cost view: cost & limits, then this machine. */
export const costBlocks = (sp: Spend | null, now: number): Block[] => [costBlock(sp, now), ...ledgerBlocks(sp)];
