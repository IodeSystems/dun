// A notice as the dun TUI draws it (internal/tui): a one-row notice that
// opens, a finished job's command and output, the docs proactive search
// surfaced, a message between agents. RN primitives only, so web and
// Android draw the same.

import { useState } from 'react';
import { Pressable, Text, View } from 'react-native';

import { PlainCode } from '@/components/code/Code';
import { Fold } from '@/components/fold/Fold';
import { Markdown } from '@/components/markdown/Markdown';
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
  return (
    <Text style={{ color, fontSize: font.sm, flexShrink: 0 }} numberOfLines={1}>
      {open ? '▾ ' : '▸ '}
    </Text>
  );
}

function Label({ text }: { text: string }) {
  const c = useColors();
  return (
    <Text
      style={{
        color: c.muted,
        fontSize: 16,
        borderWidth: 1,
        borderColor: c.border,
        borderRadius: 4,
        paddingHorizontal: 4,
        marginRight: space.xs,
        flexShrink: 0,
      }}
      numberOfLines={1}
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
    <View style={{ flexDirection: 'row', alignItems: 'flex-start' }}>
      {openable ? <Glyph open={open} color={color} /> : null}
      {n.label ? <Label text={n.label} /> : null}
      <Text
        selectable={open}
        numberOfLines={open ? undefined : 1}
        style={{ color, fontSize: font.sm, flex: 1, minWidth: 0 }}
        ellipsizeMode="tail"
        testID="notice-text"
      >
        {n.icon} {open ? n.text : first}
      </Text>
    </View>
  );
  return (
    <View style={{ paddingHorizontal: space.md }}>
      <Fold open={open} onToggle={openable ? () => setOpen(!open) : undefined} inset={space.md} head={row} />
    </View>
  );
}

function Job({ n }: { n: Extract<Notice, { type: 'job' }> }) {
  const c = useColors();
  const [open, setOpen] = useState(false);
  return (
    <View style={{ paddingHorizontal: space.md }}>
      <Fold
        open={open}
        onToggle={() => setOpen(!open)}
        inset={space.md}
        head={
          <View style={{ flexDirection: 'row' }}>
            <Glyph open={open} color={c.notice} />
            <Text
              numberOfLines={open ? undefined : 1}
              style={{ color: c.notice, fontSize: font.sm, flex: 1, minWidth: 0 }}
              ellipsizeMode="tail"
              testID="fold-title"
            >
              {open ? `🔔 ${n.head} ${n.took}` : jobLine(n)}
            </Text>
          </View>
        }
      >
        {open ? (
          <View style={{ gap: space.xs }}>
            <View style={{ backgroundColor: c.fold, borderRadius: 6, padding: space.sm }}>
              <PlainCode text={`$ ${n.command}`} />
            </View>
            {n.output ? <PlainCode text={n.output} color={c.muted} /> : null}
          </View>
        ) : null}
      </Fold>
    </View>
  );
}

function Docs({ n }: { n: Extract<Notice, { type: 'docs' }> }) {
  const c = useColors();
  const [open, setOpen] = useState(false);
  return (
    <View style={{ paddingHorizontal: space.md }}>
      <Fold
        open={open}
        onToggle={() => setOpen(!open)}
        inset={space.md}
        headStyle={{ flexDirection: 'row' }}
        head={
          <>
            <Glyph open={open} color={c.notice} />
            <Text
              numberOfLines={open ? undefined : 1}
              style={{ color: c.notice, fontSize: font.sm, flex: 1, minWidth: 0 }}
              ellipsizeMode="tail"
              testID="fold-title"
            >
              {docsHead(n, open)}
            </Text>
          </>
        }
      >
        {open ? (
          <View style={{ gap: space.xs }}>
            {n.docs.map((d, i) => (
              <DocRow key={i} d={d} />
            ))}
          </View>
        ) : null}
      </Fold>
    </View>
  );
}

function DocRow({ d }: { d: Doc }) {
  const c = useColors();
  const [open, setOpen] = useState(false);
  return (
    <Fold
      open={open}
      onToggle={d.line ? () => setOpen(!open) : undefined}
      nested
      inset={space.md}
      head={
        <Text style={{ fontSize: font.sm, color: c.text }}>
          <Text style={{ color: c.muted }}>{d.line ? (open ? '▾ ' : '▸ ') : '• '}</Text>
          {d.title}
          <Text style={{ color: c.muted }}> ({docWhy(d)})</Text>
        </Text>
      }
    >
      {d.source || (open && d.line) ? (
        <View style={{ gap: 1 }}>
          {d.source ? (
            <Text selectable style={{ color: c.muted, fontSize: 17, paddingLeft: space.md }}>
              {d.source}
            </Text>
          ) : null}
          {open && d.line ? (
            <Text selectable style={{ color: c.muted, fontSize: 17, paddingLeft: space.md, fontFamily: mono }}>
              {d.line}
            </Text>
          ) : null}
        </View>
      ) : null}
    </Fold>
  );
}

function Msg({ n }: { n: Extract<Notice, { type: 'msg' }> }) {
  const c = useColors();
  const long = msgLong(n.text);
  const [open, setOpen] = useState(false);
  const folded = long && !open;
  const footer = !n.carried ? '' : !n.done ? 'sending…' : n.failed ? '✗ ' + n.status : n.status;
  return (
    <View style={{ paddingHorizontal: space.md }}>
      <Fold
        open={open}
        onToggle={long ? () => setOpen(!open) : undefined}
        plain={!long}
        inset={space.md}
        head={
          <Text style={{ fontSize: font.sm, color: c.text }} numberOfLines={1} ellipsizeMode="tail" testID="fold-title">
            <Text style={{ color: c.muted }}>✉ </Text>
            <Text style={{ fontWeight: '700' }}>{n.from}</Text>
            <Text style={{ color: c.muted }}> → </Text>
            <Text style={{ fontWeight: '700' }}>{n.to}</Text>
          </Text>
        }
      >
        {/* The text as markdown, as the TUI's box renders it; folded, its first lines plain. */}
        <View style={{ gap: 2 }}>
          <View style={open ? undefined : { borderLeftWidth: 2, borderColor: c.border, paddingLeft: space.sm }}>
            {folded ? (
              <Text numberOfLines={msgFoldLines} style={{ color: c.text, fontSize: font.md }}>
                {n.text.trim()}
              </Text>
            ) : (
              <Markdown text={n.text.trim()} />
            )}
            {long ? (
              <Pressable
                onPress={() => setOpen(!open)}
                accessibilityRole="button"
                accessibilityState={{ expanded: open }}
              >
                <Text style={{ color: c.muted, fontSize: font.sm }}>{open ? 'less' : 'more…'}</Text>
              </Pressable>
            ) : null}
          </View>
          {footer ? (
            <Text style={{ color: n.failed ? c.danger : c.muted, fontSize: font.sm, paddingLeft: space.sm }}>
              {footer}
            </Text>
          ) : null}
        </View>
      </Fold>
    </View>
  );
}
