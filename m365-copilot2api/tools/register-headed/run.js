#!/usr/bin/env node
'use strict';

const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const os = require('os');
const path = require('path');
const WebSocket = require('ws');

const SITE = process.env.REGISTER_SITE || 'https://office.965007.xyz';
const DISPLAY_NAME = process.env.REGISTER_DISPLAY || 'UserProbe';
const USERNAME = process.env.REGISTER_USER || '24s05probe9';
const PASSWORD = process.env.REGISTER_PASS || '***REMOVED-CREDENTIAL***';
const PLAN_ID = process.env.REGISTER_PLAN || '1';
const DOMAIN_ID = process.env.REGISTER_DOMAIN || '1';
const CHROME = process.env.CHROME || '/root/.cache/ms-playwright/chromium-1080/chrome-linux/chrome';
const OUT_DIR = process.env.REGISTER_OUT || '/tmp/register-headed';
const WAIT_MS = Number(process.env.REGISTER_WAIT_MS || 75000);
const SUBMIT = process.env.REGISTER_SUBMIT === '1';

fs.mkdirSync(OUT_DIR, { recursive: true });
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function httpGet(url) {
  return new Promise((resolve, reject) => {
    const req = http.get(url, (res) => {
      let body = '';
      res.on('data', (chunk) => { body += chunk; });
      res.on('end', () => resolve({ status: res.statusCode, body }));
    });
    req.on('error', reject);
    req.setTimeout(4000, () => req.destroy(new Error('timeout')));
  });
}

class CDP {
  constructor(ws) {
    this.ws = ws;
    this.id = 0;
    this.pending = new Map();
    this.onEvent = () => {};
    this.ws.on('message', (raw) => {
      let msg;
      try { msg = JSON.parse(raw.toString()); } catch (_) { return; }
      if (msg.method) this.onEvent(msg);
      if (!msg.id || !this.pending.has(msg.id)) return;
      const { resolve, reject } = this.pending.get(msg.id);
      this.pending.delete(msg.id);
      if (msg.error) reject(new Error(msg.error.message || JSON.stringify(msg.error)));
      else resolve(msg.result || {});
    });
  }

  send(method, params = {}, timeoutMs = 8000) {
    const id = ++this.id;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error('cdp timeout: ' + method));
      }, timeoutMs);
      this.pending.set(id, {
        resolve: (v) => { clearTimeout(timer); resolve(v); },
        reject: (e) => { clearTimeout(timer); reject(e); },
      });
      this.ws.send(JSON.stringify({ id, method, params }));
    });
  }
}

async function waitJSON(url, tries = 80) {
  let last = '';
  for (let i = 0; i < tries; i++) {
    try {
      const { status, body } = await httpGet(url);
      if (status === 200) return JSON.parse(body);
      last = 'status ' + status;
    } catch (err) {
      last = err.message;
    }
    await sleep(250);
  }
  throw new Error('not ready: ' + url + ' ' + last);
}

function pickDisplay() {
  for (let n = 170; n < 200; n++) {
    if (!fs.existsSync('/tmp/.X11-unix/X' + n)) return ':' + n;
  }
  return ':209';
}

function startXvfb(display) {
  return spawn('Xvfb', [display, '-screen', '0', '1280x2000x24', '-ac', '-nolisten', 'tcp'], {
    stdio: ['ignore', 'ignore', 'pipe'],
  });
}

function startChrome(display, userData, port) {
  const args = [
    `--remote-debugging-port=${port}`,
    `--user-data-dir=${userData}`,
    '--no-first-run',
    '--no-default-browser-check',
    '--disable-sync',
    '--disable-background-networking',
    '--disable-features=Translate,MediaRouter',
    '--disable-dev-shm-usage',
    '--no-sandbox',
    '--disable-setuid-sandbox',
    '--window-size=720,1800',
    '--window-position=0,0',
    'about:blank',
  ];
  const errFile = fs.openSync(path.join(OUT_DIR, 'chrome.err'), 'w');
  const proc = spawn(CHROME, args, {
    env: { ...process.env, DISPLAY: display },
    stdio: ['ignore', 'ignore', errFile],
  });
  proc.on('exit', (code, signal) => {
    try { fs.closeSync(errFile); } catch (_) {}
    fs.appendFileSync(path.join(OUT_DIR, 'chrome.err'), `\nchrome exit code=${code} signal=${signal}\n`);
  });
  return proc;
}

async function screenshot(cdp, file) {
  const { data } = await cdp.send('Page.captureScreenshot', { format: 'png' }, 20000);
  fs.writeFileSync(file, Buffer.from(data, 'base64'));
}

async function clickXY(cdp, x, y) {
  await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y });
  await sleep(50);
  await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x, y, button: 'left', clickCount: 1 });
  await sleep(40);
  await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x, y, button: 'left', clickCount: 1 });
}

