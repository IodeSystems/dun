import type { GraphQLClient } from 'graphql-request';
import { useState } from 'react';
import { Pressable, Text, View } from 'react-native';

import { Sheet } from '@/components/header/Sheet';
import { useNow } from '@/components/header/motion';
import { Markdown } from '@/components/markdown/Markdown';
import { NoticeRow } from '@/components/notices/NoticeRow';
import { Thinking } from '@/components/thinking/Thinking';
import { ToolRun } from '@/components/tools/ToolRun';
import { Button, Field } from '@/components/ui';
import { rows, type Agent, type Job, type Row, type Tone } from '@/lib/activity';
import { confirm } from '@/lib/confirm';
import type { AgentView, Item } from '@/lib/events';
import { font, mono, space, TOUCH, useColors, type Colors } from '@/lib/theme';

import { JobOutput } from './JobOutput';

const toneColor = (c: Colors, t: Tone) =>
  t === 'working' ? c.working : t === 'failed' ? c.danger : t === 'notice' ? c.notice : c.muted;

export type TaskActions = {
  /** Opens sub-agent id's conversation (agent view); closeAgent leaves it. */
  openAgent: (id: number) => void;
  closeAgent: () => void;
  tell: (id: number, text: string) => void;
  kill: (job: number) => void;
};

/**
 * The background work, as the TUI's activity zone (activity.go): a row per
 * sub-agent (▸ running, ✓ idle, ⏸ stopped) and per job (⚙ running, ✓ ok,
 * ✗ failed, ⏹ killed). An agent opens its conversation, followed live, and
 * takes a message; a job opens its output, and a running one can be killed.
 *
 * Which agent is open is the session's agent view (agentView), not this
 * sheet's: a tool call, a notice or a plan node opens the same view
 * (components/agents/AgentOpen), and this sheet shows it.
 */
export function TasksSheet({
  agents,
  jobs,
  agentView,
  client,
  session,
  act,
  onClose,
}: {
  agents: Agent[];
  jobs: Job[];
  agentView: AgentView | null;
  client: GraphQLClient | null;
  session: string;
  act: TaskActions;
  onClose: () => void;
}) {
  const c = useColors();
  const [job, setJob] = useState<number | null>(null);
  const open: { kind: 'agent' | 'job'; id: number } | null = agentView
    ? { kind: 'agent', id: agentView.id }
    : job !== null
      ? { kind: 'job', id: job }
      : null;
  const [tellText, setTellText] = useState('');
  const anyRunning = agents.some((a) => a.state === 'running') || jobs.some((j) => j.state === 'running');
  const now = useNow(anyRunning);
  const all = rows(agents, jobs, now);
  const sel = open ? all.find((r) => r.kind === open.kind && r.id === open.id) : undefined;

  const back = () => {
    if (open?.kind === 'agent') act.closeAgent();
    setJob(null);
    setTellText('');
  };
  const close = () => {
    if (open?.kind === 'agent') act.closeAgent();
    onClose();
  };
  const pick = (r: Row) => {
    if (r.kind === 'agent') act.openAgent(r.id);
    else setJob(r.id);
  };

  // An agent the agents event does not list (yet) still opens: its
  // conversation, with no row above it.
  if (open && (sel || open.kind === 'agent')) {
    const job = open.kind === 'job' ? jobs.find((j) => j.id === open.id) : undefined;
    return (
      <Sheet
        title={open.kind === 'agent' ? `Agent #${open.id}` : `Job ${open.id}`}
        onClose={close}
        footer={
          open.kind === 'agent' ? (
            <View style={{ flexDirection: 'row', gap: space.sm }}>
              <View style={{ flex: 1 }}>
                <Field value={tellText} onChangeText={setTellText} placeholder="tell it something…" />
              </View>
              <Button
                title="Tell"
                primary
                disabled={!tellText.trim()}
                onPress={() => {
                  act.tell(open.id, tellText.trim());
                  setTellText('');
                }}
              />
            </View>
          ) : job?.state === 'running' ? (
            <View style={{ flexDirection: 'row', justifyContent: 'flex-end' }}>
              <Button
                title="Kill job"
                onPress={() =>
                  void confirm('Kill job', `Kill job ${open.id} (${job.command}) and everything it started?`).then(
                    (ok) => ok && act.kill(open.id),
                  )
                }
              />
            </View>
          ) : undefined
        }
      >
        <Pressable
          accessibilityRole="button"
          onPress={back}
          style={{ minHeight: TOUCH, justifyContent: 'center', alignSelf: 'flex-start' }}
        >
          <Text style={{ color: c.accent, fontSize: font.sm }}>‹ All tasks</Text>
        </Pressable>
        {sel ? <TaskRow row={sel} /> : null}
        {open.kind === 'job' ? (
          client ? (
            <JobOutput client={client} session={session} job={open.id} running={job?.state === 'running'} />
          ) : null
        ) : (
          <AgentConversation view={agentView?.id === open.id ? agentView : null} />
        )}
      </Sheet>
    );
  }

  return (
    <Sheet title="Background tasks" onClose={close}>
      {all.length === 0 ? (
        <Text style={{ color: c.muted, fontSize: font.sm }}>No sub-agents or background jobs in this session.</Text>
      ) : null}
      {all.map((r) => (
        <Pressable key={r.key} accessibilityRole="button" onPress={() => pick(r)} style={{ minHeight: TOUCH }}>
          <TaskRow row={r} />
        </Pressable>
      ))}
    </Sheet>
  );
}

function TaskRow({ row }: { row: Row }) {
  const c = useColors();
  return (
    <View style={{ flexDirection: 'row', gap: space.sm }}>
      <Text style={{ color: toneColor(c, row.tone), fontSize: font.md, fontFamily: mono, width: font.md }}>
        {row.mark}
      </Text>
      <View style={{ flex: 1, gap: 2 }}>
        <Text style={{ color: row.attn ? c.danger : c.text, fontSize: font.md }} numberOfLines={1}>
          {row.label}
        </Text>
        {row.detail ? (
          <Text style={{ color: c.muted, fontSize: font.sm }} numberOfLines={2}>
            {row.detail}
          </Text>
        ) : null}
      </View>
    </View>
  );
}

/** A sub-agent's conversation, with the session transcript's pieces. */
function AgentConversation({ view }: { view: AgentView | null }) {
  const c = useColors();
  if (!view?.loaded) return <Text style={{ color: c.muted, fontSize: font.sm }}>loading its conversation…</Text>;
  if (!view.view.items.length) return <Text style={{ color: c.muted, fontSize: font.sm }}>nothing yet</Text>;
  return (
    <View style={{ gap: space.sm }}>
      {view.view.items.map((it) => (
        <AgentItem key={it.key} item={it} />
      ))}
    </View>
  );
}

function AgentItem({ item }: { item: Item }) {
  const c = useColors();
  switch (item.kind) {
    case 'user':
      return (
        <View style={{ backgroundColor: c.user, borderRadius: 6, padding: space.md }}>
          <Text selectable style={{ color: c.text, fontSize: font.md }}>
            {item.text}
          </Text>
        </View>
      );
    case 'assistant':
      return <Markdown text={item.text} />;
    case 'tools':
      return <ToolRun run={item} />;
    case 'thinking':
      return <Thinking item={item} />;
    case 'notice':
      return <NoticeRow notice={item.notice} />;
    case 'note':
    case 'question':
      return <Text style={{ color: c.muted, fontSize: font.sm }}>{item.text}</Text>;
  }
}
