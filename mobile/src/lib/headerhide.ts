// Whether the session header steps aside, from the transcript's scroll
// offset alone. y is the distance from the newest row (0 = at the newest),
// h the content's height. The header overlays the transcript, so hiding it
// moves nothing; the thresholds keep small jitter from toggling it.

/** Scroll up this far (dp, cumulative) into the history: the header hides. */
export const HIDE_AFTER = 24;
/** Scroll back down this far: the header comes back. */
export const SHOW_AFTER = 24;
/** Within this of the newest row the header is always shown. */
export const AT_NEWEST = 8;

export type HeaderHide = {
  hidden: boolean;
  /** Shown: the lowest offset since it showed; hidden: the highest since it hid. */
  anchor: number;
  /** The last offset and content height seen. */
  y: number;
  h: number;
};

export const headerStart: HeaderHide = { hidden: false, anchor: 0, y: 0, h: -1 };

/** The next state for a scroll event at offset y with content height h. */
export function headerScroll(s: HeaderHide, y: number, h: number): HeaderHide {
  if (y <= AT_NEWEST) return { hidden: false, anchor: y, y, h };
  // The content grew or shrank (a row arrived, a block folded): the offset
  // moved with it, not with the reader. Carry the anchor along; no toggle.
  if (s.h >= 0 && h !== s.h) return { ...s, anchor: s.anchor + (y - s.y), y, h };
  if (s.hidden) {
    const anchor = Math.max(s.anchor, y);
    return anchor - y >= SHOW_AFTER ? { hidden: false, anchor: y, y, h } : { hidden: true, anchor, y, h };
  }
  const anchor = Math.min(s.anchor, y);
  return y - anchor >= HIDE_AFTER ? { hidden: true, anchor: y, y, h } : { hidden: false, anchor, y, h };
}

/** Brought back by a tap on the strip: shown, and hides again only after HIDE_AFTER more. */
export function headerShow(s: HeaderHide): HeaderHide {
  return { ...s, hidden: false, anchor: s.y };
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