const PAGE_SCRIPT = `(() => {
  if (window.__m365Headed) return;
  window.__m365Headed = 1;
  const displayName = ${JSON.stringify(DISPLAY_NAME)};
  const username = ${JSON.stringify(USERNAME)};
  const password = ${JSON.stringify(PASSWORD)};
  function say(kind, data) {
    try { console.log('M365|' + kind + '|' + JSON.stringify(data || {})); } catch (e) {}
  }
  function set(id, v) {
    const el = document.getElementById(id);
    if (!el) return false;
    if (el.value === v) return true;
    el.focus();
    el.value = v;
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.dispatchEvent(new Event('change', { bubbles: true }));
    return true;
  }
  function rectOf(el) {
    if (!el) return { x: 0, y: 0, w: 0, h: 0 };
    const r = el.getBoundingClientRect();
    return { x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height) };
  }
  function tokenValue() {
    const el = document.querySelector('input[name=cf-turnstile-response]');
    return (el && el.value || '').trim();
  }
  let filled = false;
  let laid = false;
  let last = '';
  function layout() {
    if (laid) return;
    const info = document.querySelector('.info-column');
    if (info) info.style.display = 'none';
    const shell = document.querySelector('.page-shell');
    if (shell) {
      shell.style.display = 'block';
      shell.style.gridTemplateColumns = '1fr';
      shell.style.width = '100%';
    }
    laid = true;
  }
  function tick() {
    const user = document.getElementById('username');
    const box = document.getElementById('turnstileBox');
    if (!user) return;
    layout();
    if (!filled) {
      set('displayName', displayName);
      set('username', username);
      set('password', password);
      filled = true;
      const domain = ((document.getElementById('emailDomain') || {}).textContent || '').trim();
      say('filled', { display: displayName, user: username, domain: domain });
    }
    if (!box) return;
    if (!window.__m365Scrolled && box.offsetHeight > 0) {
      box.scrollIntoView({ block: 'center' });
      window.__m365Scrolled = 1;
    }
    const iframe = box.querySelector('iframe') || document.querySelector('iframe[src*="challenges.cloudflare.com"]');
    const target = iframe || box;
    const rect = rectOf(target);
    const token = tokenValue();
    const state = JSON.stringify({
      filled: filled,
      tokenLen: token.length,
      iframes: document.querySelectorAll('iframe').length,
      hidden: box.classList.contains('hidden'),
      rect: rect,
    });
    if (token.length > 20) {
      say('token', { len: token.length, token: token, rect: rect });
      return;
    }
    if (state !== last) {
      last = state;
      say('widget', {
        filled: filled,
        domain: domain,
        iframes: document.querySelectorAll('iframe').length,
        hidden: box.classList.contains('hidden'),
        rect: rect,
        clickX: Math.floor(rect.x + 26),
        clickY: Math.floor(rect.y + Math.max(20, rect.h / 2)),
      });
    }
  }
  say('boot', { href: location.href });
  setInterval(tick, 800);
  if (document.readyState !== 'loading') tick();
  else document.addEventListener('DOMContentLoaded', tick);
})();`;

