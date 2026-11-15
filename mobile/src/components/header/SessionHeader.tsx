import { router } from 'expo-router';
import { useEffect, useState } from 'react';
import { Animated, Pressable, Text, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { badge } from '@/lib/activity';
import { lastUser, type View as Shown } from '@/lib/events';
import { spin } from '@/lib/plan';
import { tokK, type Stats } from '@/lib/stats';
import { headerStatus, type State } from '@/lib/turnstate';
import { font, space, TOUCH, useColors, type Colors } from '@/lib/theme';

import { useNow, usePhase, useReducedMotion } from './motion';

const HIDE_MS = 180;

/** The status line's colour by state; the question alternates pink and orange (planview motion). */
function stateColor(c: Colors, s: State, phase: number): string {
  switch (s) {
    case 'ask':
      return Math.trunc(phase / 4) % 2 ? c.notice : c.ask;
    case 'retry':
      return c.notice;
    case 'working':
      return c.working;
  }
  return c.muted;
}

/** The ctx figure, as the TUI's top bar: ctx 41.3k/131k. */
function ctxText(s: Stats, active: number, window: number): string {
  const used = s.seen ? s.ctx : active;
  const win = s.seen && s.window ? s.window : window;
  if (!used && !win) return 'ctx –';
  return win ? `ctx ${tokK(used)}/${tokK(win)}` : `ctx ${tokK(used)}`;
}

export type HeaderActions = {
  ctx: () => void;
  plan: () => void;
  tasks: () => void;
  model: () => void;
  session: () => void;
};

/**
 * The session screen's header, in place of the stack's: what the agent is
 * doing (the TUI's status line: working… <tool> · 3s, ❓ a question is
 * waiting, ⏳ retrying…, ready), the person's last message, and the ways
 * into the details: ctx, the plan (only when there is one), the background
 * tasks (only when there are some), and the overflow (model, session panel).
 * Each line is one line: what does not fit is cut with an ellipsis.
 *
 * hidden slides it up out of the way (the reader is in the history); a thin
 * strip in the state's colour stays, and tapping it brings the header back.
 */
export function SessionHeader({
  view,
  stats,
  hidden,
  panel,
  onShow,
  act,
}: {
  view: Shown;
  stats: Stats;
  hidden: boolean;
  panel: boolean;
  onShow: () => void;
  act: HeaderActions;
}) {
  const c = useColors();
  const insets = useSafeAreaInsets();
  const reduced = useReducedMotion();
  const [more, setMore] = useState(false);
  const now = useNow(view.busy);
  const st = headerStatus(view, now);
  const moving = !reduced && (st.state === 'working' || st.state === 'ask');
  const phase = usePhase(moving);
  const color = stateColor(c, st.state, phase);
  const tasks = badge(view.bg.agents, view.bg.jobs);
  const plan = view.bg.plan;
  const last = lastUser(view);

  // Slide: the wrapper's height goes to 0 while the content moves up with it,
  // so the transcript below takes the room and nothing in it moves.
  const [h, setH] = useState(0);
  const [shown] = useState(() => new Animated.Value(hidden ? 0 : 1));
  useEffect(() => {
    const to = hidden ? 0 : 1;
    if (reduced) shown.setValue(to);
    else Animated.timing(shown, { toValue: to, duration: HIDE_MS, useNativeDriver: false }).start();
  }, [hidden, reduced, shown]);

  const chip = (label: string, onPress: () => void, a11y: string, tint?: string) => (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={a11y}
      onPress={onPress}
      style={{ minHeight: TOUCH, minWidth: TOUCH, paddingHorizontal: space.sm, justifyContent: 'center' }}
    >
      <Text style={{ color: tint ?? c.muted, fontSize: font.sm, fontVariant: ['tabular-nums'] }} numberOfLines={1}>
        {label}
      </Text>
    </Pressable>
  );

  return (
    <View style={{ backgroundColor: c.bg, paddingTop: insets.top, zIndex: 1 }}>
      <Animated.View
        style={
          h
            ? {
                height: shown.interpolate({ inputRange: [0, 1], outputRange: [0, h] }),
                overflow: 'hidden',
              }
            : undefined
        }
      >
        <Animated.View
          onLayout={(e) => setH(e.nativeEvent.layout.height)}
          style={
            h
              ? { transform: [{ translateY: shown.interpolate({ inputRange: [0, 1], outputRange: [-h, 0] }) }] }
              : undefined
          }
        >
          <View
            style={{
              borderBottomWidth: 1,
              borderColor: c.border,
              paddingRight: space.xs,
              width: '100%',
              maxWidth: 860,
              alignSelf: 'center',
            }}
          >
            <View style={{ flexDirection: 'row', alignItems: 'center' }}>
              <Pressable
                accessibilityRole="button"
                accessibilityLabel="Back to the sessions"
                onPress={() => (router.canGoBack() ? router.back() : router.replace('/'))}
                style={{ minHeight: TOUCH, minWidth: TOUCH, alignItems: 'center', justifyContent: 'center' }}
              >
                <Text style={{ color: c.text, fontSize: font.lg }}>‹</Text>
              </Pressable>
              <Text
                accessibilityLiveRegion="polite"
                style={{ flex: 1, color, fontSize: font.sm, fontWeight: st.state === 'idle' ? '400' : '500' }}
                numberOfLines={1}
              >
                {st.state === 'working' ? (moving ? spin[phase % spin.length] : '⠿') + ' ' : ''}
                {st.text}
              </Text>
              {chip(
                '⋯',
                () => setMore((m) => !m),
                more ? 'Hide the session menu' : 'Session menu',
                more ? c.accent : c.text,
              )}
            </View>
            <View style={{ flexDirection: 'row', alignItems: 'center', paddingLeft: space.md }}>
              <Text style={{ flex: 1, color: c.muted, fontSize: font.sm }} numberOfLines={1}>
                {last ? '› ' + last.replace(/\s+/g, ' ') : ' '}
              </Text>
              {chip(ctxText(stats, view.bg.ctx.active, view.bg.ctx.window), act.ctx, 'Context details')}
              {plan
                ? chip(`☰ ${plan.done}/${plan.total}`, act.plan, `Plan, ${plan.done} of ${plan.total} done`)
                : null}
              {tasks
                ? chip(
                    `⚙ ${tasks.running}`,
                    act.tasks,
                    `Background tasks, ${tasks.running} running${tasks.failed ? ', one failed' : ''}`,
                    tasks.failed ? c.danger : tasks.running ? c.working : c.muted,
                  )
                : null}
            </View>
            {more ? (
              <View style={{ flexDirection: 'row', paddingLeft: space.md, gap: space.sm }}>
                {chip(view.settings?.model || 'Model', act.model, 'Switch model', c.text)}
                {chip(panel ? 'Hide session' : 'Session', act.session, 'Session commands', panel ? c.accent : c.text)}
              </View>
            ) : null}
          </View>
        </Animated.View>
      </Animated.View>
      {hidden ? (
        <Pressable
          accessibilityRole="button"
          accessibilityLabel={'Show the header: ' + st.text}
          onPress={onShow}
          hitSlop={{ top: 8, bottom: 24 }}
          style={{ height: space.xs, backgroundColor: st.state === 'idle' ? c.border : color }}
        />
      ) : null}
    </View>
  );
}
