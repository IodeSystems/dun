/// <reference types="jest" />
// The open question: multi checks the recommendations to start with and
// sends the picks comma-separated; single sends the option tapped; noCustom
// says to pick from the options.
import { act } from 'react';
import { createRoot, type TestInstance } from 'test-renderer';

import { AskPanel } from '../src/components/AskPanel';
import type { Ask } from '../src/lib/events';

const text = (n: TestInstance | string): string => (typeof n === 'string' ? n : n.children.map(text).join(''));
const render = (el: React.ReactElement) => {
  const root = createRoot();
  act(() => {
    root.render(el);
  });
  return root;
};
const buttons = (root: ReturnType<typeof createRoot>) =>
  root.container.queryAll((x) => x.props.accessibilityRole === 'button' && typeof x.props.onClick === 'function');
const press = (root: ReturnType<typeof createRoot>, label: string) => {
  const b = buttons(root).find((x) => text(x).includes(label));
  if (!b) throw new Error('no button ' + label + ' in ' + buttons(root).map(text).join(' | '));
  act(() => (b.props.onClick as (e: object) => void)({ nativeEvent: {} }));
};

const base: Ask = {
  askId: 'a',
  question: 'which?',
  options: ['red', 'blue', 'green'],
  recommended: 'blue',
  recommendedAll: ['blue'],
  multi: false,
  noCustom: false,
};

test('single: the tapped option is the answer', () => {
  const got: string[] = [];
  const root = render(<AskPanel ask={base} answer={(v) => got.push(v)} />);
  press(root, 'green');
  expect(got).toEqual(['green']);
});

test('multi: recommendations checked, toggles, picks sent comma-separated in option order', () => {
  const got: string[] = [];
  const root = render(
    <AskPanel ask={{ ...base, multi: true, recommendedAll: ['green'] }} answer={(v) => got.push(v)} />,
  );
  expect(buttons(root).map(text)).toContain('☑ green ★');
  press(root, 'red');
  press(root, 'Send 2');
  expect(got).toEqual(['red, green']);
});

test('noCustom says the answer is one of the options', () => {
  const root = render(<AskPanel ask={{ ...base, noCustom: true }} answer={() => {}} />);
  expect(text(root.container)).toContain('pick from the options');
});
