/// <reference types="jest" />
import { HIDE_AFTER, SHOW_AFTER, headerScroll, headerShow, headerStart, type HeaderHide } from '../src/lib/headerhide';

const H = 5000;
// Feed offsets (same content height); the hidden flag after each.
function run(ys: number[], s: HeaderHide = headerStart, h = H): { s: HeaderHide; seen: boolean[] } {
  const seen: boolean[] = [];
  for (const y of ys) {
    s = headerScroll(s, y, h);
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
  const grown = headerScroll(hid, 390, H + 300);
  expect(grown.hidden).toBe(true);
  // Shown, then growth: stays shown, and the anchor moved with the offset.
  const s = headerShow(grown);
  const g2 = headerScroll(s, 690, H + 600);
  expect(g2.hidden).toBe(false);
  expect(headerScroll(g2, 690 + HIDE_AFTER - 1, H + 600).hidden).toBe(false);
  expect(headerScroll(g2, 690 + HIDE_AFTER, H + 600).hidden).toBe(true);
});

test('a tap shows it until the reader scrolls on', () => {
  const s = headerShow(run([400]).s);
  expect(s.hidden).toBe(false);
  expect(run([410], s).s.hidden).toBe(false);
  expect(run([400 + HIDE_AFTER], s).s.hidden).toBe(true);
});
