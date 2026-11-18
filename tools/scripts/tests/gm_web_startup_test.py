"""GM 网页启动的离线契约测试：不接触 Docker、HTTP 服务或真实业务进程。"""
from copy import deepcopy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import MagicMock, patch


SOURCE = Path(__file__).resolve().parents[3] / "java/gm_admin/run_local.py"
spec = importlib.util.spec_from_file_location("gm_web_startup_under_test", SOURCE)
gm = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gm)


class WebStartupTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.runtime = Path(temporary.name).resolve()
        (self.runtime / "logs").mkdir()
        (self.runtime / "uploads").mkdir()
        self.values = {name: "test_only_private_value_12345678901234567890" for name in gm.SECRET_FIELDS}
        self.values["adminUsername"] = "admin"
        self.private = self.runtime / "private.json"
        self.private.write_text(json.dumps(self.values), encoding="utf-8")
        self.identities = {}
        self.owners = {port: [] for port in gm.WEB_PORTS.values()}
        self.commands = {kind: [sys.executable, str(self.runtime / (kind + ".py"))] for kind in gm.WEB_PORTS}
        self.patches = [
            patch.object(gm, "RUNTIME", self.runtime), patch.object(gm, "PRIVATE", self.private),
            patch.object(gm, "command", side_effect=lambda kind: (self.commands[kind], self.runtime)),
            patch.object(gm, "process_identity", side_effect=lambda pid: deepcopy(self.identities.get(pid))),
            patch.object(gm, "listener_owners", side_effect=lambda port: deepcopy(self.owners[port])),
            patch.object(gm, "http_ready", return_value=True), patch.object(gm.socket, "socket"),
            patch.object(gm, "protect"), patch.object(gm.subprocess, "Popen"),
            patch.object(gm, "materials", side_effect=AssertionError("startup must not initialize credentials")),
        ]
        self.mocks = [context.start() for context in self.patches]
        for context in self.patches:
            self.addCleanup(context.stop)
        self.popen = self.mocks[8]
        self.http_ready = self.mocks[5]

    def identity(self, pid, args):
        return {"pid": pid, "created": "created-" + str(pid), "executable": args[0], "argv": list(args)}

    def active(self, kind, *, legacy=False, ready=True):
        worker_pid, child_pid = (101, 102) if kind == "backend" else (201, 202)
        log = self.runtime / "logs" / (kind + ".log")
        self.identities[worker_pid] = self.identity(worker_pid, gm.worker_command(kind, log))
        self.identities[child_pid] = self.identity(child_pid, self.commands[kind])
        record = {"kind": kind, "workerPid": worker_pid, "childPid": child_pid, "state": "running", "log": str(log)}
        if not legacy:
            record.update(workerIdentity=deepcopy(self.identities[worker_pid]), childIdentity=deepcopy(self.identities[child_pid]))
        (self.runtime / (kind + "-process.json")).write_text(json.dumps(record), encoding="utf-8")
        if ready:
            self.owners[gm.WEB_PORTS[kind]] = [{"pid": child_pid, "address": "127.0.0.1"}]
        return record

    def test_preflight_is_read_only_and_does_not_return_credentials_or_commands(self):
        self.active("backend")
        self.active("frontend")
        before = {str(path): path.read_bytes() for path in self.runtime.rglob("*") if path.is_file()}
        with patch.object(gm, "atomic_json", side_effect=AssertionError("preflight wrote a receipt")):
            result = gm.web_preflight()
        self.assertEqual({entry["state"] for entry in result["services"].values()}, {"ready"})
        self.assertEqual(before, {str(path): path.read_bytes() for path in self.runtime.rglob("*") if path.is_file()})
        self.assertNotIn("private", json.dumps(result))
        self.assertNotIn("argv", json.dumps(result))
        self.popen.assert_not_called()

    def test_missing_or_invalid_private_credentials_fail_without_initialization(self):
        self.private.unlink()
        with self.assertRaisesRegex(RuntimeError, "credentials"):
            gm.web_preflight()
        self.private.write_text('{"GM_DB_PASSWORD":"short"}', encoding="utf-8")
        with self.assertRaisesRegex(RuntimeError, "credentials"):
            gm.ensure_web_started()
        self.popen.assert_not_called()

    def test_missing_frontend_dependency_is_rejected_before_backend_launch(self):
        def unavailable(kind):
            if kind == "frontend":
                raise RuntimeError("Prepared GM frontend or installed Vite is missing")
            return self.commands[kind], self.runtime
        with patch.object(gm, "command", side_effect=unavailable):
            with self.assertRaisesRegex(RuntimeError, "Vite"):
                gm.ensure_web_started()
        self.popen.assert_not_called()

    def test_reused_pids_do_not_count_as_existing_gm_processes(self):
        record = self.active("backend", ready=False)
        for pid in (record["workerPid"], record["childPid"]):
            self.identities[pid] = self.identity(pid, [sys.executable, "unrelated.py"])
        self.assertEqual("stopped", gm.web_preflight()["services"]["backend"]["state"])
        self.http_ready.assert_not_called()

    def test_matching_commands_with_changed_creation_time_are_not_adopted(self):
        record = self.active("backend", ready=False)
        for pid in (record["workerPid"], record["childPid"]):
            self.identities[pid]["created"] = "replacement-process"
        self.assertEqual("stopped", gm.web_state("backend")["state"])

    def test_unknown_port_owner_and_wildcard_binding_are_rejected(self):
        record = self.active("backend")
        for owner in ({"pid": 999, "address": "127.0.0.1"}, {"pid": record["childPid"], "address": "0.0.0.0"}):
            self.owners[18090] = [owner]
            with self.assertRaisesRegex(RuntimeError, "unverified"):
                gm.web_preflight()
        self.popen.assert_not_called()

    def test_http_success_without_a_verified_process_is_never_adopted(self):
        self.owners[18090] = [{"pid": 999, "address": "127.0.0.1"}]
        with self.assertRaisesRegex(RuntimeError, "unverified"):
            gm.ensure_web_started()
        self.http_ready.assert_not_called()
        self.popen.assert_not_called()

    def test_healthy_processes_are_not_restarted_or_rewritten(self):
        self.active("backend")
        self.active("frontend")
        with patch.object(gm, "atomic_json", side_effect=AssertionError("healthy receipt rewritten")):
            result = gm.ensure_web_started()
        self.assertEqual({entry["action"] for entry in result["services"].values()}, {"already-running"})
        self.popen.assert_not_called()

    def test_legacy_receipts_upgrade_only_after_real_identity_and_http_match(self):
        record = self.active("backend", legacy=True)
        self.active("frontend")
        self.assertNotIn("childIdentity", gm.load_process_receipt("backend"))
        gm.web_preflight()
        self.assertNotIn("childIdentity", gm.load_process_receipt("backend"))
        gm.ensure_web_started()
        upgraded = gm.load_process_receipt("backend")
        self.assertEqual(self.identities[record["childPid"]], upgraded["childIdentity"])
        self.assertEqual(self.identities[record["workerPid"]], upgraded["workerIdentity"])
        self.popen.assert_not_called()

    def test_legacy_receipt_with_extra_command_argument_is_rejected(self):
        record = self.active("backend", legacy=True)
        self.identities[record["childPid"]]["argv"].append("--unapproved-config")
        with self.assertRaisesRegex(RuntimeError, "unverified"):
            gm.web_state("backend")

    def test_supervisor_intent_prevents_duplicate_start_before_child_receipt(self):
        log = self.runtime / "logs" / "pending.log"
        identity = self.identity(301, gm.worker_command("backend", log))
        self.identities[301] = identity
        gm.atomic_json(self.runtime / "backend-launch.json", {"kind": "backend", "workerPid": 301,
            "workerIdentity": identity, "log": str(log)})
        self.assertEqual("starting", gm.web_state("backend")["state"])
        self.assertFalse(gm.launch_locked("backend"))
        self.popen.assert_not_called()

    def test_receipt_published_during_listener_query_refreshes_all_identities(self):
        old = self.active("backend", ready=False)
        old.update(workerPid=901, childPid=902)
        gm.atomic_json(self.runtime / "backend-process.json", old)
        calls = []
        def publish(port):
            calls.append(port)
            if len(calls) == 1:
                self.active("backend")
            return deepcopy(self.owners[port])
        with patch.object(gm, "listener_owners", side_effect=publish):
            self.assertEqual("ready", gm.web_state("backend")["state"])
        self.assertEqual([18090, 18090], calls)
        self.http_ready.assert_called_once_with("backend")

    def test_continually_changing_receipt_has_at_most_two_snapshot_refreshes(self):
        self.active("backend")
        calls = []
        def changing(port):
            calls.append(port)
            record = gm.load_process_receipt("backend")
            record["revision"] = len(calls)
            gm.atomic_json(self.runtime / "backend-process.json", record)
            return deepcopy(self.owners[port])
        with patch.object(gm, "listener_owners", side_effect=changing):
            with self.assertRaisesRegex(RuntimeError, "receipt changed repeatedly"):
                gm.web_state("backend")
        self.assertEqual(3, len(calls))
        self.http_ready.assert_not_called()

    def pending_frontend(self):
        log = self.runtime / "logs" / "frontend-pending.log"
        identity = self.identity(301, gm.worker_command("frontend", log))
        self.identities[301] = identity
        pending = {"kind": "frontend", "workerPid": 301, "workerIdentity": deepcopy(identity), "log": str(log)}
        gm.atomic_json(self.runtime / "frontend-launch.json", pending)
        self.identities[302] = self.identity(302, self.commands["frontend"])
        self.owners[3101] = [{"pid": 302, "address": "127.0.0.1"}]
        return pending

    def publish_frontend(self, pending):
        record = dict(pending, childPid=302, childIdentity=deepcopy(self.identities[302]), state="running")
        gm.atomic_json(self.runtime / "frontend-process.json", record)

    def test_verified_pending_supervisor_waits_for_vite_receipt_without_trusting_listener(self):
        pending = self.pending_frontend()
        state = gm.web_state("frontend")
        self.assertEqual("starting", state["state"])
        self.assertIsNone(state["child"])
        self.http_ready.assert_not_called()
        self.assertFalse(gm.launch_locked("frontend"))
        self.popen.assert_not_called()
        self.publish_frontend(pending)
        self.assertEqual("ready", gm.web_state("frontend")["state"])

    def test_unverified_pending_supervisor_cannot_hide_an_unknown_listener(self):
        self.pending_frontend()
        self.identities[301]["created"] = "reused-pid"
        with self.assertRaisesRegex(RuntimeError, "unverified"):
            gm.web_state("frontend")
        self.http_ready.assert_not_called()

    def test_receipt_published_during_pending_identity_check_also_refreshes(self):
        pending = self.pending_frontend()
        def identify(pid):
            if pid == 301 and not (self.runtime / "frontend-process.json").exists():
                self.publish_frontend(pending)
            return deepcopy(self.identities.get(pid))
        with patch.object(gm, "process_identity", side_effect=identify):
            self.assertEqual("ready", gm.web_state("frontend")["state"])
        self.http_ready.assert_called_once_with("frontend")

    def test_start_order_is_backend_http_ready_before_frontend_and_supervisors_are_hidden(self):
        events = []
        def launch(args, **kwargs):
            kind = args[2]
            events.append("launch-" + kind)
            self.assertEqual(gm.HIDDEN, kwargs["creationflags"])
            for field in ("stdin", "stdout", "stderr"):
                self.assertEqual(subprocess.DEVNULL, kwargs[field])
            record = self.active(kind)
            # 模拟父进程写意图前，worker 已快速写好运行回执；不能覆盖该回执。
            record["log"] = args[-1]
            self.identities[record["workerPid"]] = self.identity(record["workerPid"], args)
            record["workerIdentity"] = deepcopy(self.identities[record["workerPid"]])
            gm.atomic_json(self.runtime / (kind + "-process.json"), record)
            return MagicMock(pid=record["workerPid"])
        self.popen.side_effect = launch
        self.http_ready.side_effect = lambda kind: events.append("ready-" + kind) or True
        result = gm.ensure_web_started()
        self.assertLess(events.index("ready-backend"), events.index("launch-frontend"))
        self.assertEqual([entry["action"] for entry in result["services"].values()], ["started", "started"])
        self.assertEqual(self.popen.call_count, 2)
        self.assertIn("childPid", gm.load_process_receipt("frontend"))

    def test_timeout_is_explicit_and_does_not_start_frontend_or_retry_backend(self):
        self.active("backend", ready=False)
        with patch.object(gm.time, "monotonic", side_effect=[0, 181]), patch.object(gm.time, "sleep") as sleep:
            with self.assertRaisesRegex(RuntimeError, "HTTP-ready"):
                gm.ensure_web_started()
        sleep.assert_not_called()
        self.popen.assert_not_called()

    def test_dead_startup_is_an_explicit_failure(self):
        with patch.object(gm, "web_state", return_value={"state": "stopped"}):
            with self.assertRaisesRegex(RuntimeError, "exited"):
                gm.wait_web_ready("backend")

    def test_wait_steps_are_short_and_do_not_use_wall_clock(self):
        self.active("backend")
        ready = gm.web_state("backend")
        with patch.object(gm, "web_state", side_effect=[{"state": "starting"}, ready]), \
                patch.object(gm.time, "monotonic", side_effect=[100, 100.1]), patch.object(gm.time, "sleep") as sleep:
            gm.wait_web_ready("backend")
        sleep.assert_called_once_with(0.5)

    def test_parallel_web_launch_is_rejected_by_its_own_lock(self):
        with gm.web_start_lock():
            with self.assertRaisesRegex(RuntimeError, "Another GM web startup"):
                with gm.web_start_lock():
                    self.fail("second startup acquired the lock")
        self.assertTrue((self.runtime / ".gm-web-start.lock").is_file())

    def test_changed_process_during_http_readiness_is_rejected(self):
        record = self.active("backend")
        def changed(kind):
            self.identities[record["childPid"]]["created"] = "replaced-during-http"
            return True
        self.http_ready.side_effect = changed
        with self.assertRaisesRegex(RuntimeError, "identity changed"):
            gm.web_state("backend")

    def test_worker_records_creation_identity_and_redacts_secret_output(self):
        log = self.runtime / "logs" / "worker.log"
        self.identities[os.getpid()] = self.identity(os.getpid(), gm.worker_command("backend", log))
        self.identities[401] = self.identity(401, self.commands["backend"])
        secret = self.values["GM_DB_PASSWORD"]
        self.popen.return_value = MagicMock(pid=401, stdout=iter(["log " + secret + "\n"]))
        self.popen.return_value.wait.return_value = 0
        with patch.object(gm, "environment", return_value={"GM_DB_PASSWORD": secret}):
            gm.worker("backend", log, self.values)
        record = gm.load_process_receipt("backend")
        self.assertEqual(self.identities[401], record["childIdentity"])
        self.assertEqual(self.identities[os.getpid()], record["workerIdentity"])
        self.assertEqual("exited", record["state"])
        self.assertNotIn(secret, log.read_text(encoding="utf-8"))


