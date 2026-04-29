package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
	"www.bamsoftware.com/git/champa.git/noise"
	"www.bamsoftware.com/git/champa.git/turbotunnel"
)

const (
	// idleTimeout must be comfortably longer than quotaBackoffMax so that the
	// smux session survives the entire slow-mode period without being torn
	// down by a keep-alive timeout. Previous value of 2m caused session drops
	// whenever slow mode extended beyond 2 minutes.
	idleTimeout = 25 * time.Minute

	// keepAliveInterval controls how often smux sends keep-alive frames.
	// Set low enough that the server does not close an idle session, but not
	// so low that keep-alives waste quota during slow mode.
	keepAliveInterval = 20 * time.Second

	reconnectDelay = 5 * time.Second

	// Local address for the HTTP status/monitoring endpoint. "" = disabled.
	statusAddr = "127.0.0.1:7001"
)

// dashboardHTML is the compiled-in session monitor page served at /status.
// It polls /status.json every second and renders live connection metrics.
const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Champa Session Monitor</title>
<style>
*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }

body {
  background: #0d1117;
  color: #c9d1d9;
  font-family: 'Courier New', Consolas, monospace;
  font-size: 13px;
  padding: 16px;
  min-height: 100vh;
}

/* ── Header ── */
.header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 14px;
  padding-bottom: 10px;
  border-bottom: 1px solid #21262d;
}
.header-title { font-size: 14px; color: #58a6ff; letter-spacing: 0.05em; }
.header-right { display: flex; align-items: center; gap: 10px; }
.uptime { font-size: 12px; color: #8b949e; }

/* ── Status badge ── */
.status-badge {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 3px 10px;
  border-radius: 12px;
  font-size: 11px;
  font-weight: bold;
}
.badge-ok  { background: #1b3a24; color: #3fb950; border: 1px solid #2ea043; }
.badge-warn{ background: #3d2b00; color: #e3b341; border: 1px solid #9e6a03; }
.badge-slow{ background: #3d0000; color: #f85149; border: 1px solid #da3633; }
.badge-off { background: #1c2128; color: #6e7681; border: 1px solid #30363d; }

.pulse {
  width: 8px; height: 8px; border-radius: 50%;
  animation: pulse 1.6s ease-in-out infinite;
}
.pulse.green { background: #3fb950; }
.pulse.yellow{ background: #e3b341; }
.pulse.red   { background: #f85149; animation-duration: 0.7s; }
.pulse.gray  { background: #6e7681; animation: none; }
@keyframes pulse { 0%,100%{opacity:1} 50%{opacity:0.35} }

/* ── Cache strip ── */
.cache-strip {
  display: flex; gap: 6px; margin-bottom: 12px; flex-wrap: wrap;
}
.cache-pill {
  font-size: 10px; padding: 2px 9px; border-radius: 10px;
  border: 1px solid #30363d; color: #6e7681;
  transition: border-color 0.4s, color 0.4s;
}
.cache-pill.active  { border-color: #2ea043; color: #3fb950; }
.cache-pill.current { border-color: #58a6ff; color: #58a6ff; }
.cache-pill.error   { border-color: #da3633; color: #f85149; }

/* ── Metric grid ── */
.grid-4 {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 8px;
  margin-bottom: 10px;
}
@media(max-width:700px){ .grid-4 { grid-template-columns: repeat(2, 1fr); } }

.metric {
  background: #161b22;
  border: 1px solid #21262d;
  border-radius: 8px;
  padding: 10px 12px;
}
.metric-label { font-size: 10px; color: #6e7681; margin-bottom: 4px; }
.metric-value { font-size: 22px; font-weight: bold; line-height: 1; }
.metric-sub   { font-size: 10px; color: #484f58; margin-top: 3px; }

.c-green  { color: #3fb950; }
.c-yellow { color: #e3b341; }
.c-red    { color: #f85149; }
.c-blue   { color: #58a6ff; }
.c-gray   { color: #8b949e; }

/* ── Progress bar ── */
.bar-track {
  height: 6px; background: #21262d; border-radius: 3px;
  overflow: hidden; margin-top: 5px;
}
.bar-fill {
  height: 100%; border-radius: 3px;
  transition: width 0.9s ease, background 0.5s;
}

/* ── Wide metric row ── */
.grid-2 {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 8px;
  margin-bottom: 10px;
}
@media(max-width:500px){ .grid-2 { grid-template-columns: 1fr; } }

/* ── Timeline ── */
.timeline-wrap {
  background: #161b22;
  border: 1px solid #21262d;
  border-radius: 8px;
  padding: 10px 12px;
  margin-bottom: 10px;
}
.timeline-header {
  display: flex; align-items: center; gap: 12px;
  font-size: 10px; color: #6e7681; margin-bottom: 8px;
}
.legend { display: flex; align-items: center; gap: 4px; }
.legend-dot {
  width: 9px; height: 9px; border-radius: 2px; flex-shrink: 0;
}
.tl-ok     { background: #2ea043; }
.tl-quota  { background: #9e6a03; }
.tl-err    { background: #da3633; }
.tl-empty  { background: #21262d; }
.tl-slow   { background: #1158cc; }

.timeline-dots {
  display: flex; gap: 3px; flex-wrap: wrap;
}
.tl-dot {
  width: 10px; height: 10px; border-radius: 2px;
  flex-shrink: 0; cursor: default;
  transition: opacity 0.3s;
}

/* ── Sparkline ── */
.spark-wrap {
  background: #161b22;
  border: 1px solid #21262d;
  border-radius: 8px;
  padding: 10px 12px;
  margin-bottom: 10px;
}
.spark-header { font-size: 10px; color: #6e7681; margin-bottom: 6px; }
.spark-canvas { width: 100%; height: 50px; display: block; }

/* ── Log ── */
.log-wrap {
  background: #161b22;
  border: 1px solid #21262d;
  border-radius: 8px;
  padding: 8px 10px;
  height: 150px;
  overflow-y: auto;
  margin-bottom: 10px;
}
.log-line {
  padding: 2px 0;
  border-bottom: 1px solid #1c2128;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  font-size: 11px;
}
.log-line:last-child { border: none; }
.ll-ok    { color: #3fb950; }
.ll-warn  { color: #e3b341; }
.ll-err   { color: #f85149; }
.ll-info  { color: #8b949e; }
.ll-slow  { color: #79c0ff; }

/* ── Footer ── */
.footer {
  font-size: 10px; color: #30363d; text-align: center;
  border-top: 1px solid #21262d; padding-top: 8px;
}

/* ── Offline banner ── */
.offline-banner {
  display: none;
  background: #3d0000;
  border: 1px solid #da3633;
  border-radius: 6px;
  padding: 6px 12px;
  font-size: 11px;
  color: #f85149;
  margin-bottom: 10px;
  text-align: center;
}

/* ── Slow mode overlay indicator ── */
.slow-bar {
  display: none;
  background: #1158cc22;
  border: 1px solid #1158cc;
  border-radius: 6px;
  padding: 6px 12px;
  font-size: 11px;
  color: #79c0ff;
  margin-bottom: 10px;
  text-align: center;
}
</style>
</head>
<body>

<div class="header">
  <div class="header-title">▸ Champa Session Monitor</div>
  <div class="header-right">
    <span class="uptime" id="uptime-display">online: —</span>
    <span class="status-badge badge-off" id="main-badge">
      <span class="pulse gray" id="main-pulse"></span>
      <span id="main-badge-text">Connecting...</span>
    </span>
  </div>
</div>

<div class="offline-banner" id="offline-banner">
  ✗ Cannot reach http://127.0.0.1:7001/status — is champa-client.exe running?
</div>

<div class="slow-bar" id="slow-bar">
  ⬇ SLOW MODE ACTIVE — poll rate throttled to ~0.07 req/s (quota back-off)
</div>

<div class="cache-strip" id="cache-strip">
  <span class="cache-pill current" id="cache-active-pill">cdn.ampproject.org</span>
</div>

<div class="grid-4">
  <div class="metric">
    <div class="metric-label">THROUGHPUT</div>
    <div class="metric-value c-green" id="m-bps">— KB/s</div>
    <div class="metric-sub" id="m-bps-sub">bytes/sec</div>
    <div class="bar-track"><div class="bar-fill" id="bar-bps" style="width:0%;background:#2ea043"></div></div>
  </div>
  <div class="metric">
    <div class="metric-label">POLL RATE</div>
    <div class="metric-value c-blue" id="m-rate">— /s</div>
    <div class="metric-sub" id="m-rate-sub">requests/sec</div>
    <div class="bar-track"><div class="bar-fill" id="bar-rate" style="width:0%;background:#1f6feb"></div></div>
  </div>
  <div class="metric">
    <div class="metric-label">AVG RTT</div>
    <div class="metric-value c-yellow" id="m-rtt">— ms</div>
    <div class="metric-sub">round-trip time</div>
    <div class="bar-track"><div class="bar-fill" id="bar-rtt" style="width:0%;background:#9e6a03"></div></div>
  </div>
  <div class="metric">
    <div class="metric-label">ERROR RATE</div>
    <div class="metric-value c-gray" id="m-err">—%</div>
    <div class="metric-sub">last window</div>
    <div class="bar-track"><div class="bar-fill" id="bar-err" style="width:0%;background:#3fb950"></div></div>
  </div>
</div>

<div class="grid-2">
  <div class="metric">
    <div class="metric-label">SESSION (server)</div>
    <div class="metric-value c-blue" id="m-elapsed">—</div>
    <div class="metric-sub" id="m-conv">conv: —</div>
  </div>
  <div class="metric">
    <div class="metric-label">QUOTA ERRORS  <span style="color:#484f58;font-size:9px">(threshold: 30)</span></div>
    <div class="metric-value" id="m-qerr" style="color:#f85149">—</div>
    <div class="metric-sub" id="m-qmode">slow mode: off</div>
    <div class="bar-track"><div class="bar-fill" id="bar-qerr" style="width:0%;background:#2ea043"></div></div>
  </div>
</div>

<div class="grid-2">
  <div class="metric">
    <div class="metric-label">PAYLOAD / CONCURRENCY</div>
    <div style="display:flex;align-items:baseline;gap:8px">
      <span class="metric-value c-gray" id="m-payload">—</span>
      <span style="font-size:12px;color:#6e7681">bytes max</span>
      <span style="font-size:16px;color:#8b949e;margin-left:8px" id="m-conc">× — workers</span>
    </div>
    <div class="bar-track"><div class="bar-fill" id="bar-payload" style="width:0%;background:#1f6feb"></div></div>
  </div>
  <div class="metric">
    <div class="metric-label">ACTIVE CACHE</div>
    <div class="metric-value" style="font-size:14px;color:#58a6ff;margin-top:2px" id="m-cache">—</div>
    <div class="metric-sub">rotation on quota hit</div>
  </div>
</div>

<div class="spark-wrap">
  <div class="spark-header">Throughput history (last 60 windows)</div>
  <canvas class="spark-canvas" id="spark" height="50"></canvas>
</div>

<div class="timeline-wrap">
  <div class="timeline-header">
    Recent poll results (newest left)
    <div class="legend"><div class="legend-dot tl-ok"></div>success</div>
    <div class="legend"><div class="legend-dot tl-quota"></div>quota err</div>
    <div class="legend"><div class="legend-dot tl-err"></div>error</div>
    <div class="legend"><div class="legend-dot tl-slow"></div>slow mode</div>
  </div>
  <div class="timeline-dots" id="timeline"></div>
</div>

<div class="log-wrap" id="log"></div>

<div class="footer">
  Polling http://127.0.0.1:7001/status every second &nbsp;|&nbsp;
  champa-client session monitor
</div>

<script>
// ── State ──────────────────────────────────────────────────────────────────────
const STATUS_URL = '/status.json';
const POLL_MS    = 1000;

let prevSnap      = null;
let logs          = [];
let tl            = new Array(60).fill('empty');
let sparkData     = new Array(60).fill(0);
let sessionStart  = null;
let failCount     = 0;
let lastConv      = null;
// onlineTime tracks accumulated seconds where the endpoint was reachable.
let onlineSec     = 0;
let onlineTickAt  = null; // timestamp of last successful fetch
let isOnline      = false; // true only while endpoint is reachable

// ── Utility ──────────────────────────────────────────────────────────────────
function now() {
  const d = new Date();
  return ` + "`" + `${String(d.getHours()).padStart(2,'0')}:` + "`" + `
       + ` + "`" + `${String(d.getMinutes()).padStart(2,'0')}:` + "`" + `
       + ` + "`" + `${String(d.getSeconds()).padStart(2,'0')}` + "`" + `;
}

function fmtSecs(s) {
  s = Math.floor(Math.abs(s));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const ss = s % 60;
  if (h > 0) return ` + "`" + `${h}h ${m}m ${String(ss).padStart(2,'0')}s` + "`" + `;
  return ` + "`" + `${m}m ${String(ss).padStart(2,'0')}s` + "`" + `;
}

function clamp(v,lo,hi){ return Math.min(Math.max(v,lo),hi); }

function addLog(msg, type) {
  logs.unshift({ t: now(), msg, type });
  if (logs.length > 80) logs.pop();
  renderLog();
}

function renderLog() {
  const el = document.getElementById('log');
  el.innerHTML = logs.map(e =>
    ` + "`" + `<div class="log-line ll-${e.type}">[${e.t}] ${e.msg}</div>` + "`" + `
  ).join('');
}

// ── Sparkline ────────────────────────────────────────────────────────────────
function drawSpark() {
  const canvas = document.getElementById('spark');
  const W = canvas.offsetWidth;
  const H = canvas.offsetHeight;
  canvas.width  = W;
  canvas.height = H;
  const ctx = canvas.getContext('2d');
  ctx.clearRect(0,0,W,H);

  const max = Math.max(...sparkData, 1);
  const step = W / (sparkData.length - 1);

  // Grid line
  ctx.strokeStyle = '#21262d';
  ctx.lineWidth = 1;
  ctx.beginPath(); ctx.moveTo(0, H/2); ctx.lineTo(W, H/2); ctx.stroke();

  // Fill
  ctx.beginPath();
  ctx.moveTo(0, H);
  sparkData.forEach((v, i) => {
    const x = i * step;
    const y = H - (v / max) * (H - 4) - 2;
    ctx.lineTo(x, y);
  });
  ctx.lineTo(W, H);
  ctx.closePath();
  const grad = ctx.createLinearGradient(0, 0, 0, H);
  grad.addColorStop(0, '#1f6feb55');
  grad.addColorStop(1, '#1f6feb00');
  ctx.fillStyle = grad;
  ctx.fill();

  // Line
  ctx.beginPath();
  sparkData.forEach((v, i) => {
    const x = i * step;
    const y = H - (v / max) * (H - 4) - 2;
    i === 0 ? ctx.moveTo(x, y) : ctx.lineTo(x, y);
  });
  ctx.strokeStyle = '#58a6ff';
  ctx.lineWidth = 1.5;
  ctx.stroke();
}

// ── Update UI ────────────────────────────────────────────────────────────────
function applySnap(s) {
  // Badge
  const pulse  = document.getElementById('main-pulse');
  const badge  = document.getElementById('main-badge');
  const btext  = document.getElementById('main-badge-text');
  const sbar   = document.getElementById('slow-bar');

  if (s.connecting) {
    badge.className = 'status-badge badge-warn';
    pulse.className = 'pulse yellow';
    btext.textContent = 'CONNECTING...';
    sbar.style.display = 'none';
  } else if (s.is_slow_mode) {
    badge.className = 'status-badge badge-slow';
    pulse.className = 'pulse red';
    btext.textContent = 'SLOW MODE';
    sbar.style.display = 'block';
  } else if (s.error_rate > 0.1 || s.quota_errors > 15) {
    badge.className = 'status-badge badge-warn';
    pulse.className = 'pulse yellow';
    btext.textContent = 'DEGRADED';
    sbar.style.display = 'none';
  } else {
    badge.className = 'status-badge badge-ok';
    pulse.className = 'pulse green';
    btext.textContent = 'OK';
    sbar.style.display = 'none';
  }

  // Session elapsed
  document.getElementById('m-elapsed').textContent =
    s.connecting ? 'Connecting...' : fmtSecs(s.session_elapsed_sec);
  document.getElementById('m-elapsed').className =
    s.connecting ? 'metric-value c-yellow' : 'metric-value c-blue';
  document.getElementById('m-conv').textContent = s.connecting ? 'waiting for handshake...' : ` + "`" + `conv: ${s.conv || '—'}` + "`" + `;

  // Detect reconnect
  if (lastConv && s.conv && lastConv !== s.conv) {
    addLog(` + "`" + `Session reconnected — new conv ${s.conv}` + "`" + `, 'warn');
  }
  lastConv = s.conv;

  // Cache strip
  const activeCache = s.current_cache || '—';
  document.getElementById('cache-active-pill').textContent = activeCache;
  document.getElementById('m-cache').textContent = activeCache;

  // Throughput
  const bps = s.bytes_per_sec || 0;
  const bpsK = bps / 1024;
  document.getElementById('m-bps').textContent = bpsK.toFixed(1) + ' KB/s';
  document.getElementById('m-bps').className = 'metric-value ' +
    (bpsK > 20 ? 'c-green' : bpsK > 5 ? 'c-yellow' : 'c-red');
  const bpsBar = document.getElementById('bar-bps');
  bpsBar.style.width = clamp(bpsK / 50 * 100, 0, 100) + '%';
  bpsBar.style.background = bpsK > 20 ? '#2ea043' : bpsK > 5 ? '#9e6a03' : '#da3633';

  // Rate
  const rate = s.rate_per_sec || 0;
  document.getElementById('m-rate').textContent = rate.toFixed(2) + ' /s';
  document.getElementById('m-rate').className = 'metric-value ' +
    (s.is_slow_mode ? 'c-red' : rate > 8 ? 'c-green' : rate > 3 ? 'c-blue' : 'c-yellow');
  document.getElementById('m-rate-sub').textContent = s.is_slow_mode
    ? 'throttled — slow mode'
    : 'requests/sec';
  document.getElementById('bar-rate').style.width = clamp(rate / 12 * 100, 0, 100) + '%';

  // RTT
  const rtt = s.avg_rtt_ms || 0;
  document.getElementById('m-rtt').textContent = rtt.toFixed(0) + ' ms';
  document.getElementById('m-rtt').className = 'metric-value ' +
    (rtt < 250 ? 'c-green' : rtt < 500 ? 'c-yellow' : 'c-red');
  const rttBar = document.getElementById('bar-rtt');
  rttBar.style.width = clamp(rtt / 600 * 100, 0, 100) + '%';
  rttBar.style.background = rtt < 250 ? '#2ea043' : rtt < 500 ? '#9e6a03' : '#da3633';

  // Error rate
  const errPct = (s.error_rate || 0) * 100;
  document.getElementById('m-err').textContent = errPct.toFixed(1) + '%';
  document.getElementById('m-err').className = 'metric-value ' +
    (errPct < 5 ? 'c-gray' : errPct < 15 ? 'c-yellow' : 'c-red');
  const errBar = document.getElementById('bar-err');
  errBar.style.width = clamp(errPct / 30 * 100, 0, 100) + '%';
  errBar.style.background = errPct < 5 ? '#2ea043' : errPct < 15 ? '#9e6a03' : '#da3633';

  // Quota errors
  const qe = s.quota_errors || 0;
  document.getElementById('m-qerr').textContent = qe;
  document.getElementById('m-qerr').className = 'metric-value ' +
    (qe < 10 ? 'c-gray' : qe < 25 ? 'c-yellow' : 'c-red');
  document.getElementById('m-qmode').textContent = s.is_slow_mode
    ? ` + "`" + `slow mode: ON (rate=${rate.toFixed(3)}/s)` + "`" + `
    : 'slow mode: off';
  const qBar = document.getElementById('bar-qerr');
  qBar.style.width = clamp(qe / 30 * 100, 0, 100) + '%';
  qBar.style.background = qe < 10 ? '#2ea043' : qe < 25 ? '#9e6a03' : '#da3633';

  // Payload + concurrency
  const mp = s.max_payload || 0;
  document.getElementById('m-payload').textContent = mp.toLocaleString();
  document.getElementById('m-conc').textContent = ` + "`" + `× ${s.concurrency || '—'} workers` + "`" + `;
  document.getElementById('bar-payload').style.width = clamp(mp / 7000 * 100, 0, 100) + '%';

  // Uptime
  // Accumulate online time only while endpoint is reachable.
  if (onlineTickAt !== null) {
    onlineSec += (Date.now() - onlineTickAt) / 1000;
  }
  onlineTickAt = Date.now();
  isOnline = true;

  // Sparkline
  sparkData.push(bpsK);
  if (sparkData.length > 60) sparkData.shift();
  drawSpark();

  // Timeline: infer what just happened
  if (prevSnap) {
    const deltaQe = (s.quota_errors || 0) - (prevSnap.quota_errors || 0);
    const deltaEr = (s.error_rate || 0) - (prevSnap.error_rate || 0);
    const slot = s.is_slow_mode ? 'slow' :
                 deltaQe > 0   ? 'quota' :
                 deltaEr > 0.05 ? 'err' : 'ok';
    tl.unshift(slot);
    if (tl.length > 60) tl.pop();
  }

  // Render timeline
  document.getElementById('timeline').innerHTML = tl.map(s =>
    ` + "`" + `<div class="tl-dot tl-${s}" title="${s}"></div>` + "`" + `
  ).join('');

  // Detect state changes for log
  if (prevSnap) {
    const wasS = prevSnap.is_slow_mode;
    const nowS = s.is_slow_mode;
    if (!wasS && nowS) addLog('Entered SLOW MODE — quota back-off active', 'slow');
    if (wasS && !nowS) addLog('Left slow mode — rate resuming to normal', 'ok');
    const wasC = prevSnap.current_cache;
    const nowC = s.current_cache;
    if (wasC && nowC && wasC !== nowC)
      addLog(` + "`" + `Cache rotated: ${wasC} → ${nowC}` + "`" + `, 'warn');
    if (deltaQeLog(prevSnap, s)) {
      addLog(` + "`" + `Quota errors: ${s.quota_errors} / 30 threshold` + "`" + `, 'warn');
    }
  }
  prevSnap = s;
}

function deltaQeLog(prev, cur) {
  const d = (cur.quota_errors||0) - (prev.quota_errors||0);
  return d > 0 && (cur.quota_errors||0) % 5 === 0;
}

// ── Fetch ─────────────────────────────────────────────────────────────────────
async function fetchStatus() {
  try {
    const res = await fetch(STATUS_URL, { cache: 'no-store', signal: AbortSignal.timeout(1800) });
    if (!res.ok) throw new Error(` + "`" + `HTTP ${res.status}` + "`" + `);
    const data = await res.json();
    failCount = 0;
    document.getElementById('offline-banner').style.display = 'none';
    applySnap(data);
  } catch (err) {
    failCount++;
    if (failCount === 3) {
      onlineTickAt = null; // freeze online time while offline
      isOnline = false;
      document.getElementById('offline-banner').style.display = 'block';
      document.getElementById('main-badge').className = 'status-badge badge-off';
      document.getElementById('main-pulse').className = 'pulse gray';
      document.getElementById('main-badge-text').textContent = 'Offline';
      document.getElementById('slow-bar').style.display = 'none';
      if (failCount === 3) addLog('Status endpoint unreachable — is the client running?', 'err');
    }
  }
}

// ── Init ──────────────────────────────────────────────────────────────────────
addLog('Monitor started — polling ' + STATUS_URL, 'info');
fetchStatus();
setInterval(fetchStatus, POLL_MS);
window.addEventListener('resize', drawSpark);

// Update the uptime display every second independently of the fetch cycle
// so the counter visibly ticks while online, and visibly freezes while offline.
setInterval(function() {
  const el = document.getElementById('uptime-display');
  if (!isOnline) {
    el.textContent = 'offline';
    el.style.color = 'var(--c-red, #f85149)';
    return;
  }
  // Add time elapsed since last successful fetch tick
  const displaySec = onlineTickAt
    ? onlineSec + (Date.now() - onlineTickAt) / 1000
    : onlineSec;
  el.textContent = 'online: ' + fmtSecs(displaySec);
  el.style.color = '';
}, 1000);
</script>
</body>
</html>
`

func readKeyFromFile(filename string) ([]byte, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return noise.ReadKey(f)
}

// ── Noise transport layer ──────────────────────────────────────────────────────

type noisePacketConn struct {
	sess *noise.Session
	net.PacketConn
}

func readNoiseMessageOfTypeFrom(conn net.PacketConn, wantedType byte) ([]byte, net.Addr, error) {
	for {
		msgType, msg, addr, err := noise.ReadMessageFrom(conn)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				continue
			}
			return nil, nil, err
		}
		if msgType == wantedType {
			return msg, addr, nil
		}
	}
}

func noiseDial(conn net.PacketConn, addr net.Addr, pubkey []byte) (*noisePacketConn, error) {
	p := []byte{noise.MsgTypeHandshakeInit}
	pre, p, err := noise.InitiateHandshake(p, pubkey)
	if err != nil {
		return nil, err
	}
	if _, err = conn.WriteTo(p, addr); err != nil {
		return nil, err
	}
	msg, _, err := readNoiseMessageOfTypeFrom(conn, noise.MsgTypeHandshakeResp)
	if err != nil {
		return nil, err
	}
	sess, err := pre.FinishHandshake(msg)
	if err != nil {
		return nil, err
	}
	return &noisePacketConn{sess, conn}, nil
}

func (c *noisePacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	msg, addr, err := readNoiseMessageOfTypeFrom(c.PacketConn, noise.MsgTypeTransport)
	if err != nil {
		return 0, nil, err
	}
	dec, err := c.sess.Decrypt(nil, msg)
	if errors.Is(err, noise.ErrInvalidNonce) {
		// Out-of-order or replayed packet — discard silently.
		// With concurrency <= 4 this should be rare. If it still appears
		// frequently, reduce adpMaxConcurrency further in adaptive.go.
		log.Printf("[noise] discarded out-of-order packet (nonce reuse)")
		return 0, addr, nil
	} else if err != nil {
		return 0, nil, err
	}
	return copy(p, dec), addr, nil
}

func (c *noisePacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	buf := []byte{noise.MsgTypeTransport}
	buf, err := c.sess.Encrypt(buf, p)
	if err != nil {
		return 0, err
	}
	return c.PacketConn.WriteTo(buf, addr)
}

// ── TCP stream handling ────────────────────────────────────────────────────────

func handle(local *net.TCPConn, sess *smux.Session, conv uint32) error {
	stream, err := sess.OpenStream()
	if err != nil {
		return fmt.Errorf("session %08x: open stream: %v", conv, err)
	}
	defer func() {
		log.Printf("end stream %08x:%d", conv, stream.ID())
		stream.Close()
	}()
	log.Printf("begin stream %08x:%d", conv, stream.ID())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := io.Copy(stream, local); err != nil &&
			!errors.Is(err, io.ErrClosedPipe) && err != io.EOF {
			log.Printf("stream %08x:%d local->stream: %v", conv, stream.ID(), err)
		}
		local.CloseRead()
		stream.Close()
	}()
	go func() {
		defer wg.Done()
		if _, err := io.Copy(local, stream); err != nil &&
			!errors.Is(err, io.ErrClosedPipe) && err != io.EOF {
			log.Printf("stream %08x:%d stream->local: %v", conv, stream.ID(), err)
		}
		local.CloseWrite()
	}()
	wg.Wait()
	return nil
}

// ── Session dialling ───────────────────────────────────────────────────────────

func dialSession(
	rt http.RoundTripper,
	serverURL *url.URL,
	rotor *CacheRotor,
	front string,
	pubkey []byte,
	quota *QuotaManager,
) (*smux.Session, *PollingPacketConn, uint32, error) {
	poll := PollFunc(func(ctx context.Context, p []byte) (io.ReadCloser, error) {
		return exchangeAMP(ctx, rt, serverURL, rotor.Current(), front, p)
	})

	pconn := NewPollingPacketConn(turbotunnel.DummyAddr{}, poll, quota, rotor)

	nconn, err := noiseDial(pconn, turbotunnel.DummyAddr{}, pubkey)
	if err != nil {
		pconn.Close()
		return nil, nil, 0, fmt.Errorf("noise handshake: %v", err)
	}

	conn, err := kcp.NewConn2(turbotunnel.DummyAddr{}, nil, 0, 0, nconn)
	if err != nil {
		pconn.Close()
		return nil, nil, 0, fmt.Errorf("kcp: %v", err)
	}
	conv := conn.GetConv()
	log.Printf("begin session %08x (cache: %s)", conv, rotor.CurrentName())

	conn.SetStreamMode(true)
	conn.SetNoDelay(1, 10, 2, 1)
	conn.SetACKNoDelay(true)
	conn.SetWindowSize(2048, 2048)

	smuxCfg := smux.DefaultConfig()
	smuxCfg.Version = 2
	smuxCfg.KeepAliveInterval = keepAliveInterval
	smuxCfg.KeepAliveTimeout = idleTimeout
	smuxCfg.MaxReceiveBuffer = 8 * 1024 * 1024
	smuxCfg.MaxStreamBuffer = 2 * 1024 * 1024

	sess, err := smux.Client(conn, smuxCfg)
	if err != nil {
		conn.Close()
		pconn.Close()
		return nil, nil, 0, fmt.Errorf("smux: %v", err)
	}
	return sess, pconn, conv, nil
}

// ── Session holder ─────────────────────────────────────────────────────────────

type sessionHolder struct {
	mu           sync.RWMutex
	sess         *smux.Session
	conv         uint32
	pconn        *PollingPacketConn
	sessionStart time.Time

	rt        http.RoundTripper
	serverURL *url.URL
	front     string
	pubkey    []byte
	quota     *QuotaManager
	rotor     *CacheRotor
}

func newSessionHolder(
	rt http.RoundTripper,
	serverURL *url.URL,
	rotor *CacheRotor,
	front string,
	pubkey []byte,
) *sessionHolder {
	h := &sessionHolder{
		rt:        rt,
		serverURL: serverURL,
		front:     front,
		pubkey:    pubkey,
		quota:     NewQuotaManager(),
		rotor:     rotor,
	}
	go h.keepAlive()
	return h
}

func (h *sessionHolder) keepAlive() {
	for {
		sess, pconn, conv, err := dialSession(
			h.rt, h.serverURL, h.rotor, h.front, h.pubkey, h.quota,
		)
		if err != nil {
			log.Printf("dial failed (retry in %v): %v", reconnectDelay, err)
			time.Sleep(reconnectDelay)
			continue
		}

		h.mu.Lock()
		h.sess = sess
		h.conv = conv
		h.pconn = pconn
		h.sessionStart = time.Now()
		h.mu.Unlock()

		for !sess.IsClosed() {
			time.Sleep(500 * time.Millisecond)
		}
		pconn.Close()
		log.Printf("end session %08x -- reconnecting in %v", conv, reconnectDelay)
		time.Sleep(reconnectDelay)
	}
}

func (h *sessionHolder) get() (*smux.Session, uint32) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sess, h.conv
}

func (h *sessionHolder) Status() StatusSnapshot {
	h.mu.RLock()
	var elapsedSec float64
	if !h.sessionStart.IsZero() {
		elapsedSec = time.Since(h.sessionStart).Seconds()
	}
	snap := StatusSnapshot{
		SessionElapsedSec: elapsedSec,
		Conv:              fmt.Sprintf("%08x", h.conv),
		CurrentCache:      h.rotor.CurrentName(),
		Connecting:        h.sess == nil,
	}
	pconn := h.pconn
	h.mu.RUnlock()

	snap.IsSlowMode = h.quota.IsSlowMode()
	snap.QuotaErrors = h.quota.ErrorCount()

	if pconn != nil {
		snap.RatePerSec = pconn.CurrentRate()
		bps, rtt, errRate := pconn.adaptive.GetStats()
		snap.BytesPerSec = bps
		snap.AvgRTTms = rtt
		snap.ErrorRate = errRate
		mp, conc, _ := pconn.adaptive.GetParams()
		snap.MaxPayload = mp
		snap.Concurrency = conc
	}
	return snap
}

// ── Main run loop ──────────────────────────────────────────────────────────────

func run(rt http.RoundTripper, serverURL *url.URL, rotor *CacheRotor, front, localAddr string, pubkey []byte) error {
	if t, ok := rt.(*http.Transport); ok {
		t.MaxConnsPerHost = 8
	}

	ln, err := net.Listen("tcp", localAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %v", localAddr, err)
	}
	defer ln.Close()
	log.Printf("listening on %s", localAddr)

	holder := newSessionHolder(rt, serverURL, rotor, front, pubkey)


	if statusAddr != "" {
		mux := http.NewServeMux()

		// /status.json  →  raw JSON (for curl and the in-page fetch)
		mux.HandleFunc("/status.json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			if err := json.NewEncoder(w).Encode(holder.Status()); err != nil {
				log.Printf("status encode: %v", err)
			}
		})

		// /status  →  browser gets the HTML dashboard; curl/API gets JSON
		mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
			accept := r.Header.Get("Accept")
			wantsHTML := strings.Contains(accept, "text/html")
			if wantsHTML {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("Cache-Control", "no-cache")
				fmt.Fprint(w, dashboardHTML)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			if err := json.NewEncoder(w).Encode(holder.Status()); err != nil {
				log.Printf("status encode: %v", err)
			}
		})

		go func() {
			log.Printf("status endpoint: http://%s/status", statusAddr)
			srv := &http.Server{Addr: statusAddr, Handler: mux}
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("status server: %v", err)
			}
		}()
		// Open the dashboard in the default browser after a short delay so the
		// server socket is ready before the browser makes its first request.
		go openBrowser("http://"+statusAddr+"/status", 800)
	}

	for {
		local, err := ln.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				continue
			}
			return err
		}
		go func() {
			defer local.Close()
			sess, conv := holder.get()
			if sess == nil || sess.IsClosed() {
				log.Printf("session not ready -- dropping connection from %v", local.RemoteAddr())
				return
			}
			if err := handle(local.(*net.TCPConn), sess, conv); err != nil {
				log.Printf("handle: %v", err)
			}
		}()
	}
}

// ── Entry point ────────────────────────────────────────────────────────────────

// openBrowser opens url in the default system browser.
// It waits delayMs milliseconds first so the HTTP server has time to start.
func openBrowser(url string, delayMs int) {
	time.Sleep(time.Duration(delayMs) * time.Millisecond)
	var err error
	switch runtime.GOOS {
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default: // linux and others
		err = exec.Command("xdg-open", url).Start()
	}
	if err != nil {
		log.Printf("could not open browser: %v", err)
	}
}

func main() {
	var (
		cachesStr      string
		front          string
		pubkeyFilename string
		pubkeyString   string
	)

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `Usage:
  %[1]s -pubkey-file PUBKEYFILE [options] SERVERURL LOCALADDR

Options:
`, os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(flag.CommandLine.Output(), `
Examples:

  Two AMP caches with domain fronting (recommended):
    %[1]s -pubkey-file server.pub \
        -caches "https://cdn.ampproject.org/,https://amp.cloudflare.com/" \
        -front www.google.com \
        https://your-server.example/champa/ 127.0.0.1:7000

  Check live status:
    curl http://127.0.0.1:7001/status

`, os.Args[0])
	}

	flag.StringVar(&cachesStr, "caches",
		"https://cdn.ampproject.org/,https://amp.cloudflare.com/",
		"comma-separated AMP cache URLs to rotate between when quota is hit")
	flag.StringVar(&front, "front", "",
		"domain-fronting host (e.g. www.google.com)")
	flag.StringVar(&pubkeyString, "pubkey", "",
		fmt.Sprintf("server public key as %d hex digits", noise.KeyLen*2))
	flag.StringVar(&pubkeyFilename, "pubkey-file", "",
		"path to file containing the server public key")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.LUTC)

	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(1)
	}

	serverURL, err := url.Parse(flag.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot parse server URL: %v\n", err)
		os.Exit(1)
	}
	localAddr := flag.Arg(1)

	var cacheList []string
	for _, s := range strings.Split(cachesStr, ",") {
		if s = strings.TrimSpace(s); s != "" {
			cacheList = append(cacheList, s)
		}
	}
	if len(cacheList) == 0 {
		cacheList = []string{""}
	}
	rotor := NewCacheRotor(cacheList)
	log.Printf("cache rotation list: %v", cacheList)

	if pubkeyFilename != "" && pubkeyString != "" {
		fmt.Fprintf(os.Stderr, "error: use only one of -pubkey and -pubkey-file\n")
		os.Exit(1)
	}

	var pubkey []byte
	switch {
	case pubkeyFilename != "":
		pubkey, err = readKeyFromFile(pubkeyFilename)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot read pubkey file: %v\n", err)
			os.Exit(1)
		}
	case pubkeyString != "":
		pubkey, err = noise.DecodeKey(pubkeyString)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: pubkey format error: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "error: -pubkey or -pubkey-file is required\n")
		os.Exit(1)
	}

	if err := run(http.DefaultTransport, serverURL, rotor, front, localAddr, pubkey); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}
