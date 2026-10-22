import { Pressable, ScrollView, Text, View } from 'react-native';

import type { Command } from '@/lib/events';
import { font, space, useColors } from '@/lib/theme';

// The session's commands as controls, built from the engine's commands
// event: a toggle shows its state and offers its choices; an action is a
// button; a command that takes text puts its prefix in the composer.

const GROUPS = ['tools', 'git', 'memory', 'session'];

export function SessionPanel({ commands, run, prefill, addServer }: {
  commands: Command[];
  run: (line: string) => void;
  prefill: (text: string) => void;
  addServer: () => void;
}) {
  const c = useColors();
  const groups = GROUPS.filter((g) => commands.some((x) => x.group === g));
  return (
    <ScrollView style={{ maxHeight: 320 }} contentContainerStyle={{ gap: space.md, paddingVertical: space.sm }}>
      {groups.map((g) => (
        <View key={g} style={{ gap: space.sm }}>
          <Text style={{ color: c.muted, fontSize: font.sm, textTransform: 'uppercase' }}>{g}</Text>
          {commands
            .filter((x) => x.group === g)
            .map((cmd) => (
              <View key={cmd.name} style={{ gap: space.xs }}>
                <View style={{ flexDirection: 'row', alignItems: 'center', gap: space.sm, flexWrap: 'wrap' }}>
                  <Text style={{ color: c.text, fontSize: font.md, fontWeight: '600' }}>/{cmd.name}</Text>
                  {cmd.state ? (
                    <Text style={{ color: cmd.state === 'off' ? c.muted : c.accent, fontSize: font.sm }}>
                      {cmd.state}
                      {cmd.detail ? ` · ${cmd.detail}` : ''}
                    </Text>
                  ) : null}
                </View>
                <Text style={{ color: c.muted, fontSize: font.sm }}>{cmd.description}</Text>
                <View style={{ flexDirection: 'row', flexWrap: 'wrap', gap: space.xs }}>
                  {(cmd.choices ?? []).map((ch) => (
                    <Chip key={ch.value} label={ch.value} active={cmd.state === ch.value} onPress={() => run(`${cmd.name} ${ch.value}`)} />
                  ))}
                  {!cmd.choices?.length && !cmd.text ? <Chip label="run" onPress={() => run(cmd.name)} /> : null}
                  {cmd.name === 'mcp' ? <Chip label="add…" onPress={addServer} /> : null}
                  {cmd.text ? <Chip label="write…" onPress={() => prefill(`/${cmd.name} `)} /> : null}
                </View>
              </View>
            ))}
        </View>
      ))}
    </ScrollView>
  );
}

function Chip({ label, onPress, active }: { label: string; onPress: () => void; active?: boolean }) {
  const c = useColors();
  return (
    <Pressable
      accessibilityRole="button"
      onPress={onPress}
      style={({ pressed }) => ({
        paddingHorizontal: space.md,
        paddingVertical: space.xs + 2,
        borderRadius: 999,
        borderWidth: 1,
        borderColor: active ? c.accent : c.border,
        backgroundColor: active ? c.accent : c.surface,
        opacity: pressed ? 0.7 : 1,
      })}>
      <Text style={{ color: active ? c.accentText : c.text, fontSize: font.sm }}>{label}</Text>
    </Pressable>
  );
}

export function Palette({ rows, pick }: { rows: { insert: string; label: string; hint: string }[]; pick: (insert: string) => void }) {
  const c = useColors();
  if (!rows.length) return null;
  return (
    <View style={{ borderWidth: 1, borderColor: c.border, borderRadius: 8, backgroundColor: c.surface, maxHeight: 240, overflow: 'hidden' }}>
      <ScrollView keyboardShouldPersistTaps="handled">
        {rows.slice(0, 12).map((r) => (
          <Pressable key={r.insert} onPress={() => pick(r.insert)} style={({ pressed }) => ({ padding: space.sm, backgroundColor: pressed ? c.sunken : 'transparent' })}>
            <Text style={{ color: c.text, fontSize: font.md }}>{r.label}</Text>
            {r.hint ? <Text style={{ color: c.muted, fontSize: font.sm }}>{r.hint}</Text> : null}
          </Pressable>
        ))}
      </ScrollView>
    </View>
  );
}
