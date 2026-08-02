#!/usr/bin/env node
/**
 * Perf lab / JMeter — concurrent HTTP VU runner with multi-step scenarios (extract/assert/CSV/think).
 * Primary production engine is Apache JMeter (scripts/jmeter-run.sh); this is the Node fallback.
 *
 * Usage:
 *   node scripts/load-runner.mjs --scenario scenario.json [--agent URL] [--run-id RID] [--profile soak|spike|ramp]
 */
import fs from 'fs';
import crypto from 'crypto';
import http from 'http';
import https from 'https';
import { URL } from 'url';

function arg(name, def = '') {
  const i = process.argv.indexOf(name);
  if (i < 0) return def;
  return process.argv[i + 1] || def;
}

function hex(n) {
  return crypto.randomBytes(n).toString('hex');
}

function requestOnce(method, urlStr, headers, body, timeoutMs) {
  return new Promise((resolve) => {
    const start = Date.now();
    try {
      const u = new URL(urlStr);
      const lib = u.protocol === 'https:' ? https : http;
      const req = lib.request({
        hostname: u.hostname,
        port: u.port || (u.protocol === 'https:' ? 443 : 80),
        path: u.pathname + u.search,
        method: method || 'GET',
        headers,
        timeout: timeoutMs || 15000,
      }, (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () => resolve({
          latency_ms: Date.now() - start,
          status_code: res.statusCode,
          ok: res.statusCode >= 200 && res.statusCode < 300,
          url: urlStr,
          body: Buffer.concat(chunks).toString('utf8').slice(0, 1 << 20),
        }));
      });
      req.on('error', () => resolve({ latency_ms: Date.now() - start, status_code: 0, ok: false, url: urlStr, body: '' }));
      req.on('timeout', () => {
        req.destroy();
        resolve({ latency_ms: Date.now() - start, status_code: 0, ok: false, url: urlStr, body: '' });
      });
      if (body) req.write(body);
      req.end();
    } catch {
      resolve({ latency_ms: Date.now() - start, status_code: 0, ok: false, url: urlStr, body: '' });
    }
  });
}

function metricsHeaders() {
  const headers = { 'content-type': 'application/json' };
  const tok = String(process.env.OPA_PERF_RUNNER_TOKEN || '').trim();
  if (tok) {
    headers['X-OPA-Perf-Runner-Token'] = tok;
    headers.Authorization = `Bearer ${tok}`;
  }
  return headers;
}

function percentile(sorted, p) {
  if (!sorted.length) return 0;
  const idx = Math.min(sorted.length - 1, Math.ceil((p / 100) * sorted.length) - 1);
  return sorted[idx];
}

function applyProfile(scenario, profile) {
  const p = String(profile || '').toLowerCase();
  const out = { ...scenario };
  if (p === 'soak') {
    out.vus = Number(out.vus || 5);
    out.duration_seconds = Number(out.duration_seconds || 300);
    out.think_ms = Number(out.think_ms || 100);
  } else if (p === 'spike') {
    out.vus = Number(out.vus || 100);
    out.duration_seconds = Number(out.duration_seconds || 30);
    out.think_ms = Number(out.think_ms || 10);
  } else if (p === 'ramp') {
    out.vus = Number(out.vus || 20);
    out.duration_seconds = Number(out.duration_seconds || 120);
    out.think_ms = Number(out.think_ms || 50);
    out.ramp = true;
  }
  out._profile = p || 'steady';
  return out;
}

function activeVUs(profile, baseVUs, elapsedMs, durationMs) {
  if (profile !== 'ramp') return baseVUs;
  const half = Math.max(1, durationMs * 0.5);
  const frac = Math.min(1, elapsedMs / half);
  return Math.max(1, Math.ceil(baseVUs * frac));
}

function expand(s, vars) {
  let out = String(s ?? '');
  for (const [k, v] of Object.entries(vars)) {
    if (k.startsWith('_')) continue;
    out = out.split('${' + k + '}').join(v);
    out = out.split('{{' + k + '}}').join(v);
  }
  return out;
}

function loadCsvRows(scenario) {
  const csv = scenario.datasets?.csv;
  if (!csv) return [];
  // Agent-dispatched runs: only inline CSV (ignore filename to avoid host file reads).
  const text = csv.inline || '';
  if (!text) return [];
  const lines = text.trim().split(/\r?\n/).filter(Boolean);
  const delim = csv.delimiter || ',';
  const names = (csv.variableNames || '').split(',').map((s) => s.trim()).filter(Boolean);
  return lines.map((line) => {
    const cols = line.split(delim);
    const row = {};
    names.forEach((n, i) => { row[n] = cols[i] ?? ''; });
    return row;
  });
}

