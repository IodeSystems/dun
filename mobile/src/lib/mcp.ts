// Adding an MCP server: the spec the engine's `/mcp add` takes, built as the
// TUI's form builds it (cmd/dun mcpform.go: mcpFormSpec, mcpIDOK).

export type ServerSpec = { id: string; url?: string; bearer?: string; command?: string; args?: string[] };

/** An id is lowercase letters, digits, - and _: it prefixes the server's tools. */
export function mcpIdOk(id: string): boolean {
  return /^[a-z0-9][a-z0-9_-]{0,31}$/.test(id);
}

/** A URL is an HTTP server (a bearer applies); anything else is a command. */
export function mcpSpec(id: string, target: string, bearer: string): ServerSpec {
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
  return spec;
}
