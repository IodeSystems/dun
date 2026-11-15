/// <reference types="jest" />
// Hiding the session header changes no layout: the same elements in flow,
// the same sizes, padding and margins; only the slide's transform differs,
// and what hidden adds (the strip) is out of flow. A layout that moved with
// the header moved the transcript's scroll offset, which toggled the header
// again (lib/headerhide).
import { act } from 'react';
import { StyleSheet } from 'react-native';
import { createRoot, type TestInstance } from 'test-renderer';

import { SessionHeader } from '../src/components/header/SessionHeader';
import { empty } from '../src/lib/events';
import { noStats } from '../src/lib/stats';

jest.mock('expo-router', () => ({
  router: { back: () => undefined, canGoBack: () => true, replace: () => undefined },
}));
jest.mock('react-native-safe-area-context', () => ({
  useSafeAreaInsets: () => ({ top: 24, bottom: 0, left: 0, right: 0 }),
}));

const noop = () => undefined;
const act5 = { ctx: noop, plan: noop, tasks: noop, model: noop, session: noop };

// Every host element, in order, with its flattened style minus the transform.
type El = { type: string; style: Record<string, unknown>; label?: string };
function els(n: TestInstance, out: El[] = []): El[] {
  const style = { ...StyleSheet.flatten(n.props.style as never) } as Record<string, unknown>;
  delete style.transform;
  out.push({ type: String(n.type), style, label: n.props.accessibilityLabel as string | undefined });
  for (const c of n.children) if (typeof c !== 'string') els(c, out);
  return out;
}
function render(hidden: boolean): El[] {
  const root = createRoot();
  act(() => {
    root.render(
      <SessionHeader
        view={empty}
        stats={noStats}
        hidden={hidden}
        panel={false}
        onShow={noop}
        onHeight={noop}
        act={act5}
      />,
    );
  });
  return els(root.container);
}

test('hiding the header changes no layout, only the transform', () => {
  const shown = render(false);
  const hidden = render(true);
  // What hidden adds is absolutely positioned: out of the transcript's flow.
  const extra = hidden.filter((e) => e.label?.startsWith('Show the header'));
  expect(extra.length).toBe(1);
  for (const e of extra) expect(e.style.position).toBe('absolute');
  const rest = hidden.filter((e) => !extra.includes(e));
  expect(rest).toEqual(shown);
  // The whole header is out of flow too: it overlays the transcript.
  expect(shown.find((e) => e.style.position)?.style.position).toBe('absolute');
});
