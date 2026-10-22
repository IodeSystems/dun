// Drive the dun web app (mobile/, react-native-web) against a TEST dund on
// localhost, on a fresh session it creates and removes. Never point it at a
// real service: it types the token into the page.
//
//   DUN_HOME=<scratch> dun dund token > /tmp/tok
//   DUN_HOME=<scratch> dun dund serve --http 127.0.0.1:28005 --web-root mobile/dist &
//   PLAYWRIGHT=<path to a playwright package> node tools/e2e-web.cjs \
//     http://127.0.0.1:28005 /tmp/tok <workspace> <screenshot dir>
//
// Steps print ok/FAIL; a failing step leaves a screenshot. Exit 1 on any FAIL.
const { chromium } = require(process.env.PLAYWRIGHT || 'playwright');
const fs = require('fs');
const [base, tokFile, ws, out] = process.argv.slice(2);
const tok = fs.readFileSync(tokFile, 'utf8').trim();
const H = { Authorization: `Bearer ${tok}`, 'Content-Type': 'application/json' };
const api = async (method, path, body) => (await fetch(base + path, { method, headers: H, body: body && JSON.stringify(body) })).json();
(async () => {
  const ses = (await api('POST', '/api/v1/sessions', { workspace: ws })).id;
  console.log('session', ses);
  const b = await chromium.launch();
  const p = await b.newPage({ viewport: { width: 1100, height: 900 } });
  const errors = [];
  p.on('pageerror', (e) => errors.push(String(e)));
  p.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
  let failed = 0;
  const step = async (name, fn) => { try { await fn(); console.log('ok   ', name); } catch (e) { failed++; console.log('FAIL ', name, String(e).split('\n')[0]); await p.screenshot({ path: `${out}/fail-${name.replace(/\W+/g, '_')}.png` }); } };
  const idle = async () => { // the turn is over: no "working…" for a moment
    for (let i = 0; i < 240; i++) { if (!(await p.getByText('working…').count())) { await p.waitForTimeout(800); if (!(await p.getByText('working…').count())) return; } await p.waitForTimeout(500); }
    throw new Error('still working after 2 min');
  };
  await p.goto(base + '/');
  await step('sign in with the token', async () => {
    await p.getByText('Sign in to dund').waitFor({ timeout: 10000 });
    await p.getByPlaceholder('dun_…').fill(tok);
    await p.getByRole('button', { name: 'Sign in' }).click();
    await p.getByText(ses).waitFor({ timeout: 10000 });
  });
  await step('open the session', async () => {
    await p.getByText(ses).click();
    await p.getByPlaceholder(/ask dun to do something/).waitFor({ timeout: 10000 });
  });
  const box = p.getByPlaceholder(/ask dun to do something/);
  await step('the / palette lists commands', async () => {
    await box.fill('/re');
    await p.getByText('/rephrase [on|off|status]').waitFor({ timeout: 15000 });
  });
  await step('a slash command answers; a panel chip runs one', async () => {
    await box.fill('/rephrase on'); await box.press('Enter');
    await p.getByText('rephrase: on', { exact: true }).waitFor({ timeout: 10000 });
    await p.getByText('Session', { exact: true }).click();
    await p.getByRole('button', { name: 'auto', exact: true }).last().click();
    await p.getByText('suggestions: auto', { exact: true }).waitFor({ timeout: 10000 });
    await p.getByText('Hide session').click();
    await box.fill('/rephrase off'); await box.press('Enter');
    await p.getByText('rephrase: off', { exact: true }).waitFor({ timeout: 10000 });
  });
  await step('an unknown command is refused locally', async () => {
    await box.fill('/skill nope'); await box.press('Enter');
    await p.getByText(/no command \/skill nope/).waitFor({ timeout: 5000 });
  });
  await step('a message gets a reply live', async () => {
    await box.fill('Reply with exactly the word: pineapple'); await box.press('Enter');
    await p.getByText('pineapple', { exact: true }).waitFor({ timeout: 120000 });
    await idle();
  });
  await step('a question offers options, the recommendation marked; the answer renders', async () => {
    await box.fill('Use the ask_user tool to ask me which fruit I prefer, apple or mango, recommending mango. Then reply with just the fruit I picked.');
    await box.press('Enter');
    const rec = p.getByRole('button', { name: 'mango ★' });
    await rec.waitFor({ timeout: 120000 });
    await p.screenshot({ path: `${out}/q1-asked.png` });
    await rec.click();
    await p.getByText(/mango \(recommended\)/).waitFor({ timeout: 30000 });
    await idle();
  });
  await p.screenshot({ path: `${out}/q2-answered.png` });
  await step('/model opens the picker with every provider\'s models', async () => {
    await box.fill('/model'); await box.press('Enter');
    const search = p.getByPlaceholder('search: words of the provider or model');
    await search.waitFor({ timeout: 10000 });
    await p.getByText('llm.iodesystems.com').nth(1).waitFor({ timeout: 20000 });
    await search.fill('qwen 27b');
    await p.getByText(/Qwen3\.8-27B/i).first().waitFor({ timeout: 5000 });
    await p.getByText('Close', { exact: true }).click();
  });
  console.log('page errors:', errors.length ? errors.slice(0, 5) : 'none');
  await b.close();
  await api('DELETE', `/api/v1/sessions/${ses}`);
  process.exit(failed ? 1 : 0);
})();
