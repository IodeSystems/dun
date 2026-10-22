import type { GraphQLClient } from 'graphql-request';
import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';

import { makeClient } from '@/api/client';
import { loadBaseUrl, loadToken, saveBaseUrl, saveToken } from '@/lib/storage';

type Auth = {
  ready: boolean;
  baseUrl: string;
  token: string | null;
  client: GraphQLClient | null;
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
    await saveToken(null);
    setToken(null);
  }, []);

  const client = useMemo(() => (token && baseUrl ? makeClient(baseUrl, token) : null), [baseUrl, token]);

  const value = useMemo(
    () => ({ ready, baseUrl, token, client, signIn, signOut }),
    [ready, baseUrl, token, client, signIn, signOut],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useAuth(): Auth {
  const a = useContext(Ctx);
  if (!a) throw new Error('useAuth outside AuthProvider');
  return a;
}
