// A session's events, folded into what a screen shows. Pure: the screen
// feeds it what /api/graphql sessionEvents returns, in order.
//
// The events are the protocol's flat JSON frames (dun proto/events.go); only
// the fields read here are relied on.

import type { McpTool, McpTools } from './mcp';
import { autoResumePrefix, parseUpgrade, type Upgrade } from './upgrade';

export type Item =
  | { key: string; kind: 'user'; text: string; queued?: boolean }
  | { key: string; kind: 'assistant'; text: string }
  | { key: string; kind: 'tool'; title: string; body: string }
  | { key: string; kind: 'note'; text: string; error?: boolean }
  | { key: string; kind: 'question'; text: string; options: string[]; recommended?: string };

export type Ask = { question: string; options: string[]; recommended?: string };

/** A slash command the engine carries out (proto.CommandSpec). */
export type Command = {
  name: string;
  usage?: string;
  description: string;
  group: string;
  choices?: { value: string; description?: string }[];
  text?: boolean;
  state?: string;
  detail?: string;
};

export type Skill = { name: string; description: string };

export type ModelProvider = { name: string; url: string; ids: string[] | null; error?: string; default?: boolean; key?: boolean };
export type Settings = { model: string; provider: string; url: string };

export type View = {
  items: Item[];
  /** The reply streaming in, before its message event settles it. */
  streaming: string | null;
  busy: boolean;
  status: string;
  ask: Ask | null;
  /** The engine's slash commands, with their current state (commands event). */
  commands: Command[];
  skills: Skill[];
  /** Every provider's models, once asked for (control "models"). */
  models: ModelProvider[] | null;
  settings: Settings | null;
  /** The last server tool list the engine sent (control "mcptools"). */
  mcpTools: McpTools | null;
  /** The engine said the session is closed: the client should leave it. */
  closed: boolean;
  /** dund upgrading (the upgrade event): the banner; null when none. */
  upgrade: Upgrade | null;
  n: number; // key counter
};

export const empty: View = { items: [], streaming: null, busy: false, status: '', ask: null, commands: [], skills: [], models: null, settings: null, mcpTools: null, closed: false, upgrade: null, n: 0 };

type Ev = Record<string, unknown>;
const str = (v: unknown) => (typeof v === 'string' ? v : '');

/** What proactive search surfaced: each document, why it won, and where it came from (an older engine sends neither). */
export function docsNote(ev: Ev): string {
  const docs = Array.isArray(ev.docs) ? (ev.docs as Ev[]) : [];
  const lines = [`🔎 ${Number(ev.surfaced ?? 0)} doc(s) surfaced`];
  for (const d of docs) {
    const why = str(d.why);
    lines.push(`• ${str(d.title)}${why ? ` (${why})` : ''}`);
    if (str(d.source)) lines.push(`  ${str(d.source)}`);
  }
  return lines.join('\n');
}

/** An item before it has a key (Omit spread over each member of the union). */
type WithoutKey<T> = T extends unknown ? Omit<T, 'key'> : never;
type NewItem = WithoutKey<Item>;

function push(v: View, item: NewItem): View {
  return { ...v, n: v.n + 1, items: [...v.items, { ...item, key: String(v.n) }] };
}

/** A user message; dund's automatic resume after a restart is a note, not the person's. */
function pushUser(v: View, text: string, queued?: boolean): View {
  if (text.startsWith(autoResumePrefix)) return push(v, { kind: 'note', text: '↻ resumed after the restart — ' + text.slice(autoResumePrefix.length) });
  return push(v, queued ? { kind: 'user', text, queued } : { kind: 'user', text });
}

function settleQueued(v: View): View {
  if (!v.items.some((i) => i.kind === 'user' && i.queued)) return v;
  return { ...v, items: v.items.map((i) => (i.kind === 'user' && i.queued ? { ...i, queued: false } : i)) };
}

/** An ask_user call as the question it is, not as JSON. */
function pushQuestion(v: View, args: unknown): View {
  const a = (args ?? {}) as Ev;
  return push(v, { kind: 'question', text: str(a.question), options: (a.options as string[]) ?? [], recommended: str(a.recommended) || undefined });
}

/** A history item (history event, snapshot) as a shown item. */
function fromHistory(v: View, it: Ev): View {
  switch (it.kind) {
    case 'user':
      return pushUser(v, str(it.content));
    case 'assistant':
      return push(v, { kind: 'assistant', text: str(it.content) });
    case 'tool_call':
      if (it.tool === 'ask_user') return pushQuestion(v, it.args);
      return push(v, { kind: 'tool', title: '⚙ ' + str(it.tool), body: JSON.stringify(it.args ?? {}, null, 2) });
    case 'tool_result':
      if (it.tool === 'ask_user') return push(v, { kind: 'user', text: str(it.content) });
      return push(v, { kind: 'tool', title: '↳ ' + str(it.tool), body: str(it.content) });
    case 'notification':
      return push(v, { kind: 'note', text: '🔔 ' + str(it.content) });
  }
  return v;
}

