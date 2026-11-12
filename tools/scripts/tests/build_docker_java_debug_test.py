"""Offline short-workspace and source-provenance regression checks; no build tools."""
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import shutil
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import build_docker_java_debug as builder


class ShortWorkspaceTests(unittest.TestCase):
    def test_deep_evidence_keeps_snapshot_and_toolchain_short_and_recorded(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            output = root / 'build/verification' / ('deep-evidence-' * 6) / ('nested-run-' * 6)
            output.mkdir(parents=True)
            details = {}
            with patch.object(builder, 'REPO_ROOT', root):
                snapshot, toolchain = builder.create_workspace(output, details)
            workspace = snapshot.parent
            self.assertEqual(workspace.parent, root / 'build/jv')
            self.assertRegex(workspace.name, r'^[a-f0-9]{12}$')
            self.assertEqual(snapshot.name, 's')
            self.assertEqual(toolchain, workspace / 't')
            self.assertLess(len(str(snapshot)), len(str(output / 'snapshot')))
            self.assertFalse((output / 'snapshot').exists())
            self.assertFalse((output / 'toolchain').exists())
            receipt = json.loads((output / 'workspace.json').read_text())
            self.assertEqual(receipt, json.loads((workspace / 'owner.json').read_text()))
            self.assertEqual(receipt['sourceRoot'], str(root))
            self.assertEqual(receipt['evidence'], str(output))
            self.assertEqual(receipt['snapshot'], str(snapshot))
            self.assertTrue(receipt['retained'])
            self.assertEqual(details['snapshot'], str(snapshot))
            self.assertTrue(snapshot.is_dir())
            self.assertTrue(toolchain.is_dir())

    def test_concurrent_invocations_get_exclusive_fresh_workspaces(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            outputs = [root / 'build/verification' / str(index) for index in range(8)]
            for output in outputs:
                output.mkdir(parents=True)
            with patch.object(builder, 'REPO_ROOT', root), ThreadPoolExecutor(max_workers=4) as workers:
                results = list(workers.map(lambda output: builder.create_workspace(output, {}), outputs))
            snapshots = [snapshot for snapshot, _ in results]
            self.assertEqual(len(set(snapshots)), len(outputs))
            self.assertTrue(all(snapshot.is_dir() and not list(snapshot.iterdir()) for snapshot in snapshots))
            self.assertTrue(all(toolchain.is_dir() for _, toolchain in results))

    def test_random_collision_refuses_old_directory_and_preserves_its_jar(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            first, second = root / 'first-evidence', root / 'second-evidence'
            first.mkdir(); second.mkdir()
            with patch.object(builder, 'REPO_ROOT', root), patch.object(builder.uuid, 'uuid4', return_value=SimpleNamespace(hex='a' * 32)):
                snapshot, toolchain = builder.create_workspace(first, {})
                old = snapshot / 'target/service.jar'
                old.parent.mkdir()
                old.write_bytes(b'OLD_JAR_MUST_NEVER_BE_ADOPTED')
                with self.assertRaises(FileExistsError):
                    builder.create_workspace(second, {})
            self.assertEqual(old.read_bytes(), b'OLD_JAR_MUST_NEVER_BE_ADOPTED')
            self.assertTrue(toolchain.is_dir())
            self.assertFalse((second / 'workspace.json').exists())

    def test_source_manifest_keys_remain_repo_relative_at_short_snapshot(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            java = root / 'java'
            java.mkdir()
            (java / 'pom.xml').write_text('<project xmlns="http://maven.apache.org/POM/4.0.0"><modules>'
                                        '<module>authority_jdbc</module><module>gateway_node</module></modules></project>')
            for module in builder.REACTOR_MODULES:
                source = java / module / 'src/main/java/Example.java'
                source.parent.mkdir(parents=True)
                source.write_text('class Example {}')
                (java / module / 'pom.xml').write_text('<project/>')
            proto = root / builder.PROTO_ROOT
            proto.parent.mkdir(parents=True)
            proto.write_text('syntax = "proto3";')
            output = root / 'build/verification/deep/evidence'
            output.mkdir(parents=True)
            with patch.object(builder, 'REPO_ROOT', root):
                before = builder.source_files()
                snapshot, _ = builder.create_workspace(output, {})
                shutil.copytree(java, snapshot / 'java')
                shutil.copytree(root / 'proto', snapshot / 'proto')
                builder.assert_inputs(before, snapshot)
                self.assertEqual(builder.snapshot_files(snapshot), before)
                self.assertTrue(all(not Path(name).is_absolute() and not name.startswith('build/') for name in before))
                self.assertIn('java/authority_jdbc/src/main/java/Example.java', before)
                (snapshot / 'java/authority_jdbc/src/main/java/Example.java').write_text('class Modified {}')
                with self.assertRaisesRegex(RuntimeError, 'Sealed Maven inputs changed'):
                    builder.assert_inputs(before, snapshot)
                self.assertTrue(snapshot.exists())
                self.assertTrue((output / 'workspace.json').exists())


if __name__ == '__main__':
    unittest.main()
