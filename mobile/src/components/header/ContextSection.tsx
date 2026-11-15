import { Text, View } from 'react-native';

import { contextBlocks, type CtxView } from '@/lib/ctxview';
import { font, mono, space, useColors } from '@/lib/theme';

/**
 * The rest of the TUI's /context (tui_context.go contextText): the window's
 * room and ratio, the conversation, what shaped the context, tool results and
 * queued messages. Its own component: the header's other sections (request
 * figures, cost and limits) sit beside it in the Context sheet.
 */
export function ContextSection({ ctxv, blocks }: { ctxv: CtxView; blocks: number }) {
  const c = useColors();
  return (
    <>
      {contextBlocks(ctxv, blocks).map((s) => (
        <View key={s.title} style={{ gap: space.xs }} testID={`context-${s.title}`}>
          <Text style={{ color: c.text, fontSize: font.sm, fontWeight: '600' }}>{s.title}</Text>
          {s.rows.map((r) => (
            <View key={r.label} style={{ flexDirection: 'row', gap: space.sm, alignItems: 'baseline' }}>
              <Text style={{ flex: 1, color: c.muted, fontSize: font.sm }} numberOfLines={1}>
                {r.label}
              </Text>
              <View style={{ alignItems: 'flex-end', flexShrink: 1 }}>
                <Text style={{ color: c.text, fontSize: font.sm, fontFamily: mono, textAlign: 'right' }}>
                  {r.value}
                </Text>
                {r.note ? (
                  <Text
                    style={{
                      color: r.note.startsWith('←') ? c.notice : c.muted,
                      fontSize: font.sm,
                      textAlign: 'right',
                    }}
                  >
                    {r.note}
                  </Text>
                ) : null}
              </View>
            </View>
          ))}
        </View>
      ))}
    </>
  );
}
