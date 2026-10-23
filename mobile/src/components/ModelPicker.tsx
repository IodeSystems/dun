import { useState } from 'react';
import { ActivityIndicator, FlatList, Pressable, Text, View } from 'react-native';

import { Button, Field, Muted } from '@/components/ui';
import { filterModels, type ModelProvider } from '@/lib/events';
import type { ProviderOp } from '@/lib/provider';
import { font, mono, space, useColors } from '@/lib/theme';

// /model: every saved provider's models (fetched by the engine), searched by
// any words of provider or id, as the TUI's picker does. Below them the
// logged-in providers: log in again, log out, a model used by id (for a
// provider that does not list its models), and log in to another.
export function ModelPicker({
  providers,
  current,
  initial,
  pick,
  op,
  edit,
  close,
}: {
  providers: ModelProvider[] | null;
  current: { model: string; provider: string } | null;
  initial: string;
  pick: (url: string, id: string, provider: string) => void;
  op: (op: ProviderOp, url?: string) => void;
  edit: (p: ModelProvider | null) => void;
  close: () => void;
}) {
  const c = useColors();
  const [q, setQ] = useState(initial);
  const [byId, setById] = useState<{ provider: string; id: string } | null>(null);
  const rows = providers ? filterModels(providers, q) : [];
  return (
    <View
      style={{
        gap: space.sm,
        borderWidth: 1,
        borderColor: c.border,
        borderRadius: 8,
        padding: space.sm,
        backgroundColor: c.surface,
      }}
    >
      <View style={{ flexDirection: 'row', alignItems: 'center', gap: space.sm }}>
        <Text style={{ color: c.text, fontSize: font.md, fontWeight: '600', flex: 1 }}>
          Model{current ? `: ${current.model} on ${current.provider || '?'}` : ''}
        </Text>
        <Pressable onPress={close} style={{ padding: space.xs }}>
          <Text style={{ color: c.muted }}>Close</Text>
        </Pressable>
      </View>
      <Field
        value={q}
        onChangeText={setQ}
        placeholder="search: words of the provider or model"
        autoCapitalize="none"
        autoCorrect={false}
        autoFocus
      />
      {!providers ? <ActivityIndicator /> : null}
      <FlatList
        style={{ maxHeight: 300 }}
        keyboardShouldPersistTaps="handled"
        data={rows}
        keyExtractor={(r) => r.provider + '/' + r.id}
        renderItem={({ item: r }) => {
          const on = current?.model === r.id && current?.provider === r.provider;
          return (
            <Pressable
              onPress={() => pick(r.url, r.id, r.provider)}
              style={({ pressed }) => ({ padding: space.sm, backgroundColor: pressed ? c.sunken : 'transparent' })}
            >
              <Text style={{ color: on ? c.accent : c.text, fontFamily: mono, fontSize: font.sm }}>
                {on ? '● ' : ''}
                {r.id}
              </Text>
              <Text style={{ color: c.muted, fontSize: font.sm }}>{r.provider}</Text>
            </Pressable>
          );
        }}
        ListEmptyComponent={providers ? <Muted>no model matches</Muted> : null}
      />
      <Text style={{ color: c.text, fontSize: font.sm, fontWeight: '600' }}>Providers</Text>
      {(providers ?? []).map((p) => (
        <View key={p.name} style={{ gap: space.xs, borderTopWidth: 1, borderTopColor: c.border, paddingTop: space.xs }}>
          <Text style={{ color: p.error ? c.danger : c.text, fontSize: font.sm }}>
            {p.default ? '★ ' : ''}
            {p.name} — {p.url} · {p.key ? 'key set' : 'no key'} · {p.error ? p.error : `${(p.ids ?? []).length} models`}
          </Text>
          <View style={{ flexDirection: 'row', gap: space.sm, flexWrap: 'wrap' }}>
            <Button title="Log in again" onPress={() => edit(p)} />
            <Button title="Log out" onPress={() => op({ op: 'logout', name: p.name })} />
            <Button title="Use model id…" onPress={() => setById({ provider: p.name, id: '' })} />
          </View>
          {byId?.provider === p.name ? (
            <View style={{ flexDirection: 'row', gap: space.sm, alignItems: 'center' }}>
              <View style={{ flex: 1 }}>
                <Field
                  value={byId.id}
                  onChangeText={(id) => setById({ provider: p.name, id })}
                  placeholder="model id, e.g. gpt-4o"
                  autoCapitalize="none"
                  autoCorrect={false}
                  autoFocus
                />
              </View>
              <Button
                title="Use"
                primary
                disabled={!/^\S+$/.test(byId.id.trim())}
                onPress={() => {
                  op({ op: 'use', name: p.name, model: byId.id.trim() }, p.url);
                  setById(null);
                }}
              />
            </View>
          ) : null}
        </View>
      ))}
      {providers && providers.length === 0 ? (
        <Muted>Not logged in to a provider: dun talks to no model until you log in.</Muted>
      ) : null}
      <Button title="Log in to a provider" onPress={() => edit(null)} />
    </View>
  );
}