function jsonPath(body, path) {
  path = String(path || '').replace(/^\$\./, '');
  try {
    let cur = JSON.parse(body);
    for (const part of path.split('.')) {
      if (!part) continue;
      cur = cur?.[part];
    }
    if (cur == null) return '';
    return typeof cur === 'string' ? cur : JSON.stringify(cur);
  } catch {
    return '';
  }
}

async function runSteps(steps, baseHeaders, vars, samples, stepStats) {
  let lastOk = true;
  for (const step of steps) {
    const type = step.type || 'http';
    if (type === 'extract') {
      const body = vars._body || '';
      let val = '';
      if (step.engine === 'jsonpath' || String(step.expression || '').startsWith('$.')) {
        val = jsonPath(body, step.expression);
      } else if (step.expression) {
        const m = body.match(new RegExp(step.expression));
        val = m ? (m[1] ?? m[0]) : '';
      }
      if (step.var) vars[step.var] = val;
      continue;
    }
    if (type === 'assert') {
      if (step.status != null && String(vars._status) !== String(step.status)) lastOk = false;
      if (step.body_contains && !(vars._body || '').includes(step.body_contains)) lastOk = false;
      if (!lastOk) {
        samples.push({ latency_ms: 0, status_code: Number(vars._status) || 0, ok: false, url: 'assert:' + (step.name || ''), step_name: step.name });
      }
      continue;
    }
    if (type === 'transaction') continue;

    const url = expand(step.url, vars);
    const method = (step.method || 'GET').toUpperCase();
    const body = expand(step.body || '', vars);
    const headers = Object.assign({}, baseHeaders, step.headers || {});
    for (const [k, v] of Object.entries(headers)) headers[k] = expand(v, vars);
    const s = await requestOnce(method, url, headers, body || null, 15000);
    vars._body = s.body || '';
    vars._status = String(s.status_code);
    const sample = { latency_ms: s.latency_ms, status_code: s.status_code, ok: s.ok && lastOk, url, step_name: step.name || url };
    samples.push(sample);
    const key = step.name || url;
    if (!stepStats[key]) stepStats[key] = { n: 0, err: 0, lats: [] };
    stepStats[key].n += 1;
    if (!sample.ok) stepStats[key].err += 1;
    stepStats[key].lats.push(s.latency_ms);
    lastOk = sample.ok;
    const think = Number(step.think_ms || 0);
    if (think > 0) await new Promise((r) => setTimeout(r, think));
  }
}

