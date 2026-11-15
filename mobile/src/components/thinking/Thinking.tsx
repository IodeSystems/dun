import { useEffect, useState } from 'react';
import { Text, View } from 'react-native';

import { Fold } from '../fold/Fold';
import { thinkHeader, thoughtHead, thoughtMeta, type Thought } from '@/lib/thinking';
import { font, space, useColors } from '@/lib/theme';

/** Under this width (dp) the word count goes in a letter, as on a phone in the TUI. */
const NARROW = 360;

/**
 * A thought (lib/thinking.ts): one line folded — the headline, then how long
 * and how much — and its whole text when tapped. While it streams the line
 * follows the latest heading and the time counts up.
 */
export function Thinking({ item, nested }: { item: Thought; nested?: boolean }) {
  const c = useColors();
  const [open, setOpen] = useState(false);
  const [width, setWidth] = useState(0);
  const now = useNow(item.live);
  const inset = nested ? space.sm : space.md;
  const text = item.text.trim();
  return (
    <View
      onLayout={(e) => setWidth(e.nativeEvent.layout.width)}
      style={{ paddingHorizontal: nested ? 0 : space.md, paddingVertical: space.xs }}
    >
      <Fold
        open={open}
        onToggle={() => setOpen(!open)}
        nested={nested}
        inset={inset}
        headStyle={{ flexDirection: 'row', alignItems: 'baseline', gap: space.sm }}
        head={
          open ? (
            <Text style={{ color: c.muted, fontSize: font.sm }}>{thinkHeader(item, now)}</Text>
          ) : (
            <>
              <Text
                style={{ color: c.muted, fontSize: font.sm, flex: 1, minWidth: 0 }}
                numberOfLines={1}
                ellipsizeMode="tail"
                testID="fold-title"
              >
                {'✻ ' + thoughtHead(item)}
              </Text>
              <Text style={{ color: c.muted, fontSize: font.sm, flexShrink: 0 }} numberOfLines={1}>
                {thoughtMeta(item, now, width > 0 && width < NARROW)}
              </Text>
            </>
          )
        }
      >
        {open && text ? (
          <Text selectable style={{ color: c.muted, fontSize: font.sm }}>
            {text}
          </Text>
        ) : null}
      </Fold>
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
