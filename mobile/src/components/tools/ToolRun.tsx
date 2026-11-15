// A run of tool calls, the TUI's box (internal/tui/toolrun.go): a header with
// the summary when it has more than one call, then a line per call:
//
//	⚙ 3 steps — exec ×2, read
//	✓ $ go test ./...  → ✓ 12 lines · ok
//	✗ $ make lint  → ✗ 1 · main.go:3: unused x
//	    main.go:3: unused x
//	… read internal/a.go  → running…
//
// A call is one line, collapsed; a press opens its body (registry.ts). A
// failed call keeps up to two lines of its error under the line, as the TUI.

import { createElement, useState } from 'react';
import { Text, View } from 'react-native';

import type { Thought } from '@/lib/thinking';
import type { Run, ToolCall } from '@/lib/toolcalls';
import { callText, failLines, preview, runSummary, type Tone } from '@/lib/tooltext';
import { font, mono, space, useColors } from '@/lib/theme';

import { Fold } from '../fold/Fold';
import { Thinking } from '../thinking/Thinking';
import { bodyFor } from './registry';

/** How a preview joins its call: a range or a count follows the path, a verdict follows an arrow. */
const joint = (p: string) => (/^[:(+]/.test(p) ? (p.startsWith(':') ? '' : ' ') : '  → ');

export function ToolCallRow({ call, flat }: { call: ToolCall; flat?: boolean }) {
  const c = useColors();
  const [open, setOpen] = useState(false);
  const p = preview(call);
  const tone: Record<Tone, string> = { ok: c.text, err: c.danger, muted: c.muted };
  const mark = call.state === 'pending' ? '… ' : call.state === 'error' ? '✗ ' : '✓ ';
  const markColor = call.state === 'pending' ? c.muted : call.state === 'error' ? c.danger : c.accent;
  const text = { fontFamily: mono, fontSize: font.sm };
  return (
    <View style={{ gap: space.xs }}>
      <Fold
        open={open}
        onToggle={() => setOpen(!open)}
        plain={flat}
        nested
        inset={space.sm}
        headStyle={{ paddingVertical: 2 }}
        head={
          <View style={{ flexDirection: 'row', alignItems: 'baseline' }}>
            <Text style={[text, { color: c.muted, flexShrink: 0 }]} numberOfLines={1}>
              {open ? '▾ ' : '▸ '}
              <Text style={{ color: markColor }}>{mark}</Text>
            </Text>
            <Text
              style={[text, { color: c.text, flex: 1, minWidth: 0 }]}
              numberOfLines={1}
              ellipsizeMode="tail"
              testID="fold-title"
            >
              {callText(call.tool, call.args)}
            </Text>
            {p.text ? (
              <Text style={[text, { color: tone[p.tone], flexShrink: 0, maxWidth: '50%' }]} numberOfLines={1}>
                {joint(p.text)}
                {p.text}
              </Text>
            ) : null}
          </View>
        }
      >
        {open ? createElement(bodyFor(call), { call }) : null}
      </Fold>
      {!open && call.state === 'error' && call.result
        ? failLines(call.result).map((l, i) => (
            <Text key={i} selectable style={[text, { color: c.danger, paddingLeft: space.lg }]} numberOfLines={1}>
              {l}
            </Text>
          ))
        : null}
    </View>
  );
}

/** The run's calls and thoughts in order (a thought's `at` is the calls before it). */
function steps(run: Run): ({ call: ToolCall } | { thought: Thought })[] {
  const out: ({ call: ToolCall } | { thought: Thought })[] = [];
  const ts = run.thoughts ?? [];
  let k = 0;
  for (let i = 0; i <= run.calls.length; i++) {
    while (k < ts.length && ts[k]!.at <= i) out.push({ thought: ts[k++]! });
    if (i < run.calls.length) out.push({ call: run.calls[i]! });
  }
  return out;
}

/**
 * A run of one call is that call's line alone (toolrun.go single: one call
 * said once). A longer run is one section, collapsed to its summary; a press
 * opens the calls.
 */
export function ToolRun({ run }: { run: Run }) {
  const c = useColors();
  const [open, setOpen] = useState(false);
  const thoughts = run.thoughts ?? [];
  if (run.calls.length === 1 && thoughts.length === 0) {
    return (
      <View style={{ backgroundColor: c.sunken, borderRadius: 8, padding: space.sm }}>
        <ToolCallRow call={run.calls[0]!} flat />
      </View>
    );
  }
  const s = runSummary(run.calls, thoughts);
  return (
    <Fold
      solid
      open={open}
      onToggle={() => setOpen(!open)}
      headStyle={{ paddingVertical: 2 }}
      head={
        <View style={{ flexDirection: 'row', alignItems: 'baseline' }}>
          <Text style={{ color: c.muted, fontFamily: mono, fontSize: font.sm, flexShrink: 0 }} numberOfLines={1}>
            {open ? '▾ ' : '▸ '}
          </Text>
          <Text
            style={{ color: c.text, fontFamily: mono, fontSize: font.sm, flex: 1, minWidth: 0 }}
            numberOfLines={1}
            ellipsizeMode="tail"
            testID="fold-title"
          >
            {s.head}
          </Text>
          {s.thought || s.running || s.failed ? (
            <Text style={{ fontFamily: mono, fontSize: font.sm, flexShrink: 0 }} numberOfLines={1}>
              <Text style={{ color: c.muted }}>{s.thought}</Text>
              <Text style={{ color: c.muted }}>{s.running}</Text>
              <Text style={{ color: c.danger }}>{s.failed}</Text>
            </Text>
          ) : null}
        </View>
      }
    >
      {open ? (
        <View style={{ gap: space.xs }}>
          {steps(run).map((st, i) =>
            'call' in st ? <ToolCallRow key={i} call={st.call} /> : <Thinking key={i} item={st.thought} nested />,
          )}
        </View>
      ) : null}
    </Fold>
  );
}
