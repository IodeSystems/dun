import { Platform } from 'react-native';

// What a web sign-in with the identity provider landed with: dund's
// /auth/callback sends the browser to /signin#token=… (or #error=…). Read
// once, the first time anything asks, and cleared from the address so the
// token is not left in the history.

export type Landing = { token: string; error: string } | null;

let taken: Landing | undefined;

export function landing(): Landing {
  if (taken !== undefined) return taken;
  taken = null;
  if (Platform.OS !== 'web' || typeof window === 'undefined') return taken;
  const i = window.location.hash.indexOf('#');
  const f = new URLSearchParams(i >= 0 ? window.location.hash.slice(i + 1) : '');
  if (f.get('token') || f.get('error')) {
    window.history.replaceState(null, '', window.location.pathname);
    taken = { token: f.get('token') ?? '', error: f.get('error') ?? '' };
  }
  return taken;
}
