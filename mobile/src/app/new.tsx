import { keepPreviousData, useMutation, useQuery } from '@tanstack/react-query';
import { Redirect, router } from 'expo-router';
import { useEffect, useState } from 'react';
import { ActivityIndicator, ScrollView } from 'react-native';

import { explain } from '@/api/client';
import { CreateSession, ListDirs, ListWorkspaces } from '@/api/ops';
import { NewSessionForm } from '@/components/NewSessionForm';
import { Screen } from '@/components/ui';
import { pickerView, resolvedLine } from '@/lib/newsession';
import { space } from '@/lib/theme';
import { useAuth } from '@/providers/AuthProvider';

// Start a session: a recent workspace in one tap, or a typed directory that
// dund completes and reads (listDirs). A route of its own, so Back closes it
// (WEB-10).
export default function NewSession() {
  const auth = useAuth();
  const [typed, setTyped] = useState('');
  const [asked, setAsked] = useState(''); // typed, once typing paused
  useEffect(() => {
    const t = setTimeout(() => setAsked(typed), 150);
    return () => clearTimeout(t);
  }, [typed]);

  const recent = useQuery({
    queryKey: ['workspaces', auth.baseUrl],
    enabled: !!auth.client,
    queryFn: async () => {
      try {
        return (await auth.client!.request(ListWorkspaces)).dund.listWorkspaces?.items ?? [];
      } catch (e) {
        throw explain(e);
      }
    },
  });
  const dirs = useQuery({
    queryKey: ['dirs', auth.baseUrl, asked],
    enabled: !!auth.client && !!asked.trim(),
    placeholderData: keepPreviousData, // the list does not blank between keystrokes
    queryFn: async () => {
      try {
        return (await auth.client!.request(ListDirs, { path: asked })).dund.listDirs ?? null;
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
    // Replace: Back from the new session goes to the list, not here.
    onSuccess: (id) => id && router.replace({ pathname: '/session/[id]', params: { id } }),
  });

  if (!auth.ready) return <ActivityIndicator style={{ marginTop: space.xl }} />;
  if (!auth.token) return <Redirect href="/signin" />;

  // The line under the field is for what is in it now, not a stale answer.
  const current = asked === typed && !dirs.isPlaceholderData ? dirs.data : null;
  return (
    <Screen>
      <ScrollView
        keyboardShouldPersistTaps="handled"
        contentContainerStyle={{ padding: space.lg, maxWidth: 860, width: '100%', alignSelf: 'center' }}
      >
        <NewSessionForm
          typed={typed}
          onType={(s) => {
            setTyped(s);
            create.reset();
          }}
          view={pickerView(typed, recent.data, dirs.data)}
          resolved={resolvedLine(typed, current)}
          error={create.error?.message ?? recent.error?.message ?? dirs.error?.message}
          pending={create.isPending}
          onStart={(ws) => create.mutate(ws)}
        />
        {recent.isLoading ? <ActivityIndicator style={{ marginTop: space.lg }} /> : null}
      </ScrollView>
    </Screen>
  );
}