class SystemQueryTest(unittest.TestCase):
    def test_cold_system_query_has_a_bounded_twenty_second_budget(self):
        completed = subprocess.CompletedProcess([], 0, '{"pid":123}', '')
        with patch.object(gm.subprocess, "run", return_value=completed) as run:
            self.assertEqual({"pid": 123}, gm.system_query("test-only-query"))
        self.assertEqual(20, run.call_args.kwargs["timeout"])
        self.assertEqual(gm.HIDDEN, run.call_args.kwargs["creationflags"])
        self.assertEqual(1, run.call_count)

    def test_timeout_is_distinct_and_does_not_expose_captured_process_arguments(self):
        error = subprocess.TimeoutExpired(["private-command-argument"], 20,
            output="private-process-output", stderr="private-process-error")
        with patch.object(gm.subprocess, "run", side_effect=error):
            with self.assertRaisesRegex(RuntimeError, "process identity query timed out after 20 seconds") as caught:
                gm.system_query("test-only-query", kind="process identity")
        self.assertNotIn("private", str(caught.exception))

    def test_nonzero_exit_is_distinct_and_never_includes_stdout_or_stderr(self):
        completed = subprocess.CompletedProcess([], 7, 'private-process-output', 'private-process-error')
        with patch.object(gm.subprocess, "run", return_value=completed), \
                patch.object(Path, "write_text", side_effect=AssertionError("query output must not be logged")):
            with self.assertRaisesRegex(RuntimeError, "TCP listener ownership query failed.*exit 7") as caught:
                gm.system_query("test-only-query", kind="TCP listener ownership")
        self.assertNotIn("private", str(caught.exception))

    def test_invalid_json_and_unavailable_shell_fail_without_raw_details(self):
        for outcome, message in ((subprocess.CompletedProcess([], 0, 'private-invalid-json', ''), "invalid JSON"),
                (FileNotFoundError("private-path"), "could not start")):
            with patch.object(gm.subprocess, "run", **({"side_effect": outcome} if isinstance(outcome, Exception)
                    else {"return_value": outcome})):
                with self.assertRaisesRegex(RuntimeError, message) as caught:
                    gm.system_query("test-only-query")
            self.assertNotIn("private", str(caught.exception))


