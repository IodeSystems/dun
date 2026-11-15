// Notices: what the engine says beside the conversation, read the way the
// dun TUI reads them (internal/tui: tui_events.go "notification", agentmsg.go,
// shellnotice.go, tui_entry.go docsBlock). Pure: events.ts folds them in,
// components/notices draws them.
//
//   - a plain notice: one row, the rest when opened (noticeEntry);
//   - a finished background job: its head, the command, the output's tail;
//   - what proactive search surfaced: a docs block, each document with
//     where it came from and why it won;
//   - a message between agents: who sent it to whom, and what was said.

/** The TUI's colour roles: stNote (214, proactive), StDim (240), StErr. */
export type Tone = 'note' | 'dim' | 'error';

export type Doc = {
  title: string;
  /** The snippet: shown when the document is opened. */
  line: string;
  score: number;
  /** skill, memory, rule, mcp-doc, doc, attached doc; '' from an older engine. */
  kind: string;
  /** Where it came from, on one line; '' from an older engine. */
  source: string;
  /** Why it won; '' from an older engine. */
  why: string;
};

export type AgentMsg = {
  from: string;
  to: string;
  text: string;
  /** A tool call of this session sent it: it waits for that call's result. */
  carried: boolean;
  tool?: string;
  /** The carrying call's id, when the engine sent one: its result pairs by it. */
  id?: string;
  done: boolean;
  failed: boolean;
  /** The carrying call's result, short: the footer. */
  status: string;
};

export type Notice =
  | { type: 'plain'; tone: Tone; icon: string; text: string; label?: string }
  | { type: 'job'; head: string; took: string; command: string; output: string }
  | { type: 'docs'; found: number; surfaced: number; tokens: number; docs: Doc[] }
  | ({ type: 'msg' } & AgentMsg);

type Ev = Record<string, unknown>;
const str = (v: unknown) => (typeof v === 'string' ? v : '');
const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);

/** The notification kinds the TUI draws its own way; any other is labelled. */
const knownKinds = new Set(['', 'docs', 'budget', 'schedule', 'peer']);

