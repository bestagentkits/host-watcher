"""Unit and CLI tests for the Python scripts. Standard library only:

    python -m unittest discover -s tests -v
"""

import io
import json
import os
import subprocess
import sys
import tempfile
import time
import unittest

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SCRIPTS = os.path.join(ROOT, "scripts")
sys.path.insert(0, SCRIPTS)

from hostwatch import caches, common, gate, inbox, report, sampling, watcher  # noqa: E402

CFG = dict(common.DEFAULTS)


def run_script(name, *args, env=None):
    e = dict(os.environ)
    e.update(env or {})
    return subprocess.run([sys.executable, os.path.join(SCRIPTS, name), *args],
                          capture_output=True, text=True, env=e, timeout=120)


class EvaluateTest(unittest.TestCase):
    def rep(self, **kw):
        r = {"reasons": [], "cpu": None, "memory": None, "disk": None, "space": []}
        r.update(kw)
        return report.evaluate(r, CFG)

    def test_unknown_when_nothing_sampled(self):
        self.assertEqual(self.rep()["verdict"], "unknown")

    def test_levels_and_severity_order(self):
        self.assertEqual(self.rep(cpu={"busy_pct": 10})["verdict"], "go")
        self.assertEqual(self.rep(cpu={"busy_pct": 86})["verdict"], "throttle")
        self.assertEqual(self.rep(memory={"used_pct": 96})["verdict"], "hold")
        self.assertEqual(self.rep(disk={"active_pct": 75, "latency_ms": 1})["verdict"], "throttle")
        self.assertEqual(self.rep(disk={"active_pct": 10, "latency_ms": 120})["verdict"], "hold")
        low = [{"path": "/", "free_gb": 3, "total_gb": 100}]
        self.assertEqual(self.rep(cpu={"busy_pct": 86}, space=low)["verdict"], "clean")
        self.assertEqual(self.rep(cpu={"busy_pct": 99}, space=low)["verdict"], "hold")

    def test_throughput_only_disk_is_not_a_pressure_metric(self):
        r = self.rep(disk={"active_pct": None, "latency_ms": None, "throughput_mb_s": 900})
        self.assertEqual(r["verdict"], "unknown")

    def test_config_overrides_thresholds(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "c.json")
            with open(p, "w") as fh:
                json.dump({"thresholds": {"cpu_throttle_pct": 50, "bogus": 1, "min_free_gb": "x"}}, fh)
            cfg = common.load_config(p)
            self.assertEqual(cfg["cpu_throttle_pct"], 50)
            self.assertEqual(cfg["min_free_gb"], 25.0)
            with open(p, "w") as fh:
                fh.write("{broken")
            with self.assertRaises(ValueError):
                common.load_config(p)


