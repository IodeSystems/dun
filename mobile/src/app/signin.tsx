import { router } from 'expo-router';
import * as WebBrowser from 'expo-web-browser';
import { useEffect, useState } from 'react';
import { Platform, ScrollView, Text, View } from 'react-native';

import { explain, makeClient, UnauthorizedError } from '@/api/client';
import { ListSessions } from '@/api/ops';
import { Button, ErrorText, Field, Muted, Screen } from '@/components/ui';
import { landing } from '@/lib/landing';
import { defaultBaseUrl } from '@/lib/storage';
import { font, space, useColors } from '@/lib/theme';
import { useAuth } from '@/providers/AuthProvider';

// Two ways in: sign in with the identity provider (when this dund has it on:
// /api/v1/hello says sign_in), or paste the local token from `dun dund
// token`. The address is where that dund is: dun.iodesystems.com through hz,
// or a standalone install's own address.

/** The fragment a sign-in lands with (#token=…&return=… or #error=…). */
function fragmentParams(raw: string): URLSearchParams {
  const i = raw.indexOf('#');
  return new URLSearchParams(i >= 0 ? raw.slice(i + 1) : '');
}
export default function SignIn() {
  const auth = useAuth();
  const c = useColors();
  const [url, setUrl] = useState(auth.baseUrl || defaultBaseUrl());
  const [token, setToken] = useState('');
  // A refused sign-in with the provider lands here with why.
  const [err, setErr] = useState(() => landing()?.error ?? '');
  const [busy, setBusy] = useState(false);
  const [sso, setSso] = useState(false);
  const base = url.trim().replace(/\/+$/, '');

  // Whether this dund signs in with the identity provider. hello is open:
  // a plain fetch, since GraphQL needs a token this screen does not have.
  useEffect(() => {
    let live = true;
    fetch(base + '/api/v1/hello')
      .then((r) => r.json())
      .then((h: { sign_in?: boolean }) => live && setSso(h.sign_in === true))
      .catch(() => live && setSso(false));
    return () => {
      live = false;
    };
  }, [base]);

  async function keep(u: string, t: string) {
    setErr('');
    setBusy(true);
    try {
      // Prove the pair works before keeping it.
      await makeClient(u, t).request(ListSessions);
      await auth.signIn(u, t);
    } catch (e) {
      const x = explain(e);
      setErr(x instanceof UnauthorizedError ? 'That token was refused. Make a new one with `dun dund token`.' : x.message);
    } finally {
      setBusy(false);
    }
  }

  // Signed in (a token kept, here or from the provider's landing): go on.
  useEffect(() => {
    if (auth.ready && auth.token) router.replace('/');
  }, [auth.ready, auth.token]);

  async function submit() {
    await keep(base, token.trim());
  }

  async function withProvider() {
    if (Platform.OS === 'web') {
      window.location.href = base + '/auth/login?return=' + encodeURIComponent('/');
      return;
    }
    const r = await WebBrowser.openAuthSessionAsync(base + '/auth/login?return=' + encodeURIComponent('dun://auth'), 'dun://auth');
    if (r.type !== 'success') return;
    const f = fragmentParams(r.url);
    if (f.get('error')) setErr(f.get('error') ?? '');
    else await keep(base, f.get('token') ?? '');
  }

  return (
    <Screen>
      <ScrollView contentContainerStyle={{ padding: space.lg, gap: space.md, maxWidth: 560, width: '100%', alignSelf: 'center' }}>
        <Text style={{ color: c.text, fontSize: font.lg }}>Sign in to dund</Text>
        <Muted>Server</Muted>
        <Field value={url} onChangeText={setUrl} autoCapitalize="none" autoCorrect={false} keyboardType="url" />
        {sso ? (
          <View style={{ gap: space.sm }}>
            <Button title="Sign in with iodesystems" primary onPress={() => void withProvider()} disabled={busy} />
            <Muted>or, on the machine dund runs on:</Muted>
          </View>
        ) : null}
        <Muted>Token (from `dun dund token`)</Muted>
        <Field value={token} onChangeText={setToken} autoCapitalize="none" autoCorrect={false} secureTextEntry placeholder="dun_…" onSubmitEditing={() => void submit()} />
        {err ? <ErrorText>{err}</ErrorText> : null}
        <View>
          <Button title={busy ? 'Checking…' : 'Sign in with the token'} primary={!sso} onPress={() => void submit()} disabled={busy || !token.trim()} />
        </View>
      </ScrollView>
    </Screen>
  );
}
