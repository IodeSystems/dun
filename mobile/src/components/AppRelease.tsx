import { useQuery } from '@tanstack/react-query';
import * as Application from 'expo-application';
import { Linking, Platform, Pressable, Text, View } from 'react-native';

import { Hello } from '@/api/ops';
import { font, space, useColors } from '@/lib/theme';
import { useAuth } from '@/providers/AuthProvider';

// The Android release dund announces (tools/release-android → /api/v1/hello):
// in the Android app, an update banner when it is newer than this install; in
// a browser, a link to get the app. Nothing elsewhere, or when none exists.
export function AppRelease() {
  const auth = useAuth();
  const c = useColors();
  const hello = useQuery({
    queryKey: ['hello', auth.baseUrl],
    enabled: !!auth.client && (Platform.OS === 'android' || Platform.OS === 'web'),
    staleTime: 10 * 60_000,
    queryFn: async () => (await auth.client!.request(Hello)).dund.hello?.android ?? null,
  });
  const rel = hello.data;
  if (!rel?.apkPath || Number(rel.versionCode) <= 0) return null;
  const url = auth.baseUrl.replace(/\/+$/, '') + rel.apkPath;
  let label: string;
  if (Platform.OS === 'android') {
    const installed = Number(Application.nativeBuildVersion ?? 0);
    if (installed >= Number(rel.versionCode)) return null;
    label = `dun ${rel.versionName ?? ''} is available — tap to install`;
  } else {
    label = `Get the Android app (${rel.versionName ?? ''})`;
  }
  return (
    <Pressable
      onPress={() => void Linking.openURL(url)}
      style={{ padding: space.md, borderRadius: 8, borderWidth: 1, borderColor: c.accent }}
    >
      <View style={{ gap: 2 }}>
        <Text style={{ color: c.accent, fontSize: font.md }}>{label}</Text>
        {Platform.OS === 'android' && rel.notes ? (
          <Text style={{ color: c.muted, fontSize: font.sm }}>{rel.notes}</Text>
        ) : null}
      </View>
    </Pressable>
  );
}
