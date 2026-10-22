import type { GraphQLClient } from 'graphql-request';
import type { Client as LiveClient } from 'graphql-ws';
import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';

import { makeClient } from '@/api/client';
import { SignOut } from '@/api/ops';
import { makeLive } from '@/api/live';
import { landing } from '@/lib/landing';
import { loadBaseUrl, loadToken, saveBaseUrl, saveToken } from '@/lib/storage';

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
    // A sign-in token is ended on dund too; the local token is not (it is
    // dund's own credential — `dun dund token` replaces it).
    if (token?.startsWith('dun_s_') && baseUrl) {
      await makeClient(baseUrl, token).request(SignOut).catch(() => undefined);
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
