// A session's events, folded into what a screen shows. Pure: the screen
// feeds it what /api/graphql sessionEvents returns, in order.
//
// The events are the protocol's flat JSON frames (dun proto/events.go); only
// the fields read here are relied on.

import { noBackground, noteBackground, type Background } from './background';
import type { McpTool, McpTools } from './mcp';
import * as notices from './notices';
import { endsThinkingOld, endThought, evTime, liveThought, type Thought } from './thinking';
import { agentOf, withCall, withResult, withThought, type Run } from './toolcalls';
import { autoResumePrefix, parseUpgrade, type Upgrade } from './upgrade';

export type Item =
  // id: the user event's (dun delivery.go); delivered and withdrawn name it.
  // lost: queued, and its engine stopped before delivering it.
  | { key: string; kind: 'user'; text: string; queued?: boolean; id?: string; lost?: boolean }
  | { key: string; kind: 'assistant'; text: string }
  // Consecutive tool calls, each merged with its result (toolcalls.ts).
  | Run
  | { key: string; kind: 'note'; text: string; error?: boolean }
  | ({ key: string; kind: 'thinking' } & Thought)
  // callId: the ask_user call asking it, which history's tool_call and the ask event both name.
  | {
      key: string;
      kind: 'question';
      text: string;
      options: string[];
      recommended?: string;
      recommendedAll?: string[];
      multi?: boolean;
      callId?: string;
    }
  // What the engine says beside the conversation (notices.ts): it ends a run.
  // agent: the sub-agent it is from or about (the engine's `agent`), which a tap opens.
  | { key: string; kind: 'notice'; notice: notices.Notice; agent?: number };

/**
 * The open question (proto.Ask). askId names it in the answer. multi: pick
 * any number (recommendedAll are checked to start with). noCustom: the
 * answer must be one of the options, no own words.
 */
export type Ask = {
  askId?: string;
  question: string;
  options: string[];
  recommended?: string;
  recommendedAll: string[];
  multi: boolean;
  noCustom: boolean;
};

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

export type ModelProvider = {
  name: string;
  url: string;
  ids: string[] | null;
  error?: string;
  default?: boolean;
  key?: boolean;
};
export type Settings = { model: string; provider: string; url: string };

export type View = {
  items: Item[];
  /** The reply streaming in, before its message event settles it. */
  streaming: string | null;
  /** turn_state's phase is not idle. */
  busy: boolean;
  status: string;
  ask: Ask | null;
  /** The session's turn_state (the engine's; dund's when the engine stopped); null before the first. */
  turn: Turn | null;
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
  /** Agents, jobs, the plan, the context figures, the turn's tool and clock (background.ts). */
  bg: Background;
  /** The sub-agent whose conversation is open (agent view), its items folded like the session's. */
  agentView: AgentView | null;
  /** The engine stamps rounds (reasoning, token, tool_call carry `round`): reasoning_end closes a thought. */
  rounds: boolean;
  n: number; // key counter
};

/** proto.TurnState as the client keeps it (docs/session-events.md). */
export type Turn = {
  phase: 'idle' | 'working' | 'waiting_answer' | 'interrupting' | 'retrying';
  /** When the turn began (ms), 0 idle. */
  since: number;
  /** The provider wait's line, '' when none. */
  retry: string;
  queued: { id: string; text: string }[];
  /** News that came while idle and started no turn (no ask open, the hour's automatic turns used). */
  notes?: number;
  /** Waiting for the network to continue an ask (dun harness netwait.go): since and next probe, ms. */
  net?: { since: number; next: number };
};

export type AgentView = { id: number; prompt: string; loaded: boolean; view: View };

export const empty: View = {
  items: [],
  streaming: null,
  busy: false,
  status: '',
  ask: null,
  turn: null,
  commands: [],
  skills: [],
  models: null,
  settings: null,
  mcpTools: null,
  closed: false,
  upgrade: null,
  bg: noBackground,
  agentView: null,
  rounds: false,
  n: 0,
};

type Ev = Record<string, unknown>;
const str = (v: unknown) => (typeof v === 'string' ? v : '');

