import { router } from 'expo-router';
import { useState } from 'react';
import { ScrollView, Text, View } from 'react-native';

import { explain, makeClient, UnauthorizedError } from '@/api/client';
import { ListSessions } from '@/api/ops';
import { Button, ErrorText, Field, Muted, Screen } from '@/components/ui';
import { defaultBaseUrl } from '@/lib/storage';
import { font, space, useColors } from '@/lib/theme';
import { useAuth } from '@/providers/AuthProvider';

// The token comes from `dun dund token` on the machine dund runs on. The
// address is where that dund is: dun.iodesystems.com through hz, or a
// standalone install's own address.
export default function SignIn() {
  const auth = useAuth();
  const c = useColors();
  const [url, setUrl] = useState(auth.baseUrl || defaultBaseUrl());
  const [token, setToken] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  async function submit() {
    setErr('');
    setBusy(true);
    const u = url.trim().replace(/\/+$/, '');
    const t = token.trim();
    try {
      // Prove the pair works before keeping it.
      await makeClient(u, t).request(ListSessions);
      await auth.signIn(u, t);
      router.replace('/');
    } catch (e) {
      const x = explain(e);
      setErr(x instanceof UnauthorizedError ? 'That token was refused. Make a new one with `dun dund token`.' : x.message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Screen>
      <ScrollView contentContainerStyle={{ padding: space.lg, gap: space.md, maxWidth: 560, width: '100%', alignSelf: 'center' }}>
        <Text style={{ color: c.text, fontSize: font.lg }}>Sign in to dund</Text>
        <Muted>Server</Muted>
        <Field value={url} onChangeText={setUrl} autoCapitalize="none" autoCorrect={false} keyboardType="url" />
        <Muted>Token (from `dun dund token`)</Muted>
        <Field value={token} onChangeText={setToken} autoCapitalize="none" autoCorrect={false} secureTextEntry placeholder="dun_…" onSubmitEditing={submit} />
        {err ? <ErrorText>{err}</ErrorText> : null}
        <View>
          <Button title={busy ? 'Checking…' : 'Sign in'} primary onPress={submit} disabled={busy || !token.trim()} />
        </View>
      </ScrollView>
    </Screen>
  );
}
