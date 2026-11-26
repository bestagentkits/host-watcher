#!/usr/bin/env node
/**
 * Tests for host-pressure-reminder.cjs
 * Run: node --test kits/engineer/hooks/__tests__/host-pressure-reminder.test.cjs
 */

const { describe, it, beforeEach, afterEach } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { runHostPressureReminder, shouldRemind, ownProcesses, worktreeHash } = require('../hooks/claude-code/host-pressure-reminder.cjs');

const NOW = Date.parse('2026-11-25T10:00:00Z');
let tmpDir;

const statusFile = () => path.join(tmpDir, 'host', 'status.json');

function writeCache(verdict, { ageMs = 5000, envelope = true, raw, extra = {} } = {}) {
  const payload = {
    verdict,
    reasons: [],
    sampled_at: new Date(NOW - ageMs).toISOString(),
    disk: { active_pct: 97, latency_ms: 640 },
    space: [{ path: 'D:\\', free_gb: 18, low: verdict === 'clean' }],
    ...extra
  };
  const body = raw ?? JSON.stringify(envelope ? { schema_version: 1, kind: 'host.status', data: payload } : payload);
  fs.mkdirSync(path.dirname(statusFile()), { recursive: true });
  fs.writeFileSync(statusFile(), body);
}

function run(overrides = {}, input = { session_id: 's1', hook_event_name: 'UserPromptSubmit' }) {
  const spawns = [];
  const result = runHostPressureReminder(input, {
    now: NOW,
    tmpDir,
    hostDir: path.join(tmpDir, 'host'),
    findWatcher: () => null,
    isEnabled: () => true,
    spawn: (...args) => { spawns.push(args); return { on() {}, unref() {} }; },
    ...overrides
  });
  return { result, spawns };
}

