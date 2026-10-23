/// <reference types="jest" />
import { apply, empty } from '../src/lib/events';
import { mcpIdOk, mcpSpec, toggleAction, toolsRequest, toolsSummary } from '../src/lib/mcp';

const el = {
  server: 'el',
  tools: [
    { name: 'tts_say', tokens: 900, enabled: true },
    { name: 'tts_list', tokens: 400, enabled: false, hiddenBy: 'tts_l*' },
    { name: 'voices', tokens: 300, enabled: true },
  ],
};

test('the tools summary counts what is on and what it costs', () => {
  expect(toolsSummary(el)).toBe('2 of 3 on · ~1.2k tokens');
});

test('a toggle flips the tool it is given', () => {
  expect(toggleAction('el', el.tools[0]!)).toBe('toggle el tts_say off');
  expect(toggleAction('el', el.tools[1]!)).toBe('toggle el tts_list on');
});

test('/mcp tools <id> names the server; bare or anything else does not', () => {
  expect(toolsRequest('/mcp tools el')).toBe('el');
  expect(toolsRequest('mcp TOOLS el ')).toBe('el');
  expect(toolsRequest('/mcp tools')).toBeNull();
  expect(toolsRequest('/mcp tools el x')).toBeNull();
});

test('an mcp_tools event keeps the list and notes its message', () => {
  const v = apply(empty, { type: 'mcp_tools', ...el, message: 'el: tts_say off' });
  expect(v.mcpTools).toEqual(el);
  expect(v.items.at(-1)).toMatchObject({ kind: 'note', text: 'el: tts_say off' });
  // The cost listing (no server) is a note and leaves the list alone.
  const w = apply(v, { type: 'mcp_tools', server: '', tools: [], message: 'mcp tool cost per request' });
  expect(w.mcpTools).toEqual(el);
});

test('ids follow the TUI form rule', () => {
  for (const ok of ['docs', 'my-server_2', 'a']) expect(mcpIdOk(ok)).toBe(true);
  for (const bad of ['', 'Docs', '-x', '_x', 'a b', 'x'.repeat(33)]) expect(mcpIdOk(bad)).toBe(false);
});

test('a URL is an HTTP server with its bearer; a command drops the bearer', () => {
  expect(mcpSpec('d', ' https://mcp.example/x ', ' tok ')).toEqual({
    id: 'd',
    url: 'https://mcp.example/x',
    bearer: 'tok',
  });
  expect(mcpSpec('d', 'raglit serve --mcp', 'tok')).toEqual({ id: 'd', command: 'raglit', args: ['serve', '--mcp'] });
});

test('direct is the default placement and is not written; the shell is', () => {
  expect(mcpSpec('d', 'x', '')).not.toHaveProperty('via');
  expect(mcpSpec('d', 'x', '', 'direct')).not.toHaveProperty('via');
  expect(mcpSpec('d', 'https://h/mcp', 't', 'shell')).toEqual({
    id: 'd',
    url: 'https://h/mcp',
    bearer: 't',
    via: 'shell',
  });
});
