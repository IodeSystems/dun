// dund's coordinated upgrade, as the web client shows it (proto.UpgradeStatus).
// Pure: the screen folds the upgrade event into View.upgrade (events.ts) and
// renders these lines as a banner.

export type UpgradeWait = { session: string; what: string; id?: number; detail?: string; hint?: string };
export type Upgrade = { state: 'pending' | 'restarting'; version: string; deadline: number; waiting: UpgradeWait[] };

const str = (v: unknown) => (typeof v === 'string' ? v : '');

/** The upgrade event as state; null for anything else. */
export function parseUpgrade(ev: Record<string, unknown>): Upgrade | null {
  // 'failed': a service's dund could not put the new build in place and runs
  // on with nothing pending — the banner goes.
  if (ev.type !== 'upgrade' || ev.state === 'failed') return null;
  const waiting = Array.isArray(ev.waiting) ? (ev.waiting as Record<string, unknown>[]) : [];
  return {
    state: ev.state === 'restarting' ? 'restarting' : 'pending',
    version: str(ev.version),
    deadline: typeof ev.deadline === 'number' ? ev.deadline : 0,
    waiting: waiting.map((w) => ({
      session: str(w.session),
      what: str(w.what),
      id: typeof w.id === 'number' ? w.id : undefined,
      detail: str(w.detail) || undefined,
      hint: str(w.hint) || undefined,
    })),
  };
}

const build = (v: string) => v || 'a new build';

function hhmm(unix: number): string {
  const d = new Date(unix * 1000);
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}

/** One waited-for thing, and how to end it from this screen. */
function waitLine(w: UpgradeWait, session: string): string {
  const here = w.session === session;
  const where = here ? 'in this session' : `in session ${w.session} (another client)`;
  switch (w.what) {
    case 'turn':
      return here ? 'the turn in this session (Stop interrupts it)' : `a turn ${where}`;
    case 'question':
      return here ? 'a question in this session (answer it, or Stop)' : `a question ${where}`;
    case 'job':
      return `job #${w.id ?? '?'} ${w.detail ?? ''}`.trimEnd() + (here ? ` (/jobs kill ${w.id ?? ''})` : ` ${where}`);
    case 'agent':
      return `sub-agent #${w.id ?? '?'} ${where}`;
  }
  return `${w.what} ${where}`;
}

/** The banner's lines: the headline, then what dund waits for. */
export function upgradeLines(u: Upgrade | null, session: string): string[] {
  if (!u) return [];
  if (u.state === 'restarting') return [`⟳ dund is restarting into ${build(u.version)}… this reconnects`];
  if (!u.waiting.length) return [`⟳ dund is upgrading to ${build(u.version)} — restarting`];
  const head =
    `⟳ dund is upgrading to ${build(u.version)} — waiting for:` +
    (u.deadline ? ` (or it restarts at ${hhmm(u.deadline)})` : '');
  return [head, ...u.waiting.map((w) => '· ' + waitLine(w, session))];
}

/** Whether this session's own turn is among what dund waits for (the banner's Stop). */
export function waitsOnTurnHere(u: Upgrade | null, session: string): boolean {
  return (
    !!u &&
    u.state === 'pending' &&
    u.waiting.some((w) => w.session === session && (w.what === 'turn' || w.what === 'question'))
  );
}

/**
 * Whether the page should reload: the dund it reaches now is another build
 * than the one it loaded from (a restart into a new build serves a new
 * client). A dev build, or an unknown one, never reloads.
 */
export function shouldReload(loaded: string | null, now: string): boolean {
  if (!loaded || !now || loaded === 'dev' || now === 'dev') return false;
  return loaded !== now;
}

/** dund's automatic resume message (internal/dund resumePrefix). */
export const autoResumePrefix = '[automatic] ';
