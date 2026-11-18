import * as Clipboard from 'expo-clipboard';

import type { Item } from './events';

/** The raw source of a message: what was typed or sent, before rendering. */
export function rawText(item: Item): string | null {
  switch (item.kind) {
    case 'user':
    case 'assistant':
    case 'question':
      return item.text || null;
    default:
      return null;
  }
}

/** Copies an item's raw markdown to the clipboard; the line count, or 0 when it has none. */
export async function copyRaw(item: Item): Promise<number> {
  const text = rawText(item);
  if (!text) return 0;
  await Clipboard.setStringAsync(text);
  return text.split('\n').length;
}