/** Replace what is shown with a conversation (history replaces, never adds). */
export function replaceWith(items: unknown[], keep?: View): View {
  let v: View = { ...empty, commands: keep?.commands ?? [], skills: keep?.skills ?? [], models: keep?.models ?? null, settings: keep?.settings ?? null, mcpTools: keep?.mcpTools ?? null };
  for (const it of items) if (it && typeof it === 'object') v = fromHistory(v, it as Ev);
  return v;
}

export function apply(v: View, ev: Ev): View {
  switch (ev.type) {
    case 'snapshot':
      return applySnapshot(ev, v);
    case 'history':
      return { ...replaceWith((ev.items as unknown[]) ?? [], v), busy: v.busy, status: v.status, ask: v.ask, upgrade: v.upgrade };
    case 'user':
      return pushUser(v, str(ev.content), ev.queued === true);
    case 'upgrade':
      return { ...v, upgrade: parseUpgrade(ev) };
    case 'turn':
      return { ...v, busy: true, status: 'working…' };
    case 'token':
      return { ...v, busy: true, streaming: (v.streaming ?? '') + str(ev.text) };
    case 'message': {
      const text = str(ev.content) || v.streaming || '';
      const w = { ...v, streaming: null };
      return text ? push(w, { kind: 'assistant', text }) : w;
    }
    case 'tool_call': {
      // A tool call ends the text streamed before it.
      let w = v;
      if (v.streaming) w = push({ ...v, streaming: null }, { kind: 'assistant', text: v.streaming });
      // ask_user's call and result arrive once it is answered: they are what
      // shows the question and the answer, live as from history.
      if (ev.tool === 'ask_user') return pushQuestion(w, ev.args);
      return push(w, { kind: 'tool', title: '⚙ ' + str(ev.tool), body: JSON.stringify(ev.args ?? {}, null, 2) });
    }
    case 'tool_result':
      if (ev.tool === 'ask_user') return push(v, { kind: 'user', text: str(ev.result) });
      return push(v, { kind: 'tool', title: '↳ ' + str(ev.tool), body: str(ev.result) });
    case 'done':
      if (ev.reason === 'interrupted') return interrupted(v, str(ev.detail));
      // Messages queued into the turn have been read by it.
      return { ...settleQueued(v), busy: false, status: '', streaming: null };
    case 'error':
      return push({ ...settleQueued(v), busy: false, status: '', streaming: null }, { kind: 'note', text: 'error: ' + str(ev.error), error: true });
    case 'ask':
      return { ...v, ask: { question: str(ev.question), options: (ev.options as string[]) ?? [], recommended: str(ev.recommended) || undefined } };
    case 'answered':
      // The question and the answer are shown from ask_user's call and result.
      return { ...v, ask: null };
    case 'notification':
      if (ev.kind === 'docs') return push(v, { kind: 'note', text: docsNote(ev) });
      return ev.text ? push(v, { kind: 'note', text: '🔔 ' + str(ev.text) }) : v;
    case 'commands':
      return { ...v, commands: (ev.items as Command[]) ?? [] };
    case 'models':
      return { ...v, models: (ev.providers as ModelProvider[]) ?? [] };
    case 'mcp_tools': {
      // No server: the cost listing, a note only. A list keeps the last one
      // when an answer carries none (a refused toggle names why).
      const tools = (ev.tools as McpTool[] | undefined) ?? [];
      const server = str(ev.server);
      const w = server && (tools.length || v.mcpTools?.server !== server) ? { ...v, mcpTools: { server, tools } } : v;
      return ev.message ? push(w, { kind: 'note', text: str(ev.message) }) : w;
    }
    case 'settings':
      return { ...v, settings: { model: str(ev.model), provider: str(ev.provider), url: str(ev.url) } };
    case 'catalog':
      return { ...v, skills: ((ev.skills as Skill[]) ?? []).map((k) => ({ name: k.name, description: k.description })) };
    case 'control': {
      let w = ev.id === 'close' && str(ev.message).startsWith('closed') ? { ...v, closed: true } : v;
      // No turn to interrupt: drop the "interrupting…".
      if (ev.id === 'interrupt' && w.status === 'interrupting…') w = { ...w, status: '' };
      return ev.message ? push(w, { kind: 'note', text: str(ev.message) }) : w;
    }
    case 'server':
      return ev.message ? push(v, { kind: 'note', text: str(ev.message) }) : v;
    case 'reset':
      return { ...empty, commands: v.commands, skills: v.skills, models: v.models, settings: v.settings, upgrade: v.upgrade, n: v.n };
    case 'engine':
      if (ev.state === 'started') return v;
      return push({ ...v, busy: false, ask: ev.state === 'restarting' ? null : v.ask },
        { kind: 'note', text: `engine ${str(ev.state)}${ev.reason ? ': ' + str(ev.reason) : ''}`, error: ev.state === 'gave_up' });
    case 'retry':
      return { ...v, status: str(ev.text) || 'retrying…' };
    case 'queued':
      return { ...v, status: `${Number(ev.count ?? 0)} message(s) queued for the running turn` };
  }
  return v;
}

/** The line an interrupted turn ends with (the TUI's too). */
export const interruptedNote = '⏹ interrupted — your next message continues from here';

