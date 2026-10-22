import { useState } from 'react';
import { ActivityIndicator, FlatList, Pressable, Text, View } from 'react-native';

import { Field, Muted } from '@/components/ui';
import { filterModels, type ModelProvider } from '@/lib/events';
import { font, mono, space, useColors } from '@/lib/theme';

// /model: every saved provider's models (fetched by the engine), searched by
// any words of provider or id, as the TUI's picker does.
export function ModelPicker({ providers, current, initial, pick, close }: {
  providers: ModelProvider[] | null;
  current: { model: string; provider: string } | null;
  initial: string;
  pick: (url: string, id: string, provider: string) => void;
  close: () => void;
}) {
  const c = useColors();
  const [q, setQ] = useState(initial);
  const rows = providers ? filterModels(providers, q) : [];
  const failed = (providers ?? []).filter((p) => p.error);
  return (
    <View style={{ gap: space.sm, borderWidth: 1, borderColor: c.border, borderRadius: 8, padding: space.sm, backgroundColor: c.surface }}>
      <View style={{ flexDirection: 'row', alignItems: 'center', gap: space.sm }}>
        <Text style={{ color: c.text, fontSize: font.md, fontWeight: '600', flex: 1 }}>
          Model{current ? `: ${current.model} on ${current.provider || '?'}` : ''}
        </Text>
        <Pressable onPress={close} style={{ padding: space.xs }}>
          <Text style={{ color: c.muted }}>Close</Text>
        </Pressable>
      </View>
      <Field value={q} onChangeText={setQ} placeholder="search: words of the provider or model" autoCapitalize="none" autoCorrect={false} autoFocus />
      {!providers ? <ActivityIndicator /> : null}
      {failed.map((p) => (
        <Muted key={p.name}>
          {p.name}: {p.error}
        </Muted>
      ))}
      <FlatList
        style={{ maxHeight: 300 }}
        keyboardShouldPersistTaps="handled"
        data={rows}
        keyExtractor={(r) => r.provider + '/' + r.id}
        renderItem={({ item: r }) => {
          const on = current?.model === r.id && current?.provider === r.provider;
          return (
            <Pressable onPress={() => pick(r.url, r.id, r.provider)} style={({ pressed }) => ({ padding: space.sm, backgroundColor: pressed ? c.sunken : 'transparent' })}>
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
    </View>
  );
}