class HttpReadinessTest(unittest.TestCase):
    def check(self, kind, status, content_type, body):
        connection = MagicMock()
        response = connection.getresponse.return_value
        response.status = status
        response.getheader.return_value = content_type
        response.read.return_value = body
        with patch.object(gm.http.client, "HTTPConnection", return_value=connection):
            result = gm.http_ready(kind)
        expected = "/jeecg-boot/gm/overview" if kind == "backend" else "/"
        connection.request.assert_called_once_with("GET", expected)
        connection.close.assert_called_once()
        return result

    def test_backend_requires_native_unauthenticated_result_without_captcha_writes(self):
        self.assertTrue(self.check("backend", 401, "application/json;charset=UTF-8", b'{"code":401,"success":false}'))
        self.assertFalse(self.check("backend", 200, "application/json", b'{"code":401,"success":false}'))
        self.assertFalse(self.check("backend", 401, "application/json", b'{"code":403,"success":false}'))
        self.assertFalse(self.check("backend", 401, "text/html", b'<html>unauthorized</html>'))

    def test_frontend_requires_html_and_native_vite_entry(self):
        valid = b'<html><script type="module" src="/@vite/client"></script><script type="module" src="/src/main.ts"></script></html>'
        self.assertTrue(self.check("frontend", 200, "text/html", valid))
        self.assertFalse(self.check("frontend", 200, "text/html", b'<html>unrelated service</html>'))
        self.assertFalse(self.check("frontend", 503, "text/html", valid))


if __name__ == "__main__":
    unittest.main()
