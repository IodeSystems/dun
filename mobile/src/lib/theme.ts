import { useColorScheme } from 'react-native';

const light = {
  bg: '#fbfbfa',
  surface: '#ffffff',
  sunken: '#f4f3ef',
  border: '#e3e2de',
  text: '#1d1d1b',
  muted: '#6b6b66',
  accent: '#2f6fdb',
  accentText: '#ffffff',
  user: '#eef3fc',
  danger: '#b3261e',
};

const dark: typeof light = {
  bg: '#161615',
  surface: '#1e1e1c',
  sunken: '#232321',
  border: '#2c2c2a',
  text: '#e8e7e3',
  muted: '#9a9993',
  accent: '#7aa7f5',
  accentText: '#0d1726',
  user: '#1f2a3d',
  danger: '#f2867d',
};

export type Colors = typeof light;

export function useColors(): Colors {
  return useColorScheme() === 'dark' ? dark : light;
}

export const space = { xs: 4, sm: 8, md: 12, lg: 16, xl: 24 };
export const font = { sm: 13, md: 15, lg: 17, xl: 22 };
export const mono = 'monospace';
/** Minimum touch target, in dp. */
export const TOUCH = 44;
