#!/usr/bin/env node
/**
 * Host Pressure Reminder Hook
 *
 * Reminds the agent to pace heavy commands (builds, tests, preflights,
 * installs) while the machine is under disk, CPU, memory, or free-space
 * pressure. Reads the machine-wide watcher snapshot
 * `<HOST_WATCHER_HOME or ~/.host-watcher>/status.json`; when it is missing or
 * stale and the `host-watcher` binary is on PATH, starts
 * `host-watcher watch --detach` at most once per minute per machine.
 * Disable with HOST_WATCHER_HOOK=off; disable auto-start with
 * HOST_WATCHER_AUTOSTART=0. Works for any agent whose hooks pass JSON with
 * `cwd`/`session_id` on stdin and read `additionalContext` back.
 * Never blocks the turn, never touches the network, always exits 0.
 */

'use strict';

const fs = require('fs');
const os = require('os');
const path = require('path');
const crypto = require('crypto');
const { spawn: nodeSpawn } = require('child_process');

const HOOK_NAME = 'host-pressure-reminder';
const STATUS_FILE_NAME = 'status.json';
const SPAWN_MARKER_NAME = 'watch-spawn.marker';
const REFRESH_AFTER_MS = 60 * 1000;
const STALE_AFTER_MS = 3 * 60 * 1000;
const DEDUPE_WINDOW_MS = 5 * 60 * 1000;
const MAX_CACHE_BYTES = 256 * 1024;
const INBOX_MAX_AGE_MS = 10 * 60 * 1000;

// Only these verdicts produce a reminder; go and unknown stay silent.
const SEVERITY = Object.freeze({ throttle: 1, clean: 2, hold: 3 });

/** State dir shared with the scripts and the binary: HOST_WATCHER_HOME, else ~/.host-watcher. */
function resolveHostDir(env = process.env) {
  return env.HOST_WATCHER_HOME || path.join(os.homedir(), '.host-watcher');
}

/** Nearest ancestor of dir holding a `.git` entry (repo or worktree root). */
function findWorktreeRoot(dir) {
  if (typeof dir !== 'string' || !dir) return null;
  let current = path.resolve(dir);
  for (let depth = 0; depth < 64; depth += 1) {
    try {
      fs.statSync(path.join(current, '.git'));
      return current;
    } catch {}
    const parent = path.dirname(current);
    if (parent === current) return null;
    current = parent;
  }
  return null;
}

function samePath(a, b, platform = process.platform) {
  if (typeof a !== 'string' || typeof b !== 'string' || !a || !b) return false;
  const norm = (p) => {
    const r = path.resolve(p).replace(/[\\/]+$/, '');
    return platform === 'win32' ? r.toLowerCase() : r;
  };
  return norm(a) === norm(b);
}

