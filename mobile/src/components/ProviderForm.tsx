import { useState } from 'react';
import { Text, View } from 'react-native';

import { Button, ErrorText, Field, Muted } from '@/components/ui';
import { providerNameOk, providerUrlOk, type ProviderOp } from '@/lib/provider';
import { font, space, useColors } from '@/lib/theme';

// /model add and /model edit: a form, not a line in the composer — a key
// typed there would be kept in the sent history. Editing keeps the saved key
// unless a new one is typed.
export function ProviderForm({ edit, save, close }: {
  edit: { name: string; url: string } | null;
  save: (op: ProviderOp) => void;
  close: () => void;
}) {
  const c = useColors();
  const [name, setName] = useState(edit?.name ?? '');
  const [url, setUrl] = useState(edit?.url ?? '');
  const [key, setKey] = useState('');
  const [err, setErr] = useState('');
  function submit() {
    if (!providerNameOk(name)) return setErr('a name is one word');
    if (!providerUrlOk(url)) return setErr('the URL starts with http:// or https://');
    save({ op: edit ? 'edit' : 'add', name: name.trim(), url: url.trim(), key: key.trim() });
    close();
  }
  return (
    <View style={{ gap: space.sm, borderWidth: 1, borderColor: c.border, borderRadius: 8, padding: space.sm, backgroundColor: c.surface }}>
      <Text style={{ color: c.text, fontSize: font.md, fontWeight: '600' }}>{edit ? `Edit provider ${edit.name}` : 'Add a provider'}</Text>
      {edit ? null : <Field value={name} onChangeText={setName} placeholder="name (e.g. corp)" autoCapitalize="none" autoCorrect={false} />}
      <Field value={url} onChangeText={setUrl} placeholder="https://… (an OpenAI-compatible endpoint, e.g. LiteLLM)" autoCapitalize="none" autoCorrect={false} />
      <Field value={key} onChangeText={setKey} placeholder={edit ? 'new API key (empty keeps the saved one)' : 'API key (empty for none)'} autoCapitalize="none" autoCorrect={false} secureTextEntry />
      <Muted>Saved on the machine dund runs on (0600); the key is never sent back.</Muted>
      {err ? <ErrorText>{err}</ErrorText> : null}
      <View style={{ flexDirection: 'row', gap: space.sm }}>
        <Button title="Save" primary onPress={submit} />
        <Button title="Cancel" onPress={close} />
      </View>
    </View>
  );
}
