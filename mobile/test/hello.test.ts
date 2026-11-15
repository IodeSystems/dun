/// <reference types="jest" />
import { fetchHello, NotDundError } from '../src/lib/hello';

const reply = (body: string, type: string, status = 200) =>
  (() => Promise.resolve(new Response(body, { status, headers: { 'content-type': type } }))) as typeof fetch;

test('dund JSON is accepted', async () => {
  const h = await fetchHello(
    'https://dund.example',
    reply('{"version":"1.2.3","protocol":4,"sign_in":true}', 'application/json'),
  );
  expect(h).toEqual({ version: '1.2.3', protocol: 4, sign_in: true });
});

test('an HTML index fallback with status 200 is refused, with the address', async () => {
  const p = fetchHello('https://static.example', reply('<!doctype html><html></html>', 'text/html'));
  await expect(p).rejects.toThrow(NotDundError);
  await expect(p).rejects.toThrow('No dund at this address: https://static.example');
});

test('JSON without version and protocol is refused', async () => {
  await expect(fetchHello('https://x.example', reply('{"ok":true}', 'application/json'))).rejects.toThrow(NotDundError);
});

test('a JSON content-type over an HTML body is refused', async () => {
  await expect(fetchHello('https://x.example', reply('<html>', 'application/json'))).rejects.toThrow(NotDundError);
});

test('a network failure is refused', async () => {
  const down = (() => Promise.reject(new Error('offline'))) as typeof fetch;
  await expect(fetchHello('https://x.example', down)).rejects.toThrow(NotDundError);
});
