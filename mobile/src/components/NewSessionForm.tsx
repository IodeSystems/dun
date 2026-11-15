import { Pressable, Text, View } from 'react-native';

import { Button, ErrorText, Field, Muted } from '@/components/ui';
import { baseName, canStart, descend, type PickerView } from '@/lib/newsession';
import { ago } from '@/lib/sessionlist';
import { font, mono, space, useColors } from '@/lib/theme';

// The new-session screen's body: a directory field with Start beside it,
// the line saying where Start would start (dund's words), and under it the
// recent workspaces (a tap starts there) or, once something is typed, the
// folders that complete it (a tap opens the folder).
export function NewSessionForm({
  typed,
  onType,
  view,
  resolved,
  error,
  pending,
  onStart,
}: {
  typed: string;
  onType: (s: string) => void;
  view: PickerView;
  resolved: { text: string; ok: boolean };
  error?: string;
  pending: boolean;
  onStart: (workspace: string) => void;
}) {
  const c = useColors();
  const row = {
    paddingVertical: space.sm,
    paddingHorizontal: space.md,
    borderRadius: 8,
    borderWidth: 1,
    borderColor: c.border,
    backgroundColor: c.surface,
    gap: 2,
  } as const;
  return (
    <View style={{ gap: space.sm }}>
      <Text style={{ color: c.text, fontSize: font.md, fontWeight: '600' }}>Directory</Text>
      <View style={{ flexDirection: 'row', gap: space.sm, alignItems: 'center' }}>
        <View style={{ flex: 1, minWidth: 0 }}>
          <Field
            value={typed}
            onChangeText={onType}
            placeholder="~/src/project"
            autoCapitalize="none"
            autoCorrect={false}
            autoFocus
            onSubmitEditing={() => canStart(typed, pending) && onStart(typed.trim())}
            style={{ fontFamily: mono }}
          />
        </View>
        <Button
          title={pending ? 'Starting…' : 'Start'}
          primary
          disabled={!canStart(typed, pending)}
          onPress={() => onStart(typed.trim())}
        />
      </View>
      {error ? <ErrorText>{error}</ErrorText> : resolved.text ? <Muted>{resolved.text}</Muted> : null}

      {view.kind === 'recent' ? (
        <View style={{ gap: space.sm, marginTop: space.lg }}>
          <Text style={{ color: c.text, fontSize: font.md, fontWeight: '600' }}>Start in a recent workspace</Text>
          {view.rows.length ? null : <Muted>None yet. Type a directory above.</Muted>}
          {view.rows.map((w) => (
            <Pressable
              key={w.path}
              accessibilityRole="button"
              disabled={pending}
              onPress={() => onStart(w.path)}
              style={({ pressed }) => ({ ...row, opacity: pressed ? 0.8 : 1 })}
            >
              <View style={{ flexDirection: 'row', gap: space.sm, alignItems: 'center' }}>
                <Text style={{ color: c.text, fontSize: font.md, flex: 1, minWidth: 0 }} numberOfLines={1}>
                  {baseName(w.path)}
                </Text>
                <Text style={{ color: w.running ? c.accent : c.muted, fontSize: font.sm, flexShrink: 0 }}>
                  {[w.running ? 'running' : '', ago(w.lastActive)].filter(Boolean).join('  ·  ')}
                </Text>
              </View>
              <Text
                style={{ color: c.muted, fontSize: font.sm, fontFamily: mono }}
                numberOfLines={1}
                ellipsizeMode="head"
              >
                {w.path}
              </Text>
            </Pressable>
          ))}
        </View>
      ) : (
        <View style={{ gap: space.sm, marginTop: space.lg }}>
          {view.dir ? (
            <Text
              style={{ color: c.text, fontSize: font.md, fontWeight: '600' }}
              numberOfLines={1}
              ellipsizeMode="head"
            >
              Folders in {view.dir}
            </Text>
          ) : null}
          {view.dir && !view.rows.length ? <Muted>No folders here.</Muted> : null}
          {view.rows.map((p) => (
            <Pressable
              key={p}
              accessibilityRole="button"
              onPress={() => onType(descend(p))}
              style={({ pressed }) => ({ ...row, opacity: pressed ? 0.8 : 1 })}
            >
              <Text style={{ color: c.text, fontSize: font.md, fontFamily: mono }} numberOfLines={1}>
                {baseName(p)}/
              </Text>
            </Pressable>
          ))}
          {view.truncated ? <Muted>More folders than shown: type more of the name.</Muted> : null}
        </View>
      )}
    </View>
  );
}
