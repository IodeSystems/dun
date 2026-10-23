import type { GraphQLClient } from 'graphql-request';
import { createClient, type Client } from 'graphql-ws';

import { explain, UnauthorizedError } from '@/api/client';
import { SessionEventsLive, SubscribeToken } from '@/api/ops';

// Live events over graphql-transport-ws on /api/graphql (RT-1). One lazy
// socket per app (RT-3): it opens on the first subscription, closes after the
// last, and multiplexes every session followed. Each (re)subscribe mints a
// fresh token through an authenticated call (RT-2) — the socket itself
// carries no bearer — and resumes after the last seq received, of the epoch
// that numbered it: a restarted dund numbers from 1 again under a new epoch,
// and a seq from the old one gets a snapshot instead of a replay from the
// middle of the new numbering.

export function makeLive(baseUrl: string): Client {
  const url = baseUrl.replace(/\/+$/, '').replace(/^http/, 'ws') + '/api/graphql';
  return createClient({ url, lazy: true, retryAttempts: Infinity, shouldRetry: () => true });
}

export type Follow = {
  onEvent: (ev: Record<string, unknown>) => void;
  onConn: (problem: string) => void; // '' when connected
  onUnauthorized: () => void;
};

/** Where a follower is in a session's events: what it resumes from. */
export type Cursor = { after: string; epoch: string };

/** A string or number as a string; anything else is fallback. */
function scalar(v: unknown, fallback: string): string {
  return typeof v === 'string' || typeof v === 'number' ? String(v) : fallback;
}

/** The cursor after ev: a snapshot's after, else the event's seq. */
export function advance(c: Cursor, ev: Record<string, unknown>): Cursor {
  const epoch = typeof ev.epoch === 'string' ? ev.epoch : c.epoch;
  if (ev.type === 'snapshot' && ev.after !== undefined) return { after: scalar(ev.after, ''), epoch };
  if (ev.seq !== undefined) return { after: scalar(ev.seq, ''), epoch };
  return c;
}

/** Follows a session until the returned stop is called. */
export function followSession(gql: GraphQLClient, live: Client, id: string, f: Follow): () => void {
  let stopped = false;
  let at: Cursor = { after: '0', epoch: '' };
  let unsubscribe: (() => void) | null = null;
  let backoff = 1000;

  const retry = (problem: string) => {
    if (stopped) return;
    f.onConn(problem);
    setTimeout(() => void start(), backoff);
    backoff = Math.min(backoff * 2, 15_000);
  };

  async function start() {
    if (stopped) return;
    let tok;
    try {
      tok = (await gql.request(SubscribeToken, { id })).dund.subscribeToken;
    } catch (e) {
      const x = explain(e);
      if (x instanceof UnauthorizedError) return f.onUnauthorized();
      return retry(`connection lost — retrying (${x.message})`);
    }
    if (!tok || stopped) return;
    let refused = '';
    unsubscribe = live.subscribe(
      {
        query: String(SessionEventsLive),
        variables: { session: id, after: at.after, epoch: at.epoch, principal: tok.principal, timestamp: tok.timestamp, hmac: tok.hmac },
      },
      {
        next: (r) => {
          const ev = (r.data as { dund_sessionEvents?: { event?: Record<string, unknown> } } | null)?.dund_sessionEvents?.event;
          if (!ev) return;
          // A stream dund could not open (no such session) is an unsequenced error
          // event and then the end of the stream: retry with backoff.
          if (ev.type === 'error' && ev.seq === undefined) {
            refused = scalar(ev.error, 'refused');
            return;
          }
          backoff = 1000;
          f.onConn('');
          at = advance(at, ev);
          f.onEvent(ev);
        },
        error: (e: unknown) => {
          unsubscribe = null;
          const msg = e instanceof Error ? e.message : Array.isArray(e) ? ((e[0] as { message?: string } | undefined)?.message ?? 'error') : 'connection closed';
          retry(`connection lost — retrying (${msg})`);
        },
        // The service ended the stream (it fell behind, or restarted):
        // resume after the last seq, in its epoch.
        complete: () => {
          unsubscribe = null;
          if (refused) return retry(`connection lost — retrying (${refused})`);
          if (!stopped) setTimeout(() => void start(), 0);
        },
      },
    );
  }

  void start();
  return () => {
    stopped = true;
    unsubscribe?.();
  };
}
