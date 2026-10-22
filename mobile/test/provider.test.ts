/// <reference types="jest" />
import { modelLine, providerNameOk, providerUrlOk } from '../src/lib/provider';

test('names and URLs follow the engine rules', () => {
  expect(providerNameOk('corp')).toBe(true);
  expect(providerNameOk('my corp')).toBe(false);
  expect(providerNameOk('')).toBe(false);
  expect(providerUrlOk('https://litellm.corp/v1')).toBe(true);
  expect(providerUrlOk('litellm.corp')).toBe(false);
});

test('typed /model lines: search, the form, or an operation', () => {
  expect(modelLine('qwen 27b')).toEqual({ kind: 'search', query: 'qwen 27b' });
  expect(modelLine('add')).toEqual({ kind: 'add' });
  expect(modelLine('add corp https://x sk-1').kind).toBe('usage'); // a key in the composer is refused
  expect(modelLine('edit corp')).toEqual({ kind: 'edit', name: 'corp' });
  expect(modelLine('rm corp')).toEqual({ kind: 'op', op: { op: 'remove', name: 'corp' } });
  expect(modelLine('use corp gpt-x')).toEqual({ kind: 'op', op: { op: 'use', name: 'corp', model: 'gpt-x' } });
  expect(modelLine('use corp').kind).toBe('usage');
});
