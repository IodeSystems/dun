import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Stack } from 'expo-router/stack';
import { StatusBar } from 'expo-status-bar';
import { useEffect, useState } from 'react';
import { Platform } from 'react-native';
import { SafeAreaProvider } from 'react-native-safe-area-context';

import { UnauthorizedError } from '@/api/client';
import { useColors } from '@/lib/theme';
import { AuthProvider } from '@/providers/AuthProvider';

function makeQueryClient() {
  return new QueryClient({
    defaultOptions: {
      // A refused token will not fix itself; a network error might, once.
      queries: { retry: (n, e) => !(e instanceof UnauthorizedError) && n < 1, staleTime: 5_000 },
    },
  });
}

export default function RootLayout() {
  const [qc] = useState(makeQueryClient);
  const c = useColors();
  // A new web build is invisible to an open tab: the service worker serves
  // the old hashed bundles cache-first. Poll the deploy stamp and reload
  // when it changes.
  useEffect(() => {
    if (Platform.OS !== 'web') return;
    const loc = (globalThis as { location?: { reload(): void } }).location;
    let first: string | null = null;
    const tick = () => {
      void fetch('/version.json', { cache: 'no-store' })
        .then((r) => r.json())
        .then((j: { v?: string }) => {
          const v = j.v ?? '';
          if (first === null) first = v;
          else if (v && v !== first) loc?.reload();
        })
        .catch(() => undefined);
    };
    tick();
    const t = setInterval(tick, 15_000);
    return () => clearInterval(t);
  }, []);
  return (
    <SafeAreaProvider>
      <QueryClientProvider client={qc}>
        <AuthProvider>
          <StatusBar style="auto" />
          <Stack
            screenOptions={{
              headerStyle: { backgroundColor: c.bg },
              headerTintColor: c.text,
              contentStyle: { backgroundColor: c.bg },
            }}
          >
            <Stack.Screen name="index" options={{ title: 'dun' }} />
            <Stack.Screen name="signin" options={{ title: 'Sign in' }} />
            <Stack.Screen name="session/[id]" options={{ title: 'Session' }} />
          </Stack>
        </AuthProvider>
      </QueryClientProvider>
    </SafeAreaProvider>
  );
}
