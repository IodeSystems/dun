/// <reference types="jest" />
import {
  HIDE_AFTER,
  SETTLE,
  SHOW_AFTER,
  fromNewest,
  headerScroll,
  headerShow,
  headerStart,
  type HeaderHide,
} from '../src/lib/headerhide';

const H = 5000;
let clock = 0; // events 1 s apart unless a test says otherwise: past SETTLE
// Feed offsets (same content height); the hidden flag after each.
function run(ys: number[], s: HeaderHide = headerStart, h = H): { s: HeaderHide; seen: boolean[] } {
  const seen: boolean[] = [];
  for (const y of ys) {
    s = headerScroll(s, y, h, (clock += 1000));
    seen.push(s.hidden);
  }
  return { s, seen };
}

test('a small scroll up does not hide the header', () => {
  expect(run([5, 10, 15, 20, HIDE_AFTER - 1]).seen.every((x) => !x)).toBe(true);
});

test('scrolling up past the threshold hides it, once', () => {
  const { seen } = run([10, 20, 30, 40, 60, 80, 120]);
  expect(seen).toEqual([false, false, true, true, true, true, true]);
});

test('jitter around a point toggles nothing', () => {
  const up = run([100]).s; // from the newest straight to 100: hidden
  expect(up.hidden).toBe(true);
  const { seen } = run([110, 95, 112, 92, 108, 100], up);
  expect(seen.every((x) => x)).toBe(true);
});

test('scrolling back down by the threshold shows it', () => {
  const hid = run([200, 300]).s;
  expect(hid.hidden).toBe(true);
  expect(run([300 - SHOW_AFTER + 1], hid).s.hidden).toBe(true);
  expect(run([300 - SHOW_AFTER], hid).s.hidden).toBe(false);
});

test('the newest row always shows it', () => {
  const hid = run([500]).s;
  expect(run([4], hid).s.hidden).toBe(false);
});

test('content growth moves the offset without toggling', () => {
  const hid = run([100, 90]).s; // hidden at 100; 10 back down is under SHOW_AFTER
  expect(hid.hidden).toBe(true);
  // A 300 dp row arrives: the offset jumps with it, the header stays hidden.
  const grown = headerScroll(hid, 390, H + 300, (clock += 1000));
  expect(grown.hidden).toBe(true);
  // Shown, then growth: stays shown, and the anchor moved with the offset.
  const s = headerShow(grown, (clock += 1000));
  const g2 = headerScroll(s, 690, H + 600, (clock += 1000));
  expect(g2.hidden).toBe(false);
  expect(headerScroll(g2, 690 + HIDE_AFTER - 1, H + 600, (clock += 1000)).hidden).toBe(false);
  expect(headerScroll(g2, 690 + HIDE_AFTER, H + 600, (clock += 1000)).hidden).toBe(true);
});

test('a tap shows it until the reader scrolls on', () => {
  const s = headerShow(run([400]).s, (clock += 1000));
  expect(s.hidden).toBe(false);
  expect(run([410], s).s.hidden).toBe(false);
  expect(run([400 + HIDE_AFTER], s).s.hidden).toBe(true);
});

// The raw offsets the transcript reports. Web: column-reverse, scrollTop runs
// 0 (newest) to -max, and iOS Safari's rubber band at the newest end pushes it
// positive. Native: inverted list, 0 to max, and iOS bounce at the newest end
// runs negative.
const MAX = H - 800;
function raw(ys: number[], web: boolean, s: HeaderHide = headerStart): { s: HeaderHide; seen: boolean[] } {
  const seen: boolean[] = [];
  for (const y of ys) {
    s = headerScroll(s, fromNewest(y, web, MAX), H, (clock += 1000));
    seen.push(s.hidden);
  }
  return { s, seen };
}

test('web: the rubber band past the newest row does not hide it', () => {
  // A fling down to the newest: scrollTop overshoots positive and settles at 0.
  const { seen } = raw([-120, -60, -10, 0, 15, 40, 60, 40, 15, 0], true, headerStart);
  expect(seen.slice(3)).toEqual([false, false, false, false, false, false, false]);
});

