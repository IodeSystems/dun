import { ClientError, GraphQLClient } from 'graphql-request';

// The typed GraphQL client (WEB-4): documents in ops.ts, types generated into
// src/gql by `pnpm gen` from the committed schema.graphql.

/** The service refused the token. */
export class UnauthorizedError extends Error {
  constructor() {
    super('The service refused the token.');
    this.name = 'UnauthorizedError';
  }
}

export function makeClient(baseUrl: string, token: string): GraphQLClient {
  return new GraphQLClient(baseUrl.replace(/\/+$/, '') + '/api/graphql', {
    headers: { Authorization: `Bearer ${token}` },
  });
}

/** A readable message for any request failure, and the 401 as its own type. */
export function explain(e: unknown): Error {
  if (e instanceof ClientError) {
    if (e.response.status === 401) return new UnauthorizedError();
    const msg = e.response.errors?.[0]?.message;
    return new Error(msg ?? `HTTP ${e.response.status}`);
  }
  if (e instanceof Error) return e;
  return new Error(String(e));
}
