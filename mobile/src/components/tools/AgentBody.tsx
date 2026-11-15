// The agent tool's open body: the child it reached, as the agents event has
// it (model, state, status, its task), the way to its view, then the call's
// args and result as any tool's.

import { Text, View } from 'react-native';

import { agentMeta } from '@/lib/agentcall';
import { font, space, useColors } from '@/lib/theme';

import { OpenAgent, useAgentOpen } from '../agents/AgentOpen';
import { GenericBody, type BodyProps } from './bodies';

export function AgentBody({ call }: BodyProps) {
  const c = useColors();
  const ctx = useAgentOpen();
  const a = call.agent ? ctx?.agents.find((x) => x.id === call.agent) : undefined;
  const meta = agentMeta(a);
  return (
    <View style={{ gap: space.xs }}>
      {call.agent ? (
        <View style={{ gap: 2 }}>
          <Text style={{ color: c.text, fontSize: font.sm }} testID="agent-head">
            agent #{call.agent}
            {meta ? <Text style={{ color: c.muted }}> · {meta}</Text> : null}
          </Text>
          {a?.status ? (
            <Text selectable style={{ color: c.muted, fontSize: font.sm }}>
              status: {a.status}
            </Text>
          ) : null}
          <OpenAgent agent={call.agent} />
        </View>
      ) : null}
      <GenericBody call={call} />
    </View>
  );
}
