"""Offline refresh regression tests: synthetic environments, temporary output, no Docker."""
from contextlib import ExitStack
import importlib.util
import json
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[1] / "prepare_docker_debug.py"
SPEC = importlib.util.spec_from_file_location("prepare_refresh_under_test", SCRIPT)
prepare = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(prepare)
SOURCE_REPO = prepare.REPO


class RefreshTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.state = self.root / "build/docker-debug-current"
        self.run = self.state / "batches/existing"
        self.run.mkdir(parents=True)
        self.values = {name: f"{index:064x}" for index, name in enumerate(prepare.SECRET_NAMES, 1)}
        self.values.update(DEBUG_RUN_DIR=self.run.as_posix(), COMPOSE_PROJECT_NAME="existing-project",
            DEBUG_SUBNET="172.29.220.0/24", GATEWAY_CLIENT_PORT="19081", GATEWAY_PORT="19082",
            GATE_PORT="29000", BATTLE_PORT="29010", DEBUG_DATA_VOLUME_PREFIX="existing-data",
            KAFKA_CLUSTER_ID="synthetic-cluster", NODE_DEBUG_IMAGE="local/nodes:existing",
            INTERNAL_AUTH_SECRET="retired-synthetic-value", CUSTOM_SECRET="keep-synthetic-value")
        self.write_env()
        (self.run / ".env").write_text("MYSQL_ROOT_PASSWORD=obsolete-batch-value\n", encoding="utf-8")
        self.repo_patch = patch.object(prepare, "REPO", self.root)
        self.repo_patch.start()
        self.addCleanup(self.repo_patch.stop)

    def write_env(self):
        (self.state / ".env").write_text("".join(f"{k}={v}\n" for k, v in self.values.items()), encoding="utf-8")

    def test_canonical_values_win_over_batch_and_defaults(self):
        args, original = prepare.refresh_existing_options()
        self.assertEqual(args.output, str(self.run.resolve()))
        self.assertEqual(args.gateway_client_port, 19081)
        self.assertEqual(args.gateway_port, 19082)
        self.assertEqual(args.environment, self.values)
        self.assertEqual(original, (self.state / ".env").read_bytes())

    def test_outside_path_duplicate_key_and_missing_password_rejected(self):
        self.values["DEBUG_RUN_DIR"] = self.root.as_posix()
        self.write_env()
        with self.assertRaisesRegex(ValueError, "outside"):
            prepare.refresh_existing_options()
        self.values["DEBUG_RUN_DIR"] = self.run.as_posix()
        self.write_env()
        with (self.state / ".env").open("a", encoding="utf-8") as stream:
            stream.write("GATE_PORT=29999\n")
        with self.assertRaisesRegex(ValueError, "duplicate"):
            prepare.refresh_existing_options()
        del self.values["MYSQL_ROOT_PASSWORD"]
        self.write_env()
        with self.assertRaises(ValueError):
            prepare.refresh_existing_options()

    def test_read_only_options_do_not_generate_additive_keys(self):
        del self.values[prepare.ADDITIVE_SECRET_NAMES[0]]
        self.write_env()
        with patch.object(prepare.secrets, "token_hex", side_effect=AssertionError("random generation")):
            args, _ = prepare.refresh_existing_options()
        self.assertNotIn(prepare.ADDITIVE_SECRET_NAMES[0], args.environment)

    def test_partial_identity_pair_is_rejected(self):
        self.values["INTERNAL_AUTH_SIGNING_KEY"] = "a" * 64
        self.write_env()
        with self.assertRaisesRegex(ValueError, "incomplete"):
            prepare.refresh_existing_options()

    def publication_fixture(self):
        staged = self.root / "staged"
        staged.mkdir()
        (staged / "sample").write_bytes(b"new runtime")
        (staged / ".env").write_bytes(b"new environment")
        (staged / "source-manifest.json").write_bytes(b"{}")
        (self.run / "sample").write_bytes(b"old runtime")
        (self.root / "input").write_bytes(b"original")
        return staged, (self.state / ".env").read_bytes(), prepare.source_hashes(["input"])

    def test_late_source_or_control_drift_restores_runtime(self):
        for drift in ("source", "canonical"):
            with self.subTest(drift=drift):
                staged, original, hashes = self.publication_fixture()
                copyfile = shutil.copyfile
                inode = (self.run / "sample").stat().st_ino
                def copy_and_change(source, target):
                    result = copyfile(source, target)
                    if source == staged / "sample":
                        changed = self.root / "input" if drift == "source" else self.state / ".env"
                        changed.write_bytes(b"concurrent change")
                    return result
                with patch.object(prepare.shutil, "copyfile", side_effect=copy_and_change):
                    with self.assertRaisesRegex(ValueError, "changed before refresh commit"):
                        prepare.publish_refresh(staged, self.run, original, ["input"], hashes)
                self.assertEqual((self.run / "sample").read_bytes(), b"old runtime")
                self.assertEqual((self.run / "sample").stat().st_ino, inode)
                self.assertEqual((self.state / ".env").read_bytes(),
                                 original if drift == "source" else b"concurrent change")
                self.assertFalse(list(self.run.parent.glob(".refresh-recovery-*")))
                shutil.rmtree(staged)
                self.write_env()

    def test_copy_failure_restores_partially_written_file(self):
        staged, original, hashes = self.publication_fixture()
        copyfile = shutil.copyfile
        def fail_copy(source, target):
            if source == staged / "sample":
                target.write_bytes(b"partial")
                raise OSError("synthetic copy failure")
            return copyfile(source, target)
        with patch.object(prepare.shutil, "copyfile", side_effect=fail_copy):
            with self.assertRaisesRegex(OSError, "synthetic"):
                prepare.publish_refresh(staged, self.run, original, ["input"], hashes)
        self.assertEqual((self.run / "sample").read_bytes(), b"old runtime")
        self.assertEqual((self.state / ".env").read_bytes(), original)
        self.assertFalse(list(self.run.parent.glob(".refresh-recovery-*")))

    def test_failed_rollback_retains_private_backups_and_receipt(self):
        staged, original, hashes = self.publication_fixture()
        copyfile = shutil.copyfile
        def fail_copy(source, target):
            if target == self.run / "sample":
                target.write_bytes(b"partial")
                raise OSError("synthetic failure")
            return copyfile(source, target)
        with patch.object(prepare.shutil, "copyfile", side_effect=fail_copy):
            with self.assertRaisesRegex(RuntimeError, "rollback incomplete"):
                prepare.publish_refresh(staged, self.run, original, ["input"], hashes)
        recoveries = list(self.run.parent.glob(".refresh-recovery-*"))
        self.assertEqual(len(recoveries), 1)
        receipt = json.loads((recoveries[0] / "recovery.json").read_text())
        self.assertEqual(receipt["status"], "rollback-incomplete")
        self.assertEqual(Path(receipt["backups"][str(self.run / "sample")]).read_bytes(), b"old runtime")
        self.assertNotIn(self.values["MYSQL_ROOT_PASSWORD"], (recoveries[0] / "recovery.json").read_text())

    def test_publish_rejects_source_or_control_drift_without_touching_runtime(self):
        staged = self.root / "staged"
        staged.mkdir()
        (staged / "sample").write_text("new")
        source = self.root / "input"
        source.write_text("original")
        original = (self.state / ".env").read_bytes()
        hashes = prepare.source_hashes(["input"])
        source.write_text("changed")
        with self.assertRaisesRegex(ValueError, "Source inputs changed"):
            prepare.publish_refresh(staged, self.run, original, ["input"], hashes)
        self.assertFalse((self.run / "sample").exists())
        source.write_text("original")
        (self.state / ".env").write_text("concurrent edit")
        with self.assertRaisesRegex(ValueError, "Canonical environment changed"):
            prepare.publish_refresh(staged, self.run, original, ["input"], hashes)
        self.assertFalse((self.run / "sample").exists())

    def test_full_sandbox_render_preserves_settings_and_refreshes_both_manifests(self):
        self.values.update(INTERNAL_AUTH_SIGNING_KEY="a" * 64, INTERNAL_AUTH_PUBLIC_KEYS="b" * 64)
        self.write_env()
        sources = [f"go/{service}/etc/{filename}" for service, filename in prepare.GO_CONFIGS.items()]
        sources += ["bin/etc/base_deploy_config.yaml", "bin/etc/game_config.yaml",
                    "bin/etc/battle_rewards.json", "deploy/mysql-init/00_init_zone_dbs.sql",
                    "tools/scripts/prepare_docker_debug.py", *prepare.SQL_BOOT_SOURCES]
        for name in sources:
            target = self.root / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(SOURCE_REPO / name, target)
        args, original = prepare.refresh_existing_options()
        with ExitStack() as stack, patch.object(prepare, "check_subnet") as network, \
                patch.object(prepare.subprocess, "run") as command, patch("builtins.print"):
            command.return_value.returncode = 0
            prepare.prepare(args, stack, original)
        network.assert_called_once()
        self.assertEqual(command.call_count, 1)
        self.assertEqual(command.call_args.args[0][:2], ["git", "check-ignore"])
        actual = prepare.read_environment(self.state / ".env")
        for key, value in self.values.items():
            if key != "INTERNAL_AUTH_SECRET":
                self.assertEqual(actual[key], value, key)
        self.assertNotIn("INTERNAL_AUTH_SECRET", actual)
        for key, value in prepare.DEV_INTERNAL_IDENTITY.items():
            self.assertEqual(actual[key], self.values[key])
        for name in (".env", "source-manifest.json"):
            self.assertEqual((self.state / name).read_bytes(), (self.run / name).read_bytes())
        manifest = json.loads((self.state / "source-manifest.json").read_text())
        renderer = "tools/scripts/prepare_docker_debug.py"
        self.assertEqual(manifest[renderer], prepare.source_hashes([renderer])[renderer])
        self.assertEqual(manifest, prepare.source_hashes(list(manifest)))
        self.assertFalse((self.state / "config").exists())
        self.assertTrue((self.run / "config/base_deploy_config.yaml").is_file())


if __name__ == "__main__":
    unittest.main()
