// The open body of each kind of tool call: what the TUI's pager shows for it
// (internal/tui/toolrun.go page: the call in full, then the renderer's body).

import { Fragment, useState, type ReactNode } from 'react';
import { Pressable, Text, View } from 'react-native';

import { CodeText, DiffText, NumberedText, PlainCode, SideScroll } from '@/components/code/Code';
import { langForPath, type Lang } from '@/components/code/languages';
import { exitMarker, type ToolCall } from '@/lib/toolcalls';
import { argFull, isDiff, prettyJson } from '@/lib/tooltext';
import { font, mono, space, useColors, type Colors } from '@/lib/theme';

export type BodyProps = { call: ToolCall };

/** Lines shown before "show all": a long output is collapsed. */
export const CLIP_LINES = 30;

/** Monospace text, selectable, cut to CLIP_LINES with a press to show the rest. */
export function Clipped({
  text,
  color,
  lineColor,
  draw,
}: {
  text: string;
  color?: string;
  lineColor?: (l: string) => string;
  /** Draws the shown lines instead (highlighted code). */
  draw?: (shown: string[]) => ReactNode;
}) {
  const c = useColors();
  const [all, setAll] = useState(false);
  const lines = text.replace(/\n+$/, '').split('\n');
  const cut = !all && lines.length > CLIP_LINES;
  const shown = cut ? lines.slice(0, CLIP_LINES) : lines;
  const style = { fontFamily: mono, fontSize: font.sm, color: color ?? c.text };
  return (
    <View>
      <SideScroll>
        {draw ? (
          draw(shown)
        ) : lineColor ? (
          <Text selectable style={style}>
            {shown.map((l, i) => (
              <Fragment key={i}>
                <Text style={{ color: lineColor(l) }}>{l}</Text>
                {i < shown.length - 1 ? '\n' : ''}
              </Fragment>
            ))}
          </Text>
        ) : (
          <Text selectable style={style}>
            {shown.join('\n')}
          </Text>
        )}
      </SideScroll>
      {cut ? (
        <Pressable accessibilityRole="button" onPress={() => setAll(true)} style={{ paddingVertical: space.xs }}>
          <Text style={{ color: c.accent, fontSize: font.sm }}>show all {lines.length} lines</Text>
        </Pressable>
      ) : null}
    </View>
  );
}

/** The grammar for a file tool's path argument, or null (plain). */
const pathLang = (call: ToolCall): Lang | null =>
  langForPath(typeof call.args?.path === 'string' ? call.args.path : null);

/** A diff: highlighted inside its +/- lines when the path has a grammar, else coloured by line. */
function Diff({ text, lang }: { text: string; lang: Lang | null }) {
  const c = useColors();
  if (!lang) return <Clipped text={text} lineColor={diffColor(c)} />;
  return <Clipped text={text} draw={(shown) => <DiffText lines={shown} lang={lang} />} />;
}

/** A unified diff's line colour (render.go colorizeDiff): +, -, @@; file headers neutral. */
export function diffColor(c: Colors): (l: string) => string {
  return (l) => {
    if (l.startsWith('+++') || l.startsWith('---')) return c.text;
    if (l.startsWith('+')) return c.accent;
    if (l.startsWith('-')) return c.danger;
    if (l.startsWith('@@')) return c.muted;
    return c.text;
  };
}

function Label({ children }: { children: string }) {
  const c = useColors();
  return <Text style={{ color: c.muted, fontSize: font.sm, marginTop: space.xs }}>{children}</Text>;
}

function Result({ call }: BodyProps) {
  const c = useColors();
  if (call.result === undefined) return <Label>running…</Label>;
  if (!call.result.trim()) return <Label>(no output)</Label>;
  return isDiff(call.result) ? (
    <Clipped text={call.result} lineColor={diffColor(c)} />
  ) : (
    <Clipped text={call.result} color={call.state === 'error' ? c.danger : c.text} />
  );
}

/** Any tool (genericRender): the args in full, then the raw result. */
export function GenericBody({ call }: BodyProps) {
  const args = argFull(call.args);
  return (
    <View style={{ gap: space.xs }}>
      {args ? <Clipped text={args} /> : null}
      <Result call={call} />
    </View>
  );
}

/** exec (renderers.go execRender, tui_commands.go callFull): the command verbatim, the exit, the output. */
export function ShellBody({ call }: BodyProps) {
  const c = useColors();
  const cmd = typeof call.args?.command === 'string' ? call.args.command : '';
  const rest = { ...call.args };
  delete rest.command;
  const other = argFull(rest);
  const marker = call.result === undefined ? null : exitMarker(call.result);
  const out = marker === null ? call.result : call.result!.trimEnd().split('\n').slice(0, -1).join('\n');
  return (
    <View style={{ gap: space.xs }}>
      <PlainCode text={`$ ${cmd}`} />
      {other ? <Clipped text={other} color={c.muted} /> : null}
      {call.result === undefined ? (
        <Label>running…</Label>
      ) : out?.trim() ? (
        <Clipped text={out} lineColor={isDiff(out) ? diffColor(c) : undefined} />
      ) : (
        <Label>(no output)</Label>
      )}
      {marker !== null ? <Text style={{ color: c.danger, fontSize: font.sm }}>exit: {marker}</Text> : null}
    </View>
  );
}

/** file_edit, node_edit (filerender.go fileEditRender, renderers.go diffRender): the diff, coloured. */
export function DiffBody({ call }: BodyProps) {
  if (call.result === undefined || call.state === 'error') return <Result call={call} />;
  return <Diff text={call.result} lang={pathLang(call)} />;
}

/** file_read, node_read (fileReadRender, nodeReadRender): what was read, verbatim. */
export function ReadBody({ call }: BodyProps) {
  const lang = pathLang(call);
  if (!lang || call.result === undefined || call.state === 'error' || !call.result.trim())
    return <Result call={call} />;
  return <Clipped text={call.result} draw={(shown) => <NumberedText lines={shown} lang={lang} />} />;
}

/** file_write (fileWriteRender): what was written, from the call's content. */
export function WriteBody({ call }: BodyProps) {
  const content = typeof call.args?.content === 'string' ? call.args.content : '';
  return (
    <View style={{ gap: space.xs }}>
      <Result call={call} />
      {content ? <WriteContent text={content} lang={pathLang(call)} /> : null}
    </View>
  );
}

function WriteContent({ text, lang }: { text: string; lang: Lang | null }) {
  if (!lang) return <Clipped text={text} />;
  return <Clipped text={text} draw={(shown) => <CodeText code={shown.join('\n')} lang={lang} />} />;
}

/** file_grep, file_read with grep (fileGrepRender): the hits, verbatim. */
export function SearchBody({ call }: BodyProps) {
  return <Result call={call} />;
}

/** search, node_query, … (jsonRender): the JSON pretty-printed; generic when it is not JSON. */
export function JsonBody({ call }: BodyProps) {
  const pretty = call.result === undefined ? null : prettyJson(call.result);
  if (pretty === null) return <GenericBody call={call} />;
  return <Clipped text={pretty} />;
}
