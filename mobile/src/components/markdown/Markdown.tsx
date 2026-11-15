// An assistant reply as Markdown, drawn with RN primitives only so web and Android match.
// The parse is flowdown's (src/lib/markdown.ts); each top-level block is memoized by its
// identity, so a streamed token re-renders the last block, not the reply.
//
// flowdown's dialect marks nodes with a form (formOf): a quote is a callout, a list is
// steps. Each known form is drawn here; an unknown one draws as the Markdown it still is.
import { memo, useState, type ReactNode } from 'react';
import { Linking, Pressable, Text, View, type TextStyle } from 'react-native';

import {
  codeText,
  createReply,
  formOf,
  mathLines,
  parseBlock,
  safeUrl,
  tableColumns,
  toText,
  treeLines,
  type MarkdownBlock,
  type Node,
} from '@/lib/markdown';
import { Code, DiffText, SideScroll } from '@/components/code/Code';
import { langForPath } from '@/components/code/languages';
import { font, mono, space, useColors, type Colors } from '@/lib/theme';

export function Markdown({ text, streaming = false }: { text: string; streaming?: boolean }) {
  const [reply] = useState(createReply);
  const { blocks, closer } = reply.update(text, streaming);
  return (
    <View style={{ gap: space.sm }}>
      {blocks.map((b, i) => (
        <Block key={i} block={b} closer={i === blocks.length - 1 ? closer : ''} />
      ))}
    </View>
  );
}

const Block = memo(function Block({ block, closer }: { block: MarkdownBlock; closer: string }) {
  const c = useColors();
  return <>{blocksOf(parseBlock(block, closer), c, closer !== '')}</>;
});

const HEADING = [0, font.xl, font.lg + 2, font.lg, font.md, font.md, font.sm];

/** A callout's label and colour by kind (flowdown tui/render.js CALLOUT_STYLES). */
const CALLOUTS: Record<string, { label: string; color: (c: Colors) => string }> = {
  note: { label: 'Note', color: (c) => c.accent },
  tip: { label: 'Tip', color: (c) => c.working },
  important: { label: 'Important', color: (c) => c.ask },
  warning: { label: 'Warning', color: (c) => c.notice },
  caution: { label: 'Caution', color: (c) => c.danger },
};

/** Paragraphs that head a form: drawn bold. */
const TITLES = new Set(['steps-title', 'stats-title', 'tabs-title', 'sources-title', 'summary']);

const bodyStyle = (c: Colors): TextStyle => ({ color: c.text, fontSize: font.md, lineHeight: font.md * 1.4 });

/** open: the block is a fence still streaming (its closer is owed). */
function blocksOf(node: Node, c: Colors, open = false): ReactNode[] {
  return (node.children ?? []).map((n, i) => blockOf(n, i, c, open));
}

function blockOf(n: Node, key: number, c: Colors, open: boolean): ReactNode {
  const body = bodyStyle(c);
  const form = formOf(n);
  switch (n.type) {
    case 'paragraph':
      return (
        <Text
          key={key}
          selectable
          style={
            form === undefined
              ? body
              : [
                  body,
                  TITLES.has(form.form) && { fontWeight: '700' },
                  form.form === 'quote-source' && { color: c.muted },
                ]
          }
        >
          {form?.form === 'quote-source' ? '— ' : null}
          {inlines(n, c)}
        </Text>
      );
    case 'heading': {
      const size = HEADING[n.depth ?? 1] ?? font.md;
      return (
        <Text
          key={key}
          selectable
          accessibilityRole="header"
          style={{ color: c.text, fontSize: size, fontWeight: '700', marginTop: space.xs }}
        >
          {inlines(n, c)}
        </Text>
      );
    }
    case 'thematicBreak':
      return <View key={key} style={{ height: 1, backgroundColor: c.border, marginVertical: space.xs }} />;
    case 'blockquote':
      return quote(n, key, c, open);
    case 'list':
      return list(n, key, c, open);
    case 'code':
      return fence(n, key, c, open);
    case 'math':
      return <Formula key={key} latex={n.value ?? ''} />;
    case 'table':
      return (
        <SideScroll key={key}>
          <View style={{ flexDirection: 'row', borderWidth: 1, borderColor: c.border, borderRadius: 4 }}>
            {tableColumns(n).map((col, i) => (
              <View
                key={i}
                style={{
                  alignItems: col.align === 'right' ? 'flex-end' : col.align === 'center' ? 'center' : 'flex-start',
                  borderLeftWidth: i === 0 ? 0 : 1,
                  borderColor: c.border,
                }}
              >
                {col.cells.map((cell, j) => (
                  <Text
                    key={j}
                    selectable
                    style={[
                      body,
                      { paddingHorizontal: space.sm, paddingVertical: space.xs },
                      cell.header && { fontWeight: '700' },
                    ]}
                  >
                    {inlines(cell, c)}
                  </Text>
                ))}
              </View>
            ))}
          </View>
        </SideScroll>
      );
    case 'footnotes':
      return (
        <View key={key} style={{ borderTopWidth: 1, borderTopColor: c.border, paddingTop: space.xs, gap: space.xs }}>
          {(n.children ?? []).map((def, i) => (
            <View key={i} style={{ flexDirection: 'row', gap: space.sm }}>
              <Text style={[body, { color: c.muted, fontSize: font.sm }]}>[{def.index ?? i + 1}]</Text>
              <View style={{ flex: 1, gap: space.xs }}>{blocksOf(def, c, open)}</View>
            </View>
          ))}
        </View>
      );
    default:
      // html and anything else: its text, never markup.
      return (
        <Text key={key} selectable style={body}>
          {toText(n)}
        </Text>
      );
  }
}

