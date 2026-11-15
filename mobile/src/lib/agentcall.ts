// The agent tool's call line, from structure only: its args, the sub-agent
// the engine named on it (ToolCall.agent), that agent's row in the agents
// event, and the plan's node titles. Pure.

import type { Agent } from './activity';
import type { ToolCall } from './toolcalls';

const str = (v: unknown) => (typeof v === 'string' ? v.trim() : '');
const clip = (s: string, n: number) => (s.length > n ? s.slice(0, n - 1) + '…' : s);
const oneLine = (s: string) => s.replace(/\s+/g, ' ').trim();

/**
 * What the call did, said once: "start agent #2 on node 3 build", "tell
 * agent #2: …", "dismiss agent #2". A spawn before its result names no
 * number yet: "start agent on node 3".
 */
export function agentCallTitle(call: Pick<ToolCall, 'args' | 'agent'>, nodes?: Record<string, string>): string {
  const a = call.args ?? {};
  const who = call.agent ? `agent #${call.agent}` : 'agent';
  const node = str(a.node);
  const onNode = node ? ` on node ${node}${nodes?.[node] ? ' ' + nodes[node] : ''}` : '';
  if (a.id === undefined) return clip('start ' + who + onNode, 80);
  const tell = str(a.tell);
  if (a.dismiss === true) return 'dismiss ' + who;
  if (a.resume === true) return 'resume ' + who;
  if (node)
    return clip(
      `give ${who} node ${node}${nodes?.[node] ? ' ' + nodes[node] : ''}${tell ? ': ' + oneLine(tell) : ''}`,
      80,
    );
  if (tell) return clip(`tell ${who}: ${oneLine(tell)}`, 80);
  return who + ' report';
}

/** The agent's model and state, from the agents event: "kid · idle". '' when it is not listed. */
export function agentMeta(agent: Agent | undefined): string {
  if (!agent) return '';
  return [agent.model, agent.state].filter(Boolean).join(' · ');
}
