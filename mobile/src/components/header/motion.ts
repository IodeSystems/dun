import { useEffect, useState } from 'react';
import { AccessibilityInfo } from 'react-native';

/** The person asked for less motion (the OS setting; prefers-reduced-motion on the web). */
export function useReducedMotion(): boolean {
  const [reduced, setReduced] = useState(false);
  useEffect(() => {
    let live = true;
    void AccessibilityInfo.isReduceMotionEnabled()
      .then((r) => {
        if (live) setReduced(r);
      })
      .catch(() => undefined);
    const sub = AccessibilityInfo.addEventListener('reduceMotionChanged', setReduced);
    return () => {
      live = false;
      sub?.remove();
    };
  }, []);
  return reduced;
}

/** A tick count every ms while on (the TUI's spinner phase), 0 while off. */
export function usePhase(on: boolean, ms = 100): number {
  const [phase, setPhase] = useState(0);
  useEffect(() => {
    if (!on) return;
    const t = setInterval(() => setPhase((p) => p + 1), ms);
    return () => clearInterval(t);
  }, [on, ms]);
  return on ? phase : 0;
}

/** Date.now(), again every second while on. */
export function useNow(on: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!on) return;
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, [on]);
  return now;
}
