import { useMutation, useQuery } from '@tanstack/react-query';
import { Redirect, router, Stack } from 'expo-router';
import { useEffect, useState } from 'react';
import { ActivityIndicator, FlatList, Pressable, Text, View } from 'react-native';

import { explain, UnauthorizedError } from '@/api/client';
import { CreateSession, ListSessions } from '@/api/ops';
import { AppRelease } from '@/components/AppRelease';
import { Button, ErrorText, Field, Muted, Screen } from '@/components/ui';
import { font, mono, space, useColors } from '@/lib/theme';
import { useAuth } from '@/providers/AuthProvider';

// The caller's sessions, every workspace, newest first.
export default function Sessions() {
  const auth = useAuth();
  const c = useColors();
  const [ws, setWs] = useState('');
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
  const create = useMutation({
    mutationFn: async (workspace: string) => {
      try {
        return (await auth.client!.request(CreateSession, { workspace })).dund.createSession?.id;
      } catch (e) {
        throw explain(e);
      }
    },
    onSuccess: (id) => id && router.push({ pathname: '/session/[id]', params: { id } }),
  });

  const refused = list.error instanceof UnauthorizedError;
  useEffect(() => {
    if (refused) void auth.signOut();
  }, [refused, auth]);

  if (!auth.ready) return <ActivityIndicator style={{ marginTop: space.xl }} />;
  if (!auth.token) return <Redirect href="/signin" />;

  const items = [...(list.data ?? [])].sort((a, b) => b.id.localeCompare(a.id));
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
            {list.isLoading ? <ActivityIndicator /> : null}
            {list.error && !refused ? <ErrorText>{list.error.message}</ErrorText> : null}
            {!list.isLoading && !items.length ? (
              <Muted>No sessions yet — start one below, or with dun in a terminal.</Muted>
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
              <Text style={{ color: c.text, fontFamily: mono, fontSize: font.sm }}>{s.id}</Text>
              <Text style={{ color: s.running ? c.accent : c.muted, fontSize: font.sm }}>
                {s.running ? 'running' : 'stopped'}
              </Text>
            </View>
            <Text style={{ color: c.muted, fontSize: font.sm }} numberOfLines={2}>
              {s.workspace}
            </Text>
          </Pressable>
        )}
        ListFooterComponent={
          <View style={{ gap: space.sm, marginTop: space.lg }}>
            <Muted>New session in a workspace (an absolute path on the machine dund runs on)</Muted>
            <Field
              value={ws}
              onChangeText={setWs}
              placeholder="/home/you/src/project"
              autoCapitalize="none"
              autoCorrect={false}
            />
            {create.error ? <ErrorText>{create.error.message}</ErrorText> : null}
            <Button
              title={create.isPending ? 'Starting…' : 'New session'}
              primary
              disabled={!ws.trim() || create.isPending}
              onPress={() => create.mutate(ws.trim())}
            />
          </View>
        }
      />
    </Screen>
  );
}
