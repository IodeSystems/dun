import * as SecureStore from 'expo-secure-store';
import { Platform } from 'react-native';

// Where the token and the server address live: the keystore on a phone,
// localStorage in a browser (expo-secure-store has no web implementation).

const TOKEN_KEY = 'dun.token';
const BASE_URL_KEY = 'dun.baseUrl';

/** The service behind hz. A standalone install is reached by changing it. */
export const REMOTE_BASE_URL = 'https://dun.iodesystems.com';

/**
 * In a browser the page is served by dund itself, so its own origin is the
 * service; a phone has no origin and starts at the remote address.
 */
export function defaultBaseUrl(): string {
  if (Platform.OS === 'web' && typeof window !== 'undefined' && window.location?.origin) {
    return window.location.origin;
  }
  return REMOTE_BASE_URL;
}

async function get(key: string): Promise<string | null> {
  if (Platform.OS === 'web') {
    try {
      return window.localStorage.getItem(key);
    } catch {
      return null;
    }
  }
  return SecureStore.getItemAsync(key);
}

async function set(key: string, value: string | null): Promise<void> {
  if (Platform.OS === 'web') {
    try {
      if (value === null) window.localStorage.removeItem(key);
      else window.localStorage.setItem(key, value);
    } catch {
      // private mode or blocked storage: the session lasts the tab
    }
    return;
  }
  if (value === null) await SecureStore.deleteItemAsync(key);
  else await SecureStore.setItemAsync(key, value);
}

export const loadToken = () => get(TOKEN_KEY);
export const saveToken = (t: string | null) => set(TOKEN_KEY, t);
export async function loadBaseUrl(): Promise<string> {
  return (await get(BASE_URL_KEY)) ?? defaultBaseUrl();
}
export const saveBaseUrl = (u: string) => set(BASE_URL_KEY, u);