(async () => {
  const scenarioPath = arg('--scenario', arg('-s'));
  if (!scenarioPath) {
    console.error('Usage: node scripts/load-runner.mjs --scenario file.json [--agent URL] [--run-id ID] [--profile soak|spike|ramp]');
    process.exit(2);
  }
  const profile = arg('--profile', process.env.OPA_LOAD_PROFILE || '');
  let scenario = applyProfile(JSON.parse(fs.readFileSync(scenarioPath, 'utf8')), profile);
  const agent = arg('--agent', process.env.OPA_AGENT_URL || '');
  let runId = arg('--run-id', process.env.OPA_LOAD_RUN_ID || '');
  const vus = Number(scenario.vus || 10);
  const durationMs = Number(scenario.duration_seconds || 60) * 1000;
  const steps = Array.isArray(scenario.steps) && scenario.steps.length
    ? scenario.steps
    : [{ type: 'http', name: 'main', method: scenario.method || 'GET', url: scenario.target_url || scenario.url, body: scenario.body || '', think_ms: scenario.think_ms || 50 }];
  if (!steps.some((s) => (s.type || 'http') === 'http')) {
    console.error('scenario needs at least one http step or target_url');
    process.exit(2);
  }

  if (agent && !runId) {
    try {
      const create = await fetch(`${agent.replace(/\/$/, '')}/api/perf/runs`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ scenario_id: scenario.id || '', vus, profile: scenario._profile || profile, engine: 'node' }),
      });
      const j = await create.json();
      runId = j.load_run_id || j.id;
    } catch (e) {
      console.error('failed to create run', e.message);
    }
  }

  const csvRows = loadCsvRows(scenario);
  const samples = [];
  const stepStats = {};
  const endAt = Date.now() + durationMs;
  const startAt = Date.now();
  let assertFailed = false;
  const workers = [];
  for (let i = 0; i < vus; i++) {
    const workerIndex = i;
    workers.push((async () => {
      let csvIdx = workerIndex;
      while (Date.now() < endAt) {
        const elapsed = Date.now() - startAt;
        if (workerIndex >= activeVUs(scenario._profile, vus, elapsed, durationMs)) {
          await new Promise((r) => setTimeout(r, 100));
          continue;
        }
        const vars = {};
        if (csvRows.length) {
          Object.assign(vars, csvRows[csvIdx % csvRows.length]);
          csvIdx += vus;
        }
        const tid = hex(16);
        const sid = hex(8);
        const headers = Object.assign({}, scenario.headers || {}, {
          traceparent: `00-${tid}-${sid}-01`,
          'X-OPA-Load-Run-Id': runId || '',
          baggage: runId ? `load_run_id=${runId}` : '',
        });
        const before = samples.length;
        await runSteps(steps, headers, vars, samples, stepStats);
        const slice = samples.slice(before);
        if (slice.some((s) => !s.ok && String(s.url).startsWith('assert:'))) assertFailed = true;

        // Partial metrics every ~50 samples when agent set
        if (agent && runId && samples.length % 50 < vus) {
          const lats = samples.map((s) => s.latency_ms).sort((a, b) => a - b);
          const errors = samples.filter((s) => !s.ok).length;
          const partial = {
            requests: samples.length,
            error_rate: samples.length ? errors / samples.length : 0,
            p50_ms: percentile(lats, 50),
            p95_ms: percentile(lats, 95),
            partial: true,
          };
          fetch(`${agent.replace(/\/$/, '')}/api/perf/runs/${encodeURIComponent(runId)}/metrics`, {
            method: 'POST',
            headers: metricsHeaders(),
            body: JSON.stringify({
              scenario_id: scenario.id || '',
              status: 'running',
              vus,
              summary: partial,
              samples: samples.slice(-20),
            }),
          }).catch(() => {});
        }

        await new Promise((r) => setTimeout(r, Number(scenario.think_ms || 10)));
      }
    })());
  }
  await Promise.all(workers);

  const lats = samples.map((s) => s.latency_ms).sort((a, b) => a - b);
  const errors = samples.filter((s) => !s.ok).length;
  const per_step = {};
  for (const [k, v] of Object.entries(stepStats)) {
    const sl = v.lats.slice().sort((a, b) => a - b);
    per_step[k] = { requests: v.n, error_rate: v.n ? v.err / v.n : 0, p95_ms: percentile(sl, 95) };
  }
  const summary = {
    requests: samples.length,
    error_rate: samples.length ? errors / samples.length : 0,
    p50_ms: percentile(lats, 50),
    p95_ms: percentile(lats, 95),
    p99_ms: percentile(lats, 99),
    load_run_id: runId,
    profile: scenario._profile || 'steady',
    per_step,
    engine: 'node',
  };

  let status = 'completed';
  const sla = scenario.sla || scenario.thresholds || {};
  const slaKeys = sla.p95_ms != null || sla.error_rate_max != null || sla.rps_min != null
    || Object.keys(sla).length > 0;
  if (summary.requests === 0) {
    status = 'failed';
  } else {
    if (sla.p95_ms != null && (summary.p95_ms == null || summary.p95_ms > Number(sla.p95_ms))) status = 'failed';
    if (sla.error_rate_max != null && (summary.error_rate == null || summary.error_rate > Number(sla.error_rate_max))) status = 'failed';
    if (sla.rps_min != null && (summary.rps == null || summary.rps < Number(sla.rps_min))) status = 'failed';
    if (assertFailed) status = 'failed';
    if (status !== 'failed' && slaKeys) status = 'passed';
  }

  console.log(JSON.stringify({ ...summary, status }));

  if (agent && runId) {
    const url = `${agent.replace(/\/$/, '')}/api/perf/runs/${encodeURIComponent(runId)}/metrics`;
    await fetch(url, {
      method: 'POST',
      headers: metricsHeaders(),
      body: JSON.stringify({
        scenario_id: scenario.id || '',
        status,
        vus,
        summary,
        samples: samples.slice(0, 200),
      }),
    }).catch((e) => console.error('metrics post failed', e.message));
  }
  if (status === 'failed') process.exit(1);
})();