/** Names of top_io processes running in this session's worktree. */
function ownProcesses(payload, cwd, platform = process.platform) {
  const topIo = Array.isArray(payload.top_io) ? payload.top_io : [];
  const root = findWorktreeRoot(cwd);
  if (!root || !topIo.length) return [];
  return topIo
    .filter((p) => p && typeof p.name === 'string' && samePath(p.worktree, root, platform))
    .slice(0, 3)
    .map((p) => `${p.name}${Number.isFinite(p.pid) ? ` (pid ${p.pid}` : ' ('}${ioRate(p, ', ')})`);
}

/** I/O rate preferring write bytes, which fill and saturate a disk. */
function ioRate(p, sep = ' ') {
  const write = Number(p.write_mb_s || 0);
  const read = Number(p.read_mb_s || 0);
  if (write > 0) return `${sep}I/O write ${Math.round(write)} MB/s`;
  if (read > 0) return `${sep}I/O read ${Math.round(read)} MB/s`;
  return '';
}

/** Qualifies rates that are not disk-only (Windows counts network and pipes). */
function ioScopeNote(scope) {
  return typeof scope === 'string' && scope.trim() ? ` (${scope.trim()})` : '';
}

function readJsonFile(file, maxBytes = MAX_CACHE_BYTES) {
  try {
    const stat = fs.statSync(file);
    if (!stat.isFile() || stat.size > maxBytes) return null;
    return { value: JSON.parse(fs.readFileSync(file, 'utf8')), mtimeMs: stat.mtimeMs };
  } catch {
    return null;
  }
}

/** Accepts the render.WrapJSON envelope ({data: ...}) or a bare payload. */
function extractPayload(doc) {
  if (!doc || typeof doc !== 'object') return null;
  const payload = doc.data && typeof doc.data === 'object' ? doc.data : doc;
  return typeof payload.verdict === 'string' ? payload : null;
}

/** Age from sampled_at when parseable, otherwise from the file mtime. */
function cacheAgeMs(payload, mtimeMs, now) {
  const sampled = Date.parse(payload && payload.sampled_at);
  const base = Number.isFinite(sampled) ? sampled : mtimeMs;
  return Math.max(0, now - base);
}

function readHostStatus(cacheFile, now) {
  const file = readJsonFile(cacheFile);
  if (!file) return { payload: null, ageMs: Infinity };
  const payload = extractPayload(file.value);
  if (!payload) return { payload: null, ageMs: Infinity };
  return { payload, ageMs: cacheAgeMs(payload, file.mtimeMs, now) };
}

/** Finds a `host-watcher` executable on PATH without spawning a shell. */
function findWatcherOnPath(env = process.env, platform = process.platform) {
  const pathValue = env.PATH || env.Path || '';
  if (env.HOST_WATCHER_AUTOSTART === '0') return null;
  // Only a real executable: shims that need a shell are skipped silently.
  const names = platform === 'win32' ? ['host-watcher.exe'] : ['host-watcher'];
  for (const dir of pathValue.split(path.delimiter)) {
    if (!dir) continue;
    for (const name of names) {
      const candidate = path.join(dir, name);
      try {
        if (fs.statSync(candidate).isFile()) return candidate;
      } catch {}
    }
  }
  return null;
}

/**
 * Claims the cross-session refresh slot. Returns true when this process may
 * spawn the refresher; at most one claim succeeds per REFRESH_AFTER_MS.
 */
function claimRefreshLock(lockFile, now) {
  for (let attempt = 0; attempt < 2; attempt += 1) {
    try {
      fs.closeSync(fs.openSync(lockFile, 'wx'));
      return true;
    } catch (error) {
      if (!error || error.code !== 'EEXIST') return false;
      try {
        const stat = fs.statSync(lockFile);
        if (now - stat.mtimeMs < REFRESH_AFTER_MS) return false;
        fs.unlinkSync(lockFile);
      } catch {
        return false;
      }
    }
  }
  return false;
}

function spawnWatcher({ watcherPath, spawn }) {
  try {
    const child = spawn(watcherPath, ['watch', '--detach'], {
      detached: true,
      stdio: 'ignore',
      windowsHide: true
    });
    if (child && typeof child.on === 'function') child.on('error', () => {});
    if (child && typeof child.unref === 'function') child.unref();
    return true;
  } catch {
    return false;
  }
}

const STATE_FILE_RE = /^host-watcher-pressure-.+\.json$/;
const STATE_MAX_AGE_MS = 24 * 60 * 60 * 1000;
const STATE_PRUNE_LIMIT = 50;

/** Removes other sessions' state files older than a day; inspects at most 50. */
function pruneSessionStates(tmpDir, keep, now) {
  let names;
  try {
    names = fs.readdirSync(tmpDir);
  } catch {
    return;
  }
  let seen = 0;
  for (const name of names) {
    if (!STATE_FILE_RE.test(name)) continue;
    if (++seen > STATE_PRUNE_LIMIT) break;
    const file = path.join(tmpDir, name);
    if (file === keep) continue;
    try {
      if (now - fs.statSync(file).mtimeMs > STATE_MAX_AGE_MS) fs.unlinkSync(file);
    } catch {}
  }
}

function sessionStateFile(tmpDir, sessionId) {
  const key = String(sessionId || 'default').replace(/[^A-Za-z0-9_-]/g, '_').slice(0, 80) || 'default';
  return path.join(tmpDir, `host-watcher-pressure-${key}.json`);
}

/**
 * Inbox key shared with the watcher: first 16 hex of sha256 of the cleaned
 * absolute worktree root (no trailing separator), lower-cased on Windows.
 */
function worktreeHash(root, platform = process.platform) {
  // Mirrors hoststatus.WorktreeHash: backslashes to '/', POSIX clean, no trailing
  // slash (a bare drive becomes "x:/"), ASCII-only lower-casing on Windows.
  let clean = path.posix.normalize(String(root).replace(/\\/g, '/'));
  if (clean.length > 1 && clean.endsWith('/')) clean = clean.slice(0, -1);
  if (/^[A-Za-z]:$/.test(clean)) clean += '/';
  if (platform === 'win32') clean = clean.replace(/[A-Z]/g, (c) => c.toLowerCase());
  return crypto.createHash('sha256').update(clean, 'utf8').digest('hex').slice(0, 16);
}

/** Reads and deletes this worktree's inbox entry; stale entries are removed unread. */
function consumeInbox(hostDir, cwd, now, platform = process.platform) {
  const root = findWorktreeRoot(cwd);
  if (!root) return null;
  const file = path.join(hostDir, 'inbox', `${worktreeHash(root, platform)}.json`);
  const doc = readJsonFile(file, 64 * 1024);
  if (!doc) {
    try { fs.unlinkSync(file); } catch {}
    return null;
  }
  try { fs.unlinkSync(file); } catch {}
  const entry = doc.value && typeof doc.value === 'object' ? doc.value : null;
  if (!entry) return null;
  const sampled = Date.parse(entry.sampled_at);
  const age = now - (Number.isFinite(sampled) ? sampled : doc.mtimeMs);
  if (!(age < INBOX_MAX_AGE_MS)) return null;
  return formatInbox(entry);
}

function formatInbox(entry) {
  const procs = Array.isArray(entry.processes) ? entry.processes : [];
  const named = procs
    .filter((p) => p && typeof p.name === 'string')
    .slice(0, 3)
    .map((p) => `${p.name}${Number.isFinite(p.pid) ? ` pid ${p.pid}` : ''}${ioRate(p)}`);
  const verdict = typeof entry.verdict === 'string' ? entry.verdict : 'pressure';
  const action = typeof entry.action === 'string' && entry.action.trim() ? ` ${entry.action.trim()}` : '';
  return `Host watcher notice for this worktree (${verdict})${named.length ? `: your processes ${named.join(', ')}${ioScopeNote(entry.io_scope)}` : ''}.${action}`;
}

function shouldRemind(state, verdict, now) {
  if (!state || typeof state.verdict !== 'string' || !Number.isFinite(state.at)) return true;
  if ((SEVERITY[verdict] || 0) > (SEVERITY[state.verdict] || 0)) return true;
  return now - state.at >= DEDUPE_WINDOW_MS;
}

function formatNumber(value) {
  return Number.isFinite(value) ? String(Math.round(value)) : null;
}

function buildReminder(payload, own = []) {
  const verdict = payload.verdict;
  const space = Array.isArray(payload.space) ? payload.space : [];
  const lowSpace = space.filter((entry) => entry && entry.low && typeof entry.path === 'string');
  const reasons = Array.isArray(payload.reasons)
    ? payload.reasons.filter((r) => typeof r === 'string' && r.trim()).slice(0, 2).map((r) => r.trim())
    : [];
  const details = reasons.length ? reasons : metricDetails(payload);
  for (const entry of lowSpace) {
    const free = formatNumber(entry.free_gb);
    details.push(`${entry.path} ${free === null ? 'low' : `${free} GB`} free`);
  }
  const head = `Machine pressure: ${verdict}${details.length ? ` (${details.join('; ')})` : ''}.`;
  const advice = verdict === 'throttle'
    ? 'Avoid running several builds/tests/preflights or installs at once; run heavy commands one at a time.'
    : 'Do not start new builds/tests/preflights or installs now; queue them and prefer read-only work meanwhile.';
  const cleanup = lowSpace.length || verdict === 'clean'
    ? ' Free space by removing only caches this session created (`host-watcher caches` or `python scripts/cache_cleanup.py` to measure).'
    : '';
  const mine = own.length ? ` Heavy I/O from this worktree: ${own.join(', ')}${ioScopeNote(payload.io_scope)}.` : '';
  return `${head}${mine} ${advice} Run builds/tests/preflights/installs through \`host-watcher gate -- <cmd>\` (or \`python scripts/host_gate.py -- <cmd>\`).${cleanup} Keep the full task scope; this only paces heavy commands.`;
}

/** Fallback when the cache carries no reasons: list the sampled metrics. */
function metricDetails(payload) {
  const details = [];
  const disk = payload.disk || {};
  const active = formatNumber(disk.active_pct);
  const latency = formatNumber(disk.latency_ms);
  if (active || latency) {
    details.push(`disk ${[active && `active ${active}%`, latency && `${latency} ms`].filter(Boolean).join(', ')}`);
  }
  const cpu = formatNumber(payload.cpu && payload.cpu.busy_pct);
  if (cpu) details.push(`CPU ${cpu}%`);
  const mem = formatNumber(payload.memory && payload.memory.used_pct);
  if (mem) details.push(`memory ${mem}%`);
  return details;
}

function getEventName(input) {
  if (typeof input.hook_event_name === 'string' && input.hook_event_name) return input.hook_event_name;
  return typeof input.prompt === 'string' ? 'UserPromptSubmit' : 'PostToolUse';
}

/**
 * Pure-ish core with injectable dependencies for tests. Returns the JSON
 * object the hook prints.
 */
function runHostPressureReminder(input = {}, deps = {}) {
  const now = deps.now ?? Date.now();
  const tmpDir = deps.tmpDir || os.tmpdir();
  const spawn = deps.spawn || nodeSpawn;
  const findWatcher = deps.findWatcher || findWatcherOnPath;
  const isEnabled = deps.isEnabled || defaultIsEnabled;
  const silent = { continue: true };

  if (!isEnabled(HOOK_NAME)) return silent;

  const hostDir = deps.hostDir || resolveHostDir();
  const { payload, ageMs } = readHostStatus(path.join(hostDir, STATUS_FILE_NAME), now);

  if (ageMs > REFRESH_AFTER_MS) {
    const watcherPath = findWatcher();
    if (watcherPath) {
      try {
        fs.mkdirSync(hostDir, { recursive: true });
      } catch {}
      if (claimRefreshLock(path.join(hostDir, SPAWN_MARKER_NAME), now)) spawnWatcher({ watcherPath, spawn });
    }
  }

  const cwd = input.cwd || deps.cwd || process.cwd();
  const inboxText = consumeInbox(hostDir, cwd, now, deps.platform);
  let generic = null;
  if (payload && ageMs < STALE_AFTER_MS && SEVERITY[payload.verdict]) {
    const stateFile = sessionStateFile(tmpDir, input.session_id);
    const state = readJsonFile(stateFile, 4096);
    if (shouldRemind(state && state.value, payload.verdict, now)) {
      try {
        fs.writeFileSync(stateFile, JSON.stringify({ verdict: payload.verdict, at: now }));
      } catch {}
      pruneSessionStates(tmpDir, stateFile, now);
      generic = buildReminder(payload, ownProcesses(payload, cwd));
    }
  }
  const text = [inboxText, generic].filter(Boolean).join('\n');
  if (!text) return silent;
  return {
    continue: true,
    hookSpecificOutput: { hookEventName: getEventName(input), additionalContext: text }
  };
}

function defaultIsEnabled() {
  const v = String(process.env.HOST_WATCHER_HOOK || '').toLowerCase();
  return !(v === 'off' || v === '0' || v === 'false');
}

if (require.main === module) {
  let output = { continue: true };
  try {
    let raw = '';
    try {
      raw = fs.readFileSync(0, 'utf-8');
    } catch {}
    let input = {};
    try {
      input = JSON.parse(raw || '{}') || {};
    } catch {}
    output = runHostPressureReminder(input);
  } catch {
    output = { continue: true };
  }
  console.log(JSON.stringify(output));
  process.exit(0);
}

module.exports = {
  runHostPressureReminder,
  buildReminder,
  extractPayload,
  findWatcherOnPath,
  findWorktreeRoot,
  ownProcesses,
  worktreeHash,
  consumeInbox,
  resolveHostDir,
  claimRefreshLock,
  shouldRemind
};