/** An item before it has a key (Omit spread over each member of the union). */
type WithoutKey<T> = T extends unknown ? Omit<T, 'key'> : never;
type NewItem = WithoutKey<Item>;

function push(v: View, item: NewItem): View {
  return { ...v, n: v.n + 1, items: [...v.items, { ...item, key: String(v.n) }] };
}

/** A notice, with the sub-agent it is from or about when the engine named one. */
function pushNotice(v: View, notice: notices.Notice, agent: number | undefined): View {
  return push(v, { kind: 'notice', notice, ...(agent ? { agent } : {}) });
}

/** A tool call that carried a message: the message, not a row in the run. */
function pushCarried(v: View, tool: string, args: unknown, id: string): View | null {
  const notice = notices.carriedMsg(tool, args, id || undefined);
  return notice ? push(v, { kind: 'notice', notice }) : null;
}

/** That call's result: the message's footer; null when the result is a tool row's. */
function settleCarried(v: View, tool: string, result: string, id: string): View | null {
  const i = notices.pendingMsg(v.items, tool, id || undefined);
  const it = v.items[i];
  if (it?.kind !== 'notice' || it.notice.type !== 'msg') return null;
  const items = [...v.items];
  items[i] = { ...it, notice: { ...notices.settleMsg(it.notice, result), type: 'msg' } };
  return { ...v, items };
}

/** A tool call or result merged into the runs (toolcalls.ts); a new run takes the next key. */
function merge(v: View, r: { items: Item[]; added: boolean }): View {
  return r.added ? { ...v, items: r.items, n: v.n + 1 } : { ...v, items: r.items };
}

/** A user message; dund's automatic resume after a restart is a note, not the person's. */
function pushUser(v: View, text: string, queued?: boolean, id?: string): View {
  if (text.startsWith(autoResumePrefix))
    return push(v, { kind: 'note', text: '↻ resumed after the restart — ' + text.slice(autoResumePrefix.length) });
  return push(v, { kind: 'user', text, ...(queued ? { queued } : {}), ...(id ? { id } : {}) });
}

const QUEUED_STATUS = ' message(s) queued for the running turn';

const isQueued = (i: Item, id: string) => i.kind === 'user' && (i.queued === true || i.lost === true) && i.id === id;

/**
 * delivered: the engine says these queued messages reached the model. Each
 * becomes an ordinary message after what is shown so far — where history
 * puts it (after the tool result that carried it), so a reconnect shows the
 * same order.
 */
function deliver(v: View, ids: string[]): View {
  let w = v;
  for (const id of ids) {
    const it = w.items.find((i) => isQueued(i, id));
    if (it?.kind !== 'user') continue;
    w = pushUser({ ...w, items: w.items.filter((i) => i !== it) }, it.text, false, id);
  }
  return w;
}

/** withdrawn: queued messages taken back; their rows go, on every client. */
function withdraw(v: View, ids: string[]): View {
  const items = v.items.filter((i) => !ids.some((id) => isQueued(i, id)));
  return items.length === v.items.length ? v : { ...v, items };
}

/**
 * Fallback for an event log from before message ids: a queued row with no
 * id can get no delivered event, so the turn's end settles it. Rows with an
 * id wait for the engine's word.
 */
function settleUnnamedQueued(v: View): View {
  const old = (i: Item) => i.kind === 'user' && i.queued === true && !i.id;
  if (!v.items.some(old)) return v;
  return { ...v, items: v.items.map((i) => (old(i) ? { ...i, queued: false } : i)) };
}

/** is_error: the engine omits false, so with a call_id an absent one is false; without, unknown (an old log). */
const errorOf = (e: Ev): boolean | undefined =>
  typeof e.is_error === 'boolean' ? e.is_error : e.call_id ? false : undefined;

/** An ask event, or turn_state's ask. */
function parseAsk(a: Ev): Ask {
  const all = Array.isArray(a.recommended_all) ? (a.recommended_all as string[]) : [];
  const rec = all[0] ?? (str(a.recommended) || undefined);
  const options = (a.options as string[]) ?? [];
  return {
    ...(str(a.ask_id) ? { askId: str(a.ask_id) } : {}),
    question: str(a.question),
    options,
    recommended: rec,
    recommendedAll: all.length ? all : rec ? [rec] : [],
    multi: a.multi === true,
    // Meaningless without options to pick from (the TUI's rule).
    noCustom: a.no_custom === true && options.length > 0,
  };
}