function quote(n: Node, key: number, c: Colors, open: boolean): ReactNode {
  const form = formOf(n);
  switch (form?.form) {
    case 'callout': {
      const kind = CALLOUTS[form.kind ?? 'note'] ?? CALLOUTS.note!;
      const color = kind.color(c);
      return (
        <View
          key={key}
          style={{
            borderLeftWidth: 3,
            borderLeftColor: color,
            backgroundColor: c.sunken,
            borderRadius: 4,
            padding: space.sm,
            gap: space.xs,
          }}
        >
          <Text style={[bodyStyle(c), { fontWeight: '700' }]}>
            <Text style={{ color }}>{kind.label}</Text>
            {form.title ? ` · ${form.title}` : null}
          </Text>
          {blocksOf(n, c, open)}
        </View>
      );
    }
    case 'form':
    case 'tab':
      return (
        <View key={key} style={{ gap: space.sm }}>
          {blocksOf(n, c, open)}
        </View>
      );
    case 'verdicts':
      // pros and cons side by side while the width holds both.
      return (
        <View key={key} style={{ flexDirection: 'row', flexWrap: 'wrap', gap: space.md }}>
          {(n.children ?? []).map((v, i) => (
            <View key={i} style={{ flexGrow: 1, flexBasis: 220 }}>
              {quote(v, i, c, open)}
            </View>
          ))}
        </View>
      );
    case 'verdict': {
      const color = form.kind === 'cons' ? c.danger : c.working;
      return (
        <View key={key} style={{ gap: space.xs }}>
          {(n.children ?? []).map((part, i) =>
            formOf(part)?.form === 'verdict-title' ? (
              <Text key={i} selectable style={[bodyStyle(c), { fontWeight: '700', color }]}>
                {inlines(part, c)}
              </Text>
            ) : part.type === 'list' ? (
              items(part, i, c, open, () => [form.kind === 'cons' ? '✗' : '✓', color])
            ) : (
              blockOf(part, i, c, open)
            ),
          )}
        </View>
      );
    }
    case 'tabs':
      return <Tabs key={key} tabs={n.children ?? []} open={open} />;
    case 'details':
      return <Details key={key} node={n} open={open} />;
    default:
      return (
        <View key={key} style={{ borderLeftWidth: 3, borderLeftColor: c.border, paddingLeft: space.md, gap: space.xs }}>
          {blocksOf(n, c, open)}
        </View>
      );
  }
}

/** Items behind a marker each, their blocks hung beside it. */
function items(
  n: Node,
  key: number,
  c: Colors,
  open: boolean,
  marker: (i: number, item: Node) => [string, string],
): ReactNode {
  const body = bodyStyle(c);
  return (
    <View key={key} style={{ gap: n.spread ? space.sm : space.xs }}>
      {(n.children ?? []).map((item, i) => {
        const [mark, color] = marker(i, item);
        return (
          <View key={i} style={{ flexDirection: 'row', gap: space.sm }}>
            <Text style={[body, { color, minWidth: n.ordered ? 22 : 12 }]}>{mark}</Text>
            <View style={{ flex: 1, gap: space.xs }}>{blocksOf(item, c, open)}</View>
          </View>
        );
      })}
    </View>
  );
}

function list(n: Node, key: number, c: Colors, open: boolean): ReactNode {
  switch (formOf(n)?.form) {
    case 'steps':
      return steps(n, key, c, open);
    case 'stats':
      return stats(n, key, c, open);
    case 'sources':
      return items(n, key, c, open, (i) => [`[${i + 1}]`, c.muted]);
    default:
      return items(n, key, c, open, (i, item) => [
        item.checked === true ? '☑' : item.checked === false ? '☐' : n.ordered ? `${(n.start ?? 1) + i}.` : '•',
        c.muted,
      ]);
  }
}

