import { Text, View } from 'react-native';

import { costBlocks, type Spend } from '@/lib/costview';
import { font, mono, space, useColors } from '@/lib/theme';

import { useNow } from './motion';

/**
 * Cost and limits (the TUI's /usage "cost & limits" and "this machine",
 * internal/tui/spend.go): what this session spent, the key's budget, the
 * rate-limit windows, and every session's spend on this machine. Beside
 * ContextSection in the Context sheet. A window or budget under a tenth
 * left is in the notice colour, as the TUI's status-line warning.
 */
export function CostSection({ spend }: { spend: Spend | null }) {
  const c = useColors();
  const now = useNow(true); // the resets count down
  return (
    <>
      {costBlocks(spend, now).map((s) => (
        <View key={s.title} style={{ gap: space.xs }} testID={`cost-${s.title}`}>
          <Text style={{ color: c.text, fontSize: font.sm, fontWeight: '600' }}>
            {s.title}
            {s.note ? <Text style={{ color: c.muted, fontWeight: '400' }}>{' — ' + s.note}</Text> : null}
          </Text>
          {s.rows.map((r) => (
            <View key={r.label} style={{ flexDirection: 'row', gap: space.sm, alignItems: 'baseline' }}>
              <Text style={{ flex: 1, color: c.muted, fontSize: font.sm }} numberOfLines={1}>
                {r.label}
              </Text>
              <View style={{ alignItems: 'flex-end', flexShrink: 1 }}>
                {r.value ? (
                  <Text
                    style={{
                      color: r.warn ? c.notice : c.text,
                      fontSize: font.sm,
                      fontFamily: mono,
                      textAlign: 'right',
                    }}
                  >
                    {r.value}
                  </Text>
                ) : null}
                {r.note ? (
                  <Text style={{ color: r.warn ? c.notice : c.muted, fontSize: font.sm, textAlign: 'right' }}>
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
