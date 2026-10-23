import { useState } from 'react';
import { Pressable, ScrollView, Text, View } from 'react-native';

import { Button, Muted } from '@/components/ui';
import { kTokens, toolsSummary, type McpTool, type McpTools } from '@/lib/mcp';
import { font, mono, space, useColors } from '@/lib/theme';

// /mcp tools <id> on the web: one server's tools, hidden ones included, each
// with its cost and an on/off; a row opens to its description and schema.
// The engine keeps the list and does the toggle (control "mcptools"), as it
// does for the TUI's pager.
export function McpToolsPanel({
  tools,
  toggle,
  close,
}: {
  tools: McpTools;
  toggle: (t: McpTool) => void;
  close: () => void;
}) {
  const c = useColors();
  const [open, setOpen] = useState<string | null>(null);
  return (
    <View
      style={{
        gap: space.sm,
        borderWidth: 1,
        borderColor: c.border,
        borderRadius: 8,
        padding: space.sm,
        backgroundColor: c.surface,
        maxHeight: 420,
      }}
    >
      <View style={{ flexDirection: 'row', alignItems: 'center', gap: space.sm }}>
        <Text style={{ color: c.text, fontSize: font.md, fontWeight: '600', flex: 1 }}>
          {tools.server} tools · {toolsSummary(tools)}
        </Text>
        <Button title="Close" onPress={close} />
      </View>
      <Muted>A hidden tool leaves every request; the model sees the change from the next turn.</Muted>
      <ScrollView>
        {tools.tools.map((t) => (
          <View
            key={t.name}
            style={{ borderTopWidth: 1, borderColor: c.border, paddingVertical: space.xs, gap: space.xs }}
          >
            <View style={{ flexDirection: 'row', alignItems: 'center', gap: space.sm }}>
              <Pressable style={{ flex: 1 }} onPress={() => setOpen(open === t.name ? null : t.name)}>
                <Text
                  style={{ color: t.enabled ? c.text : c.muted, fontFamily: mono, fontSize: font.sm }}
                  numberOfLines={1}
                >
                  {open === t.name ? '▾ ' : '▸ '}
                  {t.name} · ~{kTokens(t.tokens)} tok · {(t.description ?? '').split('\n')[0]}
                </Text>
              </Pressable>
              <Button title={t.enabled ? 'On' : 'Off'} primary={t.enabled} onPress={() => toggle(t)} />
            </View>
            {open === t.name ? (
              <Text selectable style={{ color: c.text, fontFamily: mono, fontSize: font.sm }}>
                {t.hiddenBy ? `hidden by ${t.hiddenBy}\n\n` : ''}
                {t.description}
                {'\n\nparameters:\n'}
                {t.schema}
              </Text>
            ) : null}
          </View>
        ))}
      </ScrollView>
    </View>
  );
}
