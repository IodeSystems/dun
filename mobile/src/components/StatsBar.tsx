import { useState } from 'react';
import { Text, View } from 'react-native';

import { fit, type Stats } from '@/lib/stats';
import { font, mono, space, useColors } from '@/lib/theme';

/** The padding is spaces the web would collapse: no-break spaces keep every value's width. */
const NBSP = '\u00a0';

/** A monospace character's width, as a share of the font size. */
const CHAR = 0.6;

/**
 * The request stats, one row right-aligned (lib/stats.ts; the TUI's top-bar
 * figures). Monospace, every value padded, so a number changing moves
 * nothing; the pieces that do not fit the row are left out, least first.
 * Nothing until the session's first request.
 */
export function StatsBar({ stats }: { stats: Stats }) {
  const c = useColors();
  const [width, setWidth] = useState(0);
  const ps = width > 0 ? fit(stats, Math.floor(width / (font.sm * CHAR))) : [];
  if (!stats.seen) return null;
  return (
    <View
      onLayout={(e) => setWidth(e.nativeEvent.layout.width)}
      style={{
        paddingHorizontal: space.lg,
        paddingVertical: 2,
        alignItems: 'flex-end',
        // The transcript's column.
        maxWidth: 860,
        width: '100%',
        alignSelf: 'center',
      }}
    >
      <Text style={{ fontFamily: mono, fontSize: font.sm, color: c.muted }} numberOfLines={1}>
        {ps.map((p, i) => (
          <Text key={p.label}>
            {i > 0 ? ' · ' : ''}
            {p.label + NBSP}
            <Text style={{ color: c.text }}>{p.value.replaceAll(' ', NBSP)}</Text>
          </Text>
        ))}
      </Text>
    </View>
  );
}
