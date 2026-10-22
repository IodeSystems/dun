// Adding an MCP server: the spec the engine's `/mcp add` takes, built as the
// TUI's form builds it (cmd/dun mcpform.go: mcpFormSpec, mcpIDOK).

/** Where a server runs: direct (dun connects to it; the default) or in the shell (mcpshell hosts it inside eval). */
export type Placement = 'direct' | 'shell';

export type ServerSpec = { id: string; url?: string; bearer?: string; command?: string; args?: string[]; via?: 'shell' };

/** What each placement costs and keeps — said where the choice is made (cmd/dun mcpform.go, servers.go). */
export const placementNote: Record<Placement, string> = {
  direct: "Direct: each tool's schema rides every request; docs indexing, prompts, skills and tool views work.",
  shell:
    'In the shell: one eval namespace, no tool schemas in every request — but no docs indexing, no prompts or skills, no tool deny-list, and its calls show as eval.',
};

/** One tool of a server, as the engine's mcp_tools event lists it (proto.McpTool). */
export type McpTool = { name: string; description?: string; schema?: string; tokens: number; enabled: boolean; hiddenBy?: string };
export type McpTools = { server: string; tools: McpTool[] };

/** "3 of 5 on · ~1.2k tokens": what the enabled tools cost in every request. */
export function toolsSummary(t: McpTools): string {
  const on = t.tools.filter((x) => x.enabled);
  return `${on.length} of ${t.tools.length} on · ~${kTokens(on.reduce((n, x) => n + x.tokens, 0))} tokens`;
}

export function kTokens(n: number): string {
  return n >= 1000 ? (n / 1000).toFixed(1) + 'k' : String(n);
}

/** The control action that flips one tool (control "mcptools"). */
export function toggleAction(server: string, tool: McpTool): string {
  return `toggle ${server} ${tool.name} ${tool.enabled ? 'off' : 'on'}`;
}

/** The server a typed "/mcp tools <id>" asks for, or null. */
export function toolsRequest(line: string): string | null {
  const m = /^\/?mcp\s+tools\s+(\S+)\s*$/i.exec(line.trim());
  return m ? (m[1] ?? null) : null;
}

/** An id is lowercase letters, digits, - and _: it prefixes the server's tools. */
export function mcpIdOk(id: string): boolean {
  return /^[a-z0-9][a-z0-9_-]{0,31}$/.test(id);
}

/** A URL is an HTTP server (a bearer applies); anything else is a command. */
export function mcpSpec(id: string, target: string, bearer: string, placement: Placement = 'direct'): ServerSpec {
  const t = target.trim();
  const spec: ServerSpec = { id: id.trim() };
  if (/^https?:\/\//.test(t)) {
    spec.url = t;
    if (bearer.trim()) spec.bearer = bearer.trim();
  } else {
    const [command, ...args] = t.split(/\s+/).filter(Boolean);
    if (command) spec.command = command;
    if (args.length) spec.args = args;
  }
  if (placement === 'shell') spec.via = 'shell';
  return spec;
}
