import { Pressable, Text, View } from 'react-native';

import { Sheet } from '@/components/header/Sheet';
import { OpenAgent } from '@/components/agents/AgentOpen';
import { usePhase, useReducedMotion } from '@/components/header/motion';
import { Button } from '@/components/ui';
import { confirm } from '@/lib/confirm';
import { droppable, modeWord, motion, ownerAgent, type Live, type Plan } from '@/lib/plan';
import { font, mono, space, TOUCH, useColors, type Colors } from '@/lib/theme';

const toneColor = (c: Colors, t: ReturnType<typeof motion>['tone']) =>
  t === 'ask' ? c.ask : t === 'notice' ? c.notice : t === 'working' ? c.working : t === 'agent' ? c.accent : c.muted;

/**
 * The plan, as the TUI's /plan screen (planview.go): the tree with a mark per
 * status, done/total and the mode. Approve and drop are the engine's /plan
 * commands (control plan approve | drop <id>); edit and tell stay in the TUI.
 */
export function PlanSheet({
  plan,
  live,
  run,
  onClose,
}: {
  plan: Plan;
  live: Omit<Live, 'phase'>;
  run: (action: string) => void;
  onClose: () => void;
}) {
  const c = useColors();
  const reduced = useReducedMotion();
  const moving = !reduced && (live.busy || live.asking || Object.values(live.agents).includes('running'));
  const phase = usePhase(moving);
  const lv: Live = { ...live, phase };
  const approve = plan.mode && !plan.approved && plan.rows.length > 0;
  return (
    <Sheet
      title={`Plan · ${plan.done}/${plan.total} done`}
      onClose={onClose}
      footer={
        approve ? (
          <View style={{ flexDirection: 'row', justifyContent: 'flex-end' }}>
            <Button title="Approve the plan" primary onPress={() => run('approve')} />
          </View>
        ) : undefined
      }
    >
      <Text style={{ color: c.muted, fontSize: font.sm }}>plan mode {modeWord(plan)}</Text>
      {plan.rows.map((r, i) => {
        const m = motion(r, lv);
        const done = r.status === 'done' || r.status === 'dropped';
        return (
          <View key={r.id + ':' + i} style={{ flexDirection: 'row', gap: space.sm, paddingLeft: r.depth * space.lg }}>
            <Text style={{ color: toneColor(c, m.tone), fontSize: font.md, fontFamily: mono, width: font.md }}>
              {m.mark}
            </Text>
            <View style={{ flex: 1, gap: 2 }}>
              <Text style={{ color: done ? c.muted : c.text, fontSize: font.md }}>
                {r.title}
                {r.repeat ? ' ↑' : ''}
              </Text>
              <Text style={{ color: c.muted, fontSize: font.sm }}>
                {[r.id, r.status, m.tag, r.waiting.length ? 'waits on ' + r.waiting.join(', ') : '', r.why]
                  .filter(Boolean)
                  .join(' · ')}
              </Text>
              {ownerAgent(r.owner) ? <OpenAgent agent={ownerAgent(r.owner)} /> : null}
            </View>
            {droppable(r) && !r.repeat ? (
              <Pressable
                accessibilityRole="button"
                accessibilityLabel={`Drop ${r.id}`}
                onPress={() =>
                  void confirm('Drop', `Drop "${r.title}" from the plan?`).then((ok) => ok && run('drop ' + r.id))
                }
                style={{ minHeight: TOUCH, minWidth: TOUCH, alignItems: 'center', justifyContent: 'center' }}
              >
                <Text style={{ color: c.muted, fontSize: font.sm }}>Drop</Text>
              </Pressable>
            ) : null}
          </View>
        );
      })}
    </Sheet>
  );
}
