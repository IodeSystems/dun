// An assistant reply as Markdown, drawn with RN primitives only so web and Android match.
// The parse is flowdown's (src/lib/markdown.ts); each top-level block is memoized by its
// identity, so a streamed token re-renders the last block, not the reply.
import { memo, useState, type ReactNode } from 'react';
import { Linking, Text, View, type TextStyle } from 'react-native';

import {
  codeText,
  createReply,
  parseBlock,
  safeUrl,
  tableColumns,
  toText,
  type MarkdownBlock,
  type Node,
} from '@/lib/markdown';
import { Code, SideScroll } from '@/components/code/Code';
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

/** open: the block is a fence still streaming (its closer is owed). */
function blocksOf(node: Node, c: Colors, open = false): ReactNode[] {
  return (node.children ?? []).map((n, i) => blockOf(n, i, c, open));
}

function blockOf(n: Node, key: number, c: Colors, open: boolean): ReactNode {
  const body: TextStyle = { color: c.text, fontSize: font.md, lineHeight: font.md * 1.4 };
  switch (n.type) {
    case 'paragraph':
      return (
        <Text key={key} selectable style={body}>
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
      return (
        <View key={key} style={{ borderLeftWidth: 3, borderLeftColor: c.border, paddingLeft: space.md, gap: space.xs }}>
          {blocksOf(n, c, open)}
        </View>
      );
    case 'list':
      return (
        <View key={key} style={{ gap: n.spread ? space.sm : space.xs }}>
          {(n.children ?? []).map((item, i) => (
            <View key={i} style={{ flexDirection: 'row', gap: space.sm }}>
              <Text style={[body, { color: c.muted, minWidth: n.ordered ? 22 : 12 }]}>
                {item.checked === true
                  ? '☑'
                  : item.checked === false
                    ? '☐'
                    : n.ordered
                      ? `${(n.start ?? 1) + i}.`
                      : '•'}
              </Text>
              <View style={{ flex: 1, gap: space.xs }}>{blocksOf(item, c, open)}</View>
            </View>
          ))}
        </View>
      );
    case 'code':
      return (
        <View key={key} style={{ backgroundColor: c.sunken, borderRadius: 6, padding: space.sm, gap: space.xs }}>
          {n.lang ? <Text style={{ color: c.muted, fontSize: font.sm }}>{n.lang}</Text> : null}
          <Code code={codeText(n)} lang={n.lang} streaming={open} />
        </View>
      );
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
    default:
      // html, footnotes and anything else: its text, never markup.
      return (
        <Text key={key} selectable style={body}>
          {toText(n)}
        </Text>
      );
  }
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
      return (
        <Text key={key} style={{ fontStyle: 'italic' }}>
          {inlines(n, c)}
        </Text>
      );
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
