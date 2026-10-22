import { useState } from 'react';
import { Text, View } from 'react-native';

import { Button, ErrorText, Field, Muted } from '@/components/ui';
import { mcpIdOk, mcpSpec, placementNote, type Placement } from '@/lib/mcp';
import { font, space, useColors } from '@/lib/theme';

// /mcp add on the web: a form, not a line in the composer — a bearer token
// typed there would be kept in the sent history.
export function McpForm({ add, close }: { add: (specJSON: string) => void; close: () => void }) {
  const c = useColors();
  const [id, setId] = useState('');
  const [target, setTarget] = useState('');
  const [bearer, setBearer] = useState('');
  const [placement, setPlacement] = useState<Placement>('direct');
  const [err, setErr] = useState('');
  const isURL = /^https?:\/\//.test(target.trim());
  function submit() {
    if (!mcpIdOk(id.trim())) return setErr('an id is lowercase letters, digits, - and _ — it prefixes the server\'s tools');
    if (!target.trim()) return setErr('a URL or a command');
    add(JSON.stringify(mcpSpec(id, target, bearer, placement)));
    close();
  }
  return (
    <View style={{ gap: space.sm, borderWidth: 1, borderColor: c.border, borderRadius: 8, padding: space.sm, backgroundColor: c.surface }}>
      <Text style={{ color: c.text, fontSize: font.md, fontWeight: '600' }}>Add an MCP server</Text>
      <Field value={id} onChangeText={setId} placeholder="id (e.g. docs)" autoCapitalize="none" autoCorrect={false} />
      <Field value={target} onChangeText={setTarget} placeholder="https://… or a command" autoCapitalize="none" autoCorrect={false} />
      {isURL ? <Field value={bearer} onChangeText={setBearer} placeholder="bearer token (optional)" autoCapitalize="none" autoCorrect={false} secureTextEntry /> : null}
      <View style={{ flexDirection: 'row', gap: space.sm }}>
        <Button title="Direct" primary={placement === 'direct'} onPress={() => setPlacement('direct')} />
        <Button title="In the shell" primary={placement === 'shell'} onPress={() => setPlacement('shell')} />
      </View>
      <Muted>{placementNote[placement]}</Muted>
      <Muted>Saved for every workspace (0600 on the machine dund runs on), started now and every session; the same id replaces.</Muted>
      {err ? <ErrorText>{err}</ErrorText> : null}
      <View style={{ flexDirection: 'row', gap: space.sm }}>
        <Button title="Add" primary onPress={submit} />
        <Button title="Cancel" onPress={close} />
      </View>
    </View>
  );
}
