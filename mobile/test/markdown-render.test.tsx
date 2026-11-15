/// <reference types="jest" />
import { act } from 'react';
import { Linking, type TextStyle } from 'react-native';
import { createRoot, type TestInstance } from 'test-renderer';

import { Markdown } from '../src/components/markdown/Markdown';
import { font } from '../src/lib/theme';

import { FORMS } from './fixtures/flowdown-forms';

const render = (text: string, streaming = false) => {
  const root = createRoot();
  act(() => {
    root.render(<Markdown text={text} streaming={streaming} />);
  });
  return root;
};

// The block views: children of the reply's outer View.
const blocks = (root: ReturnType<typeof createRoot>): TestInstance[] =>
  (root.container.children[0] as TestInstance).children.filter((c): c is TestInstance => typeof c !== 'string');

const text = (n: TestInstance | string): string => (typeof n === 'string' ? n : n.children.map(text).join(''));

test('renders each node kind with RN host components', () => {
  const md = [
    '## Title',
    'a *b* **c** `d` [e](https://x.dev) ![pic](https://x.dev/p.png)',
    '1. one\n2. two',
    '- [x] done',
    '> q',
    '```sh\necho hi\n```',
    '| h1 | h2 |\n|---|---|\n| x | y |',
    '***',
    '<b>raw</b>',
  ].join('\n\n');
  const root = render(md);
  expect(root.container.toJSON()).toMatchSnapshot();
  const all = blocks(root);
  expect(all).toHaveLength(9);
  expect(text(all[0]!)).toBe('Title');
  expect(text(all[1]!)).toBe('a b c d e [image: pic]');
  expect(text(all[2]!)).toBe('1.one2.two');
  expect(text(all[3]!)).toBe('☑done');
  expect(text(all[5]!)).toBe('shecho hi');
  expect(text(all[8]!)).toContain('<b>raw</b>');
  // The image is not fetched: there is no Image host anywhere.
  expect(root.container.queryAll((n) => n.type === 'Image')).toHaveLength(0);
});

test('a link opens through Linking', () => {
  const open = jest.spyOn(Linking, 'openURL').mockResolvedValue(undefined);
  const root = render('[go](https://x.dev) [bad](javascript:alert(1))');
  const links = root.container.queryAll((n) => n.props.accessibilityRole === 'link');
  expect(links).toHaveLength(1);
  act(() => {
    (links[0]!.props.onPress as () => void)();
  });
  expect(open).toHaveBeenCalledWith('https://x.dev');
});

test('streaming an open fence renders it as code; settled blocks are not redrawn', () => {
  const root = createRoot();
  act(() => {
    root.render(<Markdown text={'Para one.\n\n```js\nlet a'} streaming />);
  });
  const first = blocks(root)[0]!;
  const firstProps = first.props;
  expect(text(blocks(root)[1]!)).toBe('jslet a');
  act(() => {
    root.render(<Markdown text={'Para one.\n\n```js\nlet a = 1;\n'} streaming />);
  });
  expect(text(blocks(root)[1]!)).toBe('jslet a = 1;');
  // A memoized block that did not re-render keeps its committed props object.
  expect(blocks(root)[0]!.props).toBe(firstProps);
});

// Every form of flowdown's dialect (the ones dun's TUI draws) as its own view, not as the
// quote, list or code block it is written as. The fixture holds one of each.
describe('flowdown forms', () => {
  const root = render(FORMS);
  const shown = () => text(root.container.children[0] as TestInstance);
  const all = shown();
  // A Pressable's host View takes the tap as onClick.
  const press = (n: TestInstance) => act(() => (n.props.onClick as (e: object) => void)({ nativeEvent: {} }));
  const tappable = (role: string) =>
    root.container.queryAll((n) => n.props.accessibilityRole === role && typeof n.props.onClick === 'function');

  test('no marker or formula source shows as text', () => {
    expect(all).not.toMatch(/\[!\w+\]/);
    expect(all).not.toContain('\\frac');
    expect(all).not.toContain('\\alpha');
    expect(all).not.toContain('$$');
  });

  test('inline forms', () => {
    expect(all).toContain('1.2 Numbered heading');
    expect(all).toContain('x² + α');
    expect(all).toContain('a price of $5.');
    expect(all).toContain('Footnote reference[1].');
    expect(all).toContain('[image: A figure]\nThe figure caption');
    expect(all).toContain('— Somebody Famous');
    const links = root.container.queryAll((n) => n.props.accessibilityRole === 'link').map((n) => text(n));
    expect(links).toEqual(expect.arrayContaining(['ref link', 'https://go.dev']));
  });

  test('callouts carry their kind and title', () => {
    for (const label of ['Note', 'Tip · A titled tip', 'Important', 'Warning', 'Caution · Careful']) {
      expect(root.container.queryAll((n) => n.type === 'Text' && text(n) === label).length).toBeGreaterThan(0);
    }
  });

  test('steps, stats, sources, verdicts', () => {
    expect(all).toContain('Recovery');
    expect(all).toContain('First 24 hours Day 1');
    const big = root.container.queryAll(
      (n) => n.type === 'Text' && (n.props.style as TextStyle | undefined)?.fontSize === font.xl,
    );
    expect(big.map((n) => text(n))).toEqual(expect.arrayContaining(['3×', '−40%']));
    expect(all).toContain('[1]Page title · example.com the main reference');
    expect(all).toContain('✓typed APIs');
    expect(all).toContain('✗a build step');
  });

  test('tabs show one panel; a tap shows another', () => {
    const tabs = tappable('tab');
    expect(tabs.map((t) => text(t))).toEqual(['macOS', 'Linux']);
    expect(shown()).toContain('brew install thing');
    expect(shown()).not.toContain('Use the package manager.');
    press(tabs[1]!);
    expect(shown()).toContain('Use the package manager.');
    expect(shown()).not.toContain('brew install thing');
  });

  test('details fold until tapped', () => {
    expect(shown()).toContain('▸ Full log');
    expect(shown()).not.toContain('The long part, quoted.');
    press(tappable('button').find((n) => text(n).includes('Full log'))!);
    expect(shown()).toContain('▾ Full log');
    expect(shown()).toContain('The long part, quoted.');
  });

  test('fences: titled code, tree, diagram, formulas', () => {
    expect(all).toContain('src/app.ts · ts');
    expect(all).toContain('├─ app.ts  entry point');
    expect(all).toContain('└─ features/');
    expect(all).toContain('   └─ chat.ts');
    expect(all).toContain('diagram · mermaid');
    expect(all).toContain('1/k ∑ᵢ₌₁ᵏ rᵢ');
    expect(all).toContain('E = mc²');
  });
});
