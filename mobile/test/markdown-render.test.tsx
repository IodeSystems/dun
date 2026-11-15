/// <reference types="jest" />
import { act } from 'react';
import { Linking } from 'react-native';
import { createRoot, type TestInstance } from 'test-renderer';

import { Markdown } from '../src/components/markdown/Markdown';

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
