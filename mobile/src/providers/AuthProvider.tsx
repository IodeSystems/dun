import type { GraphQLClient } from 'graphql-request';
import type { Client as LiveClient } from 'graphql-ws';
import { Platform } from 'react-native';
import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';

import { COOKIE_SESSION, makeClient } from '@/api/client';
import { SignOut } from '@/api/ops';
import { makeLive } from '@/api/live';
import { landing } from '@/lib/landing';
import { loadBaseUrl, loadToken, saveBaseUrl, saveToken } from '@/lib/storage';

const web = Platform.OS === 'web' && typeof window !== 'undefined';

/** Web: a 200 from the API with no bearer means the session cookie is live. */
async function cookieSignedIn(): Promise<boolean> {
  try {
    const r = await fetch('/api/v1/sessions', { credentials: 'same-origin', cache: 'no-store' });
    return r.status === 200;
  } catch {
    return false;
  }
}

type Auth = {
  ready: boolean;
  baseUrl: string;
  token: string | null;
  client: GraphQLClient | null;
  /** The one live socket (lazy: opens with the first subscription). */
  live: LiveClient | null;
  signIn: (baseUrl: string, token: string) => Promise<void>;
  signOut: () => Promise<void>;
};

const Ctx = createContext<Auth | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [ready, setReady] = useState(false);
  const [baseUrl, setBaseUrl] = useState('');
  const [token, setToken] = useState<string | null>(null);

  useEffect(() => {
    void (async () => {
      if (web) {
        // Signed in by dund's cookie, never by a stored token: drop any old one.
        landing(); // strips #token=… from the address; the cookie is already set
        await saveToken(null);
        setBaseUrl(window.location.origin);
        setToken((await cookieSignedIn()) ? COOKIE_SESSION : null);
        setReady(true);
        return;
      }
      // Back from the identity provider on the web: keep the token it
      // brought (this page's origin is the service).
      const l = landing();
      if (l?.token) {
        await saveBaseUrl(window.location.origin);
        await saveToken(l.token);
      }
      setBaseUrl(await loadBaseUrl());
      setToken(await loadToken());
      setReady(true);
    })();
  }, []);

  const signIn = useCallback(async (url: string, tok: string) => {
    await saveBaseUrl(url);
    await saveToken(tok);
    setBaseUrl(url);
    setToken(tok);
  }, []);

  const signOut = useCallback(async () => {
    if (web) {
      // dund ends the session and clears the cookie; a form post, so the
      // browser follows its redirect to the sign-in page.
      const f = document.createElement('form');
      f.method = 'POST';
      f.action = '/auth/signout';
      document.body.appendChild(f);
      f.submit();
      return;
    }
    // A sign-in token is ended on dund too; the local token is not (it is
    // dund's own credential — `dun dund token` replaces it).
    if (token?.startsWith('dun_s_') && baseUrl) {
      await makeClient(baseUrl, token)
        .request(SignOut)
        .catch(() => undefined);
    }
    await saveToken(null);
    setToken(null);
  }, [token, baseUrl]);

  const client = useMemo(() => (token && baseUrl ? makeClient(baseUrl, token) : null), [baseUrl, token]);
  const live = useMemo(() => (token && baseUrl ? makeLive(baseUrl) : null), [baseUrl, token]);
  useEffect(() => () => void live?.dispose(), [live]);

  const value = useMemo(
    () => ({ ready, baseUrl, token, client, live, signIn, signOut }),
    [ready, baseUrl, token, client, live, signIn, signOut],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useAuth(): Auth {
  const a = useContext(Ctx);
  if (!a) throw new Error('useAuth outside AuthProvider');
  return a;
}
