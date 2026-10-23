import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Stack } from 'expo-router/stack';
import { StatusBar } from 'expo-status-bar';
import { useState } from 'react';
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
