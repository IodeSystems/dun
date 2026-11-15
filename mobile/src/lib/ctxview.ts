// The /context view's own figures (internal/tui/tui_context.go contextText,
// windowBlock): what lib/ctxstats.ts does not keep — the window's room and
// ratio, the cuts, what the session did to its context (compactions, recaps,
// tool results, queued messages) and the project rules' share. Folded from the
// same events the TUI counts them on; ctxstats.ts keeps the window, system
// and token rows.

export type CtxView = {
  // window (usage / context_cost)
  window: number;
  promptTokens: number;
  reserved: number;
  charsPerToken: number;
  ratioMeasured: boolean;
  ratioRounds: number;
  cuts: number;
  folds: number;
  // system: the project rules' share of the system prompt, and the /rules mode
  rules: number;
  rulesMode: string;
  // conversation
  tools: number;
  // context shaping
  compactions: number;
  tokensCompacted: number;
  recaps: number;
  entriesRecapped: number;
  charsRecapped: number;
  // tool results
  toolResults: number;
  resultsTruncated: number;
  // out-of-band
  queued: number;
  smuggled: number;
  forced: number;
};

export const noCtxView: CtxView = {
  window: 0,
  promptTokens: 0,
  reserved: 0,
  charsPerToken: 0,
  ratioMeasured: false,
  ratioRounds: 0,
  cuts: 0,
  folds: 0,
  rules: 0,
  rulesMode: '',
  tools: 0,
  compactions: 0,
  tokensCompacted: 0,
  recaps: 0,
  entriesRecapped: 0,
  charsRecapped: 0,
  toolResults: 0,
  resultsTruncated: 0,
  queued: 0,
  smuggled: 0,
  forced: 0,
};

type Ev = Record<string, unknown>;
const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
const str = (v: unknown) => (typeof v === 'string' ? v : '');

function window(c: CtxView, ev: Ev): CtxView {
  // An absent window keeps the last; a window of 0 is "nobody stated one".
  if (!('window' in ev)) return c;
  return {
    ...c,
    window: num(ev.window),
    promptTokens: num(ev.prompt_tokens),
    reserved: num(ev.window_reserved),
    charsPerToken: num(ev.chars_per_token),
    ratioMeasured: ev.ratio_measured === true,
    ratioRounds: num(ev.ratio_rounds),
    cuts: num(ev.window_cuts),
    folds: num(ev.window_folds),
  };
}

function cost(c: CtxView, ev: Ev): CtxView {
  let n = window(c, ev);
  if (num(ev.system_tokens) > 0) n = { ...n, rules: num(ev.system_rules), rulesMode: str(ev.system_rules_mode) };
  if (num(ev.forced_calls) > 0) n = { ...n, forced: num(ev.forced_calls) };
  if (num(ev.notifications_lifted) > 0) n = { ...n, smuggled: num(ev.notifications_lifted) };
  return n;
}

/** Folds one event in; others leave c as it is. */
export function noteCtxView(c: CtxView, ev: Ev): CtxView {
  switch (ev.type) {
    case 'usage':
    case 'context_cost':
      return cost(c, ev);
    case 'ready':
    case 'server':
      return Array.isArray(ev.tools) ? { ...c, tools: ev.tools.length } : c;
    case 'recap':
      return {
        ...c,
        recaps: c.recaps + 1,
        entriesRecapped: c.entriesRecapped + Math.max(0, num(ev.entries)),
        charsRecapped: c.charsRecapped + Math.max(0, num(ev.chars)),
      };
    case 'compaction': {
      const saved = num(ev.tokens_before) - num(ev.tokens_after);
      return { ...c, compactions: c.compactions + 1, tokensCompacted: c.tokensCompacted + Math.max(0, saved) };
    }
    case 'tool_result': {
      const r = str(ev.content);
      const cut = r.includes('[') && r.includes('characters elided');
      return { ...c, toolResults: c.toolResults + 1, resultsTruncated: c.resultsTruncated + (cut ? 1 : 0) };
    }
    case 'user':
      return ev.queued === true ? { ...c, queued: c.queued + 1 } : c;
  }
  return c;
}

/** 188,160 (DES-7: thousands separators). */
const commas = (n: number) => Math.trunc(n).toLocaleString('en-US');

export type Row = { label: string; value: string; note?: string };
export type Block = { title: string; rows: Row[] };

/** The TUI's /context rows this view adds, as sections of label, value and note. */
export function contextBlocks(c: CtxView, blocks: number): Block[] {
  const out: Block[] = [];
  const win: Row[] = [];
  if (c.window > 0) {
    if (c.promptTokens > 0) {
      const free = c.window - c.promptTokens;
      win.push({
        label: 'room now',
        value: commas(free),
        ...(free < c.reserved ? { note: '← below the reservation' } : {}),
      });
    }
    win.push({
      label: 'ratio',
      value: `${c.charsPerToken.toFixed(2)} chars/token`,
      note: c.ratioMeasured
        ? `(measured over ${commas(c.ratioRounds)} rounds)`
        : `(default — nothing measured yet; ${commas(c.ratioRounds)} rounds)`,
    });
    if (c.cuts > 0)
      win.push({
        label: 'cut short',
        value: commas(c.cuts) + (c.folds > 0 ? ` (${commas(c.folds)} folded)` : ''),
        note: c.cuts === 1 ? 'reply stopped for room' : 'replies stopped for room',
      });
  }
  if (win.length) out.push({ title: 'window room', rows: win });
  out.push({
    title: 'conversation',
    rows: [
      { label: 'blocks', value: commas(blocks) },
      { label: 'tools', value: commas(c.tools) },
    ],
  });
  out.push({
    title: 'context shaping',
    rows: [
      c.compactions > 0
        ? { label: 'compactions', value: commas(c.compactions), note: `saved ${commas(c.tokensCompacted)} tokens` }
        : { label: 'compactions', value: '0' },
      c.recaps > 0
        ? {
            label: 'recaps',
            value: commas(c.recaps),
            note: `${commas(c.entriesRecapped)} entries, ${commas(c.charsRecapped)} chars → disk`,
          }
        : { label: 'recaps', value: '0' },
    ],
  });
  out.push({
    title: 'tool results',
    rows: [
      { label: 'total', value: commas(c.toolResults) },
      c.resultsTruncated > 0
        ? { label: 'truncated', value: commas(c.resultsTruncated), note: '(LOD-capped at 4000 chars)' }
        : { label: 'truncated', value: '0' },
    ],
  });
  if (c.rulesMode)
    out.push({
      title: 'project rules',
      rows: [{ label: 'rules', value: commas(c.rules), note: `tokens (/rules ${c.rulesMode})` }],
    });
  const oob: Row[] = [{ label: 'queued msgs', value: commas(c.queued), note: '(delivered mid-turn)' }];
  if (c.smuggled > 0) oob.push({ label: 'notifications', value: commas(c.smuggled), note: '(smuggled via queue)' });
  if (c.forced > 0) oob.push({ label: 'forced calls', value: commas(c.forced), note: '(host-injected)' });
  out.push({ title: 'out-of-band', rows: oob });
  return out;
}
