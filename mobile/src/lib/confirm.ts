import { Alert, Platform } from 'react-native';

/** Ask before something that cannot be undone. RN-web has no Alert. */
export function confirm(title: string, message: string): Promise<boolean> {
  if (Platform.OS === 'web') return Promise.resolve(window.confirm(`${title}\n\n${message}`));
  return new Promise((resolve) =>
    Alert.alert(title, message, [
      { text: 'Cancel', style: 'cancel', onPress: () => resolve(false) },
      { text: title, style: 'destructive', onPress: () => resolve(true) },
    ]),
  );
}
