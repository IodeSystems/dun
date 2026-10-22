// Logged-in LLM providers, changed by /login, /logout and /model use: the
// engine's providerOp (cmd/dun config.go, applyProviderOp — the one
// implementation; the TUI calls it too). The key goes only to the engine, by
// control, never into the sent history. There is no built-in provider.

/** The one kind so far: any OpenAI-compatible endpoint, bearer optional. Specific providers come later. */
export const providerOpenAI = 'openai';

export type ProviderOp =
  | { op: 'login'; name: string; kind: string; url: string; key?: string }
  | { op: 'logout'; name: string }
  | { op: 'use'; name: string; model: string };

/** A name is one word, as the engine requires. */
export function providerNameOk(name: string): boolean {
  return /^\S+$/.test(name.trim());
}

export function providerUrlOk(url: string): boolean {
  return /^https?:\/\/\S+$/.test(url.trim());
}

/** What a typed /model, /login or /logout asks for. Login opens the form: a key is never typed in the composer. */
export type ProviderLine =
  | { kind: 'search'; query: string }
  | { kind: 'login'; name: string }
  | { kind: 'op'; op: ProviderOp }
  | { kind: 'usage'; message: string };

export function providerLine(cmd: 'model' | 'login' | 'logout', args: string[]): ProviderLine {
  const [a = '', b = '', c = ''] = args;
  if (cmd === 'login') {
    return args.length <= 1 ? { kind: 'login', name: a } : { kind: 'usage', message: 'usage: /login [name] — the URL and key go in the form, so the key stays out of the history' };
  }
  if (cmd === 'logout') {
    return args.length === 1 ? { kind: 'op', op: { op: 'logout', name: a } } : { kind: 'usage', message: 'usage: /logout <provider>' };
  }
  if (a === 'use') {
    return args.length === 3 ? { kind: 'op', op: { op: 'use', name: b, model: c } } : { kind: 'usage', message: 'usage: /model use <provider> <model-id>' };
  }
  return { kind: 'search', query: args.join(' ') };
}