const phases = ['idle', 'working', 'waiting_answer', 'interrupting', 'retrying'] as const;

/**
 * turn_state: where the session's turn is, from the engine (dun
 * cmd/dun/turnstate.go). busy, the status line, the open question, a dropped
 * half-streamed reply and the queued rows are read from it, never inferred
 * from tokens, tool calls or done.
 */
function applyTurnState(v: View, ev: Ev): View {
  const phase = phases.find((p) => p === ev.phase) ?? 'idle';
  const r = (ev.retry ?? null) as Ev | null;
  const queued = (Array.isArray(ev.queued) ? (ev.queued as Ev[]) : []).map((q) => ({
    id: str(q.id),
    text: str(q.text),
  }));
  const turn: Turn = {
    phase,
    since: Number(ev.since ?? 0),
    retry: r ? str(r.text) || 'retrying…' : '',
    queued,
    notes: Number(ev.notes_waiting ?? 0),
  };
  const nw = (ev.net_wait ?? null) as Ev | null;
  if (nw) turn.net = { since: Number(nw.since ?? 0), next: Number(nw.next ?? 0) };
  const status =
    phase === 'interrupting'
      ? 'interrupting…'
      : phase === 'retrying'
        ? turn.retry
        : queued.length
          ? `${queued.length}${QUEUED_STATUS}`
          : phase === 'idle'
            ? ''
            : 'working…';
  let ask = v.ask;
  if (phase === 'waiting_answer' && ev.ask && typeof ev.ask === 'object') ask = v.ask ?? parseAsk(ev.ask as Ev);
  else if (phase !== 'interrupting') ask = null;
  // A round whose stream died is streamed again: its text goes.
  const streaming = phase === 'retrying' && ev.discard === true ? null : v.streaming;
  const w: View = { ...v, busy: phase !== 'idle', status, ask, streaming, turn };
  return reconcileQueued(w, queued);
}

/**
 * The queued rows follow the state's queue: one it lists that is not shown
 * (a client that arrived late) is added; with no turn and an empty queue, a
 * row still queued was lost with its engine (delivered and withdrawn come
 * before the state that drops a row).
 */
function reconcileQueued(v: View, queued: { id: string; text: string }[]): View {
  let w = v;
  for (const q of queued) {
    const at = w.items.findIndex((i) => i.kind === 'user' && i.id === q.id);
    const it = w.items[at];
    if (at < 0) w = pushUser(w, q.text, true, q.id);
    else if (it?.kind === 'user' && it.lost) {
      const items = [...w.items];
      items[at] = { ...it, queued: true, lost: false };
      w = { ...w, items };
    }
  }
  if (queued.length || w.busy || !w.items.some((i) => i.kind === 'user' && i.queued && i.id)) return w;
  return {
    ...w,
    items: w.items.map((i) => (i.kind === 'user' && i.queued && i.id ? { ...i, queued: false, lost: true } : i)),
  };
}

const ids = (v: unknown) => (Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : []);

/** An ask_user call (history) as the question it is, not as JSON. */
function pushQuestion(v: View, args: unknown, callId: string): View {
  const a = (args ?? {}) as Ev;
  const rec = Array.isArray(a.recommended)
    ? (a.recommended as string[])
    : str(a.recommended)
      ? [str(a.recommended)]
      : [];
  return push(v, {
    kind: 'question',
    text: str(a.question),
    options: (a.options as string[]) ?? [],
    recommended: rec[0],
    recommendedAll: rec,
    multi: a.multi === true,
    ...(callId ? { callId } : {}),
  });
}

/**
 * The ask event: the question row, live (history shows it from the ask_user
 * call). A row its call already has (a snapshot's history) is not repeated.
 */
function askRow(v: View, a: Ask, callId: string): View {
  if (callId && v.items.some((i) => i.kind === 'question' && i.callId === callId)) return v;
  return push(v, {
    kind: 'question',
    text: a.question,
    options: a.options,
    recommended: a.recommended,
    recommendedAll: a.recommendedAll,
    multi: a.multi,
    ...(callId ? { callId } : {}),
  });
}

