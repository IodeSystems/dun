import type { ReactNode } from 'react';
import { Pressable, Text, TextInput, type TextInputProps, View, type ViewStyle } from 'react-native';

import { font, space, TOUCH, useColors } from '@/lib/theme';

export function Button({ title, onPress, primary, disabled }: { title: string; onPress: () => void; primary?: boolean; disabled?: boolean }) {
  const c = useColors();
  return (
    <Pressable
      accessibilityRole="button"
      onPress={onPress}
      disabled={disabled}
      style={({ pressed }) => ({
        minHeight: TOUCH,
        paddingHorizontal: space.lg,
        borderRadius: 8,
        alignItems: 'center',
        justifyContent: 'center',
        borderWidth: 1,
        borderColor: primary ? c.accent : c.border,
        backgroundColor: primary ? c.accent : c.surface,
        opacity: disabled ? 0.5 : pressed ? 0.8 : 1,
      })}>
      <Text style={{ color: primary ? c.accentText : c.text, fontSize: font.md, fontWeight: '600' }}>{title}</Text>
    </Pressable>
  );
}

export function Field(props: TextInputProps) {
  const c = useColors();
  return (
    <TextInput
      placeholderTextColor={c.muted}
      {...props}
      style={[
        { minHeight: TOUCH, borderWidth: 1, borderColor: c.border, borderRadius: 8, paddingHorizontal: space.md,
          backgroundColor: c.surface, color: c.text, fontSize: font.md },
        props.style,
      ]}
    />
  );
}

export function Screen({ children, style }: { children: ReactNode; style?: ViewStyle }) {
  const c = useColors();
  return <View style={[{ flex: 1, backgroundColor: c.bg }, style]}>{children}</View>;
}

export function Muted({ children }: { children: ReactNode }) {
  const c = useColors();
  return <Text style={{ color: c.muted, fontSize: font.sm }}>{children}</Text>;
}

export function ErrorText({ children }: { children: ReactNode }) {
  const c = useColors();
  return <Text style={{ color: c.danger, fontSize: font.sm }}>{children}</Text>;
}
