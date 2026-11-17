"""GM Scene 单项前移的离线拒绝测试；不启动 Docker、不写真实配置或玩家数据。"""
from copy import deepcopy
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import gm_scene_promotion as guard


class PromotionTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name).resolve()
        self.out = self.root / guard.RELATIVE
        self.out.mkdir(parents=True)
        self.files = [str(self.root / value) for value in ('deploy/debug/compose.game.yml', 'deploy/debug/compose.debug.yml',
            'build/verification/authored-festival-pet-acquisition-20261116/compose.authored.json',
            'build/verification/container-recovery-20261117/compose.recovery.json')]
        self.old, self.new = 'sha256:' + 'a' * 64, 'sha256:' + 'b' * 64
        self.expected = {'services': {'scene': {'image': self.old, 'pull_policy': 'never'},
            'gate': {'image': self.old, 'pull_policy': 'never'}}, 'volumes': {'saved': {'external': True}}}
        baseline = {'passed': True, 'contract': {'project': 'xuanming-debug-current',
            'ids': {'scene': 'old-scene', 'gate': 'old-gate'}, 'images': {'scene': self.old, 'gate': self.old},
            'signatures': {'scene': 'old-scene-signature', 'gate': 'gate-signature'},
            'composeSha256': {self.files[-1]: 'old-overlay-hash', self.files[0]: 'old-compose-hash'}},
            'states': {'scene': {'startedAt': 'before'}, 'gate': {'startedAt': 'before'}}}
        source = {'schema': 1, 'language': 'cpp', 'files': {'proto/test.proto': 'c' * 64},
            'build': {'type': 'Debug', 'sdkImageId': 'sha256:' + 'd' * 64}}
        source['build']['sourceSha256'] = guard.digest(json.dumps(source['files'], sort_keys=True).encode())
        source_ref = self.save('source.json', source)
        complete = deepcopy(source)
        complete['artifacts'] = {'/app/bin/scene': 'e' * 64}
        build = {'success': True, 'images': {name: {'imageId': self.new} for name in ('scene', 'gate', 'battle')},
                 'manifestPaths': {'scene': source_ref['path']}}
        current = deepcopy(self.expected)
        current['services']['scene']['image'] = self.new
        Path(self.files[-1]).parent.mkdir(parents=True)
        Path(self.files[-1]).write_text(json.dumps(current), encoding='utf-8')
        Path(self.files[0]).parent.mkdir(parents=True)
        Path(self.files[0]).write_text('reviewed compose', encoding='utf-8')
        self.receipt = {'schema': 1, 'project': 'xuanming-debug-current', 'service': 'scene',
            'composeFiles': self.files, 'baseline': self.save('baseline.json', baseline),
            'oldScene': {'id': 'old-scene', 'image': self.old, 'signature': 'old-scene-signature'},
            'oldRecoverySha256': 'old-overlay-hash', 'newRecoverySha256': guard.digest(Path(self.files[-1]).read_bytes()),
            'oldComposeSha256': 'old-compose-hash', 'newComposeSha256': guard.digest(Path(self.files[0]).read_bytes()),
            'buildResult': self.save('build.json', build), 'imageId': self.new, 'sourceManifest': source_ref,
            'imageManifest': self.save('complete.json', complete), 'sourceSha256': source['build']['sourceSha256'],
            'sdkImageId': source['build']['sdkImageId']}
        self.rows = [{'Id': 'new-scene', 'Image': self.new, 'signature': 'new-scene-signature',
            'Config': {'Labels': {'com.docker.compose.service': 'scene'}}},
            {'Id': 'old-gate', 'Image': self.old, 'signature': 'gate-signature',
             'Config': {'Labels': {'com.docker.compose.service': 'gate'}}}]
        self.config = {'services': {'scene': {'labels': {}, 'ports': [{'target': 50001, 'published': '2355',
            'host_ip': '127.0.0.1'}], 'environment': {'MMORPG_GM_PLAYER_COMMAND_SECRET': 'test-only-' * 5,
                                                  'MMORPG_ASSET_OP_SECRET_MAIL': 'test-only-' * 5}}}}
        self.approve()

    def save(self, name, value):
        path = self.out / name
        path.write_text(json.dumps(value), encoding='utf-8')
        return {'path': str(path), 'sha256': guard.digest(path.read_bytes())}

    def approve(self):
        reference = self.save('promotion.json', self.receipt)
        self.values = {guard.KEY: reference['sha256']}
        self.rows[0]['Config']['Labels'][guard.LABEL] = reference['sha256']
        self.config['services']['scene']['labels'][guard.LABEL] = reference['sha256']

    def verify(self):
        return guard.expected_overlay(self.root, self.values, self.expected, self.rows, self.config, lambda row: row['signature'])

    def test_unapproved_receipt_does_not_change_original_contract(self):
        self.assertIs(guard.expected_overlay(self.root, {}, self.expected, [], {}, None), self.expected)

    def test_only_scene_image_can_move_and_original_contract_is_immutable(self):
        result = self.verify()
        self.assertEqual(result['services']['scene']['image'], self.new)
        self.assertEqual(result['services']['gate'], self.expected['services']['gate'])
        self.assertEqual(self.expected['services']['scene']['image'], self.old)

    def test_modified_other_image_or_runtime_is_rejected(self):
        self.rows[1]['Image'] = self.new
        with self.assertRaisesRegex(RuntimeError, 'another service'):
            self.verify()

    def test_modified_other_recovery_image_is_rejected_even_if_digest_is_updated(self):
        raw = json.loads(Path(self.files[-1]).read_bytes())
        raw['services']['gate']['image'] = self.new
        Path(self.files[-1]).write_text(json.dumps(raw), encoding='utf-8')
        self.receipt['newRecoverySha256'] = guard.digest(Path(self.files[-1]).read_bytes())
        self.approve()
        with self.assertRaisesRegex(RuntimeError, 'single approved image'):
            self.verify()

    def test_extra_or_reordered_compose_path_is_rejected(self):
        self.receipt['composeFiles'] = [*self.files, 'fifth-overlay.yml']
        self.approve()
        with self.assertRaisesRegex(RuntimeError, 'provenance'):
            self.verify()

    def test_stale_scene_runtime_identity_is_rejected_before_mutation(self):
        saved = json.loads(Path(self.receipt['baseline']['path']).read_bytes())
        fresh = deepcopy(saved)
        fresh['contract']['ids']['scene'] = 'someone-else-recreated-scene'
        with self.assertRaisesRegex(RuntimeError, 'identity or input changed'):
            guard.same_runtime(saved, fresh)

    def test_another_service_stopping_during_deployment_cannot_sign_ready(self):
        before = {'states': {'scene': {'status': 'running'}, 'gate': {'status': 'running'}}}
        after = {'states': {'scene': {'status': 'running'}, 'gate': {'status': 'exited'}}}
        with self.assertRaisesRegex(RuntimeError, 'Another service changed runtime state'):
            guard.unchanged_other_states(before, after)

    def test_stale_old_scene_identity_cannot_be_relabelled_as_promoted(self):
        self.rows[0]['Id'] = 'old-scene'
        with self.assertRaisesRegex(RuntimeError, 'runtime identity'):
            self.verify()

    def test_forged_receipt_must_match_explicit_approval_digest(self):
        self.receipt['imageId'] = 'sha256:' + 'f' * 64
        self.save('promotion.json', self.receipt)
        with self.assertRaisesRegex(RuntimeError, 'explicit approval'):
            self.verify()

    def test_tampered_build_source_or_artifact_evidence_is_rejected(self):
        for key in ('buildResult', 'sourceManifest', 'imageManifest', 'baseline'):
            path = Path(self.receipt[key]['path'])
            original = path.read_bytes()
            path.write_bytes(original + b' ')
            with self.subTest(key=key), self.assertRaisesRegex(RuntimeError, 'evidence digest'):
                self.verify()
            path.write_bytes(original)

    def test_public_port_or_wrong_grpc_port_is_rejected(self):
        for key, value in (('host_ip', '0.0.0.0'), ('target', 20001)):
            port = self.config['services']['scene']['ports'][0]
            prior = port[key]
            port[key] = value
            with self.subTest(key=key), self.assertRaisesRegex(RuntimeError, 'loopback endpoint'):
                self.verify()
            port[key] = prior

    def test_unreviewed_base_compose_is_rejected(self):
        Path(self.files[0]).write_text('changed compose', encoding='utf-8')
        with self.assertRaisesRegex(RuntimeError, 'base Compose changed'):
            self.verify()


