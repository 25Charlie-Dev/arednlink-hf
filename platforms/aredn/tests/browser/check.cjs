'use strict';
// Only synthetic credentials are supplied by the isolated fixture launcher.
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const assert = require('node:assert/strict');

(async () => {
  const [base, output, phase] = process.argv.slice(2);
  const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'native-browser-profile-'));
  const cache = fs.mkdtempSync(path.join(os.tmpdir(), 'native-browser-cache-'));
  const config = path.join(profile, 'config');
  const crashes = path.join(profile, 'crashes');
  fs.mkdirSync(config);
  fs.mkdirSync(crashes);
  const context = await chromium.launchPersistentContext(profile, {
    executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', headless: true,
    viewport: {width: 1280, height: 1000},
    env: {...process.env, XDG_CACHE_HOME: cache, XDG_CONFIG_HOME: config, CHROME_LOG_FILE: path.join(output, 'chromium-' + phase + '.log')},
    args: ['--no-sandbox', '--disable-dev-shm-usage', '--enable-logging', '--crash-dumps-dir=' + crashes]});
  const page = await context.newPage();
  const errors = [];
  const network = [];
  const expectedOutageErrors = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => {
    if (message.type() !== 'error') return;
    const location = message.location().url || '';
    if (phase === 'recovery' && location.endsWith('/cgi-bin/apps/arednlink-hf/admin') && message.text().includes('503'))
      expectedOutageErrors.push(message.text());
    else errors.push(message.text());
  });
  page.on('requestfailed', request => errors.push(request.method() + ' ' + new URL(request.url()).pathname + ' failed'));
  page.on('response', response => network.push({path: new URL(response.url()).pathname, status: response.status()}));
  const checks = [];
  const check = (condition, label) => { assert(condition, label); checks.push(label); };
  try {
    const anonymous = await context.request.get(base + '/cgi-bin/apps/arednlink-hf/admin');
    check(anonymous.status() === 401, 'real uhttpd anonymous direct URL refused');
    const authValue = fs.readFileSync(process.env.SYNTHETIC_COOKIE_FILE, 'utf8').trim();
    await context.addCookies([{name: 'authV1', value: authValue, url: base, sameSite: 'Lax'}]);
    const response = await page.goto(base + '/cgi-bin/apps/arednlink-hf/admin');
    check(response.status() === 200, 'authenticated synthetic page served by real uhttpd');
    if (phase === 'recovery') {
      await page.waitForFunction(() => document.getElementById('recovery').textContent.includes('unavailable'));
      check(await page.locator('#refresh').isEnabled() && await page.locator('#restart').isEnabled(), 'real outage leaves refresh and restart available');
      check(await page.locator('#save').isDisabled(), 'real outage blocks blind save');
      check(network.some(item => item.path.endsWith('/admin') && item.status === 503), 'actual daemon outage returned HTTP 503');
      check((await page.locator('table').innerText()).includes('Matrix federation'), 'catalog remains visible during real outage');
      const icon = await context.request.get(base + '/apps/arednlink-hf/icon.svg');
      check(icon.status() === 200, 'sidebar icon available while daemon is down');
      await page.locator('#restart').click();
      await page.waitForFunction(() => document.getElementById('recovery').textContent.includes('recovered'), null, {timeout: 40000});
      check(await page.locator('#save').isEnabled(), 'actual daemon recovery re-enables config save');
      check(JSON.parse(await page.locator('#draft').inputValue()).max_peers === 4, 'recovery loads saved editor settings');
    } else {
      await page.waitForFunction(() => document.getElementById('message').textContent === 'Complete.');
      check(await page.title() === 'AREDNlink-HF', 'page title rendered');
      check(await page.locator('h1').innerText() === 'AREDNlink-HF', 'native app heading rendered');
      check((await page.locator('#health').innerText()).includes('state'), 'daemon radio status rendered');
      check(await page.locator('#save').isEnabled(), 'owned config permits save');
      for (const summary of await page.locator('summary').all()) await summary.click();
      const running = JSON.parse(await page.locator('#running').textContent());
      const draft = JSON.parse(await page.locator('#draft').inputValue());
      if (phase === 'save') {
        check(running.max_peers === 2 && draft.max_peers === 2, 'initial running and saved config rendered');
        await page.locator('#draft').fill('{broken');
        await page.locator('#validate').click();
        await page.waitForFunction(() => document.getElementById('message').textContent === 'invalid_config');
        check(JSON.parse(await page.locator('#saved').textContent()).max_peers === 2, 'invalid draft preserved saved settings');
        draft.max_peers = 4;
        await page.locator('#draft').fill(JSON.stringify(draft, null, 2));
        await page.locator('#validate').click();
        await page.waitForFunction(() => document.getElementById('message').textContent === 'Complete.');
        checks.push('browser validation accepted valid JSON');
        await page.locator('#save').click();
        await page.waitForFunction(() => document.getElementById('pending').textContent.includes('pending a restart'));
        check(JSON.parse(await page.locator('#running').textContent()).max_peers === 2, 'save preserved running config');
        check(JSON.parse(await page.locator('#saved').textContent()).max_peers === 4, 'saved config rendered for next restart');
        const csrf = await page.locator('main').getAttribute('data-csrf');
        const foreign = await context.request.post(base + '/cgi-bin/apps/arednlink-hf/admin', {
          headers: {'Content-Type': 'text/plain;charset=UTF-8', Origin: 'http://foreign.invalid',
            'X-AREDNLink-CSRF': csrf, 'X-AREDNLink-Operation': 'status'}, data: ''});
        check(foreign.status() === 403, 'actual uhttpd foreign Origin POST refused');
        const badToken = await context.request.post(base + '/cgi-bin/apps/arednlink-hf/admin', {
          headers: {'Content-Type': 'text/plain;charset=UTF-8', Origin: base,
            'X-AREDNLink-CSRF': 'b'.repeat(64), 'X-AREDNLink-Operation': 'status'}, data: ''});
        check(badToken.status() === 403, 'actual uhttpd mismatched CSRF POST refused');
      } else {
        check(running.max_peers === 4 && draft.max_peers === 4, 'manual daemon restart loaded saved settings');
        check((await page.locator('#pending').innerText()).includes('No pending restart'), 'restart cleared pending state');
      }
      check((await page.locator('table').innerText()).includes('Signal proxy'), 'static advert service catalog rendered');
    }
    await page.screenshot({path: path.join(output, 'native-ui-' + phase + '.png'), fullPage: true});
    if (phase === 'save') {
      const icons = await context.newPage();
      await icons.setContent('<!doctype html><html><head><title>Sidebar SVG preview</title></head><body style="font:16px sans-serif;padding:24px"><h1>AREDNlink-HF sidebar SVG</h1><div style="display:flex;gap:32px;align-items:center;padding:24px;background:#fff;color:#111"><span>24 px <img width="24" height="24" src="' + base + '/apps/arednlink-hf/icon.svg"></span><span>32 px <img width="32" height="32" src="' + base + '/apps/arednlink-hf/icon.svg"></span></div><div style="display:flex;gap:32px;align-items:center;padding:24px;background:#222;color:#fff"><span>24 px <img width="24" height="24" src="' + base + '/apps/arednlink-hf/icon.svg"></span><span>32 px <img width="32" height="32" src="' + base + '/apps/arednlink-hf/icon.svg"></span></div></body></html>');
      await icons.locator('img').evaluateAll(images => Promise.all(images.map(image => image.decode())));
      check(await icons.locator('img').evaluateAll(images => images.every(image => image.complete && image.naturalWidth > 0)), 'actual SVG asset decoded at sidebar sizes');
      await icons.screenshot({path: path.join(output, 'sidebar-icon-preview.png'), fullPage: true});
      await icons.close();
    }
    check(network.some(item => item.path.endsWith('/ui.js') && item.status === 200), 'JavaScript asset loaded');
    check(network.some(item => item.path.endsWith('/ui.css') && item.status === 200), 'CSS asset loaded');
    // A deliberately invalid validation request returns JSON ok:false with HTTP 200.
    check(errors.length === 0, 'no browser console or page errors');
    fs.writeFileSync(path.join(output, 'browser-' + phase + '.json'), JSON.stringify({checks, network, errors, expectedOutageErrors}, null, 2));
    for (const label of checks) console.log(label + ': passed');
  } catch (error) {
    await page.screenshot({path: path.join(output, 'browser-failure.png'), fullPage: true}).catch(() => {});
    throw error;
  } finally {
    await context.close();
    fs.rmSync(profile, {recursive: true, force: true});
    fs.rmSync(cache, {recursive: true, force: true});
  }
})().catch(error => { console.error(error.stack); process.exitCode = 1; });
