import { useQuery } from '@tanstack/react-query';
import { Redirect, router, Stack } from 'expo-router';
import { useEffect } from 'react';
import { ActivityIndicator, FlatList, Pressable, Text, View } from 'react-native';

import { explain, UnauthorizedError } from '@/api/client';
import { ListSessions } from '@/api/ops';
import { AppRelease } from '@/components/AppRelease';
import { Button, ErrorText, Muted, Screen } from '@/components/ui';
import { ago, sessionRows } from '@/lib/sessionlist';
import { font, mono, space, useColors } from '@/lib/theme';
import { useAuth } from '@/providers/AuthProvider';

// The caller's sessions, every workspace, in the order dund lists them.
export default function Sessions() {
  const auth = useAuth();
  const c = useColors();
  const list = useQuery({
    queryKey: ['sessions', auth.baseUrl],
    enabled: !!auth.client,
    refetchInterval: 10_000,
    queryFn: async () => {
      try {
        return (await auth.client!.request(ListSessions)).dund.listSessions?.items ?? [];
      } catch (e) {
        throw explain(e);
      }
    },
  });

  const refused = list.error instanceof UnauthorizedError;
  useEffect(() => {
    if (refused) void auth.signOut();
  }, [refused, auth]);

  if (!auth.ready) return <ActivityIndicator style={{ marginTop: space.xl }} />;
  if (!auth.token) return <Redirect href="/signin" />;

  const items = sessionRows(list.data);
  return (
    <Screen>
      <Stack.Screen
        options={{
          headerRight: () => (
            <Pressable onPress={() => void auth.signOut()} style={{ paddingHorizontal: space.md }}>
              <Text style={{ color: c.muted }}>Sign out</Text>
            </Pressable>
          ),
        }}
      />
      <FlatList
        contentContainerStyle={{ padding: space.lg, gap: space.sm, maxWidth: 860, width: '100%', alignSelf: 'center' }}
        data={items}
        keyExtractor={(s) => s.id}
        ListHeaderComponent={
          <View style={{ gap: space.sm, marginBottom: space.sm }}>
            <AppRelease />
            <Button title="New session" primary onPress={() => router.push('/new')} />
            {list.isLoading ? <ActivityIndicator /> : null}
            {list.error && !refused ? <ErrorText>{list.error.message}</ErrorText> : null}
            {!list.isLoading && !items.length ? (
              <Muted>No sessions yet. Start one with New session, or run dun in a terminal.</Muted>
            ) : null}
          </View>
        }
        renderItem={({ item: s }) => (
          <Pressable
            onPress={() => router.push({ pathname: '/session/[id]', params: { id: s.id } })}
            style={{
              padding: space.md,
              borderRadius: 8,
              borderWidth: 1,
              borderColor: c.border,
              backgroundColor: c.surface,
              gap: 2,
            }}
          >
            <View style={{ flexDirection: 'row', gap: space.sm, alignItems: 'center' }}>
              <Text
                style={{ color: c.text, fontFamily: mono, fontSize: font.sm, flex: 1, minWidth: 0 }}
                numberOfLines={1}
                ellipsizeMode="tail"
              >
                {s.id}
              </Text>
              <Text
                style={{ color: s.running ? c.accent : c.muted, fontSize: font.sm, flexShrink: 0 }}
                numberOfLines={1}
              >
                {s.running ? 'running' : 'stopped'}
              </Text>
            </View>
            <Text style={{ color: c.muted, fontSize: font.sm }} numberOfLines={1} ellipsizeMode="tail">
              {[s.workspace, ago(s.lastActive)].filter(Boolean).join('  ·  ')}
            </Text>
          </Pressable>
        )}
      />
    </Screen>
  );
}