/** A waiting login notice for server, settled (failed: error). legacyOnly: only one from an engine before phases. */
function loginDone(i: Item, server: string, error = '', legacyOnly = false): Item {
  if (i.kind !== 'notice' || i.notice.type !== 'login' || i.notice.server !== server || i.notice.done) return i;
  if (legacyOnly && !i.notice.legacy) return i;
  return { ...i, notice: { ...i.notice, done: true, ...(error ? { error } : {}) } };
}

/** mcp_login: a login started (a notice), or ended (its notice settled), as the engine or dund says. */
function mcpLogin(v: View, ev: Ev): View {
  const server = str(ev.server);
  if (ev.phase !== 'done' && ev.phase !== 'failed')
    return push(v, { kind: 'notice', notice: notices.mcpLoginNotice(ev) });
  const error = ev.phase === 'failed' ? str(ev.error) || 'the login failed' : '';
  const items = v.items.map((i) => loginDone(i, server, error));
  if (items.some((x, k) => x !== v.items[k])) return { ...v, items };
  // No waiting notice (this client came later): the outcome on its own.
  return push(v, {
    kind: 'notice',
    notice: { type: 'login', server, url: '', done: true, ...(error ? { error } : {}) },
  });
}

/** A history item (history event, snapshot) as a shown item. */
function fromHistory(v: View, it: Ev): View {
  switch (it.kind) {
    case 'user':
      return pushUser(v, str(it.content), false, str(it.id) || undefined);
    case 'assistant':
      return push(v, { kind: 'assistant', text: str(it.content) });
    case 'tool_call':
      if (it.tool === 'ask_user') return pushQuestion(v, it.args, str(it.call_id));
      return (
        pushCarried(v, str(it.tool), it.args, str(it.call_id)) ??
        merge(v, withCall(v.items, str(it.tool), it.args, str(it.call_id) || undefined, String(v.n), agentOf(it.agent)))
      );
    case 'tool_result':
      if (it.tool === 'ask_user') return push(v, { kind: 'user', text: str(it.content) });
      return (
        settleCarried(v, str(it.tool), str(it.content), str(it.call_id)) ??
        merge(
          v,
          withResult(
            v.items,
            str(it.tool),
            str(it.content),
            str(it.call_id) || undefined,
            String(v.n),
            errorOf(it),
            agentOf(it.agent),
          ),
        )
      );
    case 'notification':
      return pushNotice(v, notices.fromHistoryNotification(str(it.content)), agentOf(it.agent));
    case 'reasoning':
      return pushThought(v, { text: str(it.content), start: 0, end: 0, live: false });
    case 'command':
      return push(v, { kind: 'notice', notice: notices.commandNotice(str(it.content)) });
  }
  return v;
}

/** Replace what is shown with a conversation (history replaces, never adds). */
export function replaceWith(items: unknown[], keep?: View): View {
  let v: View = {
    ...empty,
    commands: keep?.commands ?? [],
    skills: keep?.skills ?? [],
    models: keep?.models ?? null,
    settings: keep?.settings ?? null,
    mcpTools: keep?.mcpTools ?? null,
    agentView: keep?.agentView ?? null,
    rounds: keep?.rounds ?? false,
  };
  for (const it of items) if (it && typeof it === 'object') v = fromHistory(v, it as Ev);
  return v;
}

export function apply(v: View, ev: Ev): View {
  // A state event a snapshot carries (replayed: true) is state, not news: it
  // adds no row (no note, no notice), so a reconnect does not repeat them.
  // turn_state's queued rows are the state itself.
  if (ev.replayed === true && ev.type !== 'turn_state') {
    const w = applyNew(v, ev);
    return { ...w, items: v.items, n: v.n };
  }
  return applyNew(v, ev);
}