class ParseTest(unittest.TestCase):
    def test_proc_stat_and_busy(self):
        a = sampling.parse_proc_stat("cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 1 2 3 4\n")
        b = sampling.parse_proc_stat("cpu  200 0 200 900 0 0 0 0 0 0\n")
        self.assertAlmostEqual(sampling.busy_pct(a, b), 200 / 300 * 100)
        self.assertIsNone(sampling.busy_pct(a, a))

    def test_meminfo(self):
        m = sampling.parse_meminfo("MemTotal: 1048576 kB\nMemFree: 1 kB\nMemAvailable: 262144 kB\n")
        self.assertEqual(m["used_pct"], 75.0)
        self.assertEqual(m["total_gb"], 1.0)

    def test_diskstats_skips_partitions_and_loops(self):
        text = ("8 0 sda 10 0 100 5 20 0 200 15 0 50 20\n8 1 sda1 1 0 1 1 1 0 1 1 0 1 1\n"
                "7 0 loop0 1 0 1 1 1 0 1 1 0 1 1\n")
        rows = sampling.parse_diskstats(text, is_whole=lambda n: n == "sda" or n == "loop0")
        self.assertEqual(list(rows), ["sda"])
        prev = {"sda": [0] * 11}
        rates = sampling.diskstats_rates(prev, rows, 1000)
        self.assertEqual(rates["sda"]["active"], 5.0)
        self.assertAlmostEqual(rates["sda"]["latency"], 20 / 30)

    def test_vm_stat(self):
        text = ("Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free: 10.\n"
                "Pages active: 99.\nPages inactive: 5.\nPages speculative: 1.\n")
        self.assertEqual(sampling.parse_vm_stat(text), 16 * 16384)

    def test_iostat_drops_boot_row(self):
        text = ("              disk0               cpu\n    KB/t  tps  MB/s  us sy id\n"
                "   20.0  10  0.20  5 5 90\n   30.0  20  1.50  20 10 70\n")
        rows = sampling.parse_iostat(text)
        self.assertEqual(rows, [({"disk0": 1.5}, 30.0)])

    def test_windows_raw_counters(self):
        s0 = {"0 C:": {"ts": 0, "idle": 0, "idleb": 0, "lat": 0, "latb": 0, "f": 1e7, "wb": 0, "rb": 0}}
        s1 = {"0 C:": {"ts": 1e7, "idle": 2.5e6, "idleb": 0, "lat": 1e5, "latb": 10, "f": 1e7, "wb": 5e6, "rb": 0}}
        r = sampling.win_disk_rates([s0, s1])[0]["0 C:"]
        self.assertAlmostEqual(r["active"], 75.0)
        self.assertAlmostEqual(r["latency"], 1.0)
        self.assertAlmostEqual(r["write"], 5.0)

    def test_summarize_picks_busiest_by_median(self):
        r = lambda a: {"active": a, "latency": 1, "write": 1, "read": 1, "throughput": 2}  # noqa: E731
        d = sampling.summarize_disks([{"a": r(10), "b": r(90)}, {"a": r(95), "b": r(80)}, {"a": r(12), "b": r(85)}], "host")
        self.assertEqual(d["device"], "b")
        self.assertEqual(d["active_pct"], 85.0)


class WorktreeInboxTest(unittest.TestCase):
    def test_hash_matches_go_and_js_vector(self):
        self.assertEqual(common.worktree_hash("D:\\Worktrees\\x\\", True), "d49b00fbf4d3b90e")
        self.assertEqual(common.worktree_hash("D:/Worktrees/x", True), "d49b00fbf4d3b90e")
        self.assertEqual(common.worktree_hash("/home/u/wt/", False), common.worktree_hash("/home/u/wt", False))
        self.assertNotEqual(common.worktree_hash("C:/\u0130", True), common.worktree_hash("C:/i\u0307", True))
        self.assertEqual(common.normalize_worktree("C:", True), "c:/")

    def test_worktree_root(self):
        with tempfile.TemporaryDirectory() as d:
            os.makedirs(os.path.join(d, "a", "b"))
            open(os.path.join(d, ".git"), "w").close()
            self.assertEqual(common.worktree_root(os.path.join(d, "a", "b")), os.path.abspath(d))

    def test_update_read_take_and_expiry(self):
        with tempfile.TemporaryDirectory() as d:
            r = {"verdict": "hold", "top_io": [{"pid": 7, "name": "go", "write_mb_s": 40, "read_mb_s": 0, "worktree": "/w/a"},
                                               {"pid": 8, "name": "x", "write_mb_s": 1, "read_mb_s": 0, "worktree": ""}]}
            now = time.time()
            inbox.update(d, r, now)
            self.assertEqual(len(os.listdir(d)), 1)
            e = inbox.read(d, "/w/a", now)
            self.assertEqual(e["severity"], 3)
            self.assertIn("host_gate.py", e["action"])
            self.assertIsNotNone(inbox.read(d, "/w/a", now, take=True))
            self.assertIsNone(inbox.read(d, "/w/a", now))
            inbox.update(d, r, now - 11 * 60)
            self.assertIsNone(inbox.read(d, "/w/a", now))
            inbox.update(d, {"verdict": "go", "top_io": r["top_io"]}, now)
            self.assertEqual(os.listdir(d), [])

    def test_lower_severity_does_not_overwrite_fresh_entry(self):
        with tempfile.TemporaryDirectory() as d:
            top = [{"pid": 1, "name": "n", "write_mb_s": 1, "read_mb_s": 0, "worktree": "/w"}]
            now = time.time()
            inbox.update(d, {"verdict": "hold", "top_io": top}, now)
            inbox.update(d, {"verdict": "throttle", "top_io": top}, now + 10)
            self.assertEqual(inbox.read(d, "/w", now + 10)["verdict"], "hold")


