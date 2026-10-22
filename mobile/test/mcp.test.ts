/// <reference types="jest" />
import { mcpIdOk, mcpSpec } from '../src/lib/mcp';

test('ids follow the TUI form rule', () => {
  for (const ok of ['docs', 'my-server_2', 'a']) expect(mcpIdOk(ok)).toBe(true);
  for (const bad of ['', 'Docs', '-x', '_x', 'a b', 'x'.repeat(33)]) expect(mcpIdOk(bad)).toBe(false);
});

test('a URL is an HTTP server with its bearer; a command drops the bearer', () => {
  expect(mcpSpec('d', ' https://mcp.example/x ', ' tok ')).toEqual({ id: 'd', url: 'https://mcp.example/x', bearer: 'tok' });
  expect(mcpSpec('d', 'raglit serve --mcp', 'tok')).toEqual({ id: 'd', command: 'raglit', args: ['serve', '--mcp'] });
});

test('direct is the default placement and is not written; the shell is', () => {
  expect(mcpSpec('d', 'x', '')).not.toHaveProperty('via');
  expect(mcpSpec('d', 'x', '', 'direct')).not.toHaveProperty('via');
  expect(mcpSpec('d', 'https://h/mcp', 't', 'shell')).toEqual({ id: 'd', url: 'https://h/mcp', bearer: 't', via: 'shell' });
});
