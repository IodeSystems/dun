// A foldable row: a head that is one line folded, and a body that opens
// BELOW it at the same left edge. Open, the head and body read as one tinted
// unit with a left accent edge; a tap on the head or anywhere on the body
// closes it again. A nested fold takes the darker `fold` tint.

import type { ReactNode } from 'react';
import { Platform, Pressable, View, type StyleProp, type ViewStyle } from 'react-native';

import { space, useColors } from '@/lib/theme';

/** Width of the accent edge on an open fold. */
const EDGE = 3;

/** True when the page has text selected: a click that ends a selection must not toggle (web only). */
function selecting(): boolean {
  if (Platform.OS !== 'web') return false;
  const w = globalThis as { getSelection?: () => { toString(): string } | null };
  return !!w.getSelection?.()?.toString();
}

export function Fold({
  open,
  onToggle,
  head,
  headStyle,
  children,
  nested,
  inset = 0,
  solid,
  plain,
}: {
  open: boolean;
  /** Absent: the fold does not toggle (nothing to open). */
  onToggle?: () => void;
  head: ReactNode;
  headStyle?: StyleProp<ViewStyle>;
  /** The body: tap anywhere on it to toggle. A row may show part of it folded. */
  children?: ReactNode;
  /** Inside another fold: the darker tint, so the parent shows. */
  nested?: boolean;
  /** The parent's padding, so an open fold bleeds out to the parent's edge and the text stays put. */
  inset?: number;
  /** Painted folded too (a section's own frame): padded all round, rounded. */
  solid?: boolean;
  /** No tint or edge; only the tap-to-close. */
  plain?: boolean;
}) {
  const c = useColors();
  const painted = !plain && (open || solid);
  const style: ViewStyle = painted
    ? {
        backgroundColor: nested ? c.fold : c.sunken,
        borderRadius: solid ? 8 : 4,
        borderLeftWidth: open ? EDGE : 0,
        borderLeftColor: c.accent,
        gap: space.xs,
        ...(solid
          ? { padding: space.sm, paddingLeft: space.sm - (open ? EDGE : 0) }
          : {
              marginHorizontal: -inset,
              paddingHorizontal: inset,
              paddingLeft: inset - EDGE,
              paddingVertical: space.xs,
            }),
      }
    : { gap: space.xs };
  const body = () => {
    if (!selecting()) onToggle?.();
  };
  return (
    <View style={style}>
      <Pressable
        onPress={onToggle}
        disabled={!onToggle}
        accessibilityRole={onToggle ? 'button' : undefined}
        accessibilityState={onToggle ? { expanded: open } : undefined}
        style={headStyle}
      >
        {head}
      </Pressable>
      {children == null || children === false ? null : (
        <Pressable onPress={body} disabled={!onToggle} testID="fold-body">
          {children}
        </Pressable>
      )}
    </View>
  );
}
