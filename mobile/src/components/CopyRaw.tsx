import { useState } from 'react';
import { Pressable, Text } from 'react-native';

import type { Item } from '@/lib/events';
import { copyRaw, rawText } from '@/lib/rawtext';
import { font, space, useColors } from '@/lib/theme';

/** A small "Copy raw" button: puts the message's markdown source on the clipboard. */
export function CopyRaw({ item }: { item: Item }) {
  const c = useColors();
  const [note, setNote] = useState('');
  if (!rawText(item)) return null;
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel="Copy raw"
      testID="copy-raw"
      onPress={() => {
        copyRaw(item).then(
          (n) => setNote(`copied ${n} line${n === 1 ? '' : 's'} (raw markdown)`),
          () => setNote('could not copy'),
        );
      }}
      style={{ alignSelf: 'flex-end', paddingTop: space.xs }}
    >
      <Text style={{ color: c.muted, fontSize: font.sm }}>{note || 'Copy raw'}</Text>
    </Pressable>
  );
}
