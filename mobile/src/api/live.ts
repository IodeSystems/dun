import type { GraphQLClient } from 'graphql-request';
import { createClient, type Client } from 'graphql-ws';

import { explain, UnauthorizedError } from '@/api/client';
import { SessionEventsLive, SubscribeToken } from '@/api/ops';

// Live events over graphql-transport-ws on /api/graphql (RT-1). One lazy
// socket per app (RT-3): it opens on the first subscription, closes after the
// last, and multiplexes every session followed. Each (re)subscribe mints a
// fresh token through an authenticated call (RT-2) — the socket itself
// carries no bearer — and resumes after the last seq received.

export function makeLive(baseUrl: string): Client {
  const url = baseUrl.replace(/\/+$/, '').replace(/^http/, 'ws') + '/api/graphql';
  return createClient({ url, lazy: true, retryAttempts: Infinity, shouldRetry: () => true });
}

export type Follow = {
  onEvent: (ev: Record<string, unknown>) => void;
  onConn: (problem: string) => void; // '' when connected
  onUnauthorized: () => void;
};

/** Follows a session until the returned stop is called. */
export function followSession(gql: GraphQLClient, live: Client, id: string, f: Follow): () => void {
  let stopped = false;
  let after = '0';
  let unsubscribe: (() => void) | null = null;
  let backoff = 1000;

  const retry = (problem: string) => {
    if (stopped) return;
    f.onConn(problem);
    setTimeout(start, backoff);
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
        variables: { session: id, after, principal: tok.principal, timestamp: tok.timestamp, hmac: tok.hmac },
      },
      {
        next: (r) => {
          const ev = (r.data as { dund_sessionEvents?: { event?: Record<string, unknown> } } | null)?.dund_sessionEvents?.event;
          if (!ev) return;
          // A refusal (bad token, no such session) is an unsequenced error
          // event and then the end of the stream: retry with backoff.
          if (ev.type === 'error' && ev.seq === undefined) {
            refused = String(ev.error ?? 'refused');
            return;
          }
          backoff = 1000;
          f.onConn('');
          if (ev.type === 'snapshot' && ev.after !== undefined) after = String(ev.after);
          else if (ev.seq !== undefined) after = String(ev.seq);
          f.onEvent(ev);
        },
        error: (e) => {
          unsubscribe = null;
          const msg = e instanceof Error ? e.message : Array.isArray(e) ? (e[0]?.message ?? 'error') : 'connection closed';
          retry(`connection lost — retrying (${msg})`);
        },
        // The service ended the stream (it fell behind, or restarted):
        // resume after the last seq.
        complete: () => {
          unsubscribe = null;
          if (refused) return retry(`connection lost — retrying (${refused})`);
          if (!stopped) setTimeout(start, 0);
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