/** `[!STEPS]`: a numbered dot per stage and a rail down to the next. */
function steps(n: Node, key: number, c: Colors, open: boolean): ReactNode {
  const all = n.children ?? [];
  const start = Math.max(1, n.start ?? 1);
  const dot = 26;
  return (
    <View key={key}>
      {all.map((item, i) => (
        <View key={i} style={{ flexDirection: 'row', gap: space.sm }}>
          <View style={{ alignItems: 'center', width: dot }}>
            <View
              style={{
                width: dot,
                height: dot,
                borderRadius: dot / 2,
                backgroundColor: c.accent,
                alignItems: 'center',
                justifyContent: 'center',
              }}
            >
              <Text style={{ color: c.accentText, fontSize: font.sm, fontWeight: '700' }}>{start + i}</Text>
            </View>
            {i < all.length - 1 ? <View style={{ flex: 1, width: 2, backgroundColor: c.border }} /> : null}
          </View>
          <View style={{ flex: 1, gap: space.xs, paddingBottom: i < all.length - 1 ? space.md : 0 }}>
            {blocksOf(item, c, open)}
          </View>
        </View>
      ))}
    </View>
  );
}

/** `[!STATS]`: headline figures in a row, each value over what it counts. */
function stats(n: Node, key: number, c: Colors, open: boolean): ReactNode {
  const all = n.children ?? [];
  const figures = all.map((item) => {
    const only = item.children ?? [];
    if (only.length !== 1 || only[0]!.type !== 'paragraph') return null;
    const parts = only[0]!.children ?? [];
    const label = parts.filter((p) => formOf(p)?.form === 'stat-label');
    return { value: parts.filter((p) => !label.includes(p)), label: label.flatMap((p) => p.children ?? []) };
  });
  if (figures.some((f) => f === null)) return items(n, key, c, open, () => ['•', c.muted]);
  return (
    <View key={key} style={{ flexDirection: 'row', flexWrap: 'wrap', gap: space.lg }}>
      {figures.map((f, i) => (
        <View key={i} style={{ gap: 2 }}>
          <Text selectable style={{ color: c.text, fontSize: font.xl, fontWeight: '700' }}>
            {f!.value.map((p, j) => inline(p, j, c))}
          </Text>
          <Text selectable style={{ color: c.muted, fontSize: font.sm }}>
            {f!.label.map((p, j) => inline(p, j, c))}
          </Text>
        </View>
      ))}
    </View>
  );
}

/** `[!TABS]`: one panel per `#### Label`, one shown at a time. */
function Tabs({ tabs, open }: { tabs: Node[]; open: boolean }) {
  const c = useColors();
  const [at, setAt] = useState(0);
  const shown = Math.min(at, tabs.length - 1);
  return (
    <View style={{ gap: space.sm }}>
      <View style={{ flexDirection: 'row', flexWrap: 'wrap', borderBottomWidth: 1, borderBottomColor: c.border }}>
        {tabs.map((t, i) => (
          <Pressable
            key={i}
            accessibilityRole="tab"
            accessibilityState={{ selected: i === shown }}
            onPress={() => setAt(i)}
            style={{
              paddingHorizontal: space.md,
              paddingVertical: space.xs,
              borderBottomWidth: 2,
              borderBottomColor: i === shown ? c.accent : 'transparent',
              marginBottom: -1,
            }}
          >
            <Text style={{ color: i === shown ? c.text : c.muted, fontSize: font.md, fontWeight: '600' }}>
              {formOf(t)?.label ?? `${i + 1}`}
            </Text>
          </Pressable>
        ))}
      </View>
      {shown >= 0 ? <View style={{ gap: space.sm }}>{blocksOf(tabs[shown]!, c, open)}</View> : null}
    </View>
  );
}

/** `[!DETAILS]`: its content folded under its title, closed until tapped. */
function Details({ node, open }: { node: Node; open: boolean }) {
  const c = useColors();
  const [shown, setShown] = useState(false);
  const parts = node.children ?? [];
  const summary = parts.find((p) => formOf(p)?.form === 'summary');
  return (
    <View style={{ gap: space.xs }}>
      <Pressable accessibilityRole="button" accessibilityState={{ expanded: shown }} onPress={() => setShown(!shown)}>
        <Text style={[bodyStyle(c), { fontWeight: '700' }]}>
          <Text style={{ color: c.muted }}>{shown ? '▾ ' : '▸ '}</Text>
          {summary ? inlines(summary, c) : 'Details'}
        </Text>
      </Pressable>
      {shown ? (
        <View style={{ paddingLeft: space.lg, gap: space.xs }}>
          {parts.filter((p) => p !== summary).map((p, i) => blockOf(p, i, c, open))}
        </View>
      ) : null}
    </View>
  );
}

