"""本机手绘地图部署编排的离线门禁；不访问 Docker、SQL 或真实 .env。"""

import copy
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import deploy_authored_local as subject


class DeployAuthoredLocalTests(unittest.TestCase):
    def setUp(self):
        self.real_run_up = subject.run_up
        self.real_scene_logs = subject.scene_navigation_log
        self.real_navigation_check = subject.verify_navigation_runtime
        self.real_preflight = subject.preflight
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.build_root = self.root / 'build'
        self.build_root.mkdir()
        self.env = self.root / 'state' / '.env'
        self.env.parent.mkdir()
        self.env.write_text('NODE_DEBUG_IMAGE=old\nPRIVATE=never-print-this\n')
        self.build_file = self.build_root / 'result.json'
        self.manifest = self.build_root / 'runtime.json'
        self.overlay = self.build_root / 'compose.json'
        for p in (self.build_file, self.manifest, self.overlay):
            p.write_text('{}')
        self.before = {n: dict(id='old-' + n, imageId='image-' + n, running=n != 'login-sql-boot')
                       for n in subject.TARGETS | {'db'}}
        self.after = copy.deepcopy(self.before)
        for n in subject.TARGETS:
            self.after[n]['id'] = 'new-' + n
            if n in subject.rebuild.CPP:
                self.after[n]['imageId'] = 'new-cpp-image'
        self.build = dict(images={n: dict(image='local/node:new', imageId='new-cpp-image') for n in subject.rebuild.CPP})
        self.frozen = dict(envSha256=subject.rebuild.digest(self.env),
                           runtimeDataManifest=dict(sha256=subject.rebuild.digest(self.manifest)),
                           overlay=dict(sha256=subject.rebuild.digest(self.overlay)),
                           build=dict(sha256=subject.rebuild.digest(self.build_file), sourceManifestPath=str(self.build_file),
                                      sourceManifestSha256=subject.rebuild.digest(self.build_file)),
                           reusedImages={n: dict(imageId=self.before[n]['imageId']) for n in subject.TARGETS - set(subject.rebuild.CPP)})
        self.addCleanup(patch.stopall)
        patch.object(subject, 'ROOT', self.root).start()
        patch.object(subject.rebuild, 'STATE', self.env.parent).start()
        self.process = patch.object(subject.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0)).start()
        self.preflight = patch.object(subject, 'preflight', return_value=(self.build, {}, self.before, self.frozen)).start()
        self.snapshot = patch.object(subject, 'snapshot', side_effect=[self.before, self.after]).start()
        self.outside_before = {'rea-dkbi-existing-project': dict(id='outside-id', imageId='outside-image', running=True)}
        self.outside = patch.object(subject, 'outside_snapshot', return_value=self.outside_before).start()
        self.update = patch.object(subject.rebuild, 'update_images').start()
        self.up = patch.object(subject, 'run_up').start()
        self.logs = patch.object(subject, 'scene_navigation_log').start()
        self.check = patch.object(subject.rebuild, 'check', return_value=True).start()
        self.navigation_check = patch.object(subject, 'verify_navigation_runtime').start()
        self.evidence = self.build_root / 'deployment'

    def deploy(self, execute=True):
        return subject.deploy(self.build_file, self.manifest, self.overlay, self.evidence, execute)

    def test_preflight_default_never_updates_or_recreates(self):
        result = self.deploy(False)
        self.assertEqual(result['status'], 'PREFLIGHT_ONLY')
        self.update.assert_not_called()
        self.up.assert_not_called()
        self.assertFalse((self.evidence / '.env.before.private').exists())

    def test_exact_two_phases_reuse_images_and_check_after_navigation(self):
        events = []
        self.up.side_effect = lambda _o, phase, _l: events.append(tuple(phase))
        self.logs.side_effect = lambda *_: events.append('navigation')
        result = self.deploy()
        self.assertTrue(result['success'])
        self.assertEqual(events[:3], [subject.PHASES[0], 'navigation', subject.PHASES[1]])
        self.update.assert_called_once_with(self.build['images'])
        self.check.assert_called_once_with(list(subject.CONSUMERS), self.evidence / 'source-and-runtime-data-check.json', self.manifest)
        self.assertEqual((self.evidence / '.env.before.private').read_bytes(), self.env.read_bytes())
        public = (self.evidence / 'result.json').read_text() + (self.evidence / 'preflight.json').read_text()
        self.assertNotIn('never-print-this', public)

    def test_cas_rejects_concurrent_environment_change_before_update(self):
        self.env.write_text('concurrent update')
        with self.assertRaisesRegex(RuntimeError, 'changed during preflight'):
            self.deploy()
        self.update.assert_not_called()
        self.up.assert_not_called()

    def test_phase_failure_is_preserved_without_second_sql_attempt(self):
        self.up.side_effect = [None, RuntimeError('SQL boot failed')]
        self.snapshot.side_effect = [self.before, self.after]
        with self.assertRaisesRegex(RuntimeError, 'SQL boot failed'):
            self.deploy()
        self.assertEqual(self.up.call_count, 2)
        result = json.loads((self.evidence / 'result.json').read_text())
        self.assertEqual(result['status'], 'FAILED_PRESERVED')
        self.assertFalse(result['automaticRetry'])
        self.assertTrue(result['containerUpdateStarted'])
        self.assertEqual(len(result['attemptedPhases']), 2)
        self.assertEqual(len(result['completedPhases']), 1)
        self.check.assert_not_called()

    def test_navigation_failure_stops_before_new_world_consumers(self):
        self.logs.side_effect = RuntimeError('missing authored navigation')
        with self.assertRaisesRegex(RuntimeError, 'missing authored navigation'):
            self.deploy()
        self.assertEqual(self.up.call_count, 1)
        self.assertEqual(self.up.call_args.args[1], subject.rebuild.CPP)

    def test_unrelated_container_change_and_source_guard_failure_are_failures(self):
        self.after['db']['id'] = 'concurrently-replaced-db'
        with self.assertRaisesRegex(RuntimeError, 'Unrelated container'):
            self.deploy()
        self.check.assert_not_called()

    def test_run_up_whitelist_and_flags(self):
        # 独立核真实命令生成；唯一子进程仍由本测试桩替代。
        self.real_run_up(self.overlay, subject.PHASES[1], self.build_root / 'up.log')
        command = self.process.call_args.args[0]
        self.assertEqual(command[-4:], list(subject.PHASES[1]))
        for flag in ('--no-build', '--no-deps', '--force-recreate', '--wait'):
            self.assertIn(flag, command)
        self.assertEqual(command[command.index('--pull') + 1], 'never')
        self.assertEqual(command[command.index('--wait-timeout') + 1], '180')
        count = self.process.call_count
        with self.assertRaisesRegex(RuntimeError, 'Unauthorized recreation phase'):
            self.real_run_up(self.overlay, ('db',), self.build_root / 'forbidden.log')
        self.assertEqual(count, self.process.call_count)

    def test_scene_log_cannot_reuse_previous_container_files(self):
        self.evidence.mkdir()
        self.snapshot.side_effect = None
        self.snapshot.return_value = {'scene': dict(id='new-id', hostname='newhost', startedAt='2026-11-12T13:00:00Z')}
        self.process.return_value = subprocess.CompletedProcess([], 0, stdout='', stderr='')
        old = '/app/bin/logs/cpp_nodes/scene.20261111-130000.oldhost.1.log\n'
        with patch.object(subject.rebuild, 'execute', return_value=old) as command, \
                patch.object(subject.time, 'monotonic', side_effect=[0, 100]):
            with self.assertRaisesRegex(RuntimeError, 'do not confirm both authored maps'):
                self.real_scene_logs(self.overlay, self.evidence)
        self.assertEqual(command.call_count, 1)
        self.assertNotIn('tail', command.call_args.args[0])

    def test_scene_log_uses_only_fresh_hostname_file(self):
        self.evidence.mkdir()
        self.snapshot.side_effect = None
        self.snapshot.return_value = {'scene': dict(id='new-id', hostname='newhost', startedAt='2026-11-12T13:00:00Z')}
        self.process.return_value = subprocess.CompletedProcess([], 0, stdout='', stderr='')
        path = '/app/bin/logs/cpp_nodes/scene.20261112-130001.newhost.1.log'
        content = 'nav registered for scene 26000: path\nnav registered for scene 26008: path\n'
        with patch.object(subject.rebuild, 'execute', side_effect=[path + '\n', content]):
            self.real_scene_logs(self.overlay, self.evidence)
        provenance = json.loads((self.evidence / 'scene-log-provenance.json').read_text())
        self.assertEqual(provenance['files'], [path])
        self.assertEqual(provenance['containerId'], 'new-id')

    def test_frozen_build_source_and_navigation_are_required(self):
        nav = {f'{scene}/{name}': 'a' * 64 for scene in (26000, 26008)
               for name in ('scene.bin', 'scene.bin.authored', 'scene.bin.sha256')}
        files = {'data/scene_nav_bin/authored_maps/' + name: sha for name, sha in nav.items()}
        files['cpp/source.cpp'] = 'source-sha'
        meta = self.build_root / 'source_meta' / 'source-manifest.json'
        meta.parent.mkdir()
        manifest = dict(schema=1, language='cpp', files=files,
                        build=dict(sourceSha256=hashlib.sha256(json.dumps(files, sort_keys=True).encode()).hexdigest()))
        meta.write_text(json.dumps(manifest))
        build = dict(success=True, images=self.build['images'], manifestPaths={n: str(meta) for n in subject.rebuild.CPP})
        self.build_file.write_text(json.dumps(build))
        with patch.object(subject.rebuild, 'execute', return_value='new-cpp-image'), \
                patch.object(subject.rebuild, 'current_sources', return_value=files) as source:
            subject.validate_build(self.build_file, dict(navigationSha256=nav))
            source.return_value = {**files, 'cpp/source.cpp': 'changed-source'}
            with self.assertRaisesRegex(RuntimeError, 'inputs changed after frozen build'):
                subject.validate_build(self.build_file, dict(navigationSha256=nav))
            source.return_value = files
            with self.assertRaisesRegex(RuntimeError, 'omits current authored navigation'):
                subject.validate_build(self.build_file, dict(navigationSha256={**nav, '26000/scene.bin': 'b' * 64}))

    def test_postdeployment_source_check_failure_is_not_success(self):
        self.check.return_value = False
        with self.assertRaisesRegex(RuntimeError, 'Source/runtime-data verification failed'):
            self.deploy()
        result = json.loads((self.evidence / 'result.json').read_text())
        self.assertFalse(result['success'])
        self.assertEqual(result['status'], 'FAILED_PRESERVED')
        self.assertEqual(self.up.call_count, 2)

    def test_runtime_navigation_requires_readonly_exact_source_and_sha(self):
        source = self.root / 'navigation'
        data = dict(navigationDirectory=str(source), navigationSha256={'26000/scene.bin': 'a' * 64})
        target = '/app/data/scene_nav_bin/authored_maps'
        mount = dict(Type='bind', RW=False, Destination=target, Source=str(source))
        digest_line = 'a' * 64 + '  ' + target + '/26000/scene.bin\n'
        with patch.object(subject.rebuild, 'execute', side_effect=[json.dumps([mount]), digest_line] * 3):
            self.real_navigation_check(data, self.after)
        with patch.object(subject.rebuild, 'execute', return_value=json.dumps([{**mount, 'RW': True}])):
            with self.assertRaisesRegex(RuntimeError, 'recorded read-only bind'):
                self.real_navigation_check(data, self.after)
        with patch.object(subject.rebuild, 'execute', side_effect=[json.dumps([mount]), digest_line.replace('a' * 64, 'b' * 64)]):
            with self.assertRaisesRegex(RuntimeError, 'SHA mismatch'):
                self.real_navigation_check(data, self.after)

    def test_outside_container_change_is_reported_and_preserved(self):
        changed = {'rea-dkbi-existing-project': dict(id='replaced-outside-id', imageId='outside-image', running=True)}
        self.outside.side_effect = [self.outside_before, self.outside_before, changed, changed]
        with self.assertRaisesRegex(RuntimeError, 'Outside-project container identity changed'):
            self.deploy()
        result = json.loads((self.evidence / 'result.json').read_text())
        self.assertFalse(result['success'])
        self.assertTrue((self.evidence / 'outside-containers-before.json').is_file())
        self.assertTrue((self.evidence / 'outside-containers-after.json').is_file())

    def test_preflight_rejects_environment_change_during_go_source_check(self):
        before = copy.deepcopy(self.before)
        for item in before.values():
            item['project'] = 'xuanming-debug-current'
        self.snapshot.side_effect = None
        self.snapshot.return_value = before
        supporting = subject.TARGETS - set(subject.rebuild.CPP)
        config = dict(services={name: dict(image='ref-' + name) for name in supporting})

        def command(args):
            if 'config' in args:
                return json.dumps(config)
            return before[args[-1].removeprefix('ref-')]['imageId']

        def concurrent(*_):
            self.env.write_text('other window updated configuration')
            return True

        self.check.side_effect = concurrent
        with patch.object(subject.rebuild, 'load_runtime_data_manifest', return_value=dict(services={n: {} for n in subject.CONSUMERS})), \
                patch.object(subject, 'validate_overlay'), patch.object(subject.rebuild, 'check_config_inputs'), \
                patch.object(subject, 'validate_build', return_value=(self.build, {})), \
                patch.object(subject.rebuild, 'execute', side_effect=command):
            with self.assertRaisesRegex(RuntimeError, 'configuration changed during preflight'):
                self.real_preflight(self.build_file, self.manifest, self.overlay)

    def test_stopped_historical_oneoff_is_preserved_separately(self):
        canonical = dict(id='canonical-id', service='db-migrate', oneoff='False', number='1', running=False,
                         workingDir=str(self.root / 'deploy/debug'), configHash='a' * 64,
                         configFiles=','.join(str(self.root / 'deploy/debug' / p) for p in ('compose.game.yml', 'compose.debug.yml')))
        history = {**canonical, 'id': 'history-id', 'oneoff': 'True', 'number': '', 'configHash': 'b' * 64}
        result = subject.partition_snapshot([canonical, history], self.overlay)
        self.assertEqual(set(result), {'db-migrate', 'history:history-id'})
        self.assertEqual(result['history:history-id'], history)
        with self.assertRaisesRegex(RuntimeError, 'Duplicate canonical service'):
            subject.partition_snapshot([canonical, {**canonical, 'id': 'second-regular-id'}], self.overlay)
        with self.assertRaisesRegex(RuntimeError, 'Running one-off'):
            subject.partition_snapshot([canonical, {**history, 'running': True}], self.overlay)


if __name__ == '__main__':
    unittest.main()
