// The session list as dund orders it (running first, then last activity, see
// proto.SortSessions). The client shows that order and does not re-derive it:
// ordering is dund's rule, and a re-sort here would drift from the TUI's.
export type SessionRow = { id: string; workspace: string; running: boolean; lastActive?: string | null };

export function sessionRows<T extends SessionRow>(items: readonly T[] | null | undefined): T[] {
  return [...(items ?? [])];
}

// "5m ago" style age of an RFC3339 time; '' when unknown.
export function ago(iso: string | null | undefined, now: number = Date.now()): string {
  const t = iso ? Date.parse(iso) : NaN;
  if (Number.isNaN(t)) return '';
  const s = Math.max(0, Math.round((now - t) / 1000));
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}
