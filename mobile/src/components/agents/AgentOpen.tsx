// One way to open a sub-agent's view, shared by every place that names one:
// the header badge's sheet, an agent tool call, a notice from an agent, a
// plan node an agent owns. The session screen provides it (open: the tasks
// sheet on that agent's conversation); rows read it here instead of taking
// a handler through every layer.

import { createContext, useContext } from 'react';
import { Pressable, Text } from 'react-native';

import type { Agent } from '@/lib/activity';
import { font, space, TOUCH, useColors } from '@/lib/theme';

export type AgentOpen = {
  /** The agents event's list: a call's title and state read it. */
  agents: Agent[];
  /** Plan node id → title, for an agent call on a node. */
  nodes: Record<string, string>;
  open: (agent: number) => void;
};

const Ctx = createContext<AgentOpen | null>(null);

export const AgentOpenProvider = Ctx.Provider;

/** The screen's opener, or null outside a session (a row drawn in a test, an agent's own view). */
export const useAgentOpen = () => useContext(Ctx);

/** "Open agent #N ›": the explicit affordance, beside a row whose own press folds it. */
export function OpenAgent({ agent, compact }: { agent: number; compact?: boolean }) {
  const c = useColors();
  const ctx = useAgentOpen();
  if (!ctx) return null;
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={`Open agent #${agent}`}
      onPress={() => ctx.open(agent)}
      hitSlop={compact ? { top: 12, bottom: 12 } : undefined}
      style={{
        minHeight: compact ? undefined : TOUCH,
        justifyContent: 'center',
        alignSelf: 'flex-start',
        paddingLeft: compact ? space.sm : 0,
        flexShrink: 0,
      }}
      testID="open-agent"
    >
      <Text style={{ color: c.accent, fontSize: font.sm }} numberOfLines={1}>
        {compact ? 'Open ›' : `Open agent #${agent} ›`}
      </Text>
    </Pressable>
  );
}
