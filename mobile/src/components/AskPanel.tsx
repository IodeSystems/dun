// The open question above the composer (proto.Ask): pick one, or for multi
// check any number and send them; noCustom means the composer does not
// answer it (the answer must be an option). Give it key={ask.askId} so a new
// question starts with its own recommendations checked.

import { useState } from 'react';
import { Text, View } from 'react-native';

import { Button, Muted } from '@/components/ui';
import type { Ask } from '@/lib/events';
import { font, space, useColors } from '@/lib/theme';

/** Several picks go back comma-separated, as ask_user says. */
export const joinPicks = (picks: string[]) => picks.join(', ');

export function AskPanel({ ask, answer }: { ask: Ask; answer: (value: string) => void }) {
  const c = useColors();
  const [picked, setPicked] = useState<string[]>(ask.multi ? ask.recommendedAll : []);
  const toggle = (o: string) => setPicked((p) => (p.includes(o) ? p.filter((x) => x !== o) : [...p, o]));
  const star = (o: string) => (ask.recommendedAll.includes(o) ? `${o} ★` : o);
  return (
    <View
      style={{
        gap: space.sm,
        paddingVertical: space.xs,
        borderLeftWidth: 3,
        borderColor: c.accent,
        paddingLeft: space.md,
      }}
    >
      <Text style={{ color: c.text, fontSize: font.md }}>❓ {ask.question}</Text>
      <View style={{ flexDirection: 'row', flexWrap: 'wrap', gap: space.sm }}>
        {ask.options.map((o) =>
          ask.multi ? (
            <Button key={o} title={(picked.includes(o) ? '☑ ' : '☐ ') + star(o)} onPress={() => toggle(o)} />
          ) : (
            <Button key={o} title={star(o)} primary={o === ask.recommended} onPress={() => answer(o)} />
          ),
        )}
        {ask.multi ? (
          <Button
            title={picked.length ? `Send ${picked.length}` : 'Send'}
            primary
            disabled={!picked.length}
            onPress={() => answer(joinPicks(ask.options.filter((o) => picked.includes(o))))}
          />
        ) : null}
      </View>
      {ask.noCustom ? <Muted>pick from the options</Muted> : null}
    </View>
  );
}
