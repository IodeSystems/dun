/// <reference types="jest" />
import { ago, sessionRows } from '../src/lib/sessionlist';

test('the list keeps the order dund sent', () => {
  const sent = [
    { id: '20260101-000000', workspace: '/a', running: true, lastActive: '2026-01-01T00:00:00Z' },
    { id: '20260301-000000', workspace: '/a', running: false, lastActive: '2026-03-01T00:00:00Z' },
    { id: '20260201-000000', workspace: '/b', running: false, lastActive: null },
  ];
  expect(sessionRows(sent).map((s) => s.id)).toEqual(sent.map((s) => s.id));
  expect(sessionRows(undefined)).toEqual([]);
});

test('ago', () => {
  const now = Date.parse('2026-01-02T00:00:00Z');
  expect(ago('2026-01-01T23:58:00Z', now)).toBe('2m ago');
  expect(ago('2025-12-30T00:00:00Z', now)).toBe('3d ago');
  expect(ago(null, now)).toBe('');
});
