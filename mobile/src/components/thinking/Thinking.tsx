import { useEffect, useState } from 'react';
import { Pressable, Text, View } from 'react-native';

import { thinkHeader, thoughtHead, thoughtMeta, type Thought } from '@/lib/thinking';
import { font, space, useColors } from '@/lib/theme';

/** Under this width (dp) the word count goes in a letter, as on a phone in the TUI. */
const NARROW = 360;

/**
 * A thought (lib/thinking.ts): one line folded — the headline, then how long
 * and how much — and its whole text when tapped. While it streams the line
 * follows the latest heading and the time counts up.
 */
export function Thinking({ item }: { item: Thought }) {
  const c = useColors();
  const [open, setOpen] = useState(false);
  const [width, setWidth] = useState(0);
  const now = useNow(item.live);
  return (
    <View
      onLayout={(e) => setWidth(e.nativeEvent.layout.width)}
      style={{ paddingHorizontal: space.md, paddingVertical: space.xs, gap: space.xs }}
    >
      <Pressable
        onPress={() => setOpen(!open)}
        accessibilityRole="button"
        accessibilityState={{ expanded: open }}
        style={{ flexDirection: 'row', alignItems: 'baseline', gap: space.sm }}
      >
        {open ? (
          <Text style={{ color: c.muted, fontSize: font.sm }}>{thinkHeader(item, now)}</Text>
        ) : (
          <>
            <Text style={{ color: c.text, fontSize: font.sm, flexShrink: 1 }} numberOfLines={1}>
              {'✻ ' + thoughtHead(item)}
            </Text>
            <Text style={{ color: c.muted, fontSize: font.sm }}>
              {thoughtMeta(item, now, width > 0 && width < NARROW)}
            </Text>
          </>
        )}
      </Pressable>
      {open && item.text.trim() ? (
        <Text
          selectable
          style={{
            color: c.muted,
            fontSize: font.sm,
            borderLeftWidth: 2,
            borderColor: c.border,
            paddingLeft: space.sm,
          }}
        >
          {item.text.trim()}
        </Text>
      ) : null}
    </View>
  );
}

/** The time now, once a second while live. */
function useNow(live: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!live) return;
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, [live]);
  return now;
}
