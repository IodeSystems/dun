// Whether the session header steps aside, from the transcript's scroll
// offset alone. y is the distance from the newest row (0 = at the newest),
// h the content's height, t the event's time (ms). The header overlays the
// transcript, so hiding it moves nothing; the thresholds keep small jitter
// from toggling it, and the settle window keeps anything that a toggle itself
// moves (a layout change, a re-anchor) from toggling it back: a loop with no
// reader in it.

/** Scroll up this far (dp, cumulative) into the history: the header hides. */
export const HIDE_AFTER = 24;
/** Scroll back down this far: the header comes back. */
export const SHOW_AFTER = 24;
/** Within this of the newest row the header is always shown. */
export const AT_NEWEST = 8;
/** For this long after a toggle (ms; the slide is 180) offsets only re-anchor. */
export const SETTLE = 300;

export type HeaderHide = {
  hidden: boolean;
  /** Shown: the lowest offset since it showed; hidden: the highest since it hid. */
  anchor: number;
  /** The last offset and content height seen. */
  y: number;
  h: number;
  /** When it last toggled. */
  at: number;
};

export const headerStart: HeaderHide = { hidden: false, anchor: 0, y: 0, h: -1, at: -Infinity };

/** The next state for a scroll event at offset y with content height h, at time t. */
export function headerScroll(s: HeaderHide, y: number, h: number, t: number): HeaderHide {
  // The newest row shows it, settling or not: no later event would.
  if (y <= AT_NEWEST) return { hidden: false, anchor: y, y, h, at: s.hidden ? t : s.at };
  // Just toggled: whatever moved, the reader is measured from here.
  if (t - s.at < SETTLE) return { ...s, anchor: y, y, h };
  // The content grew or shrank (a row arrived, a block folded): the offset
  // moved with it, not with the reader. Carry the anchor along; no toggle.
  if (s.h >= 0 && h !== s.h) return { ...s, anchor: s.anchor + (y - s.y), y, h };
  if (s.hidden) {
    const anchor = Math.max(s.anchor, y);
    return anchor - y >= SHOW_AFTER ? { hidden: false, anchor: y, y, h, at: t } : { ...s, anchor, y, h };
  }
  const anchor = Math.min(s.anchor, y);
  return y - anchor >= HIDE_AFTER ? { hidden: true, anchor: y, y, h, at: t } : { ...s, anchor, y, h };
}

/** Brought back by a tap on the strip: shown, and hides again only after HIDE_AFTER more. */
export function headerShow(s: HeaderHide, t: number): HeaderHide {
  return { ...s, hidden: false, anchor: s.y, at: t };
}

/**
 * The distance from the newest row for a raw scroll offset y, with max the
 * furthest the content scrolls (content height minus the viewport). Web
 * (column-reverse): scrollTop runs 0 at the newest to -max. Native (inverted
 * list): 0 to max. A bounce past either end (iOS Safari's rubber band runs
 * positive at the newest end on the web, negative on native) is clamped to
 * that end, so it reads as no movement, not as a scroll the other way.
 */
export function fromNewest(y: number, web: boolean, max: number): number {
  const d = web ? -y : y;
  return Math.min(Math.max(d, 0), Math.max(max, 0));
}