async function main() {
  if (!fs.existsSync(CHROME)) throw new Error('chrome missing: ' + CHROME);
  const userData = fs.mkdtempSync(path.join(os.tmpdir(), 'headed-chrome-'));
  const port = 9222 + Math.floor(Math.random() * 300);
  const display = pickDisplay();
  const xvfb = startXvfb(display);
  await sleep(800);
  const chrome = startChrome(display, userData, port);
  const log = [];
  const note = (msg) => {
    const line = `[${new Date().toISOString()}] ${msg}`;
    log.push(line);
    console.log(line);
  };
  const cleanup = () => {
    try { chrome.kill('SIGKILL'); } catch (_) {}
    try { xvfb.kill('SIGKILL'); } catch (_) {}
  };

  try {
    note(`xvfb ${display} chrome :${port}`);
    const version = await waitJSON(`http://127.0.0.1:${port}/json/version`);
    note('browser ' + (version.Browser || ''));
    let page = null;
    for (let i = 0; i < 40; i++) {
      const list = await waitJSON(`http://127.0.0.1:${port}/json/list`);
      page = (list || []).find((p) => p.type === 'page' && p.webSocketDebuggerUrl);
      if (page) break;
      await sleep(250);
    }
    if (!page) throw new Error('no page websocket');
    note('blank ' + page.url);
    const ws = new WebSocket(page.webSocketDebuggerUrl);
    await new Promise((resolve, reject) => {
      ws.once('open', resolve);
      ws.once('error', reject);
    });
    const cdp = new CDP(ws);
    const events = [];
    cdp.onEvent = (msg) => {
      if (msg.method !== 'Runtime.consoleAPICalled') return;
      const args = (msg.params && msg.params.args) || [];
      const text = args.map((a) => a.value || a.description || '').join(' ');
      if (!text.startsWith('M365|')) return;
      const parts = text.split('|');
      const kind = parts[1];
      let data = {};
      try { data = JSON.parse(parts.slice(2).join('|')); } catch (_) {}
      events.push({ kind, data, at: Date.now() });
      note('page ' + kind + ' ' + JSON.stringify(data).slice(0, 240));
    };
    await cdp.send('Runtime.enable');
    await cdp.send('Page.enable');
    await cdp.send('Page.addScriptToEvaluateOnNewDocument', { source: PAGE_SCRIPT });
    await cdp.send('Page.navigate', { url: SITE + '/' });
    note('navigated ' + SITE);

    const started = Date.now();
    let token = '';
    let lastClick = 0;
    let clicks = 0;
    let filledShot = false;
    const deadline = Date.now() + WAIT_MS;
    while (Date.now() < deadline) {
      const lastWidget = [...events].reverse().find((e) => e.kind === 'widget');
      const lastToken = [...events].reverse().find((e) => e.kind === 'token');
      const lastFilled = [...events].reverse().find((e) => e.kind === 'filled');
      if (lastToken && lastToken.data.token) {
        token = lastToken.data.token;
        note('token len=' + token.length);
        break;
      }
      if (lastFilled && !filledShot) {
        filledShot = true;
        try { await screenshot(cdp, path.join(OUT_DIR, '01-filled.png')); } catch (err) { note('shot filled ' + err.message); }
      }
      if (lastWidget && lastWidget.data.rect && lastWidget.data.rect.h >= 50) {
        const x = lastWidget.data.clickX;
        const y = lastWidget.data.clickY;
        const inView = y > 8 && y < 1780;
        const age = Date.now() - started;
        if (inView && clicks < 5 && Date.now() - lastClick > 8000 && age > 2500) {
          note('click ' + x + ',' + y + ' size=' + lastWidget.data.rect.w + 'x' + lastWidget.data.rect.h);
          await clickXY(cdp, x, y);
          clicks += 1;
          lastClick = Date.now();
          try { await screenshot(cdp, path.join(OUT_DIR, 'click-' + clicks + '.png')); } catch (err) { note('shot click ' + err.message); }
        } else if (!inView && age > 2500 && Date.now() - lastClick > 4000) {
          note('skip offscreen click ' + x + ',' + y + ' rect=' + JSON.stringify(lastWidget.data.rect));
          lastClick = Date.now();
        }
      }
      await sleep(500);
    }

    try { await screenshot(cdp, path.join(OUT_DIR, token ? '02-token.png' : '02-stuck.png')); } catch (err) { note('shot end ' + err.message); }
    const last = [...events].reverse().find((e) => e.kind === 'widget' || e.kind === 'token' || e.kind === 'filled') || { data: {} };
    const report = {
      ok: !!token,
      ms: Date.now() - started,
      display: DISPLAY_NAME,
      user: USERNAME,
      domain: (events.find((e) => e.kind === 'filled') || { data: {} }).data.domain || '',
      tokenLen: token.length,
      tokenHead: token ? token.slice(0, 24) : '',
      iframeCount: last.data.iframes,
      rect: last.data.rect,
      clicks,
      out: OUT_DIR,
    };
    if (token && SUBMIT) {
      note('submit skipped in page context; posting from node');
      const payload = JSON.stringify({
        planId: PLAN_ID, domainId: DOMAIN_ID, inviteCode: '',
        displayName: DISPLAY_NAME, username: USERNAME, password: PASSWORD,
        verificationEmail: '', emailCode: '', turnstileToken: token,
      });
      const posted = await new Promise((resolve, reject) => {
        const url = new URL(SITE + '/api/register');
        const lib = url.protocol === 'https:' ? require('https') : http;
        const req = lib.request({
          hostname: url.hostname,
          path: url.pathname,
          method: 'POST',
          headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' },
        }, (res) => {
          let body = '';
          res.on('data', (c) => { body += c; });
          res.on('end', () => resolve({ status: res.statusCode, body }));
        });
        req.on('error', reject);
        req.write(payload);
        req.end();
      });
      report.submit = posted;
      note('submit ' + JSON.stringify(posted).slice(0, 400));
    }
    fs.writeFileSync(path.join(OUT_DIR, 'report.json'), JSON.stringify(report, null, 2));
    fs.writeFileSync(path.join(OUT_DIR, 'log.txt'), log.join('\n') + '\n');
    console.log(JSON.stringify(report, null, 2));
    cleanup();
    process.exit(token ? 0 : 2);
  } catch (err) {
    note('error ' + (err && err.stack || err));
    fs.writeFileSync(path.join(OUT_DIR, 'log.txt'), log.join('\n') + '\n');
    cleanup();
    throw err;
  }
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