function applyNew(v: View, ev: Ev): View {
  const bg = noteBackground(v.bg, ev);
  if (bg !== v.bg) v = { ...v, bg };
  v = applyMore(v, ev);
  // The round's thinking is over: the engine says so (lib/thinking.ts).
  if (typeof ev.round === 'number' && !v.rounds) v = { ...v, rounds: true };
  if (ev.type === 'reasoning_end') v = { ...v, items: endThought(v.items, evTime(ev), ev.cut === true) };
  else if (!v.rounds && endsThinkingOld.has(str(ev.type)))
    // Fallback, an event log from before round ids.
    v = { ...v, items: endThought(v.items, evTime(ev), ev.type === 'done' && ev.reason === 'interrupted') };
  switch (ev.type) {
    case 'reasoning':
      return addThinking(v, str(ev.text), evTime(ev), typeof ev.round === 'number' ? ev.round : undefined);
    case 'snapshot':
      return applySnapshot(ev, v);
    case 'history':
      // dund says why (docs/session-events.md, "History reason"). A restart's
      // is the conversation already shown: kept, with the lines added since
      // (the restart's own notice). resume, model_switch, or no reason (a
      // dund before reasons): replaced.
      if (ev.reason === 'restart' && v.items.length > 0) return v;
      return {
        ...replaceWith((ev.items as unknown[]) ?? [], v),
        busy: v.busy,
        status: v.status,
        ask: v.ask,
        turn: v.turn,
        upgrade: v.upgrade,
        bg: v.bg,
        agentView: v.agentView,
      };
    case 'user':
      return pushUser(v, str(ev.content), ev.queued === true, str(ev.id) || undefined);
    case 'delivered':
      return deliver(v, ids(ev.ids));
    case 'withdrawn':
      return withdraw(v, ids(ev.ids));
    case 'upgrade':
      return { ...v, upgrade: parseUpgrade(ev) };
    case 'turn_state':
      return applyTurnState(v, ev);
    case 'token':
      return { ...v, streaming: (v.streaming ?? '') + str(ev.text) };
    case 'message': {
      const text = str(ev.content) || v.streaming || '';
      const w = { ...v, streaming: null };
      return text ? push(w, { kind: 'assistant', text }) : w;
    }
    case 'tool_call': {
      // A tool call ends the text streamed before it.
      let w = v;
      if (v.streaming) w = push({ ...v, streaming: null }, { kind: 'assistant', text: v.streaming });
      // ask_user live: the ask event shows the question, answered the answer.
      if (ev.tool === 'ask_user') return w;
      return (
        pushCarried(w, str(ev.tool), ev.args, str(ev.call_id)) ??
        merge(w, withCall(w.items, str(ev.tool), ev.args, str(ev.call_id) || undefined, String(w.n), agentOf(ev.agent)))
      );
    }
    case 'tool_result': {
      // A queued message it carried is settled by the delivered event after it.
      const w = v;
      if (ev.tool === 'ask_user') return w;
      return (
        settleCarried(w, str(ev.tool), str(ev.result), str(ev.call_id)) ??
        merge(
          w,
          withResult(
            w.items,
            str(ev.tool),
            str(ev.result),
            str(ev.call_id) || undefined,
            String(w.n),
            errorOf(ev),
            agentOf(ev.agent),
          ),
        )
      );
    }
    case 'done':
      if (ev.reason === 'interrupted') return interrupted(v, str(ev.detail));
      // Busy and the status are turn_state's, which follows.
      return { ...settleUnnamedQueued(v), streaming: null };
    case 'error':
      return push(
        { ...settleUnnamedQueued(v), streaming: null },
        { kind: 'note', text: 'error: ' + str(ev.error), error: true },
      );
    case 'ask': {
      const a = parseAsk(ev);
      return { ...askRow(v, a, str(ev.call_id)), ask: a };
    }
    case 'answered': {
      // The open question's answer, whoever gave it. One naming another
      // question closes nothing here.
      const id = str(ev.ask_id);
      if (id && v.ask?.askId && id !== v.ask.askId) return v;
      return push({ ...v, ask: null }, { kind: 'user', text: str(ev.value) });
    }
    case 'notification':
      return pushNotice(v, notices.fromNotification(ev), agentOf(ev.agent));
    case 'recap':
      return push(v, { kind: 'notice', notice: notices.recapNotice(ev) });
    case 'compaction':
      return push(v, { kind: 'notice', notice: notices.compactionNotice(ev) });
    case 'mcp_login':
      return mcpLogin(v, ev);
    case 'agent_told': {
      const n = notices.agentToldNotice(ev);
      return n ? push(v, { kind: 'notice', notice: n }) : v;
    }
    case 'commands':
      return { ...v, commands: (ev.items as Command[]) ?? [] };
    case 'models':
      return { ...v, models: (ev.providers as ModelProvider[]) ?? [] };
    case 'mcp_tools': {
      // No server: the cost listing, a note only. A list keeps the last one
      // when an answer carries none (a refused toggle names why).
      const tools = (ev.tools as McpTool[] | undefined) ?? [];
      const server = str(ev.server);
      let w = server && (tools.length || v.mcpTools?.server !== server) ? { ...v, mcpTools: { server, tools } } : v;
      // Fallback, an engine before login phases: the server lists tools, so
      // its login (if it waited on one) went through.
      if (server && tools.length) w = { ...w, items: w.items.map((i) => loginDone(i, server, '', true)) };
      return ev.message ? push(w, { kind: 'note', text: str(ev.message) }) : w;
    }
    case 'settings':
      return { ...v, settings: { model: str(ev.model), provider: str(ev.provider), url: str(ev.url) } };
    case 'catalog':
      return {
        ...v,
        skills: ((ev.skills as Skill[]) ?? []).map((k) => ({ name: k.name, description: k.description })),
      };
    case 'control': {
      const w = ev.id === 'close' && str(ev.message).startsWith('closed') ? { ...v, closed: true } : v;
      return ev.message ? push(w, { kind: 'note', text: str(ev.message) }) : w;
    }
    case 'server':
      return ev.message ? push(v, { kind: 'note', text: str(ev.message) }) : v;
    case 'reset':
      return {
        ...empty,
        commands: v.commands,
        skills: v.skills,
        models: v.models,
        settings: v.settings,
        upgrade: v.upgrade,
        // The stop conditions outlive a /clear (dun harness/until.go).
        bg: { ...noBackground, until: v.bg.until },
        n: v.n,
      };
    case 'engine':
      if (ev.state === 'started') return v;
      // Busy and the question are turn_state's: dund sends idle for a stopped engine.
      return push(
        { ...v },
        {
          kind: 'note',
          text: `engine ${str(ev.state)}${ev.reason ? ': ' + str(ev.reason) : ''}`,
          error: ev.state === 'gave_up',
        },
      );
  }
  return v;
}

