import { router } from 'expo-router';
import * as WebBrowser from 'expo-web-browser';
import { useEffect, useState } from 'react';
import { Platform, ScrollView, Text, View } from 'react-native';

import { explain, makeClient, UnauthorizedError } from '@/api/client';
import { ListSessions } from '@/api/ops';
import { Button, ErrorText, Field, Muted, Screen } from '@/components/ui';
import { fetchHello, NotDundError } from '@/lib/hello';
import { landing } from '@/lib/landing';
import { defaultBaseUrl } from '@/lib/storage';
import { font, space, useColors } from '@/lib/theme';
import { useAuth } from '@/providers/AuthProvider';

// Two ways in: sign in with the identity provider (when this dund has it on:
// /api/v1/hello says sign_in), or paste the local token from `dun dund
// token`. The address is where that dund is: dun.office.iodesystems.com through hz,
// or a standalone install's own address.

/** The fragment a sign-in lands with (#token=…&return=… or #error=…). */
function fragmentParams(raw: string): URLSearchParams {
  const i = raw.indexOf('#');
  return new URLSearchParams(i >= 0 ? raw.slice(i + 1) : '');
}
/** A command the user can select and copy. */
function Cmd({ children }: { children: string }) {
  const c = useColors();
  return (
    <Text
      selectable
      style={{
        color: c.text,
        backgroundColor: c.surface,
        borderColor: c.border,
        borderWidth: 1,
        borderRadius: 8,
        padding: space.sm,
        fontFamily: Platform.OS === 'ios' ? 'Menlo' : 'monospace',
        fontSize: font.sm,
      }}
    >
      {children}
    </Text>
  );
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
  const [help, setHelp] = useState(false);
  const base = url.trim().replace(/\/+$/, '');

  // Whether this dund signs in with the identity provider. hello is open:
  // a plain fetch, since GraphQL needs a token this screen does not have.
  useEffect(() => {
    let live = true;
    fetchHello(base)
      .then((h) => live && setSso(h.sign_in === true))
      .catch(() => live && setSso(false));
    return () => {
      live = false;
    };
  }, [base]);

  async function keep(u: string, t: string) {
    setErr('');
    setBusy(true);
    try {
      // The address must be dund, then the pair must work.
      await fetchHello(u);
      await makeClient(u, t).request(ListSessions);
      await auth.signIn(u, t);
    } catch (e) {
      const x = explain(e);
      setErr(
        x instanceof NotDundError
          ? x.message
          : x instanceof UnauthorizedError
            ? 'That token was refused. Make a new one with `dun dund token`.'
            : x.message,
      );
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
    const r = await WebBrowser.openAuthSessionAsync(
      base + '/auth/login?return=' + encodeURIComponent('dun://auth'),
      'dun://auth',
    );
    if (r.type !== 'success') return;
    const f = fragmentParams(r.url);
    if (f.get('error')) setErr(f.get('error') ?? '');
    else await keep(base, f.get('token') ?? '');
  }

  return (
    <Screen>
      <ScrollView
        contentContainerStyle={{ padding: space.lg, gap: space.md, maxWidth: 560, width: '100%', alignSelf: 'center' }}
      >
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
        <Field
          value={token}
          onChangeText={setToken}
          autoCapitalize="none"
          autoCorrect={false}
          secureTextEntry
          placeholder="dun_…"
          onSubmitEditing={() => void submit()}
        />
        {err ? <ErrorText>{err}</ErrorText> : null}
        <View>
          <Button
            title={busy ? 'Checking…' : 'Sign in with the token'}
            primary={!sso}
            onPress={() => void submit()}
            disabled={busy || !token.trim()}
          />
        </View>
        <Button title={help ? 'Hide help' : 'No token?'} onPress={() => setHelp(!help)} />
        {help ? (
          <View style={{ gap: space.sm }}>
            <Muted>1. Install dun on the machine that runs the agents:</Muted>
            <Cmd>curl -fsSL https://dun.iodesystems.com/install.sh | sh</Cmd>
            <Muted>More ways to install: https://dun.iodesystems.com/#install</Muted>
            <Muted>2. Start dund with its web listener on. The phone must reach this address:</Muted>
            <Cmd>dun dund serve --web-experimental 0.0.0.0:20003</Cmd>
            <Muted>3. Make a token. Use any name. The token shows once. Copy it now:</Muted>
            <Cmd>dun dund token create phone</Cmd>
            <Muted>List tokens with `dun dund token list`. Revoke one with `dun dund token revoke phone`.</Muted>
          </View>
        ) : null}
      </ScrollView>
    </Screen>
  );
}
