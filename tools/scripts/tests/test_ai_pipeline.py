"""AI 验证入口的黑盒契约测试：真实子进程、临时 Git 仓和可观察交付结果。

不连接游戏服务、不修改主仓历史；伪造的成功/失败只用于验证执行器本身，
不得把这些夹具结果解释为客户端或服务端业务验收。
"""

from __future__ import annotations

import copy
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest


REPO = Path(__file__).resolve().parents[3]
RUNNER = REPO / "tools/scripts/ai_pipeline/runner.py"
KINDS = ("unit", "integration", "e2e", "visual", "performance", "contract", "build", "static")


class PipelineTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ai pipeline tests ")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "source repo"
        self.repo.mkdir()
        self.git("init", "-q")
        (self.repo / "source.txt").write_text("baseline\n", encoding="utf-8")
        self.git("add", "source.txt")
        self.git("-c", "user.name=Pipeline Tests", "-c", "user.email=pipeline@example.invalid",
                 "commit", "-qm", "Test fixture")
        self.task = {
            "schema_version": 1,
            "id": "demo",
            "title": "验证入口契约夹具",
            "goal": "验证结果不假绿，证据与实际执行保持一致。",
            "repositories": ["server_cpp"],
            "scope": {"server_cpp": ["source.txt"]},
            "checks": [{"id": "ok", "required": True}],
            "acceptance": [{"id": "A1", "description": "检查真正执行并通过。", "checks": ["ok"]}],
            "test_matrix": {kind: {"not_applicable": "此夹具不涉及这一验证层。"} for kind in KINDS},
            "delivery": {"kind": "development", "notes": "仅为执行器测试夹具。"},
        }
        self.task["test_matrix"]["unit"] = {"checks": ["ok"]}
        self.catalog = {"schema_version": 1, "checks": {}}
        self.repos = {"repositories": {"server_cpp": str(self.repo)}, "tools": {}}
        self.set_check("ok", "print('real child executed')\n")

    def git(self, *args):
        result = subprocess.run(["git", "-C", str(self.repo), *args], text=True,
                                encoding="utf-8", errors="replace", capture_output=True, timeout=15)
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        return result.stdout.strip()

    def set_check(self, name, source, *, verifier=None, timeout=10):
        script = self.root / (name + " script.py")
        script.write_text(source, encoding="utf-8")
        check = {
            "repository": "server_cpp",
            "cwd": ".",
            "argv": ["{python}", str(script)],
            "timeout_seconds": timeout,
            "verifier": verifier or {"type": "exit_code"},
            "inputs": ["source.txt"],
        }
        self.catalog["checks"][name] = check
        return check

    def add_check(self, name, source, **kwargs):
        check = self.set_check(name, source, **kwargs)
        self.task["checks"].append({"id": name, "required": True})
        self.task["acceptance"][0]["checks"].append(name)
        self.task["test_matrix"]["unit"]["checks"].append(name)
        return check

    def command(self, *, mode="run", output="result", selected=None):
        for name, value in (("task", self.task), ("catalog", self.catalog), ("repos", self.repos)):
            (self.root / (name + ".json")).write_text(
                json.dumps(value, ensure_ascii=False, indent=2), encoding="utf-8")
        command = [sys.executable, "-X", "utf8", "-B", str(RUNNER), mode,
                   "--task", str(self.root / "task.json"),
                   "--catalog", str(self.root / "catalog.json"),
                   "--repos", str(self.root / "repos.json"),
                   "--output", str(self.root / output)]
        if selected is not None:
            command.extend(["--checks", *selected])
        return command

    def run_pipeline(self, *, mode="run", output="result", selected=None):
        result = subprocess.run(self.command(mode=mode, output=output, selected=selected),
                                capture_output=True, text=True, encoding="utf-8", errors="replace",
                                cwd=self.root, timeout=30)
        report_path = self.root / output / "report.json"
        report = json.loads(report_path.read_text(encoding="utf-8")) if report_path.is_file() else None
        return result, report

    def assert_code(self, code, result):
        self.assertEqual(code, result.returncode, result.stdout + result.stderr)

    def assert_not_eligible(self, report):
        self.assertIsNotNone(report)
        self.assertFalse(report["delivery_eligible"])
        self.assertNotEqual("PASS", report["status"])

    def check_result(self, report, name="ok"):
        return next(item for item in report["checks"] if item["id"] == name)

    def log_text(self, report, name="ok", output="result"):
        log = Path(self.check_result(report, name)["log"])
        if not log.is_absolute():
            log = self.root / output / log
        return log.read_text(encoding="utf-8", errors="replace")

    def test_success_retains_real_log_and_reviewable_delivery(self):
        result, report = self.run_pipeline()
        self.assert_code(0, result)
        self.assertEqual("PASS", report["status"])
        self.assertTrue(report["delivery_eligible"])
        self.assertEqual("PASS", self.check_result(report)["status"])
        self.assertEqual(0, self.check_result(report)["exit_code"])
        self.assertGreaterEqual(self.check_result(report)["elapsed_seconds"], 0)
        self.assertIn("real child executed", self.log_text(report))
        for name in ("delivery.md", "task.json", "catalog.json"):
            self.assertTrue((self.root / "result" / name).is_file(), name)
        saved_task = json.loads((self.root / "result/task.json").read_text(encoding="utf-8"))
        self.assertEqual(self.task, saved_task)

    def test_child_nonzero_fails_and_remaining_check_is_not_run(self):
        self.set_check("ok", "import sys\nprint('expected failure')\nsys.exit(7)\n")
        self.add_check("later", "raise AssertionError('must not execute')\n")
        result, report = self.run_pipeline()
        self.assert_code(1, result)
        self.assert_not_eligible(report)
        self.assertEqual("FAIL", self.check_result(report)["status"])
        self.assertEqual(7, self.check_result(report)["exit_code"])
        self.assertEqual("NOT_RUN", self.check_result(report, "later")["status"])
        self.assertIn("expected failure", self.log_text(report))

    def test_selecting_subset_does_not_satisfy_required_check(self):
        self.add_check("later", "print('second check')\n")
        result, report = self.run_pipeline(selected=["ok"])
        self.assert_code(2, result)
        self.assert_not_eligible(report)
        self.assertEqual("PASS", self.check_result(report)["status"])
        self.assertEqual("NOT_RUN", self.check_result(report, "later")["status"])

    def test_plan_does_not_execute_and_cannot_be_delivered(self):
        marker = self.root / "must not exist"
        self.set_check("ok", f"from pathlib import Path\nPath({str(marker)!r}).touch()\n")
        result, report = self.run_pipeline(mode="plan")
        self.assert_code(0, result)
        self.assert_not_eligible(report)
        self.assertEqual("NOT_RUN", self.check_result(report)["status"])
        self.assertFalse(marker.exists())

    def test_required_explicit_skip_does_not_pass(self):
        self.task["checks"][0]["skip_reason"] = "No dedicated test environment."
        result, report = self.run_pipeline()
        self.assert_code(2, result)
        self.assert_not_eligible(report)
        self.assertEqual("SKIP", self.check_result(report)["status"])

    def test_missing_executable_is_not_run_and_not_deliverable(self):
        self.catalog["checks"]["ok"]["argv"] = ["not-installed-ai-pipeline-fixture-8e7ac294"]
        result, report = self.run_pipeline()
        self.assert_code(2, result)
        self.assert_not_eligible(report)
        self.assertEqual("NOT_RUN", self.check_result(report)["status"])

    def test_timeout_fails_with_bounded_duration(self):
        self.set_check("ok", "import time\nprint('started', flush=True)\ntime.sleep(30)\n", timeout=1)
        started = time.monotonic()
        result, report = self.run_pipeline()
        elapsed = time.monotonic() - started
        self.assert_code(1, result)
        self.assert_not_eligible(report)
        self.assertEqual("FAIL", self.check_result(report)["status"])
        self.assertLess(elapsed, 12)
        self.assertIn("started", self.log_text(report))

    def test_timeout_also_stops_descendant_process(self):
        ready = self.root / "descendant ready"
        heartbeat = self.root / "descendant heartbeat"
        child = self.root / "descendant script.py"
        child.write_text("from pathlib import Path\nimport time\n" +
                         f"Path({str(ready)!r}).touch()\n" +
                         "deadline = time.monotonic() + 5\n" +
                         "while time.monotonic() < deadline:\n" +
                         f"    Path({str(heartbeat)!r}).write_text(str(time.monotonic()))\n" +
                         "    time.sleep(0.05)\n", encoding="utf-8")
        self.set_check("ok", "import subprocess, sys, time\n" +
                       f"subprocess.Popen([sys.executable, {str(child)!r}])\n" +
                       "time.sleep(30)\n", timeout=1)
        result, report = self.run_pipeline()
        self.assert_code(1, result)
        self.assert_not_eligible(report)
        self.assertTrue(ready.exists(), "The descendant was never launched.")
        first = heartbeat.stat().st_mtime_ns
        time.sleep(0.3)
        self.assertEqual(first, heartbeat.stat().st_mtime_ns,
                         "The descendant is still writing after its check timed out.")

    def test_unknown_catalog_check_rejected(self):
        del self.catalog["checks"]["ok"]
        result, _ = self.run_pipeline()
        self.assert_code(2, result)

    def test_unknown_selected_check_rejected(self):
        result, _ = self.run_pipeline(selected=["typo"])
        self.assert_code(2, result)

    def test_duplicate_task_check_rejected(self):
        self.task["checks"].append(copy.deepcopy(self.task["checks"][0]))
        result, _ = self.run_pipeline()
        self.assert_code(2, result)

    def test_empty_acceptance_rejected(self):
        self.task["acceptance"] = []
        result, _ = self.run_pipeline()
        self.assert_code(2, result)

    def test_acceptance_unknown_check_rejected(self):
        self.task["acceptance"][0]["checks"] = ["missing"]
        result, _ = self.run_pipeline()
        self.assert_code(2, result)

    def test_unknown_template_variable_rejected_without_execution(self):
        self.catalog["checks"]["ok"]["argv"].append("{tool.not_configured}")
        result, _ = self.run_pipeline()
        self.assert_code(2, result)

    def test_null_verifier_is_a_configuration_error(self):
        self.catalog["checks"]["ok"]["verifier"] = None
        result, _ = self.run_pipeline()
        self.assert_code(2, result)

    def test_inputs_must_be_an_array(self):
        for index, value in enumerate(("source.txt", None, {"path": "source.txt"})):
            with self.subTest(value=value):
                self.catalog["checks"]["ok"]["inputs"] = value
                result, _ = self.run_pipeline(output=f"inputs-{index}")
                self.assert_code(2, result)

    def test_delivery_notes_must_be_text(self):
        self.task["delivery"]["notes"] = 42
        result, _ = self.run_pipeline()
        self.assert_code(2, result)

    def test_empty_source_scope_is_a_configuration_error(self):
        self.task["scope"]["server_cpp"] = []
        result, _ = self.run_pipeline()
        self.assert_code(2, result)

    def test_source_scope_matching_no_files_is_refused_before_execution(self):
        marker = self.root / "must not run with empty scope"
        self.task["scope"]["server_cpp"] = ["nonexistent-file.txt"]
        self.set_check("ok", f"from pathlib import Path\nPath({str(marker)!r}).touch()\n")
        self.catalog["checks"]["ok"]["inputs"] = []
        result, report = self.run_pipeline()
        self.assert_code(2, result)
        if report is not None:
            self.assert_not_eligible(report)
        self.assertFalse(marker.exists())

    def test_existing_output_directory_is_not_overwritten(self):
        output = self.root / "result"
        output.mkdir()
        sentinel = output / "report.json"
        sentinel.write_text("original evidence", encoding="utf-8")
        result = subprocess.run(self.command(), capture_output=True, text=True,
                                encoding="utf-8", errors="replace", timeout=30)
        self.assert_code(2, result)
        self.assertEqual("original evidence", sentinel.read_text(encoding="utf-8"))

    def test_arguments_with_spaces_and_shell_metacharacters_remain_literal(self):
        argument = "space ; & $(echo injected) `echo nope` | > < %PATH%"
        self.set_check("ok", "import sys\nprint(sys.argv[1])\n")
        self.catalog["checks"]["ok"]["argv"].append(argument)
        result, report = self.run_pipeline()
        self.assert_code(0, result)
        self.assertIn(argument, self.log_text(report))

    def test_declared_source_mutation_invalidates_delivery(self):
        self.set_check("ok", "from pathlib import Path\nPath('source.txt').write_text('changed', encoding='utf-8')\n")
        result, report = self.run_pipeline()
        self.assertNotEqual(0, result.returncode, result.stdout + result.stderr)
        self.assert_not_eligible(report)

    def test_declared_source_deletion_invalidates_delivery(self):
        self.set_check("ok", "from pathlib import Path\nPath('source.txt').unlink()\n")
        result, report = self.run_pipeline()
        self.assertNotEqual(0, result.returncode, result.stdout + result.stderr)
        self.assert_not_eligible(report)

    def test_unrelated_workspace_change_does_not_invalidate_scoped_verification(self):
        self.set_check("ok", "from pathlib import Path\nPath('unrelated.txt').write_text('other task', encoding='utf-8')\n")
        result, report = self.run_pipeline()
        self.assert_code(0, result)
        self.assertTrue(report["delivery_eligible"])

    def test_new_untracked_file_in_declared_directory_invalidates_delivery(self):
        (self.repo / "src").mkdir()
        (self.repo / "src/base.txt").write_text("baseline", encoding="utf-8")
        self.task["scope"]["server_cpp"] = ["src"]
        self.catalog["checks"]["ok"]["inputs"] = ["src"]
        self.set_check("ok", "from pathlib import Path\nPath('src/new.txt').write_text('new source', encoding='utf-8')\n")
        self.catalog["checks"]["ok"]["inputs"] = ["src"]
        result, report = self.run_pipeline()
        self.assertNotEqual(0, result.returncode, result.stdout + result.stderr)
        self.assert_not_eligible(report)

    def test_unittest_real_success_has_positive_test_count(self):
        self.set_check("ok", "import unittest\nclass Check(unittest.TestCase):\n    def test_real(self):\n        self.assertEqual(4, 2 + 2)\nunittest.main()\n", verifier={"type": "unittest"})
        result, report = self.run_pipeline()
        self.assert_code(0, result)
        self.assertEqual("PASS", self.check_result(report)["status"])

    def test_unittest_zero_tests_is_failure_even_with_exit_zero(self):
        self.set_check("ok", "import unittest\nunittest.main()\n", verifier={"type": "unittest"})
        result, report = self.run_pipeline()
        self.assert_code(1, result)
        self.assert_not_eligible(report)
        self.assertEqual("FAIL", self.check_result(report)["status"])

    def test_unittest_skip_is_not_a_pass(self):
        self.set_check("ok", "import unittest\nclass Check(unittest.TestCase):\n    @unittest.skip('dependency unavailable')\n    def test_dependency(self):\n        pass\nunittest.main()\n", verifier={"type": "unittest"})
        result, report = self.run_pipeline()
        self.assertNotEqual(0, result.returncode, result.stdout + result.stderr)
        self.assert_not_eligible(report)
        self.assertNotEqual("PASS", self.check_result(report)["status"])

    def junit_check(self, xml):
        check = self.set_check("ok", "from pathlib import Path\nimport sys\nPath(sys.argv[1]).write_text(" + repr(xml) + ", encoding='utf-8')\n", verifier={"type": "junit", "paths": ["{run_dir}/TEST-*.xml"]})
        check["argv"].append("{run_dir}/TEST-check.xml")

    def test_junit_success_requires_executed_cases(self):
        self.junit_check('<testsuite name="real" tests="1" failures="0" errors="0" skipped="0"><testcase classname="Check" name="ok"/></testsuite>')
        result, report = self.run_pipeline()
        self.assert_code(0, result)
        self.assertEqual("PASS", self.check_result(report)["status"])

    def test_junit_zero_tests_is_failure(self):
        self.junit_check('<testsuite tests="0" failures="0" errors="0" skipped="0"/>')
        result, report = self.run_pipeline()
        self.assert_code(1, result)
        self.assert_not_eligible(report)

    def test_junit_positive_count_without_cases_is_not_execution_evidence(self):
        self.junit_check('<testsuite tests="1" failures="0" errors="0" skipped="0"/>')
        result, report = self.run_pipeline()
        self.assert_code(1, result)
        self.assert_not_eligible(report)

    def test_junit_negative_counters_cannot_cancel_failure_or_skip(self):
        cases = (
            '<testsuite tests="1" failures="-1" errors="0" skipped="0"><testcase name="bad"><failure/></testcase></testsuite>',
            '<testsuite tests="1" failures="0" errors="-1" skipped="0"><testcase name="bad"><error/></testcase></testsuite>',
            '<testsuite tests="1" failures="0" errors="0" skipped="-1"><testcase name="skip"><skipped/></testcase></testsuite>',
        )
        for index, xml in enumerate(cases):
            with self.subTest(index=index):
                self.junit_check(xml)
                result, report = self.run_pipeline(output=f"negative-{index}")
                self.assert_code(1, result)
                self.assert_not_eligible(report)

    def test_junit_failure_is_failure_even_when_command_exits_zero(self):
        self.junit_check('<testsuite tests="1" failures="1" errors="0" skipped="0"><testcase name="bad"><failure message="expected"/></testcase></testsuite>')
        result, report = self.run_pipeline()
        self.assert_code(1, result)
        self.assert_not_eligible(report)

    def test_junit_failure_node_cannot_be_hidden_by_success_counters(self):
        self.junit_check('<testsuite tests="1" failures="0" errors="0" skipped="0"><testcase name="bad"><failure message="counter mismatch"/></testcase></testsuite>')
        result, report = self.run_pipeline()
        self.assert_code(1, result)
        self.assert_not_eligible(report)

    def test_junit_skip_node_cannot_be_hidden_by_success_counters(self):
        self.junit_check('<testsuite tests="1" failures="0" errors="0" skipped="0"><testcase name="skip"><skipped/></testcase></testsuite>')
        result, report = self.run_pipeline()
        self.assertNotEqual(0, result.returncode, result.stdout + result.stderr)
        self.assert_not_eligible(report)

    def test_junit_skip_is_not_a_pass(self):
        self.junit_check('<testsuite tests="1" failures="0" errors="0" skipped="1"><testcase name="skip"><skipped/></testcase></testsuite>')
        result, report = self.run_pipeline()
        self.assertNotEqual(0, result.returncode, result.stdout + result.stderr)
        self.assert_not_eligible(report)

    def test_junit_missing_report_is_failure(self):
        self.catalog["checks"]["ok"]["verifier"] = {"type": "junit", "paths": ["{run_dir}/missing.xml"]}
        result, report = self.run_pipeline()
        self.assert_code(1, result)
        self.assert_not_eligible(report)

    def test_junit_invalid_xml_is_failure(self):
        self.junit_check("<testsuite broken")
        result, report = self.run_pipeline()
        self.assert_code(1, result)
        self.assert_not_eligible(report)

    def test_junit_nested_suite_does_not_double_count_failure(self):
        self.junit_check('<testsuites tests="1" failures="0" errors="0"><testsuite tests="1" failures="0" errors="0" skipped="0"><testcase name="ok"/></testsuite></testsuites>')
        result, report = self.run_pipeline()
        self.assert_code(0, result)
        self.assertEqual("PASS", self.check_result(report)["status"])

    def test_repository_lock_rejects_overlap_and_releases_after_completion(self):
        started = self.root / "started.marker"
        release = self.root / "release.marker"
        self.set_check("ok", "from pathlib import Path\nimport time\n" +
                       f"Path({str(started)!r}).touch()\n" +
                       f"while not Path({str(release)!r}).exists():\n    time.sleep(0.05)\n", timeout=15)
        first = subprocess.Popen(self.command(output="first"), cwd=self.root, stdout=subprocess.PIPE,
                                 stderr=subprocess.PIPE, text=True, encoding="utf-8", errors="replace")
        try:
            deadline = time.monotonic() + 10
            while not started.exists() and first.poll() is None and time.monotonic() < deadline:
                time.sleep(0.05)
            self.assertTrue(started.exists(), "The first runner never started its child check.")
            second, report = self.run_pipeline(output="second")
            self.assert_code(2, second)
            if report is not None:
                self.assert_not_eligible(report)
        finally:
            release.touch()
            try:
                stdout, stderr = first.communicate(timeout=20)
            except subprocess.TimeoutExpired:
                first.kill()
                stdout, stderr = first.communicate(timeout=10)
        self.assertEqual(0, first.returncode, stdout + stderr)
        third, report = self.run_pipeline(output="third")
        self.assert_code(0, third)
        self.assertTrue(report["delivery_eligible"])


if __name__ == "__main__":
    unittest.main()
