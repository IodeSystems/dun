import { router, Stack, useLocalSearchParams } from 'expo-router';
import { useCallback, useEffect, useRef, useState } from 'react';
import { FlatList, KeyboardAvoidingView, Platform, Pressable, Text, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { explain } from '@/api/client';
import { followSession } from '@/api/live';
import { Answer, AskModels, Control, ForgetSession, GetUI, Hello, PutUI, SendMessage, Slash, SwitchModel, UpgradeNow } from '@/api/ops';
import { McpForm } from '@/components/McpForm';
import { McpToolsPanel } from '@/components/McpToolsPanel';
import { ModelPicker } from '@/components/ModelPicker';
import { ProviderForm } from '@/components/ProviderForm';
import { Palette, SessionPanel } from '@/components/SessionPanel';
import { Button, ErrorText, Field, Muted, Screen } from '@/components/ui';
import { confirm } from '@/lib/confirm';
import { apply, empty, interrupting, isKillAll, parseLine, suggest, type Item, type View as Shown } from '@/lib/events';
import { toggleAction, toolsRequest } from '@/lib/mcp';
import { shouldReload, upgradeLines, waitsOnTurnHere } from '@/lib/upgrade';
import { providerLine, type ProviderOp } from '@/lib/provider';
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
  const [picker, setPicker] = useState<string | null>(null); // the search, when open
  const [mcpForm, setMcpForm] = useState(false);
  const [toolsFor, setToolsFor] = useState<string | null>(null); // the server /mcp tools asked for
  // The login form: edit is a provider logged in again, name a new one's.
  const [providerForm, setProviderForm] = useState<{ edit: { name: string; url: string } | null; name?: string } | null>(null);
  // This user's UI state in the session, kept by dund and shared with their
  // other clients (the TUI's format): the sent history and an unsent draft.
  const sent = useRef<string[]>([]);
  const recall = useRef(-1); // index into sent while ↑/↓ browse it
  const uiLoaded = useRef(false);
  const listRef = useRef<FlatList<Item>>(null);

  // The dund build this page loaded against. After dund restarts into
  // another build (a snapshot follows the reconnect), the web page reloads
  // so the client matches it.
  const loadedVersion = useRef<string | null>(null);
  useEffect(() => {
    if (!auth.client || !auth.live || !id) return;
    const live = auth.live;
    const client = auth.client;
    const dundVersion = async () => {
      try {
        return (await client.request(Hello)).dund.hello?.version ?? '';
      } catch {
        return '';
      }
    };
    void dundVersion().then((v) => {
      loadedVersion.current ??= v || null;
    });
    const checkBuild = () =>
      void dundVersion().then((v) => {
        if (Platform.OS === 'web' && shouldReload(loadedVersion.current, v)) (globalThis as { location?: { reload(): void } }).location?.reload();
      });
    let lost = false;
    return followSession(client, live, id, {
      onEvent: (ev) => {
        setView((v) => apply(v, ev));
        if (ev.type === 'snapshot') checkBuild();
      },
      onConn: (problem) => {
        setConn(problem);
        if (problem) lost = true;
        else if (lost) {
          lost = false;
          checkBuild();
        }
      },
      onUnauthorized: () => void auth.signOut(),
    });
  }, [auth, id]);

  useEffect(() => {
    if (!auth.client || !id) return;
    uiLoaded.current = false;
    void auth.client
      .request(GetUI, { id })
      .then((r) => {
        const st = (r.dund.getUI?.state ?? {}) as { history?: unknown; draft?: unknown };
        if (Array.isArray(st.history)) sent.current = st.history.filter((h): h is string => typeof h === 'string');
        if (typeof st.draft === 'string' && st.draft) setText((t) => t || (st.draft as string));
      })
      .catch(() => undefined)
      .finally(() => {
        uiLoaded.current = true;
      });
  }, [auth.client, id]);

  const saveUI = useCallback(
    (draft: string) => {
      if (!auth.client) return;
      void auth.client.request(PutUI, { id, state: { history: sent.current.slice(-200), draft } }).catch(() => undefined);
    },
    [auth.client, id],
  );

  // The draft, saved a moment after typing stops.
  useEffect(() => {
    if (!uiLoaded.current) return;
    const t = setTimeout(() => saveUI(text), 1500);
    return () => clearTimeout(t);
  }, [text, saveUI]);

  function remember(line: string) {
    sent.current = [...sent.current, line].slice(-200);
    recall.current = -1;
    saveUI('');
  }

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
    if (isKillAll(line) && !(await confirm('Kill jobs', 'Kill every running background job, and everything each one started?'))) return;
    // The engine answers /mcp tools <id> with the list; the panel shows it.
    const want = toolsRequest(line);
    if (want) setToolsFor(want);
    try {
      await auth.client.request(Slash, { id, line });
    } catch (e) {
      setSendErr('/' + line + ': ' + explain(e).message);
    }
  }

  // Stop: the engine's /interrupt. The turn ends with done{interrupted}.
  function stop() {
    setView(interrupting);
    void runSlash('interrupt');
  }

  // Restart now: dund interrupts what the upgrade waits for; those sessions
  // resume after the restart.
  async function upgradeNow() {
    if (!(await confirm('Restart now', 'Interrupt the running work and restart dund into the new build? Interrupted sessions resume afterwards.'))) return;
    try {
      await auth.client?.request(UpgradeNow);
    } catch (e) {
      setSendErr('restart: ' + explain(e).message);
    }
  }

  function openPicker(query: string) {
    setPicker(query);
    setPanel(false);
    // The engine fetches the lists (it holds the keys); they arrive as a
    // models event.
    void auth.client?.request(AskModels, { id }).catch((e) => setSendErr('models: ' + explain(e).message));
  }

  // A change to the saved providers, done by the engine (it holds the keys);
  // it answers with a note and a fresh models event. Using a model by id
  // saves it as the default, then switches this session to it.
  async function providerOp(op: ProviderOp, url?: string) {
    if (op.op === 'logout' && !(await confirm('Log out', `Log out of ${op.name}: forget its URL and key?`))) return;
    try {
      await auth.client?.request(Control, { id, target: 'provider', action: JSON.stringify(op) });
    } catch (e) {
      return setSendErr('provider ' + op.op + ': ' + explain(e).message);
    }
    if (op.op === 'use' && url) await switchTo(url, op.model, op.name);
  }

  async function switchTo(url: string, model: string, provider: string) {
    setPicker(null);
    setView((v) => ({ ...v, status: `switching to ${model} on ${provider}…` }));
    try {
      await auth.client?.request(SwitchModel, { id, url, model });
    } catch (e) {
      setSendErr('switch: ' + explain(e).message);
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
    if (l.kind !== 'mcpadd' && !(l.kind === 'provider' && l.cmd === 'login' && l.args.length > 1)) remember(raw.trim());
    if (l.kind === 'slash') return runSlash(l.line);
    if (l.kind === 'provider') {
      const m = providerLine(l.cmd, l.args);
      if (m.kind === 'usage') return setSendErr(m.message);
      if (m.kind === 'login') {
        const p = view.models?.find((x) => x.name === m.name);
        return setProviderForm(p ? { edit: p } : { edit: null, name: m.name });
      }
      if (m.kind === 'op') return providerOp(m.op, m.op.op === 'use' ? view.models?.find((x) => x.name === m.op.name)?.url : undefined);
      return openPicker(m.query);
    }
    if (l.kind === 'mcpadd') return setMcpForm(true);
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
            <View style={{ flexDirection: 'row' }}>
              <Pressable onPress={() => openPicker('')} style={{ paddingHorizontal: space.md }}>
                <Text style={{ color: c.muted }} numberOfLines={1}>
                  {view.settings?.model || 'Model'}
                </Text>
              </Pressable>
              <Pressable onPress={() => setPanel((p) => !p)} style={{ paddingHorizontal: space.md }}>
                <Text style={{ color: panel ? c.accent : c.muted }}>{panel ? 'Hide session' : 'Session'}</Text>
              </Pressable>
            </View>
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
                  <Button key={o} title={o === view.ask?.recommended ? `${o} ★` : o} primary={o === view.ask?.recommended} onPress={() => void answer(o)} />
                ))}
              </View>
            </View>
          ) : null}
          {picker !== null ? (
            <ModelPicker
              providers={view.models}
              current={view.settings}
              initial={picker}
              pick={(u, m, p) => void switchTo(u, m, p)}
              op={(o, u) => void providerOp(o, u)}
              edit={(p) => setProviderForm({ edit: p })}
              close={() => setPicker(null)}
            />
          ) : null}
          {providerForm ? <ProviderForm edit={providerForm.edit} name={providerForm.name} save={(o) => void providerOp(o)} close={() => setProviderForm(null)} /> : null}
          {mcpForm ? (
            <McpForm
              close={() => setMcpForm(false)}
              add={(spec) => void auth.client?.request(Control, { id, target: 'mcp', action: 'add ' + spec }).catch((e) => setSendErr('mcp add: ' + explain(e).message))}
            />
          ) : null}
          {toolsFor && view.mcpTools?.server === toolsFor ? (
            <McpToolsPanel
              tools={view.mcpTools}
              close={() => setToolsFor(null)}
              toggle={(t) =>
                void auth.client
                  ?.request(Control, { id, target: 'mcptools', action: toggleAction(toolsFor, t) })
                  .catch((e) => setSendErr('mcp tools: ' + explain(e).message))
              }
            />
          ) : null}
          {panel ? (
            <SessionPanel
              commands={view.commands}
              run={(l) => void runSlash(l)}
              prefill={(t) => {
                setText(t);
                setPanel(false);
              }}
              addServer={() => {
                setMcpForm(true);
                setPanel(false);
              }}
            />
          ) : null}
          {text.startsWith('/') && !view.ask ? <Palette rows={suggest(text, view.commands, view.skills)} pick={setText} /> : null}
          {view.upgrade ? (
            <View style={{ gap: 2 }}>
              {upgradeLines(view.upgrade, id).map((l, i) => (
                <Muted key={i}>{l}</Muted>
              ))}
              {view.upgrade.state === 'pending' ? (
                <View style={{ flexDirection: 'row', gap: space.sm }}>
                  {waitsOnTurnHere(view.upgrade, id) && view.busy ? <Button title="Stop" onPress={stop} disabled={view.status === 'interrupting…'} /> : null}
                  <Button title="Restart now" onPress={() => void upgradeNow()} />
                </View>
              ) : null}
            </View>
          ) : null}
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
                // ↑/↓ in an empty (or recalled) composer step through what was sent.
                if (Platform.OS === 'web' && (ne.key === 'ArrowUp' || ne.key === 'ArrowDown') && (text === '' || recall.current >= 0)) {
                  const h = sent.current;
                  if (!h.length) return;
                  e.preventDefault();
                  let i = recall.current < 0 ? h.length : recall.current;
                  i = ne.key === 'ArrowUp' ? Math.max(0, i - 1) : i + 1;
                  if (i >= h.length) {
                    recall.current = -1;
                    setText('');
                  } else {
                    recall.current = i;
                    setText(h[i] ?? '');
                  }
                  return;
                }
                if (Platform.OS === 'web' && ne.key === 'Enter' && !ne.shiftKey) {
                  e.preventDefault();
                  if (view.ask && text.trim()) {
                    void answer(text.trim());
                    setText('');
                  } else void send();
                }
              }}
            />
            {view.busy ? <Button title="Stop" onPress={stop} disabled={view.status === 'interrupting…'} /> : null}
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
    case 'question':
      return (
        <View style={{ borderLeftWidth: 3, borderColor: c.accent, paddingHorizontal: space.md, paddingVertical: space.xs, gap: 2 }}>
          <Text selectable style={{ color: c.text, fontSize: font.md }}>❓ {item.text}</Text>
          {item.options.length ? (
            <Text style={{ color: c.muted, fontSize: font.sm }}>
              {item.options.map((o) => (o === item.recommended ? `${o} (recommended)` : o)).join(' · ')}
            </Text>
          ) : null}
        </View>
      );
  }
}
