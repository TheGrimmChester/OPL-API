#!/usr/bin/env node
/**
 * Optional Chromium journey runner for Synthetics depth browser checks.
 * Set OPA_BROWSER_RUNNER to this script. Reads JSON on stdin, writes JSON on stdout.
 *
 * With playwright: real goto/click/fill + screenshot_b64 + short DOM on failure.
 * Without playwright: HTTP GET for "goto" steps only (graceful degrade).
 */
'use strict';

const http = require('http');
const https = require('https');
const { URL } = require('url');

async function readStdin() {
  const chunks = [];
  for await (const c of process.stdin) chunks.push(c);
  return Buffer.concat(chunks).toString('utf8');
}

function httpGet(url, headers, timeoutMs) {
  return new Promise((resolve) => {
    try {
      const u = new URL(url);
      const lib = u.protocol === 'https:' ? https : http;
      const start = Date.now();
      const req = lib.request({
        hostname: u.hostname, port: u.port || (u.protocol === 'https:' ? 443 : 80),
        path: u.pathname + u.search, method: 'GET', headers, timeout: timeoutMs
      }, (res) => {
        const bufs = [];
        res.on('data', (d) => bufs.push(d));
        res.on('end', () => resolve({
          status: res.statusCode, body: Buffer.concat(bufs).toString('utf8'),
          latency_ms: Date.now() - start
        }));
      });
      req.on('error', (e) => resolve({ error: e.message, latency_ms: Date.now() - start }));
      req.on('timeout', () => { req.destroy(); resolve({ error: 'timeout', latency_ms: Date.now() - start }); });
      req.end();
    } catch (e) {
      resolve({ error: e.message, latency_ms: 0 });
    }
  });
}

async function tryLoadPlaywright() {
  try {
    return require('playwright');
  } catch (_) {
    try {
      const pw = await import('playwright');
      return pw.default || pw;
    } catch (_) {
      return null;
    }
  }
}

async function runWithPlaywright(pw, input, steps) {
  const outSteps = [];
  let ok = 1;
  let err = '';
  let screenshot_b64 = '';
  let dom_snapshot = '';
  const t0 = Date.now();
  const browser = await pw.chromium.launch({ headless: true });
  const page = await browser.newPage();
  try {
    for (let i = 0; i < steps.length; i++) {
      const st = steps[i] || {};
      const action = String(st.action || 'goto').toLowerCase();
      const name = st.name || `${action}_${i + 1}`;
      const step = { name, action, ok: 0 };
      const start = Date.now();
      try {
        if (action === 'goto' || action === 'navigate' || action === 'get') {
          const url = st.url || input.url;
          step.url = url;
          const tp = `00-${input.trace_id || '0'.repeat(32)}-${'1'.repeat(16)}-01`;
          await page.setExtraHTTPHeaders({ traceparent: tp });
          const resp = await page.goto(url, { waitUntil: 'domcontentloaded', timeout: input.timeout_ms || 20000 });
          step.status_code = resp ? resp.status() : 0;
          const want = st.assert_status || 0;
          if (want && step.status_code !== want) throw new Error(`status ${step.status_code}`);
          if (!want && (step.status_code < 200 || step.status_code > 399)) throw new Error(`status ${step.status_code}`);
          const needle = st.assert_body_contains || st.assert_text || st.value || '';
          if (needle) {
            const body = await page.content();
            if (!body.includes(needle)) throw new Error('assert_text failed');
          }
        } else if (action === 'click') {
          await page.click(st.selector || st.target, { timeout: input.timeout_ms || 10000 });
        } else if (action === 'fill' || action === 'type') {
          await page.fill(st.selector || st.target, String(st.value || ''), { timeout: input.timeout_ms || 10000 });
        } else if (action === 'wait' || action === 'wait_for') {
          if (st.selector) await page.waitForSelector(st.selector, { timeout: input.timeout_ms || 10000 });
          else await page.waitForTimeout(Number(st.ms || st.value || 500));
        } else if (action === 'screenshot') {
          const buf = await page.screenshot({ type: 'png', fullPage: false });
          screenshot_b64 = buf.toString('base64');
          step.note = 'screenshot captured';
        } else {
          step.note = `unknown action ${action}`;
        }
        step.ok = 1;
        step.latency_ms = Date.now() - start;
        outSteps.push(step);
      } catch (e) {
        step.error = e.message || String(e);
        step.latency_ms = Date.now() - start;
        ok = 0;
        err = `${name}: ${step.error}`;
        outSteps.push(step);
        try {
          const buf = await page.screenshot({ type: 'png', fullPage: false });
          screenshot_b64 = buf.toString('base64');
        } catch (_) { /* ignore */ }
        try {
          const html = await page.content();
          dom_snapshot = String(html || '').slice(0, 8000);
        } catch (_) { /* ignore */ }
        break;
      }
    }
  } finally {
    await browser.close().catch(() => {});
  }
  return {
    ok, error: err, latency_ms: Date.now() - t0, steps: outSteps,
    trace_id: input.trace_id || '', screenshot_b64, dom_snapshot,
    runner: 'playwright'
  };
}

