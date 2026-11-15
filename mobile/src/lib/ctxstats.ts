// What fills the context and what it cost: the TUI's /context and /usage
// figures (internal/tui/tui_context.go, spend.go), from the usage,
// context_cost and spend events. usage is one turn's accounting: its token
// counts add up here, as in the TUI, so after a snapshot they count from the
// last turn the snapshot kept. The cache figures and the spend are the
// session's own totals.

export type Part = { name: string; tokens: number };

export type Spend = {
  currency: string;
  session: number | null;
  today: number | null;
  calls: number;
  costedCalls: number;
  lastTurn: number | null;
};

export type Ctx = {
  seen: boolean;
  // tokens, summed over the turns seen
  processed: number;
  generated: number;
  cached: number;
  total: number;
  active: number;
  turns: number;
  // system cost
  systemTokens: number;
  systemExact: boolean;
  systemPrompt: number;
  systemShared: number;
  parts: Part[];
  // window
  window: number;
  reserved: number;
  cap: number;
  promptBudget: number;
  promptTokens: number;
  promptOverhead: number;
  // the session's prompt cache
  cacheRounds: number;
  cacheHitPct: number;
  cacheCached: number;
  cacheProcessed: number;
  hasCache: boolean;
  spend: Spend | null;
};

export const noCtx: Ctx = {
  seen: false,
  processed: 0,
  generated: 0,
  cached: 0,
  total: 0,
  active: 0,
  turns: 0,
  systemTokens: 0,
  systemExact: false,
  systemPrompt: 0,
  systemShared: 0,
  parts: [],
  window: 0,
  reserved: 0,
  cap: 0,
  promptBudget: 0,
  promptTokens: 0,
  promptOverhead: 0,
  cacheRounds: 0,
  cacheHitPct: 0,
  cacheCached: 0,
  cacheProcessed: 0,
  hasCache: false,
  spend: null,
};

type Ev = Record<string, unknown>;
const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
const money = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : null);

function systemCost(c: Ctx, ev: Ev): Ctx {
  if (num(ev.system_tokens) <= 0) return c;
  const parts = Array.isArray(ev.system_parts)
    ? (ev.system_parts as Ev[]).map((p) => ({ name: typeof p.name === 'string' ? p.name : '', tokens: num(p.tokens) }))
    : [];
  return {
    ...c,
    systemTokens: num(ev.system_tokens),
    systemExact: ev.system_exact === true,
    systemPrompt: num(ev.system_prompt),
    systemShared: num(ev.system_shared),
    parts,
  };
}

function windowOf(c: Ctx, ev: Ev): Ctx {
  // A window of 0 is the message ("nobody stated one"): only an absent field keeps the last.
  if (!('window' in ev)) return c;
  return {
    ...c,
    window: num(ev.window),
    reserved: num(ev.window_reserved),
    cap: num(ev.window_cap),
    promptBudget: num(ev.prompt_budget),
    promptTokens: num(ev.prompt_tokens),
    promptOverhead: num(ev.prompt_overhead),
  };
}

function spendOf(c: Ctx, src: unknown): Ctx {
  if (!src || typeof src !== 'object') return c;
  const s = src as Ev;
  return {
    ...c,
    spend: {
      currency: typeof s.currency === 'string' ? s.currency : '',
      session: money(s.session_cost),
      today: money(s.session_today),
      calls: num(s.calls),
      costedCalls: num(s.costed_calls),
      lastTurn: money(s.last_turn_cost),
    },
  };
}

/** Folds one event in; others leave c as it is. */
export function noteCtx(c: Ctx, ev: Ev): Ctx {
  switch (ev.type) {
    case 'context_cost':
      return { ...windowOf(systemCost(c, ev), ev), seen: true };
    case 'spend':
      return { ...spendOf(c, ev), seen: true };
    case 'usage': {
      let n: Ctx = {
        ...c,
        seen: true,
        processed: c.processed + num(ev.processed),
        generated: c.generated + num(ev.generated),
        cached: c.cached + num(ev.cached),
        total: c.total + num(ev.total),
        active: num(ev.active),
        turns: num(ev.turns),
      };
      n = windowOf(systemCost(n, ev), ev);
      const cache = ev.cache as Ev | undefined;
      if (cache && typeof cache === 'object')
        n = {
          ...n,
          hasCache: true,
          cacheRounds: num(cache.rounds),
          cacheHitPct: num(cache.hit_pct),
          cacheCached: num(cache.cached),
          cacheProcessed: num(cache.processed),
        };
      return ev.spend ? spendOf(n, ev.spend) : n;
    }
  }
  return c;
}

