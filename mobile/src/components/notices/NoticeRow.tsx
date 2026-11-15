// A notice as the dun TUI draws it (internal/tui): a one-row notice that
// opens, a finished job's command and output, the docs proactive search
// surfaced, a message between agents. RN primitives only, so web and
// Android draw the same.

import { useState } from 'react';
import { Pressable, Text, View } from 'react-native';

import { docsHead, docWhy, jobLine, msgFoldLines, msgLong, type Doc, type Notice, type Tone } from '@/lib/notices';
import { font, mono, space, useColors, type Colors } from '@/lib/theme';

const toneColor = (c: Colors, t: Tone) => (t === 'error' ? c.danger : t === 'dim' ? c.muted : c.notice);

export function NoticeRow({ notice }: { notice: Notice }) {
  switch (notice.type) {
    case 'plain':
      return <Plain n={notice} />;
    case 'job':
      return <Job n={notice} />;
    case 'docs':
      return <Docs n={notice} />;
    case 'msg':
      return <Msg n={notice} />;
  }
}

/** The ▸/▾ an openable row starts with. */
function Glyph({ open, color }: { open: boolean; color: string }) {
  return <Text style={{ color, fontSize: font.sm }}>{open ? '▾ ' : '▸ '}</Text>;
}

function Label({ text }: { text: string }) {
  const c = useColors();
  return (
    <Text
      style={{
        color: c.muted,
        fontSize: 11,
        borderWidth: 1,
        borderColor: c.border,
        borderRadius: 4,
        paddingHorizontal: 4,
        marginRight: space.xs,
      }}
    >
      {text}
    </Text>
  );
}

function Plain({ n }: { n: Extract<Notice, { type: 'plain' }> }) {
  const c = useColors();
  const [open, setOpen] = useState(false);
  const color = toneColor(c, n.tone);
  const [first = '', ...more] = n.text.split('\n');
  // One row; a notice with more lines (or one a phone cannot fit) opens.
  const openable = more.length > 0 || first.length > 120;
  const row = (
    <View style={{ flexDirection: 'row', alignItems: 'flex-start', paddingHorizontal: space.md }}>
      {openable ? <Glyph open={open} color={color} /> : null}
      {n.label ? <Label text={n.label} /> : null}
      <Text
        selectable={open}
        numberOfLines={open ? undefined : 1}
        style={{ color, fontSize: font.sm, flexShrink: 1 }}
        testID="notice-text"
      >
        {n.icon} {open ? n.text : first}
      </Text>
    </View>
  );
  if (!openable) return row;
  return (
    <Pressable onPress={() => setOpen(!open)} accessibilityRole="button" accessibilityState={{ expanded: open }}>
      {row}
    </Pressable>
  );
}

function Job({ n }: { n: Extract<Notice, { type: 'job' }> }) {
  const c = useColors();
  const [open, setOpen] = useState(false);
  return (
    <Pressable
      onPress={() => setOpen(!open)}
      accessibilityRole="button"
      accessibilityState={{ expanded: open }}
      style={{ paddingHorizontal: space.md, gap: 2 }}
    >
      <View style={{ flexDirection: 'row' }}>
        <Glyph open={open} color={c.notice} />
        <Text numberOfLines={open ? undefined : 1} style={{ color: c.notice, fontSize: font.sm, flexShrink: 1 }}>
          {open ? `🔔 ${n.head} ${n.took}` : jobLine(n)}
        </Text>
      </View>
      {open ? (
        <>
          <Text
            selectable
            style={{
              color: c.text,
              fontFamily: mono,
              fontSize: font.sm,
              backgroundColor: c.sunken,
              borderRadius: 6,
              padding: space.sm,
            }}
          >
            $ {n.command}
          </Text>
          {n.output ? (
            <Text selectable style={{ color: c.muted, fontFamily: mono, fontSize: font.sm }}>
              {n.output}
            </Text>
          ) : null}
        </>
      ) : null}
    </Pressable>
  );
}

function Docs({ n }: { n: Extract<Notice, { type: 'docs' }> }) {
  const c = useColors();
  const [open, setOpen] = useState(false);
  return (
    <View style={{ paddingHorizontal: space.md, gap: 2 }}>
      <Pressable
        onPress={() => setOpen(!open)}
        accessibilityRole="button"
        accessibilityState={{ expanded: open }}
        style={{ flexDirection: 'row' }}
      >
        <Glyph open={open} color={c.notice} />
        <Text numberOfLines={open ? undefined : 1} style={{ color: c.notice, fontSize: font.sm, flexShrink: 1 }}>
          {docsHead(n, open)}
        </Text>
      </Pressable>
      {open ? n.docs.map((d, i) => <DocRow key={i} d={d} />) : null}
    </View>
  );
}

function DocRow({ d }: { d: Doc }) {
  const c = useColors();
  const [open, setOpen] = useState(false);
  return (
    <Pressable
      onPress={() => setOpen(!open)}
      disabled={!d.line}
      accessibilityState={{ expanded: open }}
      style={{ paddingLeft: space.lg, gap: 1 }}
    >
      <Text style={{ fontSize: font.sm, color: c.text }}>
        <Text style={{ color: c.muted }}>{d.line ? (open ? '▾ ' : '▸ ') : '• '}</Text>
        {d.title}
        <Text style={{ color: c.muted }}> ({docWhy(d)})</Text>
      </Text>
      {d.source ? (
        <Text selectable style={{ color: c.muted, fontSize: 12, paddingLeft: space.md }}>
          {d.source}
        </Text>
      ) : null}
      {open && d.line ? (
        <Text selectable style={{ color: c.muted, fontSize: 12, paddingLeft: space.md, fontFamily: mono }}>
          {d.line}
        </Text>
      ) : null}
    </Pressable>
  );
}

function Msg({ n }: { n: Extract<Notice, { type: 'msg' }> }) {
  const c = useColors();
  const long = msgLong(n.text);
  const [open, setOpen] = useState(false);
  const folded = long && !open;
  const footer = !n.carried ? '' : !n.done ? 'sending…' : n.failed ? '✗ ' + n.status : n.status;
  return (
    <View style={{ paddingHorizontal: space.md, gap: 2 }}>
      <Text style={{ fontSize: font.sm, color: c.text }}>
        <Text style={{ color: c.muted }}>✉ </Text>
        <Text style={{ fontWeight: '700' }}>{n.from}</Text>
        <Text style={{ color: c.muted }}> → </Text>
        <Text style={{ fontWeight: '700' }}>{n.to}</Text>
      </Text>
      <Pressable
        onPress={() => setOpen(!open)}
        disabled={!long}
        accessibilityState={{ expanded: open }}
        style={{ borderLeftWidth: 2, borderColor: c.border, paddingLeft: space.sm }}
      >
        <Text
          selectable={!long || open}
          numberOfLines={folded ? msgFoldLines : undefined}
          style={{ color: c.text, fontSize: font.md }}
        >
          {n.text.trim()}
        </Text>
        {long ? <Text style={{ color: c.muted, fontSize: font.sm }}>{open ? 'less' : 'more…'}</Text> : null}
      </Pressable>
      {footer ? (
        <Text style={{ color: n.failed ? c.danger : c.muted, fontSize: font.sm, paddingLeft: space.sm }}>{footer}</Text>
      ) : null}
    </View>
  );
}
