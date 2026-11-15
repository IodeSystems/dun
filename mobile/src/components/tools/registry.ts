// Tool name → the body that opens under its call line, as the TUI registers
// its renderers (internal/tui/renderers.go and filerender.go init). A tool
// not named here, an MCP server's among them, gets GenericBody.

import type { ComponentType } from 'react';

import type { ToolCall } from '@/lib/toolcalls';
import { jsonTools, readGrep } from '@/lib/tooltext';

import { DiffBody, GenericBody, JsonBody, ReadBody, SearchBody, ShellBody, WriteBody, type BodyProps } from './bodies';

export const bodies: Record<string, ComponentType<BodyProps>> = {
  exec: ShellBody,
  file_edit: DiffBody,
  node_edit: DiffBody,
  file_read: ReadBody,
  node_read: ReadBody,
  get_document: ReadBody,
  file_write: WriteBody,
  file_grep: SearchBody,
  ...Object.fromEntries([...jsonTools].map((t) => [t, JsonBody])),
};

/** The body for a call: a file_read with a search is drawn as a grep (filerender.go). */
export function bodyFor(call: Pick<ToolCall, 'tool' | 'args'>): ComponentType<BodyProps> {
  if (call.tool === 'file_read' && readGrep(call.args)) return SearchBody;
  return bodies[call.tool] ?? GenericBody;
}
