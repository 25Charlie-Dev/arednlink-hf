'use strict';
const element = id => document.getElementById(id);
const token = document.querySelector('main').dataset.csrf;
const DEADLINE_MS = 6500;
const POLL_MS = 1000;
const RECOVERY_MS = 30000;
let busy = false;
let daemonReady = false;
let saveAllowed = false;
let draftLoaded = false;
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
async function request(operation, body = '', limitMs = DEADLINE_MS) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), limitMs);
  try {
    const response = await fetch('/cgi-bin/apps/arednlink-hf/admin', {
      method: 'POST', credentials: 'same-origin', redirect: 'error', signal: controller.signal,
      headers: {'Content-Type': 'text/plain;charset=UTF-8',
        'X-AREDNLink-CSRF': token, 'X-AREDNLink-Operation': operation}, body});
    const result = await response.json();
    if (!response.ok || !result.ok) throw new Error(result.error || 'Request failed');
    return result.result;
  } catch (error) {
    if (error.name === 'AbortError') throw new Error('Request timed out');
    throw error;
  } finally { clearTimeout(timeout); }
}
function controls() {
  element('refresh').disabled = busy;
  element('restart').disabled = busy;
  element('validate').disabled = busy || !daemonReady;
  element('save').disabled = busy || !daemonReady || !saveAllowed;
}
function unavailable(error) {
  daemonReady = false;
  saveAllowed = false;
  element('health').textContent = 'Daemon unavailable.';
  element('peers').textContent = 'Discovery unavailable.';
  element('pending').textContent = 'Settings unavailable. Save disabled until the daemon responds.';
  element('recovery').textContent = 'Service unavailable. Refresh status or restart AREDNlink-HF.';
  element('message').textContent = error.message;
  controls();
}
async function status(limitMs = DEADLINE_MS) {
  const result = await request('status', '', limitMs);
  daemonReady = true;
  element('health').textContent = JSON.stringify(result.health, null, 2);
  element('peers').textContent = JSON.stringify(result.peers, null, 2);
  element('recovery').textContent = 'Service responding.';
  controls();
}
async function configuration(loadDraft, limitMs = DEADLINE_MS) {
  const result = await request('config.get', '', limitMs);
  element('running').textContent = result.running;
  element('saved').textContent = result.saved;
  if (loadDraft || !draftLoaded) {
    element('draft').value = result.saved || result.running;
    draftLoaded = true;
  }
  element('pending').textContent = result.pending ? 'Saved settings are pending a restart.' : 'No pending restart.';
  saveAllowed = !result.save_disabled;
  if (result.save_disabled) element('pending').textContent += ' Saving disabled: ' + result.save_disabled;
  controls();
}
async function action(callback, loadDraft = false) {
  if (busy) return;
  busy = true;
  controls();
  element('message').textContent = 'Working…';
  try {
    await callback();
    await configuration(loadDraft);
    element('message').textContent = 'Complete.';
  } catch (error) {
    if (error.message === 'invalid_config') element('message').textContent = error.message;
    else unavailable(error);
  }
  finally { busy = false; controls(); }
}
async function recover() {
  if (busy) return;
  busy = true;
  controls();
  element('message').textContent = 'Restart requested…';
  element('recovery').textContent = 'Requesting service restart.';
  daemonReady = false;
  saveAllowed = false;
  controls();
  try {
    await request('service.restart');
    element('recovery').textContent = 'Restart requested. Waiting for service response…';
    const deadline = performance.now() + RECOVERY_MS;
    while (performance.now() < deadline) {
      try {
        await status(Math.min(DEADLINE_MS, Math.max(1, deadline - performance.now())));
        if (performance.now() >= deadline) break;
        await configuration(false, Math.min(DEADLINE_MS, Math.max(1, deadline - performance.now())));
        element('message').textContent = 'Service recovered.';
        element('recovery').textContent = 'Service recovered and responding.';
        return;
      } catch (_) {
        daemonReady = false;
        saveAllowed = false;
        controls();
      }
      await delay(Math.min(POLL_MS, Math.max(0, deadline - performance.now())));
    }
    throw new Error('Restart requested, but service did not recover within 30 seconds.');
  } catch (error) {
    unavailable(error);
    element('recovery').textContent = error.message === 'restart_failed'
      ? 'Restart request failed. Refresh status or try again.'
      : 'Service unavailable. Refresh status or try again.';
  } finally { busy = false; controls(); }
}
element('refresh').addEventListener('click', () => action(status));
element('restart').addEventListener('click', recover);
element('validate').addEventListener('click', () => action(() => request('config.validate', element('draft').value)));
element('save').addEventListener('click', () => action(async () => {
  await request('config.validate', element('draft').value);
  await request('config.save', element('draft').value);
}));
action(status, true);
