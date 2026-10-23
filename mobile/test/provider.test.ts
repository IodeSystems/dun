/// <reference types="jest" />
import { providerLine, providerNameOk, providerUrlOk } from '../src/lib/provider';

test('names and URLs follow the engine rules', () => {
  expect(providerNameOk('corp')).toBe(true);
  expect(providerNameOk('my corp')).toBe(false);
  expect(providerNameOk('')).toBe(false);
  expect(providerUrlOk('https://litellm.corp/v1')).toBe(true);
  expect(providerUrlOk('litellm.corp')).toBe(false);
});

test('typed /model, /login and /logout', () => {
  expect(providerLine('model', ['qwen', '27b'])).toEqual({ kind: 'search', query: 'qwen 27b' });
  expect(providerLine('model', ['use', 'corp', 'gpt-x'])).toEqual({
    kind: 'op',
    op: { op: 'use', name: 'corp', model: 'gpt-x' },
  });
  expect(providerLine('model', ['use', 'corp']).kind).toBe('usage');
  expect(providerLine('login', [])).toEqual({ kind: 'login', name: '' });
  expect(providerLine('login', ['corp'])).toEqual({ kind: 'login', name: 'corp' });
  expect(providerLine('login', ['corp', 'https://x', 'sk-1']).kind).toBe('usage'); // a key in the composer is refused
  expect(providerLine('logout', ['corp'])).toEqual({ kind: 'op', op: { op: 'logout', name: 'corp' } });
  expect(providerLine('logout', []).kind).toBe('usage');
});
