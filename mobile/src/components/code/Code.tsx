// Highlighted code with RN primitives only: one selectable Text, a nested Text per
// coloured span, so web and Android draw it alike. Used by markdown fences and the
// tool bodies; a language without a grammar draws plain monospace.
import { Fragment, memo, useMemo, type ReactNode } from 'react';
import { ScrollView, Text, type TextStyle } from 'react-native';

import { font, mono, useColors, type Colors } from '@/lib/theme';

import { highlight, plainLines, type Line } from './highlight';
import { langOf, type Lang } from './languages';

/** Lines as Text children: a span per coloured run, '\n' between lines. */
export function spans(lines: Line[], c: Colors): ReactNode[] {
  return lines.map((line, i) => (
    <Fragment key={i}>
      {line.map((s, j) =>
        s.role ? (
          <Text key={j} style={{ color: c.syntax[s.role] }}>
            {s.text}
          </Text>
        ) : (
          s.text
        ),
      )}
      {i < lines.length - 1 ? '\n' : ''}
    </Fragment>
  ));
}

/**
 * The lines of code in lang. While it streams, only the lines a newline has settled
 * are tokenized (memoized on them, so a token costs nothing until the next newline);
 * the line still growing is drawn plain.
 */
export function useHighlight(code: string, lang: Lang | null, streaming = false): Line[] {
  const cut = streaming ? code.lastIndexOf('\n') : code.length;
  const settled = cut < 0 ? '' : code.slice(0, cut);
  const lines = useMemo(() => highlight(settled, lang), [settled, lang]);
  if (!streaming) return lines;
  const tail = code.slice(cut + 1);
  return cut < 0 ? plainLines(tail) : [...lines, ...plainLines(tail)];
}

const codeStyle = (c: Colors): TextStyle => ({ color: c.text, fontFamily: mono, fontSize: font.sm });

/** code as selectable monospace Text, highlighted when lang has a grammar. */
export const CodeText = memo(function CodeText({
  code,
  lang,
  streaming = false,
}: {
  code: string;
  lang: Lang | null;
  streaming?: boolean;
}) {
  const c = useColors();
  const lines = useHighlight(code, lang, streaming);
  return (
    <Text selectable style={codeStyle(c)}>
      {spans(lines, c)}
    </Text>
  );
});

/**
 * The one sideways scroll for code and output: its lines never wrap. Nested so a
 * vertical page scroll still works around it on Android.
 */
export function SideScroll({ children }: { children: ReactNode }) {
  return (
    <ScrollView horizontal nestedScrollEnabled showsHorizontalScrollIndicator>
      {children}
    </ScrollView>
  );
}

/** A code block: the fence's info string picks the grammar (languages.ts langOf); scrolls sideways. */
export function Code({ code, lang, streaming = false }: { code: string; lang?: string | null; streaming?: boolean }) {
  return (
    <SideScroll>
      <CodeText code={code} lang={langOf(lang)} streaming={streaming} />
    </SideScroll>
  );
}

/** Output that is not code (a shell's, a job's): plain selectable monospace, never wrapped, scrolls sideways. */
export function PlainCode({ text, color }: { text: string; color?: string }) {
  const c = useColors();
  return (
    <SideScroll>
      <Text selectable style={[codeStyle(c), color ? { color } : null]}>
        {text}
      </Text>
    </SideScroll>
  );
}

/** A unified diff line's kind (bubbleteandmd diffview.Classify, render.go colorizeDiff). */
export function diffKind(l: string): 'add' | 'del' | 'hunk' | 'file' | 'ctx' {
  if (l.startsWith('+++') || l.startsWith('---')) return 'file';
  if (l.startsWith('+')) return 'add';
  if (l.startsWith('-')) return 'del';
  if (l.startsWith('@@')) return 'hunk';
  return 'ctx';
}

/**
 * A unified diff with its code highlighted (bubbleteandmd diff.go): each +, - or context
 * line's code lexed alone without its marker; the marker in the line's colour, added and
 * removed lines on a tint. Headers, hunks and other lines draw as before.
 */
export const DiffText = memo(function DiffText({ lines, lang }: { lines: string[]; lang: Lang }) {
  const c = useColors();
  const drawn = useMemo(
    () =>
      lines.map((l, i) => {
        const k = diffKind(l);
        const nl = i < lines.length - 1 ? '\n' : '';
        if (k === 'file' || k === 'hunk' || (k === 'ctx' && !l.startsWith(' '))) {
          return (
            <Fragment key={i}>
              <Text style={{ color: k === 'hunk' ? c.muted : c.text }}>{l}</Text>
              {nl}
            </Fragment>
          );
        }
        const bg = k === 'add' ? c.diffAdd : k === 'del' ? c.diffDel : undefined;
        const mark = k === 'add' ? c.accent : k === 'del' ? c.danger : c.muted;
        return (
          <Fragment key={i}>
            <Text style={bg ? { backgroundColor: bg } : undefined}>
              <Text style={{ color: mark }}>{l[0]}</Text>
              {spans(highlight(l.slice(1), lang), c)}
            </Text>
            {nl}
          </Fragment>
        );
      }),
    [lines, lang, c],
  );
  return (
    <Text selectable style={codeStyle(c)}>
      {drawn}
    </Text>
  );
});

const NUMBERED = /^(\d+)\t(.*)$/;

/** A file_read result's lines split into the head, the gutter numbers and the code; null if not that shape. */
export function splitNumbered(
  lines: string[],
): { head: string[]; nums: string[]; code: string[]; tail: string[] } | null {
  const head: string[] = [];
  const nums: string[] = [];
  const code: string[] = [];
  let i = 0;
  while (i < lines.length && !NUMBERED.test(lines[i]!)) head.push(lines[i++]!);
  while (i < lines.length) {
    const m = NUMBERED.exec(lines[i]!);
    if (!m) break;
    nums.push(m[1]!);
    code.push(m[2]!);
    i++;
  }
  if (code.length === 0) return null;
  return { head, nums, code, tail: lines.slice(i) };
}

/**
 * A file_read result (agentkit files/read.go: a head line, then "N\tline"): the code
 * tokenized as one text, so a comment or string over several lines colours right; the
 * line numbers muted; the head and any trailing note as written.
 */
export const NumberedText = memo(function NumberedText({ lines, lang }: { lines: string[]; lang: Lang }) {
  const c = useColors();
  const parts = useMemo(() => splitNumbered(lines), [lines]);
  const code = useMemo(() => (parts ? highlight(parts.code.join('\n'), lang) : []), [parts, lang]);
  if (!parts) return <CodeText code={lines.join('\n')} lang={null} />;
  const plain = (ls: string[]) => ls.map((l) => l + '\n').join('');
  return (
    <Text selectable style={codeStyle(c)}>
      {parts.head.length ? plain(parts.head) : ''}
      {code.map((line, i) => (
        <Fragment key={i}>
          <Text style={{ color: c.muted }}>{parts.nums[i]}</Text>
          {'\t'}
          {spans([line], c)}
          {i < code.length - 1 || parts.tail.length ? '\n' : ''}
        </Fragment>
      ))}
      {parts.tail.join('\n')}
    </Text>
  );
});