describe('host-pressure-reminder.cjs', () => {
  beforeEach(() => { tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'hw-hpr-')); });
  afterEach(() => { fs.rmSync(tmpDir, { recursive: true, force: true }); });

  for (const verdict of ['go', 'unknown']) {
    it(`stays silent on ${verdict}`, () => {
      writeCache(verdict);
      assert.deepStrictEqual(run().result, { continue: true });
    });
  }

  it('stays silent when the cache is missing', () => {
    assert.deepStrictEqual(run().result, { continue: true });
  });

  it('stays silent when the cache is older than 3 minutes', () => {
    writeCache('hold', { ageMs: 4 * 60 * 1000 });
    assert.deepStrictEqual(run().result, { continue: true });
  });

  it('stays silent on corrupt JSON', () => {
    writeCache('hold', { raw: '{not json' });
    assert.deepStrictEqual(run().result, { continue: true });
  });

  it('injects on hold with event name and metrics', () => {
    writeCache('hold');
    const out = run().result.hookSpecificOutput;
    assert.strictEqual(out.hookEventName, 'UserPromptSubmit');
    assert.match(out.additionalContext, /Machine pressure: hold \(disk active 97%, 640 ms\)/);
    assert.match(out.additionalContext, /Do not start new builds/);
    assert.doesNotMatch(out.additionalContext, /stop the task/i);
  });

  it('injects on clean from a bare payload and names the low path', () => {
    writeCache('clean', { envelope: false });
    const out = run({}, { session_id: 's1', hook_event_name: 'PostToolUse' }).result.hookSpecificOutput;
    assert.strictEqual(out.hookEventName, 'PostToolUse');
    assert.match(out.additionalContext, /D:\\ 18 GB free/);
    assert.match(out.additionalContext, /host-watcher caches/);
  });

  it('dedupes within 5 minutes and re-injects after the window', () => {
    writeCache('hold');
    assert.ok(run().result.hookSpecificOutput);
    assert.deepStrictEqual(run().result, { continue: true });
    const later = NOW + 5 * 60 * 1000;
    writeCache('hold', { ageMs: -5 * 60 * 1000 + 1000 }); // sampled 1 s before the later clock
    const out = runHostPressureReminder({ session_id: 's1' }, {
      now: later, tmpDir, hostDir: path.join(tmpDir, 'host'), findWatcher: () => null, isEnabled: () => true
    });
    assert.ok(out.hookSpecificOutput);
    assert.strictEqual(shouldRemind({ verdict: 'hold', at: NOW }, 'hold', later - 1), false);
  });

  it('prefers cache reasons and adds the cleanup hint for hold with low space', () => {
    const payload = {
      verdict: 'hold',
      reasons: ['cpu hold: busy 97%'],
      sampled_at: new Date(NOW).toISOString(),
      disk: { active_pct: 3, latency_ms: 1 },
      space: [{ path: 'D:\\', free_gb: 18, low: true }]
    };
    fs.mkdirSync(path.dirname(statusFile()), { recursive: true });
    fs.writeFileSync(statusFile(), JSON.stringify({ data: payload }));
    const text = run().result.hookSpecificOutput.additionalContext;
    assert.match(text, /Machine pressure: hold \(cpu hold: busy 97%; D:\\ 18 GB free\)/);
    assert.doesNotMatch(text, /active 3%/);
    assert.match(text, /host-watcher caches/);
  });

  it('dedupe is per session', () => {
    writeCache('hold');
    assert.ok(run().result.hookSpecificOutput);
    assert.ok(run({}, { session_id: 's2' }).result.hookSpecificOutput);
  });

  it('re-injects immediately when severity escalates', () => {
    writeCache('throttle');
    assert.match(run().result.hookSpecificOutput.additionalContext, /throttle/);
    writeCache('hold');
    assert.match(run().result.hookSpecificOutput.additionalContext, /hold/);
    writeCache('throttle');
    assert.deepStrictEqual(run().result, { continue: true });
  });

  it('does nothing when disabled', () => {
    writeCache('hold', { ageMs: 10 * 60 * 1000 });
    const { result, spawns } = run({ isEnabled: () => false, findWatcher: () => '/bin/host-watcher' });
    assert.deepStrictEqual(result, { continue: true });
    assert.strictEqual(spawns.length, 0);
  });

  it('spawns one detached refresher for a stale cache and locks for 60 s', () => {
    const { spawns } = run({ findWatcher: () => '/bin/host-watcher' });
    assert.strictEqual(spawns.length, 1);
    const [cmd, args, opts] = spawns[0];
    assert.strictEqual(cmd, '/bin/host-watcher');
    assert.deepStrictEqual(args, ['watch', '--detach']);
    assert.strictEqual(opts.detached, true);
    assert.strictEqual(opts.windowsHide, true);
    assert.strictEqual(run({ findWatcher: () => '/bin/host-watcher' }).spawns.length, 0);
    const later = [];
    runHostPressureReminder({}, {
      now: Date.now() + 61 * 1000, tmpDir, hostDir: path.join(tmpDir, 'host'), findWatcher: () => '/bin/host-watcher', isEnabled: () => true,
      spawn: (...a) => { later.push(a); return {}; }
    });
    assert.strictEqual(later.length, 1);
  });

  it('does not spawn when the cache is fresh or host-watcher is missing', () => {
    writeCache('go', { ageMs: 10 * 1000 });
    assert.strictEqual(run({ findWatcher: () => '/bin/host-watcher' }).spawns.length, 0);
    writeCache('go', { ageMs: 2 * 60 * 1000 });
    assert.strictEqual(run({ findWatcher: () => null }).spawns.length, 0);
  });

  it('prints continue:true and exits 0 as a process with empty stdin', () => {
    const { spawnSync } = require('child_process');
    const res = spawnSync(process.execPath, [path.join(__dirname, '..', 'hooks', 'claude-code', 'host-pressure-reminder.cjs')], {
      input: 'garbage', encoding: 'utf8',
      env: { ...process.env, HOST_WATCHER_HOME: tmpDir, TMP: tmpDir, TEMP: tmpDir, TMPDIR: tmpDir, PATH: '' }
    });
    assert.strictEqual(res.status, 0);
    assert.deepStrictEqual(JSON.parse(res.stdout.trim()), { continue: true });
  });

  it('names only this worktree\'s processes from top_io', () => {
    const wt = path.join(tmpDir, 'wt');
    fs.mkdirSync(path.join(wt, 'sub'), { recursive: true });
    fs.writeFileSync(path.join(wt, '.git'), 'gitdir: elsewhere\n');
    writeCache('hold', {
      extra: {
        top_io: [
          { pid: 11, name: 'go.exe', write_mb_s: 30.4, read_mb_s: 0.2, worktree: wt + path.sep },
          { pid: 12, name: 'node.exe', write_mb_s: 50, worktree: path.join(tmpDir, 'other') }
        ]
      }
    });
    const text = run({}, { session_id: 's1', cwd: path.join(wt, 'sub') }).result.hookSpecificOutput.additionalContext;
    assert.match(text, /this worktree: go\.exe \(pid 11, I\/O write 30 MB\/s\)/);
    assert.doesNotMatch(text, /node\.exe/);
    assert.match(text, /host-watcher gate -- <cmd>/);
    assert.deepStrictEqual(ownProcesses({ top_io: [] }, wt), []);
  });

  it('prunes other sessions\' state files older than a day when writing its own', () => {
    const old = path.join(tmpDir, 'host-watcher-pressure-gone.json');
    const fresh = path.join(tmpDir, 'host-watcher-pressure-live.json');
    const unrelated = path.join(tmpDir, 'other-old.json');
    for (const f of [old, fresh, unrelated]) fs.writeFileSync(f, '{}');
    const dayAgo = (NOW - 25 * 60 * 60 * 1000) / 1000;
    fs.utimesSync(old, dayAgo, dayAgo);
    fs.utimesSync(unrelated, dayAgo, dayAgo);
    fs.utimesSync(fresh, NOW / 1000, NOW / 1000);
    writeCache('hold');
    run();
    assert.ok(!fs.existsSync(old));
    assert.ok(fs.existsSync(fresh));
    assert.ok(fs.existsSync(unrelated));
    assert.ok(fs.existsSync(path.join(tmpDir, 'host-watcher-pressure-s1.json')));
  });

  describe('per-session inbox', () => {
    let wt;
    let inboxFile;
    beforeEach(() => {
      wt = path.join(tmpDir, 'wt');
      fs.mkdirSync(wt, { recursive: true });
      fs.mkdirSync(path.join(wt, '.git'));
      inboxFile = path.join(tmpDir, 'host', 'inbox', `${worktreeHash(wt)}.json`);
      fs.mkdirSync(path.dirname(inboxFile), { recursive: true });
    });
    const writeInbox = (ageMs) => fs.writeFileSync(inboxFile, JSON.stringify({
      worktree: wt, verdict: 'hold', sampled_at: new Date(NOW - ageMs).toISOString(),
      processes: [{ pid: 7, name: 'go.exe', write_mb_s: 40, read_mb_s: 1 }],
      action: 'Pause new heavy commands; run them through `host-watcher gate -- <cmd>`.', severity: 3
    }));
    const runIn = () => run({}, { session_id: 's1', cwd: wt }).result;

    it('hash matches the Go WorktreeHash test vector on every platform', () => {
      // Shared vector with hoststatus: `D:\Worktrees\x\` -> `d:/worktrees/x`.
      assert.strictEqual(worktreeHash('D:\\Worktrees\\x\\', 'win32'), 'd49b00fbf4d3b90e');
      assert.strictEqual(worktreeHash('D:/Worktrees/x', 'win32'), 'd49b00fbf4d3b90e');
      assert.strictEqual(worktreeHash('/home/u/wt/', 'linux'), worktreeHash('/home/u/wt', 'linux'));
      // Only ASCII letters fold, like Go's asciiLower.
      assert.notStrictEqual(worktreeHash('C:/İ', 'win32'), worktreeHash('C:/i̇', 'win32'));
    });

    it('shows the entry once and deletes it', () => {
      writeInbox(1000);
      const text = runIn().hookSpecificOutput.additionalContext;
      assert.match(text, /Host watcher notice for this worktree \(hold\): your processes go\.exe pid 7 I\/O write 40 MB\/s/);
      assert.doesNotMatch(text, /disk/);
      assert.ok(!fs.existsSync(inboxFile));
      assert.deepStrictEqual(runIn(), { continue: true });
    });

    it('says when rates count all I/O and falls back to read bytes', () => {
      fs.writeFileSync(inboxFile, JSON.stringify({
        worktree: wt, verdict: 'hold', sampled_at: new Date(NOW - 1000).toISOString(),
        processes: [{ pid: 8, name: 'svc.exe', write_mb_s: 0, read_mb_s: 12 }],
        io_scope: 'all I/O (Windows counts network and pipes)', action: 'Pause.', severity: 3
      }));
      const text = runIn().hookSpecificOutput.additionalContext;
      assert.match(text, /svc\.exe pid 8 I\/O read 12 MB\/s \(all I\/O \(Windows counts network and pipes\)\)/);
    });

    it('removes entries older than 10 minutes without showing them', () => {
      writeInbox(11 * 60 * 1000);
      assert.deepStrictEqual(runIn(), { continue: true });
      assert.ok(!fs.existsSync(inboxFile));
    });

    it('comes before the generic reminder', () => {
      writeInbox(1000);
      writeCache('hold');
      const text = runIn().hookSpecificOutput.additionalContext;
      assert.ok(text.indexOf('Host watcher notice') < text.indexOf('Machine pressure: hold'));
      assert.ok(text.indexOf('Machine pressure') > 0);
    });
  });
});