/** 188,160 (DES-7: thousands separators). */
export const commas = (n: number) => Math.trunc(n).toLocaleString('en-US');

/** An amount of money, to the cent, or below a cent to 4 places (numfmt.Money). */
export function moneyText(f: number, currency: string): string {
  const v = Math.abs(f) > 0 && Math.abs(f) < 0.01 ? f.toFixed(4) : f.toFixed(2);
  return currency ? `${v} ${currency}` : v;
}

export type DetailRow = { label: string; value: string; note?: string };
export type Section = { title: string; rows: DetailRow[] };

/** The /context and /usage figures as sections of label · value rows. */
export function sections(c: Ctx): Section[] {
  const out: Section[] = [];
  const pct = (n: number) => (c.window > 0 ? ` (${Math.trunc((n * 100) / c.window)}%)` : '');
  out.push({
    title: 'window',
    rows:
      c.window > 0
        ? [
            { label: 'size', value: `${commas(c.window)} tokens` },
            c.promptTokens > 0
              ? {
                  label: 'prompt',
                  value: `${commas(c.promptTokens)} of ${commas(c.promptBudget)} budget`,
                  note: pct(c.promptTokens).trim(),
                }
              : { label: 'prompt', value: `${commas(c.promptBudget)} budget`, note: '(nothing built yet)' },
            ...(c.promptOverhead > 0
              ? [{ label: 'overhead', value: commas(c.promptOverhead), note: '(schemas + template, once)' }]
              : []),
            { label: 'response', value: `${commas(c.reserved)} reserved`, note: `(cap ${commas(c.cap)} + margin)` },
          ]
        : [{ label: 'size', value: 'not stated', note: '— no shaping, and no cap on a response' }],
  });
  out.push({
    title: 'system',
    rows:
      c.systemTokens > 0
        ? [
            {
              label: 'prompt + schemas',
              value: commas(c.systemTokens),
              note: c.systemExact ? 'tokens (counted)' : 'tokens (estimated)',
            },
            ...(c.systemPrompt > 0 ? [{ label: '  system prompt', value: commas(c.systemPrompt) }] : []),
            ...c.parts.map((p) => ({ label: '  ' + p.name, value: commas(p.tokens) })),
            ...(c.systemShared > 0
              ? [{ label: '  tool-use overhead', value: commas(c.systemShared), note: '(provider, shared)' }]
              : []),
          ]
        : [{ label: 'prompt + schemas', value: 'not reported' }],
  });
  out.push({
    title: 'tokens',
    rows: [
      { label: 'processed', value: commas(c.processed) },
      { label: 'generated', value: commas(c.generated) },
      { label: 'cached', value: commas(c.cached) },
      { label: 'total', value: commas(c.total) },
      { label: 'active', value: commas(c.active), note: "(last turn's window)" },
      { label: 'turns', value: commas(c.turns) },
    ],
  });
  if (c.hasCache)
    out.push({
      title: 'cache',
      rows: [
        {
          label: 'rounds',
          value: `${commas(c.cacheRounds)} · ${Math.round(c.cacheHitPct)}% hit`,
          note: `${commas(c.cacheCached)} cached / ${commas(c.cacheProcessed)} processed`,
        },
      ],
    });
  const sp = c.spend;
  out.push({
    title: 'cost',
    rows:
      sp && sp.session !== null
        ? [
            {
              label: 'session spend',
              value: moneyText(sp.session, sp.currency),
              note: sp.costedCalls < sp.calls ? `${commas(sp.costedCalls)} of ${commas(sp.calls)} calls priced` : '',
            },
            ...(sp.today !== null ? [{ label: '  today', value: moneyText(sp.today, sp.currency) }] : []),
            ...(sp.lastTurn !== null ? [{ label: '  last turn', value: moneyText(sp.lastTurn, sp.currency) }] : []),
          ]
        : [{ label: 'session spend', value: '—', note: 'not reported' }],
  });
  return out;
}
