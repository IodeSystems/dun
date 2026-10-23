/// <reference types="jest" />
import type { GraphQLClient } from 'graphql-request';
import type { Client, Sink } from 'graphql-ws';

import { advance, followSession } from '../src/api/live';

jest.mock('@/api/ops', () => ({ SessionEventsLive: 'subscription', SubscribeToken: 'mutation' }));
jest.mock('@/api/client', () => ({ explain: (e: unknown) => e, UnauthorizedError: class {} }));

test('the cursor follows seqs and snapshots, in their epoch', () => {
  let c = { after: '0', epoch: '' };
  c = advance(c, { type: 'ready', seq: 4, epoch: 'a' });
  expect(c).toEqual({ after: '4', epoch: 'a' });
  c = advance(c, { type: 'ui_state' }); // unsequenced: no move
  expect(c).toEqual({ after: '4', epoch: 'a' });
  c = advance(c, { type: 'snapshot', after: 0, epoch: 'b' });
  expect(c).toEqual({ after: '0', epoch: 'b' });
});

// dund restarts: the stream completes, and the follower resubscribes after
// the last seq it saw WITH that seq's epoch — the new dund numbers from 1
// again, so a bare seq could land in the middle of its numbering.
test('a resubscribe after a restart names the epoch of its seq', async () => {
  const subs: { variables: Record<string, unknown>; sink: Sink }[] = [];
  const live = {
    subscribe: (p: { variables: Record<string, unknown> }, sink: Sink) => {
      subs.push({ variables: p.variables, sink });
      return () => {};
    },
  } as unknown as Client;
  const gql = {
    request: () => Promise.resolve({ dund: { subscribeToken: { principal: 'p', timestamp: '1', hmac: 'h' } } }),
  } as unknown as GraphQLClient;
  const seen: Record<string, unknown>[] = [];
  const stop = followSession(gql, live, 's1', {
    onEvent: (e) => seen.push(e),
    onConn: () => {},
    onUnauthorized: () => {},
  });
  const settle = () => new Promise((r) => setTimeout(r, 5));
  await settle();
  expect(subs[0]!.variables).toMatchObject({ session: 's1', after: '0', epoch: '' });

  const send = (ev: Record<string, unknown>) =>
    subs[subs.length - 1]!.sink.next({ data: { dund_sessionEvents: { event: ev } } });
  send({ type: 'session', seq: 1, epoch: 'old' });
  send({ type: 'done', seq: 40, epoch: 'old' });
  subs[0]!.sink.complete(); // dund went away
  await settle();
  expect(subs[1]!.variables).toMatchObject({ after: '40', epoch: 'old' });

  // The new dund answers with a snapshot in its own epoch; the next
  // resubscribe resumes from that.
  send({ type: 'snapshot', after: 3, epoch: 'new', items: [], state: [] });
  send({ type: 'done', seq: 4, epoch: 'new' });
  subs[1]!.sink.complete();
  await settle();
  expect(subs[2]!.variables).toMatchObject({ after: '4', epoch: 'new' });
  expect(seen.map((e) => e.type)).toEqual(['session', 'done', 'snapshot', 'done']);
  stop();
});
