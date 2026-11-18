// "Copy raw" puts a message's markdown source on the clipboard.
import * as Clipboard from 'expo-clipboard';

import type { Item } from '../src/lib/events';
import { copyRaw, rawText } from '../src/lib/rawtext';

jest.mock('expo-clipboard', () => ({ setStringAsync: jest.fn(() => Promise.resolve(true)) }));

const assistant: Item = { key: '1', kind: 'assistant', text: '# Hi\n\n**bold**' };

test('rawText is the source of a message, null for the rest', () => {
  expect(rawText(assistant)).toBe('# Hi\n\n**bold**');
  expect(rawText({ key: '2', kind: 'user', text: 'typed' })).toBe('typed');
  expect(rawText({ key: '3', kind: 'note', text: 'n' })).toBeNull();
});

test('copyRaw writes the unrendered text and counts lines', async () => {
  expect(await copyRaw(assistant)).toBe(3);
  expect(Clipboard.setStringAsync).toHaveBeenCalledWith('# Hi\n\n**bold**');
});