/** A reasoning delta: it opens the round's thought if none is live, and grows it. */
function addThinking(v: View, delta: string, at: number, round?: number): View {
  let i = liveThought(v.items);
  let w = v;
  // Another round's thinking: the live one is over (its reasoning_end was missed).
  if (i >= 0 && round !== undefined && liveRound(v.items[i]!) !== round) {
    w = { ...v, items: endThought(v.items, at) };
    i = -1;
  }
  if (i < 0)
    return pushThought(w, { text: delta, start: at, end: 0, live: true, ...(round !== undefined ? { round } : {}) });
  const items = [...v.items];
  const it = items[i]!;
  if (it.kind === 'tools') {
    const th = (it.thoughts ?? []).slice();
    const t = th[th.length - 1]!;
    th[th.length - 1] = { ...t, text: t.text + delta };
    items[i] = { ...it, thoughts: th };
  } else {
    const t = it as Extract<Item, { kind: 'thinking' }>;
    items[i] = { ...t, text: t.text + delta };
  }
  return { ...w, items };
}

/** The round of the live thought in it (a block of its own, or a run's last). */
function liveRound(it: Item): number | undefined {
  if (it.kind === 'tools') return it.thoughts?.at(-1)?.round;
  if (it.kind === 'thinking') return it.round;
  return undefined;
}

/** A thought: inside the open run, as the TUI does (thinking.go addThought), else a block of its own. */
function pushThought(v: View, t: Thought): View {
  const items = withThought(v.items, t);
  return items ? { ...v, items } : push(v, { kind: 'thinking', ...t });
}

/** The line an interrupted turn ends with (the TUI's too). */
export const interruptedNote = '⏹ interrupted — your next message continues from here';