class ReadyRuntimeTest(unittest.TestCase):
    def setUp(self):
        fixture = PromotionTest()
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        self.fixture = fixture
        self.root, self.out = fixture.root, fixture.out
        private = self.out / 'private'
        private.mkdir()
        self.gm, self.asset, self.admin = 'gm-test-only-' * 4, 'asset-test-only-' * 4, 'admin-test-only-' * 4
        self.scene_env = {'MMORPG_GM_PLAYER_COMMAND_SECRET': self.gm, 'MMORPG_ASSET_OP_SECRET_MAIL': self.asset}
        self.runtime_env = {'GM_SCENE_ADMIN_SECRET': self.gm, 'GM_SCENE_TARGETS_JSON': '{"1":"127.0.0.1:2355"}',
                            'GM_MAIL_TARGET': '127.0.0.1:2354', 'GM_MAIL_ADMIN_SECRET': self.admin}
        for name, values in (('scene.env', self.scene_env), ('gm-runtime.env', self.runtime_env)):
            (private / name).write_text(''.join(key + '=' + value + '\n' for key, value in values.items()), encoding='utf-8')
        current = json.loads(Path(fixture.receipt['baseline']['path']).read_bytes())
        current['contract']['ids']['scene'] = 'new-scene'
        current['contract']['images']['scene'] = fixture.new
        self.current = current
        self.mail = {'Id': 'mail-id', 'Name': '/xuanming-gm-mail-mail-1', 'Config': {'Env': [
            'MAIL_PRODUCER_SECRET_ADMIN=' + self.admin, 'MMORPG_ASSET_OP_SECRET_MAIL=' + self.asset]},
            'State': {'Status': 'running', 'Health': {'Status': 'healthy'}}}
        self.ready = {'schema': 1, 'passed': True, 'project': 'xuanming-debug-current',
            'promotion': {'path': str(self.out / 'promotion.json'), 'sha256': fixture.values[guard.KEY]},
            'runtime': fixture.save('runtime-after.json', current),
            'sourceCheck': fixture.save('source-check.json', {'success': True, 'services': [
                {'service': 'scene', 'success': True, 'imageId': fixture.new}]}),
            'sceneImageId': fixture.new, 'sceneContainerId': 'new-scene', 'mailContainerId': 'mail-id',
            'sceneTarget': '127.0.0.1:2355', 'sceneNodeId': 1, 'mailTarget': '127.0.0.1:2354',
            'privateRuntimePath': str(private / 'gm-runtime.env'),
            'privateRuntimeSha256': guard.digest((private / 'gm-runtime.env').read_bytes())}
        read_receipt = self.root / guard.READ_RECEIPT
        read_receipt.parent.mkdir(parents=True)
        read_receipt.write_text('{"passed":true}', encoding='utf-8')
        self.ready['readBridge'] = {'path': str(read_receipt), 'sha256': guard.digest(read_receipt.read_bytes())}
        original = self.root / 'build/docker-debug-current/.env'
        original.parent.mkdir(parents=True)
        original.write_text(guard.KEY + '=' + fixture.values[guard.KEY] + '\n', encoding='utf-8')
        self.seal()

    def seal(self):
        self.fixture.save('ready.json', self.ready)
        (self.out / 'private/ready.sha256').write_text(guard.digest((self.out / 'ready.json').read_bytes()), encoding='ascii')

    def load(self):
        return guard.ready_runtime(self.root, lambda: self.current, lambda _: [self.mail], lambda _: self.scene_env,
                                   lambda: guard.READ_TARGETS.copy())

    def test_complete_current_receipt_loads_only_four_explicit_values(self):
        self.assertEqual(self.load(), self.runtime_env)

    def test_tampered_ready_bytes_are_rejected(self):
        self.ready['sceneTarget'] = 'attacker.example:443'
        self.fixture.save('ready.json', self.ready)
        with self.assertRaisesRegex(RuntimeError, 'ready receipt digest'):
            self.load()

    def test_stale_scene_identity_is_rejected(self):
        self.current = deepcopy(self.current)
        self.current['contract']['ids']['scene'] = 'another-scene'
        with self.assertRaisesRegex(RuntimeError, 'identity or input'):
            self.load()

    def test_missing_or_failed_source_check_is_rejected(self):
        self.ready['sourceCheck'] = self.fixture.save('source-check.json', {'success': False, 'services': []})
        self.seal()
        with self.assertRaisesRegex(RuntimeError, 'artifact verification'):
            self.load()

    def test_replaced_mail_or_changed_process_secret_is_rejected(self):
        self.mail['Id'] = 'replaced-mail'
        with self.assertRaisesRegex(RuntimeError, 'Mail readiness'):
            self.load()
        self.mail['Id'] = 'mail-id'
        self.scene_env['MMORPG_GM_PLAYER_COMMAND_SECRET'] = 'wrong-test-only-secret'
        with self.assertRaisesRegex(RuntimeError, 'runtime secret'):
            self.load()

    def test_tampered_private_runtime_is_rejected(self):
        (self.out / 'private/gm-runtime.env').write_text('GM_SCENE_TARGETS_JSON={}\n', encoding='utf-8')
        with self.assertRaisesRegex(RuntimeError, 'bridge bytes'):
            self.load()

    def test_changed_read_bridge_receipt_cannot_enable_player_commands(self):
        (self.root / guard.READ_RECEIPT).write_text('{"passed":false}', encoding='utf-8')
        with self.assertRaisesRegex(RuntimeError, 'evidence digest'):
            self.load()

    def test_launcher_disables_channels_on_invalid_receipt_and_ignores_inherited_secrets(self):
        import os
        path = Path(__file__).resolve().parents[3] / 'java/gm_admin/run_local.py'
        spec = importlib.util.spec_from_file_location('gm_local_test', path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        with patch.object(guard, 'ready_runtime', side_effect=RuntimeError('test-only-invalid-ready')), \
                patch.object(guard, 'verified_read_bridge', side_effect=RuntimeError('test-only-invalid-read-bridge')):
            self.assertEqual(module.verified_game_runtime(), {})
            with patch.dict(os.environ, {key: 'test-only-inherited-value' for key in (*guard.RUNTIME_KEYS, *guard.READ_TARGETS)}), \
                    patch.object(module, 'gateway_key', return_value=''):
                values = {key: 'test-only-password' * 3 for key in module.SECRET_FIELDS}
                output = module.environment(values)
                self.assertTrue(all(key not in output for key in (*guard.RUNTIME_KEYS, *guard.READ_TARGETS)))
        with patch.object(guard, 'ready_runtime', side_effect=RuntimeError('Scene not deployed')), \
                patch.object(guard, 'verified_read_bridge', return_value=guard.READ_TARGETS.copy()), \
                patch.object(module, 'gateway_key', return_value=''):
            output = module.environment({key: 'test-only-password' * 3 for key in module.SECRET_FIELDS})
            self.assertEqual({key: output[key] for key in guard.READ_TARGETS}, guard.READ_TARGETS)
            self.assertTrue(all(key not in output for key in guard.RUNTIME_KEYS))


class ReviewedFileReplacementTest(unittest.TestCase):
    def setUp(self):
        path = Path(__file__).resolve().parents[3] / 'deploy/gm-admin/scene-deploy.py'
        spec = importlib.util.spec_from_file_location('gm_scene_deploy_test', path)
        self.module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.module)
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.target = Path(temp.name) / 'compose.yml'
        self.target.write_bytes(b'reviewed-old')

    def test_concurrent_edit_before_replacement_is_preserved(self):
        reviewed = self.target.read_bytes()
        self.target.write_bytes(b'other-task-edit')
        with self.assertRaisesRegex(RuntimeError, 'changed before replacement'):
            self.module.replace_exact(self.target, reviewed, b'reviewed-new')
        self.assertEqual(self.target.read_bytes(), b'other-task-edit')

    def test_concurrent_edit_during_temporary_write_is_preserved(self):
        original_open = Path.open
        def racing_open(path, *args, **kwargs):
            stream = original_open(path, *args, **kwargs)
            if path.name.endswith('.gm-scene.tmp'):
                with original_open(self.target, 'wb') as concurrent:
                    concurrent.write(b'other-task-edit')
            return stream
        with patch.object(Path, 'open', racing_open), self.assertRaisesRegex(RuntimeError, 'changed during replacement'):
            self.module.replace_exact(self.target, b'reviewed-old', b'reviewed-new')
        self.assertEqual(self.target.read_bytes(), b'other-task-edit')

    def test_proposed_compose_changes_only_scene_and_contains_no_secret_values(self):
        raw = b'services:\n  gate:\n    image: old\n\n  scene:\n    environment:\n      SCENE_RUN_MODE: dev\n    volumes:\n      - kept\n\n  battle:\n    image: old\n'
        changed = self.module.proposed_compose(raw)
        self.assertTrue(changed.startswith(b'services:\n  gate:\n    image: old\n'))
        self.assertTrue(changed.endswith(b'\n  battle:\n    image: old\n'))
        self.assertIn(b'127.0.0.1:2355:50001', changed)
        self.assertIn(b'${MMORPG_GM_PLAYER_COMMAND_SECRET:', changed)
        self.assertIn(b'${MMORPG_ASSET_OP_SECRET_MAIL:', changed)


class RuntimeTableCoverageTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.tables = Path(temp.name)
        (self.tables / 'world.json').write_text('{"data":[]}', encoding='utf-8')
        self.source = {'generated/tables/World.json': 'test-only-hash',
                       'generated/tables/EquipArtifact.json': 'test-only-hash',
                       'generated/tables/EquipV1Affix.json': 'test-only-hash'}

    def test_missing_required_tables_block_before_any_runtime_write(self):
        before = list(self.tables.iterdir())
        with self.assertRaisesRegex(RuntimeError, 'equipartifact.json, equipv1affix.json.*deployment is blocked'):
            guard.require_table_coverage(self.source, self.tables)
        self.assertEqual(list(self.tables.iterdir()), before)

    def test_source_names_map_to_actual_lowercase_runtime_files(self):
        for name in ('equipartifact.json', 'equipv1affix.json'):
            (self.tables / name).write_text('{"data":[]}', encoding='utf-8')
        guard.require_table_coverage(self.source, self.tables)

    def test_uppercase_host_name_does_not_claim_linux_runtime_coverage(self):
        (self.tables / 'EquipArtifact.json').write_text('{"data":[]}', encoding='utf-8')
        with self.assertRaisesRegex(RuntimeError, 'equipartifact.json'):
            guard.require_table_coverage(self.source, self.tables)

    def test_empty_source_manifest_cannot_claim_table_compatibility(self):
        with self.assertRaisesRegex(RuntimeError, 'no runtime table inputs'):
            guard.require_table_coverage({}, self.tables)


if __name__ == '__main__':
    unittest.main()
