// Saved providers, changed from /model: the engine's providerOp (cmd/dun
// config.go, applyProviderOp — the one implementation; the TUI calls it too).
// The key goes only to the engine, by control, never into the sent history.

export type ProviderOp =
  | { op: 'add' | 'edit'; name: string; url: string; key?: string }
  | { op: 'remove'; name: string }
  | { op: 'use'; name: string; model: string };

/** A name is one word, as the engine requires. */
export function providerNameOk(name: string): boolean {
  return /^\S+$/.test(name.trim());
}

export function providerUrlOk(url: string): boolean {
  return /^https?:\/\/\S+$/.test(url.trim());
}

/** What a typed `/model …` asks for. Add and edit open the form: a key is never typed in the composer. */
export type ModelLine =
  | { kind: 'search'; query: string }
  | { kind: 'add' }
  | { kind: 'edit'; name: string }
  | { kind: 'op'; op: ProviderOp }
  | { kind: 'usage'; message: string };

export function modelLine(query: string): ModelLine {
  const w = query.trim().split(/\s+/).filter(Boolean);
  const [head, a = '', b = ''] = w;
  switch (head) {
    case 'add':
      return w.length === 1 ? { kind: 'add' } : { kind: 'usage', message: 'usage: /model add — a form, so the key stays out of the history' };
    case 'edit':
      return w.length === 2 ? { kind: 'edit', name: a } : { kind: 'usage', message: 'usage: /model edit <provider>' };
    case 'remove':
    case 'rm':
      return w.length === 2 ? { kind: 'op', op: { op: 'remove', name: a } } : { kind: 'usage', message: 'usage: /model remove <provider>' };
    case 'use':
      return w.length === 3 ? { kind: 'op', op: { op: 'use', name: a, model: b } } : { kind: 'usage', message: 'usage: /model use <provider> <model-id>' };
  }
  return { kind: 'search', query: w.join(' ') };
}
