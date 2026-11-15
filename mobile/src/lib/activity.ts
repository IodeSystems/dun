// Background work: sub-agents and jobs, as the TUI's activity zone shows them
// (internal/tui/activity.go agentActRow, jobActRow). The engine sends the
// whole list each time (agents, jobs events).

export type Agent = {
  id: number;
  model: string;
  state: string; // running | idle | stopped | …
  status: string;
  prompt: string;
  tokens: number;
  seconds: number;
  /** When the event that said so arrived (ms): a running agent's clock counts from here. */
  at: number;
};

export type Job = {
  id: number;
  command: string;
  state: string; // running | ok | failed | timeout | error | killed
  bytes: number;
  started: number; // unix seconds
  ended: number;
  code: number;
  muted: boolean;
};

const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
const str = (v: unknown) => (typeof v === 'string' ? v : '');
const list = (v: unknown) => (Array.isArray(v) ? (v as Record<string, unknown>[]) : []);

export function parseAgents(ev: Record<string, unknown>, at: number): Agent[] {
  return list(ev.agents).map((a) => ({
    id: num(a.id),
    model: str(a.model),
    state: str(a.state),
    status: str(a.status),
    prompt: str(a.prompt),
    tokens: num(a.tokens),
    seconds: num(a.seconds),
    at,
  }));
}

export function parseJobs(ev: Record<string, unknown>): Job[] {
  return list(ev.jobs).map((j) => ({
    id: num(j.id),
    command: str(j.command),
    state: str(j.state),
    bytes: num(j.bytes),
    started: num(j.started),
    ended: num(j.ended),
    code: num(j.code),
    muted: j.muted === true,
  }));
}

/** 42s, 3m07s (compactDur). */
export function compactDur(sec: number): string {
  sec = Math.max(0, Math.trunc(sec));
  if (sec < 60) return `${sec}s`;
  return `${Math.trunc(sec / 60)}m${String(sec % 60).padStart(2, '0')}s`;
}

/** 940t, 18.4kt (compactTokens). */
export const compactTokens = (n: number) => (n < 1000 ? `${n}t` : `${(n / 1000).toFixed(1)}kt`);

/** What a row's mark means, so the screen picks the colour. */
export type Tone = 'working' | 'done' | 'muted' | 'failed' | 'notice';

export type Row = {
  key: string;
  kind: 'agent' | 'job';
  id: number;
  mark: string;
  tone: Tone;
  label: string;
  detail: string;
  /** A failed job: the badge goes red. */
  attn: boolean;
  running: boolean;
};

export function agentElapsed(a: Agent, now: number): number {
  if (a.state !== 'running' || !a.at || now < a.at) return a.seconds;
  return a.seconds + Math.trunc((now - a.at) / 1000);
}

export function jobElapsed(j: Job, now: number): number {
  const end = j.ended || Math.trunc(now / 1000);
  if (!j.started || end < j.started) return 0;
  return end - j.started;
}

export function agentRow(a: Agent, now: number): Row {
  let mark = '·';
  let tone: Tone = 'muted';
  if (a.state === 'running') [mark, tone] = ['▸', 'working'];
  else if (a.state === 'idle') [mark, tone] = ['✓', 'notice'];
  else if (a.state === 'stopped') mark = '⏸';
  let label = `#${a.id} ${a.state}`;
  const s = agentElapsed(a, now);
  if (s > 0) label += ' ' + compactDur(s);
  if (a.tokens > 0) label += ' ' + compactTokens(a.tokens);
  // What it is doing beats what it was asked, once it has said anything.
  return {
    key: 'a' + a.id,
    kind: 'agent',
    id: a.id,
    mark,
    tone,
    label,
    detail: a.status || a.prompt,
    attn: false,
    running: a.state === 'running',
  };
}

export function jobRow(j: Job, now: number): Row {
  let mark = '·';
  let tone: Tone = 'muted';
  let state = j.state;
  switch (j.state) {
    case 'running':
      [mark, tone] = ['⚙', 'working'];
      break;
    case 'ok':
      [mark, tone] = ['✓', 'muted'];
      break;
    case 'failed':
      [mark, tone, state] = ['✗', 'failed', `failed (exit ${j.code})`];
      break;
    case 'timeout':
    case 'error':
      [mark, tone] = ['✗', 'failed'];
      break;
    case 'killed':
      mark = '⏹';
      break;
  }
  let label = `job ${j.id} ${state}`;
  const s = jobElapsed(j, now);
  if (s > 0) label += ' ' + compactDur(s);
  if (j.muted) label += ' muted';
  return {
    key: 'j' + j.id,
    kind: 'job',
    id: j.id,
    mark,
    tone,
    label,
    detail: j.command,
    attn: j.state === 'failed' || j.state === 'timeout' || j.state === 'error',
    running: j.state === 'running',
  };
}

export const rows = (agents: Agent[], jobs: Job[], now: number): Row[] => [
  ...agents.map((a) => agentRow(a, now)),
  ...jobs.map((j) => jobRow(j, now)),
];

const count = (n: number, one: string) => `${n} ${one}${n === 1 ? '' : 's'}`;

/**
 * The header badge: what runs now, whether a job failed, and its label, what
 * the sheet lists ("2 agents", "1 agent · 1 job"). Null when there is nothing
 * to show.
 */
export function badge(agents: Agent[], jobs: Job[]): { running: number; failed: boolean; label: string } | null {
  if (!agents.length && !jobs.length) return null;
  const running = agents.filter((a) => a.state === 'running').length + jobs.filter((j) => j.state === 'running').length;
  const failed = jobs.some((j) => j.state === 'failed' || j.state === 'timeout' || j.state === 'error');
  const label = [agents.length ? count(agents.length, 'agent') : '', jobs.length ? count(jobs.length, 'job') : '']
    .filter(Boolean)
    .join(' · ');
  return { running, failed, label };
}
