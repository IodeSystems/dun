import { useState } from 'react';
import { Pressable, Text, View } from 'react-native';

import { Sheet } from '@/components/header/Sheet';
import { OpenAgent } from '@/components/agents/AgentOpen';
import { usePhase, useReducedMotion } from '@/components/header/motion';
import { Button } from '@/components/ui';
import { confirm } from '@/lib/confirm';
import {
  askClosed,
  askFoldLine,
  askTag,
  droppable,
  intakeLine,
  modeWord,
  motion,
  ownerAgent,
  spin,
  type Live,
  type Plan,
  type PlanAsk,
} from '@/lib/plan';
import { font, mono, space, TOUCH, useColors, type Colors } from '@/lib/theme';

const toneColor = (c: Colors, t: ReturnType<typeof motion>['tone']) =>
  t === 'ask' ? c.ask : t === 'notice' ? c.notice : t === 'working' ? c.working : t === 'agent' ? c.accent : c.muted;

/**
 * The plan, as the TUI's /plan screen (planview.go): the person's asks on
 * top (askfold.go: the intake row, the open asks, one row for the done and
 * filed ones that opens), then the tree with a mark per status, done/total
 * and the mode. Approve and drop are the engine's /plan commands (control
 * plan approve | drop <id>); edit and tell stay in the TUI.
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
  const [foldOpen, setFoldOpen] = useState(false);
  const approve = plan.mode && !plan.approved && plan.rows.length > 0;
  const asks = plan.asks ?? [];
  const closed = asks.filter(askClosed);
  const intake = plan.intake;
  const intakeWorks = !!intake?.working && live.busy;
  const askRow = (a: PlanAsk) => {
    const works = a.working && live.busy && !askClosed(a);
    const tag = askClosed(a) ? a.state + (a.nodes.length ? ' → ' + a.nodes.join(', ') : '') : askTag(a, live.busy);
    return (
      <View key={'ask:' + a.n} style={{ flexDirection: 'row', gap: space.sm }}>
        <Text style={{ color: works ? c.working : c.muted, fontSize: font.md, fontFamily: mono, width: font.md }}>
          {works ? spin[phase % spin.length] : a.state === 'blocked' ? '⚠' : askClosed(a) ? '✓' : '○'}
        </Text>
        <View style={{ flex: 1, gap: 2 }}>
          <Text style={{ color: askClosed(a) ? c.muted : c.text, fontSize: font.md }}>{`ask ${a.n} ${a.title}`}</Text>
          <Text style={{ color: c.muted, fontSize: font.sm }}>{tag}</Text>
        </View>
      </View>
    );
  };
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
      {intake ? (
        <Text style={{ color: intakeWorks ? c.working : c.notice, fontSize: font.md }}>
          {intakeWorks ? spin[phase % spin.length] + ' ' : ''}
          {intakeLine(intake)}
          {intakeWorks ? ' · working' : ''}
        </Text>
      ) : null}
      {asks.filter((a) => !askClosed(a)).map(askRow)}
      {closed.length ? (
        <Pressable
          accessibilityRole="button"
          accessibilityLabel={foldOpen ? 'Hide the closed asks' : 'Show the closed asks'}
          onPress={() => setFoldOpen(!foldOpen)}
          style={{ minHeight: TOUCH, justifyContent: 'center' }}
        >
          <Text style={{ color: c.muted, fontSize: font.md }}>{askFoldLine(closed)}</Text>
        </Pressable>
      ) : null}
      {foldOpen ? closed.map(askRow) : null}
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
                {[
                  r.id,
                  r.status,
                  m.tag,
                  r.ask ? 'ask ' + r.ask : '',
                  r.waiting.length ? 'waits on ' + r.waiting.join(', ') : '',
                  r.why,
                ]
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
