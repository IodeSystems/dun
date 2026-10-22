import { router, Stack, useLocalSearchParams } from 'expo-router';
import { useEffect, useRef, useState } from 'react';
import { FlatList, KeyboardAvoidingView, Platform, Pressable, Text, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { explain } from '@/api/client';
import { followSession } from '@/api/live';
import { Answer, ForgetSession, SendMessage, Slash } from '@/api/ops';
import { Palette, SessionPanel } from '@/components/SessionPanel';
import { Button, ErrorText, Field, Muted, Screen } from '@/components/ui';
import { confirm } from '@/lib/confirm';
import { apply, empty, parseLine, suggest, type Item, type View as Shown } from '@/lib/events';
import { font, mono, space, useColors } from '@/lib/theme';
import { useAuth } from '@/providers/AuthProvider';

// One session: its events followed live over graphql-ws (src/api/live.ts).
export default function Session() {
  const { id } = useLocalSearchParams<{ id: string }>();
  const auth = useAuth();
  const c = useColors();
  const insets = useSafeAreaInsets();
  const [view, setView] = useState<Shown>(empty);
  const [conn, setConn] = useState('');
  const [text, setText] = useState('');
  const [sendErr, setSendErr] = useState('');
  const [panel, setPanel] = useState(false);
  const listRef = useRef<FlatList<Item>>(null);

  useEffect(() => {
    if (!auth.client || !auth.live || !id) return;
    return followSession(auth.client, auth.live, id, {
      onEvent: (ev) => setView((v) => apply(v, ev)),
      onConn: setConn,
      onUnauthorized: () => void auth.signOut(),
    });
  }, [auth, id]);

  // /close confirmed by the engine: the service lets the session go and the
  // screen leaves it.
  useEffect(() => {
    if (!view.closed || !auth.client) return;
    void auth.client.request(ForgetSession, { id }).catch(() => undefined);
    router.replace('/');
  }, [view.closed, auth.client, id]);

  async function runSlash(line: string) {
    if (!auth.client) return;
    const name = line.trim().split(/\s+/)[0]?.toLowerCase();
    if (name === 'close' && !(await confirm('Close', 'Remove this session\'s worktree and branch and forget its transcript, for good?'))) return;
    if (name === 'clear' && !(await confirm('Clear', 'Clear the conversation and start a fresh session log?'))) return;
    try {
      await auth.client.request(Slash, { id, line });
    } catch (e) {
      setSendErr('/' + line + ': ' + explain(e).message);
    }
  }

  async function send() {
    const raw = text;
    if (!raw.trim() || !auth.client) return;
    setSendErr('');
    const l = parseLine(raw, view.skills);
    if (l.kind === 'unknown') {
      setSendErr(`no command /${l.name} — type / to see them`);
      return;
    }
    setText('');
    if (l.kind === 'slash') return runSlash(l.line);
    try {
      await auth.client.request(SendMessage, { id, content: l.text });
    } catch (e) {
      setText(raw);
      setSendErr('not sent: ' + explain(e).message);
    }
  }

  async function answer(value: string) {
    try {
      await auth.client?.request(Answer, { id, value });
    } catch (e) {
      setSendErr('answer not sent: ' + explain(e).message);
    }
  }

  const rows: Item[] = view.streaming ? [...view.items, { key: 'streaming', kind: 'assistant', text: view.streaming }] : view.items;
  return (
    <Screen>
      <Stack.Screen
        options={{
          title: id,
          headerRight: () => (
            <Pressable onPress={() => setPanel((p) => !p)} style={{ paddingHorizontal: space.md }}>
              <Text style={{ color: panel ? c.accent : c.muted }}>{panel ? 'Hide session' : 'Session'}</Text>
            </Pressable>
          ),
        }}
      />
      <KeyboardAvoidingView style={{ flex: 1 }} behavior={Platform.OS === 'ios' ? 'padding' : undefined}>
        <FlatList
          ref={listRef}
          data={rows}
          keyExtractor={(i) => i.key}
          onContentSizeChange={() => listRef.current?.scrollToEnd({ animated: false })}
          contentContainerStyle={{ padding: space.lg, gap: space.sm, maxWidth: 860, width: '100%', alignSelf: 'center' }}
          renderItem={({ item }) => <Row item={item} />}
        />
        <View style={{ padding: space.md, paddingBottom: space.md + insets.bottom, borderTopWidth: 1, borderColor: c.border, gap: space.sm }}>
          {view.ask ? (
            <View style={{ gap: space.sm, padding: space.md, borderWidth: 1, borderColor: c.accent, borderRadius: 8 }}>
              <Text style={{ color: c.text, fontSize: font.md }}>❓ {view.ask.question}</Text>
              <View style={{ flexDirection: 'row', flexWrap: 'wrap', gap: space.sm }}>
                {view.ask.options.map((o) => (
                  <Button key={o} title={o} onPress={() => void answer(o)} />
                ))}
              </View>
            </View>
          ) : null}
          {panel ? <SessionPanel commands={view.commands} run={(l) => void runSlash(l)} prefill={(t) => { setText(t); setPanel(false); }} /> : null}
          {text.startsWith('/') && !view.ask ? <Palette rows={suggest(text, view.commands, view.skills)} pick={setText} /> : null}
          {conn ? <ErrorText>{conn}</ErrorText> : view.status ? <Muted>{view.status}</Muted> : null}
          {sendErr ? <ErrorText>{sendErr}</ErrorText> : null}
          <View style={{ flexDirection: 'row', gap: space.sm, alignItems: 'flex-end' }}>
            <Field
              style={{ flex: 1, paddingVertical: space.sm }}
              value={text}
              onChangeText={setText}
              multiline
              placeholder={view.ask ? 'or type an answer…' : 'ask dun to do something… (/ for commands)'}
              onKeyPress={(e) => {
                // Enter sends on the web; Shift+Enter is a newline.
                const ne = e.nativeEvent as { key: string; shiftKey?: boolean };
                if (Platform.OS === 'web' && ne.key === 'Enter' && !ne.shiftKey) {
                  e.preventDefault();
                  if (view.ask && text.trim()) {
                    void answer(text.trim());
                    setText('');
                  } else void send();
                }
              }}
            />
            <Button
              title="Send"
              primary
              disabled={!text.trim()}
              onPress={() => {
                if (view.ask) {
                  void answer(text.trim());
                  setText('');
                } else void send();
              }}
            />
          </View>
        </View>
      </KeyboardAvoidingView>
    </Screen>
  );
}

function Row({ item }: { item: Item }) {
  const c = useColors();
  const [open, setOpen] = useState(false);
  switch (item.kind) {
    case 'user':
      return (
        <View style={{ backgroundColor: c.user, borderRadius: 8, padding: space.md }}>
          <Text selectable style={{ color: c.text, fontSize: font.md }}>
            {item.text}
            {item.queued ? '  (queued)' : ''}
          </Text>
        </View>
      );
    case 'assistant':
      return (
        <View style={{ backgroundColor: c.surface, borderWidth: 1, borderColor: c.border, borderRadius: 8, padding: space.md }}>
          <Text selectable style={{ color: c.text, fontSize: font.md }}>{item.text}</Text>
        </View>
      );
    case 'tool':
      return (
        <Pressable onPress={() => setOpen(!open)} style={{ backgroundColor: c.sunken, borderRadius: 8, padding: space.sm }}>
          <Text style={{ color: c.muted, fontFamily: mono, fontSize: font.sm }}>
            {open ? '▾ ' : '▸ '}
            {item.title}
          </Text>
          {open ? (
            <Text selectable style={{ color: c.text, fontFamily: mono, fontSize: font.sm, marginTop: space.xs }}>
              {item.body}
            </Text>
          ) : null}
        </Pressable>
      );
    case 'note':
      return <Text style={{ color: item.error ? c.danger : c.muted, fontSize: font.sm, paddingHorizontal: space.md }}>{item.text}</Text>;
  }
}