test('native: the bounce past the newest row does not hide it', () => {
  const { seen } = raw([120, 60, 10, 0, -15, -40, -60, -40, -15, 0], false);
  expect(seen.slice(3).every((x) => !x)).toBe(true);
});

test('web: a pull at the newest row while hidden shows it and stays shown', () => {
  const hid = raw([-300], true).s;
  expect(hid.hidden).toBe(true);
  expect(raw([-100, -20, 0, 30, 50, 30, 0], true, hid).seen).toEqual([false, false, false, false, false, false, false]);
});

test('the bounce past the oldest row does not show it', () => {
  // Reading at the top of the history: the bounce runs past -max and back.
  const web = raw([-(MAX - 100), -MAX, -(MAX + 40), -(MAX + 60), -(MAX + 20), -MAX], true);
  expect(web.seen.every((x) => x)).toBe(true);
  const nat = raw([MAX - 100, MAX, MAX + 40, MAX + 60, MAX + 20, MAX], false);
  expect(nat.seen.every((x) => x)).toBe(true);
});

test('a slow drag near the newest row: one hide, one show', () => {
  const ys = [0, -2, -4, -6, -8, -10, -14, -20, -26, -31, -33, -36, -30, -24, -18, -12, -6, 0, 4, 9, 4, 0];
  const { seen } = raw(ys, true);
  const flips = seen.filter((x, i) => i > 0 && x !== seen[i - 1]).length;
  expect(flips).toBe(2);
});

test('content shorter than the screen never hides it', () => {
  let s = headerStart;
  for (const y of [0, 10, 40, 60, 20, 0]) {
    s = headerScroll(s, fromNewest(y, false, 0), 300, (clock += 1000));
    expect(s.hidden).toBe(false);
  }
});

// The feedback loop: a toggle that moves the offset itself (a layout that
// changes with the header, as the old height animation did: hiding grew the
// transcript and the offset fell back by the header's height, showing shrank
// it again). Frames 16 ms apart, no input after the first scroll; the
// offset moves only by what each toggle injects one frame later.
const HEADER = 56;
function feedback(start: number, to: number): boolean[] {
  // Shown at start (a tap, or the newest row), then the reader's one scroll.
  let s = headerShow(headerScroll(headerStart, start, H, -2000), -1000);
  let y = to;
  let pending = 0;
  const seen: boolean[] = [];
  for (let t = 16; t < 3000; t += 16) {
    y += pending;
    pending = 0;
    const was = s.hidden;
    s = headerScroll(s, Math.max(0, y), H, t);
    if (s.hidden !== was) pending = s.hidden ? -HEADER : HEADER;
    seen.push(s.hidden);
  }
  return seen;
}
const flips = (seen: boolean[]) => seen.filter((x, i) => i > 0 && x !== seen[i - 1]).length;

test('a toggle that moves the offset by the header height does not toggle it back', () => {
  const seen = feedback(200, 240); // the reader scrolls 40 into the history: hidden
  expect(seen[0]).toBe(true);
  expect(flips(seen)).toBe(0);
});

test('near the newest row the same loop settles after one show', () => {
  // Hidden at 40; the injected -56 reaches the newest row, which always
  // shows it; the +56 that showing injects is ignored. Then nothing moves.
  const seen = feedback(0, 40);
  expect(flips(seen)).toBeLessThanOrEqual(1);
  expect(flips(seen.slice(20))).toBe(0);
});

test('a reader scrolling right after a toggle is heard once it settles', () => {
  let s = headerScroll(headerStart, 0, H, 0);
  s = headerScroll(s, 40, H, 16); // hidden
  expect(s.hidden).toBe(true);
  s = headerScroll(s, 10, H, 32); // inside the settle window: no show
  expect(s.hidden).toBe(true);
  s = headerScroll(s, 10, H, 32 + SETTLE); // settled, anchored at 10
  s = headerScroll(s, 9, H, 48 + SETTLE);
  expect(s.hidden).toBe(true);
  expect(headerScroll(s, 4, H, 64 + SETTLE).hidden).toBe(false); // the newest row
});
