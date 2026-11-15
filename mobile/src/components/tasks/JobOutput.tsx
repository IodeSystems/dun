import type { GraphQLClient } from 'graphql-request';
import { useEffect, useRef, useState } from 'react';
import { Text } from 'react-native';

import { explain } from '@/api/client';
import { JobOutput as JobOutputOp } from '@/api/ops';
import { PlainCode } from '@/components/code/Code';
import { font, useColors } from '@/lib/theme';

/** What the first read takes of a long log: its end. */
const TAIL = 64 << 10;
/** The most kept on screen; older output goes from the top. */
const KEEP = 256 << 10;
const POLL_MS = 1500;

/**
 * A background job's output (dundapi jobOutput): its tail, then what it adds
 * while it runs, every 1.5 s. Monospace, never wrapped, sideways scroll.
 */
export function JobOutput({
  client,
  session,
  job,
  running,
}: {
  client: GraphQLClient;
  session: string;
  job: number;
  running: boolean;
}) {
  const c = useColors();
  const [text, setText] = useState('');
  const [cut, setCut] = useState(false);
  const [err, setErr] = useState('');
  const next = useRef<string | null>(null);

  useEffect(() => {
    let live = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const read = async () => {
      try {
        const first = next.current === null;
        const r = (
          await client.request(JobOutputOp, {
            id: session,
            job: String(job),
            since: first ? '-1' : next.current,
            tail: first ? String(TAIL) : '0',
          })
        ).dund.jobOutput;
        if (!live || !r) return;
        if (first && Number(r.offset) > 0) setCut(true);
        next.current = r.next;
        if (r.data) setText((t) => (t + r.data).slice(-KEEP));
        setErr('');
      } catch (e) {
        if (live) setErr(explain(e).message);
      }
      if (live && running) timer = setTimeout(() => void read(), POLL_MS);
    };
    void read();
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [client, session, job, running]);

  return (
    <>
      {cut ? <Text style={{ color: c.muted, fontSize: font.sm }}>… the last 64 KiB</Text> : null}
      {err ? <Text style={{ color: c.danger, fontSize: font.sm }}>{err}</Text> : null}
      {text ? (
        <PlainCode text={text.replace(/\n$/, '')} />
      ) : (
        <Text style={{ color: c.muted, fontSize: font.sm }}>{running ? 'no output yet' : 'no output'}</Text>
      )}
    </>
  );
}