function fence(n: Node, key: number, c: Colors, open: boolean): ReactNode {
  const title = formOf(n)?.title;
  const code = codeText(n);
  const label =
    n.lang === 'mermaid'
      ? 'diagram · mermaid'
      : title && n.lang && n.lang !== 'tree'
        ? `${title} · ${n.lang}`
        : (title ?? n.lang);
  const tree = n.lang === 'tree' ? treeLines(code) : [];
  const diffLang = (n.lang === 'diff' || n.lang === 'patch') && title ? langForPath(title) : null;
  return (
    <View key={key} style={{ backgroundColor: c.sunken, borderRadius: 6, padding: space.sm, gap: space.xs }}>
      {label ? <Text style={{ color: c.muted, fontSize: font.sm }}>{label}</Text> : null}
      {tree.length > 0 ? (
        <SideScroll>
          <Text selectable style={{ color: c.text, fontFamily: mono, fontSize: font.sm }}>
            {tree.map((r, i) => (
              <Text key={i}>
                <Text style={{ color: c.muted }}>{r.guide}</Text>
                <Text style={r.folder ? { fontWeight: '700' } : undefined}>{r.name}</Text>
                {r.comment ? <Text style={{ color: c.muted }}>{`  ${r.comment}`}</Text> : null}
                {i < tree.length - 1 ? '\n' : ''}
              </Text>
            ))}
          </Text>
        </SideScroll>
      ) : diffLang ? (
        <SideScroll>
          <DiffText lines={code.split('\n')} lang={diffLang} />
        </SideScroll>
      ) : (
        <Code code={code} lang={n.lang} streaming={open} />
      )}
    </View>
  );
}

/** A display formula: its LaTeX as Unicode (mathLines), set in from the text. */
function Formula({ latex }: { latex: string }) {
  const c = useColors();
  return (
    <SideScroll>
      <Text
        selectable
        accessibilityLabel={latex}
        style={{ color: c.text, fontSize: font.md, fontStyle: 'italic', paddingHorizontal: space.lg }}
      >
        {mathLines(latex).join('\n')}
      </Text>
    </SideScroll>
  );
}

function inlines(node: Node, c: Colors): ReactNode[] {
  return (node.children ?? []).map((n, i) => inline(n, i, c));
}

function inline(n: Node, key: number, c: Colors): ReactNode {
  switch (n.type) {
    case 'text':
      return n.value ?? '';
    case 'break':
      return '\n';
    case 'emphasis':
      switch (formOf(n)?.form) {
        case 'section-number':
          return (
            <Text key={key} style={{ color: c.muted }}>
              {inlines(n, c)}{' '}
            </Text>
          );
        case 'caption':
          return (
            <Text key={key} style={{ color: c.muted, fontStyle: 'italic', fontSize: font.sm }}>
              {'\n'}
              {inlines(n, c)}
            </Text>
          );
        case 'source-title':
          return <Text key={key}>{inlines(n, c)}</Text>;
        case 'source-host':
          return (
            <Text key={key} style={{ color: c.muted, textDecorationLine: 'none', fontSize: font.sm }}>
              {' · '}
              {inlines(n, c)}
            </Text>
          );
        case 'step-meta':
        case 'stat-label':
        case 'source-note':
          return (
            <Text key={key} style={{ color: c.muted, fontWeight: '400' }}>
              {' '}
              {inlines(n, c)}
            </Text>
          );
        default:
          return (
            <Text key={key} style={{ fontStyle: 'italic' }}>
              {inlines(n, c)}
            </Text>
          );
      }
    case 'strong':
      return (
        <Text key={key} style={{ fontWeight: '700' }}>
          {inlines(n, c)}
        </Text>
      );
    case 'delete':
      return (
        <Text key={key} style={{ textDecorationLine: 'line-through' }}>
          {inlines(n, c)}
        </Text>
      );
    case 'inlineCode':
      return (
        <Text key={key} style={{ fontFamily: mono, fontSize: font.sm, backgroundColor: c.sunken }}>
          {n.value}
        </Text>
      );
    case 'inlineMath':
      return (
        <Text key={key} accessibilityLabel={n.value} style={{ fontStyle: 'italic' }}>
          {mathLines(n.value ?? '').join(' ')}
        </Text>
      );
    case 'footnoteReference':
      return (
        <Text key={key} style={{ color: c.muted, fontSize: font.sm }}>
          [{n.index ?? n.label}]
        </Text>
      );
    case 'link':
    case 'image': {
      // An image is not fetched: it shows as a link to itself.
      const url = safeUrl(n.url);
      const label = n.type === 'image' ? `[image: ${n.alt || n.url}]` : inlines(n, c);
      if (url === null) return <Text key={key}>{label}</Text>;
      return (
        <Text
          key={key}
          accessibilityRole="link"
          onPress={() => void Linking.openURL(url)}
          style={{ color: c.accent, textDecorationLine: 'underline' }}
        >
          {label}
        </Text>
      );
    }
    default:
      return toText(n);
  }
}
