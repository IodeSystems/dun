import { useColorScheme } from 'react-native';

export const light = {
  bg: '#fbfbfa',
  surface: '#ffffff',
  sunken: '#f4f3ef',
  /** An open fold inside another open fold: darker than sunken, so the parent shows. */
  fold: '#eae8e1',
  border: '#e3e2de',
  text: '#1d1d1b',
  muted: '#6b6b66',
  accent: '#2f6fdb',
  accentText: '#ffffff',
  user: '#eef3fc',
  danger: '#b3261e',
  /** Proactive notices: the TUI's stNote (256-colour 214, orange). */
  notice: '#9a5a00',
  /** A question waiting on the person: the TUI's stAsk (256-colour 213, pink). */
  ask: '#b02a9c',
  /** The agent at work: the TUI's spinner and StTool (green). */
  working: '#1f7a3a',
  /** Dims the screen under a sheet. */
  scrim: '#00000066',
  /** Background tints of a diff's added and removed lines. */
  diffAdd: '#e3f4ea',
  diffDel: '#fbe5e3',
  /**
   * Code colours by role (components/code/highlight.ts Role): the TUI's glamour "light"
   * chroma style (internal/tui/render.go mdStyle), darkened where under 4.5:1 on sunken.
   */
  syntax: {
    comment: '#6e6e6e',
    keyword: '#036fc4',
    type: '#7049c2',
    string: '#7e5b38',
    number: '#157d6b',
    function: '#017c44',
    builtin: '#0a1bb1',
    operator: '#c41a1a',
    punctuation: '#a04848',
    tag: '#581290',
    attr: '#8362cb',
    decorator: '#707017',
    preproc: '#cf3400',
    inserted: '#007b4d',
    deleted: '#c40303',
  },
};

export const dark: typeof light = {
  bg: '#161615',
  surface: '#1e1e1c',
  sunken: '#232321',
  fold: '#2e2e2b',
  border: '#2c2c2a',
  text: '#e8e7e3',
  muted: '#9a9993',
  accent: '#7aa7f5',
  accentText: '#0d1726',
  user: '#1f2a3d',
  danger: '#f2867d',
  notice: '#f0a742',
  ask: '#ff87ff',
  working: '#5fd75f',
  scrim: '#00000099',
  diffAdd: '#1c3326',
  diffDel: '#3a2220',
  /** The glamour "dark" chroma style; comment, type and attr lifted to 4:1 on sunken. */
  syntax: {
    comment: '#818181',
    keyword: '#00aaff',
    type: '#8282dd',
    string: '#c69669',
    number: '#6eefc0',
    function: '#00d787',
    builtin: '#ff8ec7',
    operator: '#ef8080',
    punctuation: '#e8e8a8',
    tag: '#b083ea',
    attr: '#7e7ee7',
    decorator: '#ffff87',
    preproc: '#ff875f',
    inserted: '#00d787',
    deleted: '#fd5b5b',
  },
};

export type Colors = typeof light;

export function useColors(): Colors {
  return useColorScheme() === 'dark' ? dark : light;
}

export const space = { xs: 4, sm: 8, md: 12, lg: 16, xl: 24 };
// Text sizes, in dp (x1.2 of the earlier 13/15/17/22/22).
export const font = { sm: 19, md: 22, lg: 24, xl: 31, composer: 23 };
export const mono = 'monospace';
/** Minimum touch target, in dp. */
export const TOUCH = 44;