async function runHttpFallback(input, steps) {
  const outSteps = [];
  let ok = 1;
  let err = '';
  const t0 = Date.now();
  for (let i = 0; i < steps.length; i++) {
    const st = steps[i] || {};
    const action = String(st.action || 'goto').toLowerCase();
    const name = st.name || `${action}_${i + 1}`;
    if (action === 'goto' || action === 'navigate' || action === 'get') {
      const url = st.url || input.url;
      const tp = `00-${input.trace_id || '0'.repeat(32)}-${'1'.repeat(16)}-01`;
      const r = await httpGet(url, { traceparent: tp, 'user-agent': 'OPA-Browser-Runner/1.0' }, input.timeout_ms || 20000);
      const step = { name, action, url, latency_ms: r.latency_ms, status_code: r.status || 0, ok: 0 };
      if (r.error) { step.error = r.error; ok = 0; err = `${name}: ${r.error}`; outSteps.push(step); break; }
      const want = st.assert_status || 0;
      if (want && r.status !== want) { step.error = `status ${r.status}`; ok = 0; err = step.error; outSteps.push(step); break; }
      if (!want && (r.status < 200 || r.status > 399)) { step.error = `status ${r.status}`; ok = 0; err = step.error; outSteps.push(step); break; }
      const needle = st.assert_body_contains || st.value || '';
      if (needle && !(r.body || '').includes(needle)) { step.error = 'assert_text failed'; ok = 0; err = step.error; outSteps.push(step); break; }
      step.ok = 1; outSteps.push(step);
    } else {
      outSteps.push({ name, action, ok: 1, note: 'install playwright for real browser actions (screenshot on fail)' });
    }
  }
  return {
    ok, error: err, latency_ms: Date.now() - t0, steps: outSteps,
    trace_id: input.trace_id || '', screenshot_b64: '', dom_snapshot: '',
    runner: 'http-fallback'
  };
}

(async () => {
  let input;
  try { input = JSON.parse(await readStdin() || '{}'); }
  catch (e) {
    process.stdout.write(JSON.stringify({ ok: 0, error: 'bad input json' }));
    process.exit(0);
  }
  let steps = [];
  try { steps = typeof input.steps === 'string' ? JSON.parse(input.steps || '[]') : (input.steps || []); }
  catch (_) { steps = []; }
  if (!steps.length && input.url) steps = [{ name: 'goto', action: 'goto', url: input.url }];

  const pw = await tryLoadPlaywright();
  let result;
  if (pw) {
    try {
      result = await runWithPlaywright(pw, input, steps);
    } catch (e) {
      result = await runHttpFallback(input, steps);
      result.error = (result.error ? result.error + '; ' : '') + 'playwright failed: ' + (e.message || e);
      result.ok = 0;
    }
  } else {
    result = await runHttpFallback(input, steps);
  }
  process.stdout.write(JSON.stringify(result));
})();