const childMsgRe = /^agent #(\d+): ([\s\S]+)$/;
const peerLiveRe = /^(.+?) \(([^()]*)\): ([\s\S]*)$/;
const peerLogRe =
  /^\[dun msg from (.+?) \(session ([^()]*)\)\] ([\s\S]*?)(?:\n\(A message from another dun session[\s\S]*)?$/;
const jobNoticeRe = /^(background job #\d+ [^`]*?— )`([\s\S]*?)`( \([\s\S]*)$/;

function inMsg(from: string, text: string): Notice {
  return { type: 'msg', from, to: 'you', text, carried: false, done: true, failed: false, status: '' };
}

/** The message a notification carries (agentmsg.go noticeMsg), or null. */
export function noticeMsg(kind: string, text: string): Notice | null {
  if (kind === 'peer') {
    const m = peerLiveRe.exec(text);
    return m ? inMsg('dun:' + m[1], m[3] ?? '') : null;
  }
  const c = childMsgRe.exec(text);
  if (c) return inMsg('agent #' + c[1], c[2] ?? '');
  const p = peerLogRe.exec(text);
  if (p) return inMsg('dun:' + p[1], p[3] ?? '');
  return null;
}

/** A notification's text as a notice (shellnotice.go noticeEntry). */
export function textNotice(text: string, label?: string): Notice {
  const j = jobNoticeRe.exec(text);
  if (j) {
    const head = (j[1] ?? '').trim().replace(/—$/, '').trim();
    const rest = (j[3] ?? '').replace(/^ /, '');
    const nl = rest.indexOf('\n');
    return {
      type: 'job',
      head,
      took: nl < 0 ? rest : rest.slice(0, nl),
      command: (j[2] ?? '').trim(),
      output: nl < 0 ? '' : rest.slice(nl + 1).replace(/\n+$/, ''),
    };
  }
  const n: Notice = { type: 'plain', tone: 'note', icon: '🔔', text: text.trim() };
  return label ? { ...n, label } : n;
}

export function docsFromEvent(ev: Ev): Notice {
  const docs = Array.isArray(ev.docs) ? (ev.docs as Ev[]) : [];
  return {
    type: 'docs',
    found: num(ev.found),
    surfaced: num(ev.surfaced),
    tokens: num(ev.tokens),
    docs: docs.map((d) => ({
      title: str(d.title),
      line: str(d.line),
      score: num(d.score),
      kind: str(d.kind),
      source: str(d.source),
      why: str(d.why),
    })),
  };
}

/**
 * A live notification event as a notice (tui_events.go "notification").
 * Never null: a kind the TUI does not know is a notice labelled with it, and
 * one with no text says so with what it did carry.
 */
export function fromNotification(ev: Ev): Notice {
  const kind = str(ev.kind);
  const text = str(ev.text);
  switch (kind) {
    case 'docs':
      return docsFromEvent(ev);
    case 'budget':
      return { type: 'plain', tone: 'dim', icon: '⏱', text: oneLine(text) };
    case 'schedule':
      return { type: 'plain', tone: 'dim', icon: '⏲', text: oneLine(text) };
    case 'peer':
      return (
        noticeMsg('peer', text) ?? { type: 'plain', tone: 'dim', icon: '✉', text: 'dun msg from ' + oneLine(text) }
      );
  }
  const label = knownKinds.has(kind) ? undefined : kind;
  if (!text.trim()) {
    const { type: _t, kind: _k, ...rest } = ev;
    const what = Object.keys(rest).length ? JSON.stringify(rest) : '(no text)';
    return { type: 'plain', tone: 'note', icon: '🔔', text: what, label: label ?? 'notification' };
  }
  return noticeMsg('', text) ?? textNotice(text, label);
}

/** A history item of kind notification: what the model was told (tui_convo.go). */
export function fromHistoryNotification(content: string): Notice {
  return noticeMsg('', content) ?? textNotice(content);
}

const argStr = (a: Ev, k: string) => str(a[k]).trim();

function agentArg(v: unknown): string {
  if (typeof v === 'number' && Number.isFinite(v)) return String(v);
  return typeof v === 'string' ? v.trim() : '';
}

/** The message a tool call carries (agentmsg.go carriedMsg): a jobs tell, a dun_msg. */
export function carriedMsg(tool: string, args: unknown, id?: string): Notice | null {
  const a = (args ?? {}) as Ev;
  const out = (to: string, text: string): Notice => ({
    type: 'msg',
    from: 'you',
    to,
    text,
    carried: true,
    tool,
    done: false,
    failed: false,
    status: '',
    ...(id ? { id } : {}),
  });
  if (tool === 'jobs') {
    const tell = argStr(a, 'tell');
    const n = agentArg(a.agent);
    if (!tell || !n || a.kill === true || a.resume === true) return null;
    return out('agent #' + n, tell);
  }
  if (tool === 'dun_msg') {
    const to = argStr(a, 'to');
    const text = argStr(a, 'text');
    return to && text ? out('dun:' + to, text) : null;
  }
  return null;
}

const dunSentRe = /^delivered to (.+?) \(([^()]*)\): (.*)$/;
const clip = (s: string, n: number) => (s.length > n ? s.slice(0, n - 1) + '…' : s);

/** The carrying call's result read into the footer (agentmsg.go setResult). */
export function settleMsg(m: AgentMsg, result: string): AgentMsg {
  const first = (result.trim().split('\n')[0] ?? '').trim();
  const done = { ...m, done: true };
  if (first.startsWith('ERROR'))
    return {
      ...done,
      failed: true,
      status: clip(
        first
          .replace(/^ERROR/, '')
          .replace(/^:/, '')
          .trim(),
        120,
      ),
    };
  if (m.tool === 'jobs') {
    if (first.startsWith('Told agent #')) return { ...done, status: 'sent · it is working' };
    return { ...done, failed: true, status: clip(first, 120) };
  }
  const d = dunSentRe.exec(first);
  if (!d) return { ...done, status: clip(first, 120) };
  const how = d[3] ?? '';
  const status = how.includes('busy')
    ? 'delivered · it is busy; it reads it this turn'
    : how.includes('turn started')
      ? 'delivered · woke it'
      : 'delivered · not woken; it reads it next turn';
  return { ...done, to: 'dun:' + d[1], status };
}

/** recap: one dim line that opens to what replaced the span. */
export function recapNotice(ev: Ev): Notice {
  const detail = str(ev.detail).replace(/\n+$/, '');
  return { type: 'plain', tone: 'dim', icon: '✂', text: str(ev.note) + (detail ? '\n' + detail : '') };
}

/** compaction: history folded into a summary, in the notice colour. */
export function compactionNotice(ev: Ev): Notice {
  return { type: 'plain', tone: 'note', icon: '🗜', text: str(ev.text) };
}

/** agent_told: shown only when it was refused (the child's view shows the rest). */
export function agentToldNotice(ev: Ev): Notice | null {
  const msg = str(ev.message);
  if (!msg || msg.startsWith('Told agent')) return null;
  return { type: 'plain', tone: 'note', icon: '↳', text: oneLine(msg) };
}

/** mcp_login: the URL whole, to be opened (enginecmd.LoginLine). */
export function mcpLoginNotice(ev: Ev): Notice {
  const server = str(ev.server);
  const url = str(ev.url);
  const text = url
    ? `open to log in to ${server}: ${url}`
    : `${server}: waiting for a login to it that another session started`;
  return { type: 'plain', tone: 'note', icon: '🔑', text };
}

export function oneLine(s: string): string {
  return s.replace(/\s*\n\s*/g, ' ').trim();
}

/** The docs block's collapsed row adds this for the documents (tui_entry.go brief). */
export function docsBrief(docs: Doc[], max = 60): string {
  const long: string[] = [];
  const kinds: string[] = [];
  let lone = 0;
  for (const d of docs) {
    if (!d.kind) return '';
    let p = d.title.startsWith(d.kind + ' ') ? d.title : d.kind + ' ' + d.title;
    if (d.why.startsWith('only candidate')) {
      p += ' (only candidate)';
      lone++;
    }
    long.push(p);
    kinds.push(d.kind);
  }
  let short = kinds.join(', ');
  if (lone > 0) short += ` · ${lone} only candidate`;
  for (const s of [long.join(', '), short]) if (s && Array.from(s).length <= max) return ' · ' + s;
  return '';
}

/** The docs block's head (tui_entry.go docsBlock.render). */
export function docsHead(n: Extract<Notice, { type: 'docs' }>, open: boolean): string {
  let label = `🔎 ${n.found} relevant doc(s) · ${n.surfaced} surfaced`;
  if (n.tokens > 0) label += ` · ~${n.tokens.toLocaleString('en-US')} tok`;
  return open ? label : label + docsBrief(n.docs);
}

/** A document's reason: why it won, or its score from an older engine. */
export function docWhy(d: Doc): string {
  return d.why || `score ${Number(d.score.toPrecision(2))}`;
}

/** The job notice's collapsed row (shellnotice.go). */
export function jobLine(n: Extract<Notice, { type: 'job' }>): string {
  const [first, ...more] = n.command.split('\n');
  return `🔔 ${n.head} ${n.took} · $ ${first ?? ''}${more.length ? ` (+${more.length} lines)` : ''}`;
}

/** A message longer than this folds (the TUI caps a box and scrolls inside it). */
export const msgFoldLines = 8;

export function msgLong(text: string): boolean {
  return text.trim().split('\n').length > msgFoldLines || text.length > 600;
}

type Slot = { kind: string; notice?: Notice };

/**
 * The message still waiting for this result (the tool-call pairing rule of
 * toolcalls.ts: by id, else the newest pending one of the same tool when
 * either has no id), or -1: the result is a tool row's.
 */
export function pendingMsg(items: readonly Slot[], tool: string, id?: string): number {
  if (tool !== 'jobs' && tool !== 'dun_msg') return -1;
  const open = (i: number) => {
    const n = items[i]?.kind === 'notice' ? items[i]?.notice : undefined;
    return n?.type === 'msg' && n.carried && !n.done && n.tool === tool ? n : null;
  };
  if (id) for (let i = items.length - 1; i >= 0; i--) if (open(i)?.id === id) return i;
  for (let i = items.length - 1; i >= 0; i--) {
    const n = open(i);
    if (n && (!id || !n.id)) return i;
  }
  return -1;
}