class SnapshotWatcherTest(unittest.TestCase):
    def test_snapshot_freshness(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "status.json")
            r = {"verdict": "go", "sampled_at": common.now_iso(), "reasons": []}
            common.write_atomic(p, common.envelope(common.SNAPSHOT_KIND, r))
            self.assertEqual(report.read_snapshot(p)["source"], "watcher")
            self.assertIsNone(report.read_snapshot(p, now=time.time() + 60))
            common.write_atomic(p, common.envelope("other", r))
            self.assertIsNone(report.read_snapshot(p))

    def test_watch_writes_snapshot_and_is_singleton(self):
        with tempfile.TemporaryDirectory() as d:
            p = common.paths(d)
            fake = lambda: {"verdict": "go", "sampled_at": common.now_iso(), "reasons": [], "top_io": []}  # noqa: E731
            held = watcher.FileLock(p["lock"]).acquire()
            try:
                self.assertNotEqual(watcher.running_pid(p), 0)
                with self.assertRaises(watcher.Busy):
                    watcher.watch(p, CFG, interval=0.01, collect=fake, alive=lambda: True, max_loops=1)
            finally:
                held.release()
            self.assertEqual(watcher.running_pid(p), 0)
            out = watcher.watch(p, CFG, interval=0.01, collect=fake, alive=lambda: True, max_loops=2, log=io.StringIO())
            self.assertEqual(out, "stopped")
            self.assertIsNotNone(report.read_snapshot(p["snapshot"]))
            self.assertFalse(os.path.exists(p["pid"]))

    def test_idle_exit_and_unreadable_process_table(self):
        with tempfile.TemporaryDirectory() as d:
            p = common.paths(d)
            fake = lambda: {"verdict": "go", "sampled_at": common.now_iso(), "reasons": [], "top_io": []}  # noqa: E731
            self.assertEqual(watcher.watch(p, CFG, 0.01, idle_exit=0.001, collect=fake, alive=lambda: False), "idle")
            self.assertEqual(watcher.watch(p, CFG, 0.01, idle_exit=0.001, collect=fake, alive=lambda: None,
                                           max_loops=3), "stopped")


class GateTest(unittest.TestCase):
    def clock(self):
        t = [0.0]
        return t, (lambda s: t.__setitem__(0, t[0] + s)), (lambda: t[0])

    def test_waits_while_hold_then_runs(self):
        t, sleep, now = self.clock()
        seq = iter(["hold", "hold", "go"])
        err = io.StringIO()
        gate.wait_for_host(lambda: {"verdict": next(seq), "reasons": []}, 1800, sleep, now, err)
        self.assertEqual(t[0], 45)
        self.assertEqual(err.getvalue().count("\n"), 2)

    def test_max_wait_and_non_blocking_verdicts(self):
        t, sleep, now = self.clock()
        err = io.StringIO()
        gate.wait_for_host(lambda: {"verdict": "clean", "reasons": []}, 100, sleep, now, err)
        self.assertEqual(t[0], 100)
        self.assertIn("running anyway", err.getvalue())
        self.assertIn("cache_cleanup.py", err.getvalue())
        for v in ("go", "throttle", "unknown"):
            t, sleep, now = self.clock()
            gate.wait_for_host(lambda: {"verdict": v}, 100, sleep, now, io.StringIO())
            self.assertEqual(t[0], 0)

    def test_max_wait_zero_and_errors_never_block(self):
        calls = []
        gate.wait_for_host(lambda: calls.append(1), 0)
        self.assertEqual(calls, [])

        def boom():
            raise RuntimeError("x")
        err = io.StringIO()
        gate.wait_for_host(boom, 100, err=err)
        self.assertIn("running without waiting", err.getvalue())

    def test_cli_passes_exit_code_and_validates(self):
        with tempfile.TemporaryDirectory() as d:
            env = {"HOST_WATCHER_HOME": d}
            r = run_script("host_gate.py", "--max-wait", "0", "--", sys.executable, "-c", "import sys; sys.exit(4)", env=env)
            self.assertEqual(r.returncode, 4, r.stderr)
            self.assertEqual(run_script("host_gate.py", "--max-wait", "0", env=env).returncode, 2)
            self.assertEqual(run_script("host_gate.py", "--max-wait", "0", "--", "hw-no-such-cmd-xyz", env=env).returncode, 5)


class CleanupTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = self.tmp.name
        old = time.time() - 3 * 3600
        for name in ("hwtest-old", "hwtest-active", "hwtest-new", "other-old"):
            path = os.path.join(self.root, name)
            os.makedirs(path)
            with open(os.path.join(path, "f"), "w") as fh:
                fh.write("x" * 100)
            if name != "hwtest-new":
                os.utime(os.path.join(path, "f"), (old, old))
                os.utime(path, (old, old))

    def tearDown(self):
        self.tmp.cleanup()

    def test_only_owned_idle_unreferenced_matches_are_candidates(self):
        text = "node /x/" + "hwtest-active" + "/run.js"
        found, skipped = caches.candidates(["hwtest-*"], [self.root], 60, text)
        self.assertEqual([os.path.basename(c["path"]) for c in found], ["hwtest-old"])
        reasons = {os.path.basename(s["path"]): s["reason"] for s in skipped}
        self.assertIn("running process", reasons["hwtest-active"])
        self.assertIn("modified", reasons["hwtest-new"])
        self.assertNotIn("other-old", reasons)

    def test_msys_and_forward_slash_forms_still_protect_a_live_run(self):
        text = "bash -c cd /d/tmp/hwtest-active && make"
        found, _ = caches.candidates(["hwtest-*"], [self.root], 60, text)
        self.assertNotIn("hwtest-active", [os.path.basename(c["path"]) for c in found])

    def test_cli_apply_removes_only_safe_candidates(self):
        r = run_script("cache_cleanup.py", "--no-inventory", "--glob", "hwtest-*", "--root", self.root, "--apply", "--json")
        self.assertEqual(r.returncode, 0, r.stderr)
        out = json.loads(r.stdout)
        left = sorted(os.listdir(self.root))
        removed = {os.path.basename(x) for x in out.get("removed", [])}
        self.assertTrue(removed <= {"hwtest-old", "hwtest-active"})  # only idle, owned, matching dirs
        self.assertFalse(removed & set(left))
        for kept in ("hwtest-new", "other-old"):
            self.assertIn(kept, left)

    def test_unreadable_process_list_deletes_nothing(self):
        found, skipped = caches.candidates(["hwtest-*"], [self.root], 60, None)
        self.assertEqual(found, [])
        self.assertTrue(all("unavailable" in s["reason"] or "modified" in s["reason"] for s in skipped))

    @unittest.skipIf(os.name == "nt", "symlinks need privileges on Windows")
    def test_symlink_is_never_a_candidate_nor_followed(self):
        target = os.path.join(self.root, "other-old")
        os.symlink(target, os.path.join(self.root, "hwtest-link"))
        found, skipped = caches.candidates(["hwtest-*"], [self.root], 60, "")
        self.assertNotIn("hwtest-link", [os.path.basename(c["path"]) for c in found])
        self.assertIn("not a plain directory", {os.path.basename(s["path"]): s["reason"] for s in skipped}["hwtest-link"])

    def test_cli_is_dry_run_by_default_and_refuses_broad_patterns(self):
        r = run_script("cache_cleanup.py", "--no-inventory", "--glob", "hwtest-*", "--root", self.root, "--json")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(json.loads(r.stdout)["mode"], "dry-run")
        self.assertTrue(os.path.isdir(os.path.join(self.root, "hwtest-old")))
        self.assertEqual(run_script("cache_cleanup.py", "--glob", "*", "--apply").returncode, 2)
        self.assertEqual(run_script("cache_cleanup.py", "--apply").returncode, 2)

    def test_force_remove_handles_read_only_files(self):
        path = os.path.join(self.root, "hwtest-old")
        os.chmod(os.path.join(path, "f"), 0o400)
        caches.force_remove(path)
        self.assertFalse(os.path.exists(path))