/**
 * A turn the user stopped: the reply streamed so far stays (the engine keeps
 * it too), and a note ends the turn. Queued messages stay marked: they were
 * not read by the cut turn, they start the next one. detail names the jobs
 * the cut turn started that are still running, and how to stop them.
 */
function interrupted(v: View, detail: string): View {
  let w: View = { ...v, streaming: null };
  if (v.streaming?.trim()) w = push(w, { kind: 'assistant', text: v.streaming });
  w = push(w, { kind: 'note', text: interruptedNote });
  return detail ? push(w, { kind: 'note', text: detail }) : w;
}

/** /jobs kill all ends every running job: the screen asks first. */
export function isKillAll(line: string): boolean {
  return /^\/?jobs\s+kill\s+all\s*$/i.test(line.trim());
}

/**
 * What apply does besides its switch: a question the turn asked ends with the
 * turn, however it ended (the TUI's done); the open sub-agent view follows
 * its agent_history (which replaces) and agent_entry (which adds) events, and
 * ignores another agent's, as the TUI's scope does.
 */
function applyMore(v: View, ev: Ev): View {
  switch (ev.type) {
    case 'done':
      return v.ask ? { ...v, ask: null } : v;
    case 'agent_history': {
      const a = v.agentView;
      if (!a || ev.agent !== a.id) return v;
      const view = replaceWith((ev.items as unknown[]) ?? []);
      return { ...v, agentView: { ...a, prompt: str(ev.prompt) || a.prompt, loaded: true, view } };
    }
    case 'agent_entry': {
      const a = v.agentView;
      if (!a || ev.agent !== a.id) return v;
      let view = a.view;
      for (const it of (ev.items as unknown[]) ?? [])
        if (it && typeof it === 'object') view = fromHistory(view, it as Ev);
      return { ...v, agentView: { ...a, view } };
    }
  }
  return v;
}

/** Opens sub-agent id's conversation: its history fills it once the agent view's answer arrives. */
export function watchAgent(v: View, id: number, prompt = ''): View {
  return { ...v, agentView: { id, prompt, loaded: false, view: empty } };
}

export const unwatchAgent = (v: View): View => ({ ...v, agentView: null });

/** The person's last message, for the header ('' before one). */
export function lastUser(v: View): string {
  for (let i = v.items.length - 1; i >= 0; i--) {
    const it = v.items[i]!;
    if (it.kind === 'user') return it.text;
  }
  return '';
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
export function suggest(
  typed: string,
  commands: Command[],
  skills: Skill[],
): { insert: string; label: string; hint: string }[] {
  const q = typed.replace(/^\//, '').toLowerCase();
  const [head = '', arg] = q.split(/\s+/, 2);
  const out: { insert: string; label: string; hint: string }[] = [];
  if (arg === undefined) {
    for (const c of commands)
      if (c.name.startsWith(head))
        out.push({
          insert: '/' + c.name + ' ',
          label: '/' + c.name + (c.usage ? ' ' + c.usage : ''),
          hint: c.description,
        });
    if ('skill'.startsWith(head))
      out.push({ insert: '/skill ', label: '/skill <name> [text]', hint: 'ask the model to use a skill' });
    if ('model'.startsWith(head))
      out.push({
        insert: '/model',
        label: '/model [search] | use <provider> <id>',
        hint: 'switch model: a searchable list from every logged-in provider',
      });
    if ('login'.startsWith(head))
      out.push({
        insert: '/login',
        label: '/login [name]',
        hint: 'log in to an LLM provider: an OpenAI-compatible URL, bearer optional',
      });
    if ('logout'.startsWith(head))
      out.push({ insert: '/logout ', label: '/logout <provider>', hint: 'log out of an LLM provider' });
    return out;
  }
  if (head === 'skill') {
    for (const k of skills)
      if (k.name.startsWith(arg)) out.push({ insert: `/skill ${k.name} `, label: k.name, hint: k.description });
    return out;
  }
  const c = commands.find((x) => x.name === head);
  for (const ch of c?.choices ?? [])
    if (ch.value.startsWith(arg))
      out.push({ insert: `/${head} ${ch.value}`, label: ch.value, hint: ch.description ?? '' });
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
