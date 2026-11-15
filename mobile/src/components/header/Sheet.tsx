import type { ReactNode } from 'react';
import { Modal, Pressable, ScrollView, Text, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { font, space, TOUCH, useColors } from '@/lib/theme';

import { useReducedMotion } from './motion';

/**
 * The one panel the header opens (ctx, plan, tasks): a sheet from the bottom
 * over a dimmed screen. Tapping outside it or Close closes it. Its body
 * scrolls; footer stays under it (a composer, buttons).
 */
export function Sheet({
  title,
  onClose,
  children,
  footer,
  scroll = true,
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  /** false: the body lays itself out (it has its own scroll). */
  scroll?: boolean;
}) {
  const c = useColors();
  const insets = useSafeAreaInsets();
  const reduced = useReducedMotion();
  return (
    <Modal transparent visible animationType={reduced ? 'none' : 'slide'} onRequestClose={onClose}>
      <View style={{ flex: 1, justifyContent: 'flex-end' }}>
        <Pressable
          accessibilityLabel="Close"
          onPress={onClose}
          style={{ position: 'absolute', top: 0, left: 0, right: 0, bottom: 0, backgroundColor: c.scrim }}
        />
        <View
          style={{
            maxHeight: '88%',
            minHeight: '40%',
            width: '100%',
            maxWidth: 860,
            alignSelf: 'center',
            backgroundColor: c.bg,
            borderTopLeftRadius: 6,
            borderTopRightRadius: 6,
            borderColor: c.border,
            borderWidth: 1,
            paddingBottom: insets.bottom,
          }}
        >
          <View
            style={{
              flexDirection: 'row',
              alignItems: 'center',
              paddingLeft: space.lg,
              borderBottomWidth: 1,
              borderColor: c.border,
            }}
          >
            <Text style={{ flex: 1, color: c.text, fontSize: font.md, fontWeight: '600' }} numberOfLines={1}>
              {title}
            </Text>
            <Pressable
              accessibilityRole="button"
              onPress={onClose}
              style={{ minHeight: TOUCH, minWidth: TOUCH, paddingHorizontal: space.lg, justifyContent: 'center' }}
            >
              <Text style={{ color: c.accent, fontSize: font.sm }}>Close</Text>
            </Pressable>
          </View>
          {scroll ? (
            <ScrollView contentContainerStyle={{ padding: space.lg, gap: space.md }}>{children}</ScrollView>
          ) : (
            <View style={{ flex: 1 }}>{children}</View>
          )}
          {footer ? (
            <View style={{ padding: space.md, borderTopWidth: 1, borderColor: c.border, gap: space.sm }}>{footer}</View>
          ) : null}
        </View>
      </View>
    </Modal>
  );
}
