// /api/v1/hello is open and answers with dund's JSON. A static site can answer
// the same path with 200 and an HTML page, so a 200 alone proves nothing.

export type Hello = { version: string; protocol: number; sign_in?: boolean };

/** Thrown when the address does not answer as dund. */
export class NotDundError extends Error {
  constructor(baseUrl: string) {
    super(`No dund at this address: ${baseUrl}`);
    this.name = 'NotDundError';
  }
}

/** dund's hello, or NotDundError when the reply is not dund's JSON. */
export async function fetchHello(baseUrl: string, doFetch: typeof fetch = fetch): Promise<Hello> {
  let r: Response;
  try {
    r = await doFetch(baseUrl + '/api/v1/hello');
  } catch {
    throw new NotDundError(baseUrl);
  }
  if (!r.ok || !(r.headers.get('content-type') ?? '').toLowerCase().includes('json')) {
    throw new NotDundError(baseUrl);
  }
  let h: unknown;
  try {
    h = await r.json();
  } catch {
    throw new NotDundError(baseUrl);
  }
  const o = h as Partial<Hello> | null;
  if (!o || typeof o.version !== 'string' || typeof o.protocol !== 'number') throw new NotDundError(baseUrl);
  return o as Hello;
}