class CallerSpaceTest(unittest.TestCase):
    def test_snapshot_gains_callers_low_volume(self):
        with tempfile.TemporaryDirectory() as d:
            r = {"verdict": "go", "reasons": [], "space": []}
            cfg = dict(CFG, min_free_gb=10 ** 9)
            report.add_caller_space(r, d, cfg)
            self.assertEqual(r["verdict"], "clean")
            report.add_caller_space(r, d, cfg)
            self.assertEqual(len(r["space"]), 1)


class GenericHookTest(unittest.TestCase):
    HOOK = os.path.join(ROOT, "hooks", "generic", "host_pressure_hook.py")

    def run_hook(self, home, payload, *args):
        env = dict(os.environ, HOST_WATCHER_HOME=home, TMPDIR=home, TEMP=home, TMP=home)
        return subprocess.run([sys.executable, self.HOOK, *args], input=json.dumps(payload),
                              capture_output=True, text=True, env=env, timeout=60)

    def test_silent_without_snapshot_and_reminds_once_on_hold(self):
        with tempfile.TemporaryDirectory() as d:
            r = self.run_hook(d, {"cwd": d, "session_id": "s"})
            self.assertEqual((r.returncode, json.loads(r.stdout)), (0, {"continue": True}))
            snap = {"verdict": "hold", "reasons": ["disk hold: active 97%"], "sampled_at": common.now_iso()}
            common.write_atomic(os.path.join(d, "status.json"), common.envelope(common.SNAPSHOT_KIND, snap))
            out = json.loads(self.run_hook(d, {"cwd": d, "session_id": "s", "hook_event_name": "UserPromptSubmit"}).stdout)
            ctx = out["hookSpecificOutput"]
            self.assertEqual(ctx["hookEventName"], "UserPromptSubmit")
            self.assertIn("Machine pressure: hold", ctx["additionalContext"])
            again = self.run_hook(d, {"cwd": d, "session_id": "s"}, "--format", "text")
            self.assertEqual(again.stdout, "")

    def test_garbage_stdin_still_exits_zero(self):
        with tempfile.TemporaryDirectory() as d:
            env = dict(os.environ, HOST_WATCHER_HOME=d)
            r = subprocess.run([sys.executable, self.HOOK], input="garbage", capture_output=True, text=True, env=env)
            self.assertEqual((r.returncode, json.loads(r.stdout)), (0, {"continue": True}))


class StatusCliTest(unittest.TestCase):
    def test_json_envelope_and_bad_args(self):
        with tempfile.TemporaryDirectory() as d:
            env = {"HOST_WATCHER_HOME": d}
            r = run_script("host_status.py", "--json", "--samples", "1", "--interval", "0.2", "--path", d, env=env)
            self.assertEqual(r.returncode, 0, r.stderr)
            env_doc = json.loads(r.stdout)
            self.assertEqual(env_doc["kind"], "host.status")
            self.assertIn(env_doc["data"]["verdict"], common.SEVERITY)
            self.assertEqual(run_script("host_status.py", "--samples", "0", env=env).returncode, 2)

    def test_inbox_cli_silent_without_notice(self):
        with tempfile.TemporaryDirectory() as d:
            r = run_script("host_inbox.py", env={"HOST_WATCHER_HOME": d})
            self.assertEqual((r.returncode, r.stdout), (0, ""))


if __name__ == "__main__":
    unittest.main()