/** What the screen shows from pressing Stop until the turn ends. */
export function interrupting(v: View): View {
  return v.busy ? { ...v, status: 'interrupting…' } : v;
}

/**
 * A turn the user stopped: the reply streamed so far stays (the engine keeps
 * it too), and a note ends the turn. Queued messages stay marked: they were
 * not read by the cut turn, they start the next one. detail names the jobs
 * the cut turn started that are still running, and how to stop them.
 */
function interrupted(v: View, detail: string): View {
  let w: View = { ...v, busy: false, status: '', streaming: null };
  if (v.streaming?.trim()) w = push(w, { kind: 'assistant', text: v.streaming });
  w = push(w, { kind: 'note', text: interruptedNote });
  return detail ? push(w, { kind: 'note', text: detail }) : w;
}

/** /jobs kill all ends every running job: the screen asks first. */
export function isKillAll(line: string): boolean {
  return /^\/?jobs\s+kill\s+all\s*$/i.test(line.trim());
}

/** A snapshot: the conversation, then the latest state events. */
export function applySnapshot(snap: Ev, keep?: View): View {
  let v = replaceWith((snap.items as unknown[]) ?? [], keep);
  for (const s of (snap.state as Ev[]) ?? []) if (s.type !== 'history') v = apply(v, s);
  return v;
}

/**
 * What a typed line means: a slash command for the engine, a skill (sent as
 * a message asking the model to use it, as the TUI does), or a message.
 */
export type Line =
  | { kind: 'message'; text: string }
  | { kind: 'slash'; line: string }
  | { kind: 'skill'; text: string }
  | { kind: 'provider'; cmd: 'model' | 'login' | 'logout'; args: string[] }
  | { kind: 'mcpadd' }
  | { kind: 'unknown'; name: string };

export function parseLine(raw: string, skills: Skill[]): Line {
  const text = raw.trim();
  if (!text.startsWith('/')) return { kind: 'message', text };
  const [head = '', ...rest] = text.slice(1).split(/\s+/);
  const name = head.toLowerCase();
  if (name === 'skill') {
    const [skill, ...words] = rest;
    if (!skill || !skills.some((k) => k.name === skill)) return { kind: 'unknown', name: 'skill ' + (skill ?? '') };
    const ask = `Use the skill ${skill} (call skill name=${skill} first)`;
    return { kind: 'skill', text: words.length ? `${ask}: ${words.join(' ')}` : ask + '.' };
  }
  // The model picker, the login form and the add-server form are the
  // client's (the TUI has its own of each).
  if (name === 'model' || name === 'login' || name === 'logout') return { kind: 'provider', cmd: name, args: rest };
  if (name === 'mcp' && rest[0]?.toLowerCase() === 'add') return { kind: 'mcpadd' };
  if (!name) return { kind: 'unknown', name: '' };
  return { kind: 'slash', line: text.slice(1) };
}

/** Palette rows for what has been typed after '/'. */
export function suggest(typed: string, commands: Command[], skills: Skill[]): { insert: string; label: string; hint: string }[] {
  const q = typed.replace(/^\//, '').toLowerCase();
  const [head = '', arg] = q.split(/\s+/, 2);
  const out: { insert: string; label: string; hint: string }[] = [];
  if (arg === undefined) {
    for (const c of commands) if (c.name.startsWith(head)) out.push({ insert: '/' + c.name + ' ', label: '/' + c.name + (c.usage ? ' ' + c.usage : ''), hint: c.description });
    if ('skill'.startsWith(head)) out.push({ insert: '/skill ', label: '/skill <name> [text]', hint: 'ask the model to use a skill' });
    if ('model'.startsWith(head)) out.push({ insert: '/model', label: '/model [search] | use <provider> <id>', hint: 'switch model: a searchable list from every logged-in provider' });
    if ('login'.startsWith(head)) out.push({ insert: '/login', label: '/login [name]', hint: 'log in to an LLM provider: an OpenAI-compatible URL, bearer optional' });
    if ('logout'.startsWith(head)) out.push({ insert: '/logout ', label: '/logout <provider>', hint: 'log out of an LLM provider' });
    return out;
  }
  if (head === 'skill') {
    for (const k of skills) if (k.name.startsWith(arg)) out.push({ insert: `/skill ${k.name} `, label: k.name, hint: k.description });
    return out;
  }
  const c = commands.find((x) => x.name === head);
  for (const ch of c?.choices ?? []) if (ch.value.startsWith(arg)) out.push({ insert: `/${head} ${ch.value}`, label: ch.value, hint: ch.description ?? '' });
  return out;
}

/** Model rows matching every word of the query (in the provider or the id). */
export function filterModels(ps: ModelProvider[], query: string): { provider: string; url: string; id: string }[] {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  const out: { provider: string; url: string; id: string }[] = [];
  for (const p of ps)
    for (const id of p.ids ?? []) {
      const hay = (p.name + ' ' + id).toLowerCase();
      if (words.every((w) => hay.includes(w))) out.push({ provider: p.name, url: p.url, id });
    }
  return out;
}
