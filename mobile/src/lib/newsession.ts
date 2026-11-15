// The new-session picker's rules. dund decides what a typed path means
// (listDirs: how createSession reads it, and why it would refuse it) and
// which workspaces are recent (listWorkspaces); the client shows that.

export type RecentWorkspace = { path: string; lastActive?: string | null; running: boolean };
export type DirListing = {
  workspace: string;
  problem?: string | null;
  dir: string;
  items: readonly string[];
  truncated: boolean;
};

/** What shows under the field: recent workspaces until something is typed, then its folders. */
export type PickerView =
  | { kind: 'recent'; rows: readonly RecentWorkspace[] }
  | { kind: 'dirs'; dir: string; rows: readonly string[]; truncated: boolean };

export function pickerView(
  typed: string,
  recent: readonly RecentWorkspace[] | null | undefined,
  listing: DirListing | null | undefined,
): PickerView {
  if (!typed.trim()) return { kind: 'recent', rows: recent ?? [] };
  return { kind: 'dirs', dir: listing?.dir ?? '', rows: listing?.items ?? [], truncated: !!listing?.truncated };
}

/** The line under the field: where Start would start, or dund's reason it would not. '' before dund answered. */
export function resolvedLine(typed: string, listing: DirListing | null | undefined): { text: string; ok: boolean } {
  if (!typed.trim() || !listing) return { text: '', ok: false };
  if (listing.problem) return { text: listing.problem, ok: false };
  return { text: `Starts in ${listing.workspace}`, ok: true };
}

/** Tapping a folder opens it: its path and a '/', so the list becomes its subfolders. */
export function descend(path: string): string {
  return path.replace(/\/+$/, '') + '/';
}

/** The last part of a path: a row's name, shown before the full path (DES-6). */
export function baseName(path: string): string {
  const p = path.replace(/\/+$/, '');
  return p.slice(p.lastIndexOf('/') + 1) || '/';
}

export function canStart(typed: string, pending: boolean): boolean {
  return !!typed.trim() && !pending;
}
