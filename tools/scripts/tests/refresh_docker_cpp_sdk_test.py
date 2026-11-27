"""Offline unit tests for refresh_docker_cpp_sdk.py; no Docker or network calls."""

from contextlib import ExitStack, redirect_stderr, redirect_stdout
import hashlib
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import build_docker_cpp_debug as build_cpp
import refresh_docker_cpp_sdk as subject


SAMPLE_DOCKERFILE = """\
FROM gcc:13.4.0@sha256:3617a214e52a25bde5375dc9503b5e67f01b6c7322a30137e2790aa8e6db5d1f AS deps
WORKDIR /src
COPY third_party/protobuf/ third_party/protobuf/
COPY third_party/abseil-cpp/ third_party/abseil-cpp/
COPY third_party/boringssl/ third_party/boringssl/
COPY tools/scripts/third_party/build_linux_sdk.sh tools/scripts/third_party/build_linux_sdk.sh
COPY tools/archived/setup_dependencies.sh tools/archived/setup_dependencies.sh
COPY tools/archived/muduo_linux_overlay/ \\
    tools/archived/muduo_linux_overlay/
FROM deps AS builder
COPY cpp/ cpp/
"""


class RefreshDockerCppSdkTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.toolchain = self.root / "build/docker-debug-current/cpp-toolchain.json"
        self.recipe = self.root / "deploy/k8s/Dockerfile.cpp"
        self.verifier = self.root / "tools/scripts/verify_docker_cpp_sdk.sh"

        self._write("deploy/k8s/Dockerfile.cpp", SAMPLE_DOCKERFILE)
        self._write(".dockerignore", "build/\n")
        self._write("tools/scripts/third_party/build_linux_sdk.sh", "#!/bin/bash\n")
        self._write("tools/archived/setup_dependencies.sh", "#!/bin/bash\n")
        self._write("tools/archived/muduo_linux_overlay/muduo/net/Acceptor.cc", "int a = 1;\n")
        self._write("tools/scripts/verify_docker_cpp_sdk.sh", "#!/usr/bin/env bash\nexit 0\n")
        self._write("third_party/protobuf/a.txt", "pb\n")
        self._write("third_party/abseil-cpp/b.txt", "absl\n")
        self._write("third_party/boringssl/c.txt", "bssl\n")

        self.dep_files = {
            "third_party/protobuf/a.txt": self._sha("third_party/protobuf/a.txt"),
            "third_party/abseil-cpp/b.txt": self._sha("third_party/abseil-cpp/b.txt"),
            "third_party/boringssl/c.txt": self._sha("third_party/boringssl/c.txt"),
        }
        self.repos = [
            {"path": p, "head": "1" * 40, "gitlinks": {}}
            for p in subject.INDEPENDENT_REPOS
        ]

        self.stack = ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.object(subject, "REPO", self.root))
        self.stack.enter_context(patch.object(subject, "TOOLCHAIN", self.toolchain))
        self.stack.enter_context(patch.object(subject, "RECIPE", self.recipe))
        self.stack.enter_context(patch.object(subject, "VERIFIER", self.verifier))
        self.stack.enter_context(patch.object(build_cpp, "REPO", self.root))
        self.stack.enter_context(patch.object(build_cpp, "TOOLCHAIN", self.toolchain))
        self.stack.enter_context(patch.object(subject, "collect_repositories", return_value=self.repos))
        self.stack.enter_context(patch.object(subject, "dependency_files", side_effect=lambda *_a, **_kw: dict(self.dep_files)))
        self.stack.enter_context(patch.object(build_cpp, "dependency_files", side_effect=lambda *_a, **_kw: dict(self.dep_files)))

    def _write(self, relative: str, content: str) -> Path:
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(content.encode("utf-8"))
        return path

    def _sha(self, relative: str) -> str:
        return hashlib.sha256((self.root / relative).read_bytes()).hexdigest()

    def test_parse_deps_recipe_handles_continuations_and_excludes_builder(self):
        base, roots = subject.parse_deps_recipe(self.recipe)
        self.assertEqual(
            base,
            "gcc:13.4.0@sha256:3617a214e52a25bde5375dc9503b5e67f01b6c7322a30137e2790aa8e6db5d1f",
        )
        self.assertIn("tools/archived/muduo_linux_overlay", roots)
        self.assertIn("deploy/k8s/Dockerfile.cpp", roots)
        self.assertIn(".dockerignore", roots)
        self.assertNotIn("cpp", roots)
        for independent in subject.INDEPENDENT_REPOS:
            self.assertNotIn(independent, roots)

    def test_check_reports_drift_without_modifying_toolchain_or_calling_docker(self):
        stdout = io.StringIO()
        with patch.object(build_cpp, "image_id", side_effect=AssertionError("docker should not run")), redirect_stdout(stdout):
            code = subject.main(["--check"])
        self.assertEqual(code, 1)
        self.assertIn("SDK refresh required", stdout.getvalue())
        self.assertFalse(self.toolchain.exists())

    def test_full_refresh_publishes_valid_toolchain_and_cleans_context(self):
        new_id = "sha256:" + "a" * 64
        pinned_ref = f"{subject.SDK_REPOSITORY}:pin-{'a' * 64}"

        def fake_build(context, tag, iid_file, log_path, profile, jobs):
            self.assertEqual(profile, "debug-compile-only")
            self.assertEqual(jobs, 2)
            self.assertTrue((context / "tools/archived/muduo_linux_overlay/muduo/net/Acceptor.cc").is_file())
            self.assertTrue((context / "third_party/protobuf/a.txt").is_file())
            log_path.write_text("build ok\n", encoding="utf-8")
            iid_file.write_text(new_id + "\n", encoding="utf-8")
            return new_id

        def fake_qualify(identity, profile, verifier_path, log_path):
            self.assertEqual(identity, new_id)
            log_path.write_text("qualify ok\n", encoding="utf-8")
            return hashlib.sha256(verifier_path.read_bytes()).hexdigest()

        stdout = io.StringIO()
        with (
            patch.object(build_cpp, "image_id", side_effect=lambda ref: new_id if ref == pinned_ref else "sha256:" + "b" * 64),
            patch.object(build_cpp, "pin", return_value=(pinned_ref, new_id)),
            patch.object(subject, "build_sdk_image", side_effect=fake_build),
            patch.object(subject, "qualify_sdk_image", side_effect=fake_qualify),
            redirect_stdout(stdout),
        ):
            code = subject.main(["--jobs", "2"])

        self.assertEqual(code, 0, stdout.getvalue())
        self.assertTrue(self.toolchain.is_file())
        published = json.loads(self.toolchain.read_text(encoding="utf-8"))
        self.assertEqual(published["schema"], 2)
        self.assertEqual(published["image"], pinned_ref)
        self.assertEqual(published["imageId"], new_id)
        self.assertEqual(published["qualification"]["buildProfile"], "debug-compile-only")

        # 再次运行应识别已最新并跳过构建。
        stdout2 = io.StringIO()
        with (
            patch.object(build_cpp, "image_id", return_value=new_id),
            patch.object(subject, "build_sdk_image", side_effect=AssertionError("should skip rebuild")),
            redirect_stdout(stdout2),
        ):
            self.assertEqual(subject.main(["--check"]), 0)
            self.assertEqual(subject.main([]), 0)

        # 默认清理冻结上下文目录，仅保留审计证据。
        evidence_dirs = list((self.root / "build/verification").glob("docker-sdk-refresh-*"))
        self.assertEqual(len(evidence_dirs), 1)
        self.assertFalse((evidence_dirs[0] / "sdk-context").exists())
        self.assertTrue((evidence_dirs[0] / "result.json").is_file())

    def test_host_mutation_during_build_blocks_publish(self):
        new_id = "sha256:" + "c" * 64

        def mutating_build(context, tag, iid_file, log_path, profile, jobs):
            self._write("tools/archived/muduo_linux_overlay/muduo/net/Acceptor.cc", "int a = 2;\n")
            log_path.write_text("build ok\n", encoding="utf-8")
            iid_file.write_text(new_id + "\n", encoding="utf-8")
            return new_id

        def fake_qualify(identity, profile, verifier_path, log_path):
            log_path.write_text("qualify ok\n", encoding="utf-8")
            return "0" * 64

        stderr = io.StringIO()
        with (
            patch.object(build_cpp, "image_id", return_value="sha256:" + "b" * 64),
            patch.object(build_cpp, "pin", side_effect=AssertionError("must not pin")),
            patch.object(subject, "build_sdk_image", side_effect=mutating_build),
            patch.object(subject, "qualify_sdk_image", side_effect=fake_qualify),
            redirect_stderr(stderr),
        ):
            code = subject.main([])

        self.assertEqual(code, 1)
        self.assertIn("verify-host-unchanged", stderr.getvalue())
        self.assertFalse(self.toolchain.exists())

    def test_qualification_failure_blocks_publish(self):
        new_id = "sha256:" + "d" * 64

        def fake_build(context, tag, iid_file, log_path, profile, jobs):
            log_path.write_text("build ok\n", encoding="utf-8")
            iid_file.write_text(new_id + "\n", encoding="utf-8")
            return new_id

        stderr = io.StringIO()
        with (
            patch.object(build_cpp, "image_id", return_value="sha256:" + "b" * 64),
            patch.object(build_cpp, "pin", side_effect=AssertionError("must not pin")),
            patch.object(subject, "build_sdk_image", side_effect=fake_build),
            patch.object(subject, "qualify_sdk_image", side_effect=RuntimeError("verify failed")),
            redirect_stderr(stderr),
        ):
            code = subject.main([])

        self.assertEqual(code, 1)
        self.assertIn("[qualify]", stderr.getvalue())
        self.assertFalse(self.toolchain.exists())

    def test_qualify_rejects_crlf_verifier_script(self):
        self._write("tools/scripts/verify_docker_cpp_sdk.sh", "#!/usr/bin/env bash\r\nexit 0\r\n")
        with self.assertRaisesRegex(RuntimeError, "LF line endings"):
            subject.qualify_sdk_image("sha256:" + "e" * 64, "debug-compile-only", self.verifier, self.root / "install.log")


if __name__ == "__main__":
    unittest.main()
