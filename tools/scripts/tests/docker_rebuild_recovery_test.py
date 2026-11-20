"""恢复栈重编的离线合同测试，禁止任何外部进程及真实 Docker 操作。"""
from copy import deepcopy
import json
from pathlib import Path
import subprocess
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import docker_rebuild_recovery as subject
import rebuild_all_docker_debug as rebuild
import start_docker_debug as startup
import gm_scene_promotion as gm
import start_docker_debug_test as recovery_fixture


class RecoveryRebuildTest(unittest.TestCase):
    def setUp(self):
        fixture = recovery_fixture.RecoveryProvenanceTest()
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        self.fixture, self.root, self.rows = fixture, fixture.root, fixture.rows
        self.stack = fixture.stack
        for name in ('run', 'Popen'):
            self.stack.enter_context(patch.object(subprocess, name, side_effect=AssertionError('external process')))
        self.stack.enter_context(patch.object(startup, 'signature', side_effect=lambda row: row['testSignature']))
        compose = [self.root / 'deploy/debug/compose.game.yml', self.root / 'deploy/debug/compose.debug.yml']
        self.stack.enter_context(patch.object(startup, 'COMPOSE_FILES', compose))
        for path in compose:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('reviewed', encoding='utf-8')
        for row in self.rows:
            name = row['Config']['Labels']['com.docker.compose.service']
            row['Image'] = fixture.overlay['services'][name]['image']
            row['testSignature'] = name + '-signature'
            row['Config']['Labels']['com.docker.compose.project.config_files'] = ','.join(map(str, [
                *compose, self.root / 'build/verification/authored-festival-pet-acquisition-20261116/compose.authored.json',
                startup.recovery_path()]))
        self.add_gm_origin()
        _, proof = startup.recovery_contract(self.rows, '{"services":{}}')
        self.baseline = {'services': {r['Config']['Labels']['com.docker.compose.service']: r for r in self.rows},
                         'historical': [], 'authoredOverlay': {'input': '{"services":{}}'},
                         'recoveryOverlay': {'raw': startup.recovery_path().read_bytes(), 'contract': proof}}
        self.out = self.root / 'build/verification/rebuild-test'
        self.out.mkdir()

    def save(self, path, value):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(value), encoding='utf-8')
        return subject.bound(startup, path)

    def add_gm_origin(self):
        scene = next(r for r in self.rows if r['Config']['Labels']['com.docker.compose.service'] == 'scene')
        old = deepcopy(scene)
        files = scene['Config']['Labels']['com.docker.compose.project.config_files'].split(',')
        before = {'passed': True, 'contract': {'project': startup.PROJECT,
            'ids': {r['Config']['Labels']['com.docker.compose.service']: r['Id'] for r in self.rows},
            'images': {r['Config']['Labels']['com.docker.compose.service']: r['Image'] for r in self.rows},
            'signatures': {r['Config']['Labels']['com.docker.compose.service']: r['testSignature'] for r in self.rows},
            'composeSha256': {files[-1]: startup.sha(startup.recovery_path().read_bytes()), files[0]: 'old-compose'}}}
        directory = self.root / gm.RELATIVE
        source = {'schema': 1, 'language': 'cpp', 'files': {'fixture.cpp': 'a' * 64},
                  'build': {'type': 'Debug', 'sdkImageId': 'sha256:' + 'd' * 64}}
        source['build']['sourceSha256'] = startup.sha(json.dumps(source['files'], sort_keys=True).encode())
        source_ref = self.save(directory / 'source.json', source)
        promoted = 'sha256:' + 'e' * 64
        scene['Id'], scene['Image'] = 'promoted-scene', promoted
        self.fixture.overlay['services']['scene']['image'] = promoted
        self.fixture.save_overlay()
        build = {'success': True, 'images': {n: {'imageId': promoted} for n in ('gate', 'scene', 'battle')},
                 'manifestPaths': {'scene': source_ref['path']}}
        receipt = {'schema': 1, 'project': startup.PROJECT, 'service': 'scene', 'composeFiles': files,
            'baseline': self.save(directory / 'before.json', before),
            'oldScene': {'id': old['Id'], 'image': old['Image'], 'signature': old['testSignature']},
            'oldRecoverySha256': before['contract']['composeSha256'][files[-1]],
            'newRecoverySha256': startup.sha(startup.recovery_path().read_bytes()),
            'oldComposeSha256': 'old-compose', 'newComposeSha256': startup.sha(Path(files[0]).read_bytes()),
            'buildResult': self.save(directory / 'build.json', build), 'imageId': promoted,
            'sourceManifest': source_ref, 'imageManifest': self.save(directory / 'image.json',
                {**source, 'artifacts': {'/app/scene': 'b' * 64}}),
            'sourceSha256': source['build']['sourceSha256'], 'sdkImageId': source['build']['sdkImageId']}
        binding = self.save(directory / 'promotion.json', receipt)
        startup.ENV.write_text(gm.KEY + '=' + binding['sha256'] + '\n', encoding='utf-8')
        scene['Config']['Labels'][gm.LABEL] = binding['sha256']
        self.fixture.config['services'] = {'scene': {'labels': {gm.LABEL: binding['sha256']},
            'ports': [{'target': 50001, 'published': '2355', 'host_ip': '127.0.0.1'}],
            'environment': {'MMORPG_GM_PLAYER_COMMAND_SECRET': 'test-only-' * 5,
                            'MMORPG_ASSET_OP_SECRET_MAIL': 'test-only-' * 5}}}

    def build(self, language, suffix='1'):
        directory = self.out / language
        result = {'success': True, 'images': {}, 'manifestPaths': {}}
        for name in rebuild.SERVICES[language]:
            path = directory / (name + '.json')
            self.save(path, {'schema': 1, 'language': language, 'files': {'test': 'a' * 64},
                             'artifacts': {} if language == 'cpp' else {'/app/test': 'b' * 64}})
            result['manifestPaths'][name] = str(path)
            result['images'][name] = {'image': 'local/' + name + ':new', 'imageId': 'sha256:' + suffix * 64}
        self.save(directory / 'result.json', result)
        return result

    def promote(self, languages=('cpp', 'go', 'java')):
        builds = {language: self.build(language) for language in languages}
        raw = subject.prepare(startup, self.baseline, builds, self.out)
        startup.recovery_path().write_bytes(raw)
        for row in self.rows:
            name = row['Config']['Labels']['com.docker.compose.service']
            if name not in startup.INFRA:
                row['Id'] = 'rebuilt-' + name
                row['Image'] = json.loads(raw)['services'][name]['image']
        return raw

    def test_full_rebuild_accepts_new_images_after_gm_and_preserves_anonymous_volumes(self):
        raw = self.promote()
        combined, proof = startup.recovery_contract(self.rows, '{"services":{}}')
        self.assertEqual(json.loads(combined)['services']['scene']['image'], 'sha256:' + '1' * 64)
        self.assertEqual(proof['anonymousVolumes'], self.baseline['recoveryOverlay']['contract']['anonymousVolumes'])
        self.assertEqual(json.loads(raw)['volumes'], self.fixture.overlay['volumes'])
        self.assertEqual(set(startup.validate_service_inventory(self.rows, startup.HISTORY_ONLY)['missingServices']),
                         startup.HISTORY_ONLY)

    def test_go_only_does_not_advance_gateway_registration(self):
        raw = self.promote(('go',))
        result = json.loads(raw)
        self.assertEqual(result['services']['gateway-sql-boot'], self.fixture.overlay['services']['gateway-sql-boot'])
        self.assertEqual(result['services']['db-sql-boot']['image'], 'sha256:' + '1' * 64)

    def test_second_partial_rebuild_keeps_previous_language_and_origin_proof(self):
        raw = self.promote()
        self.baseline['recoveryOverlay']['raw'] = raw
        self.out = self.root / 'build/verification/rebuild-second'
        self.out.mkdir()
        built = self.build('cpp', '2')
        updated = subject.prepare(startup, self.baseline, {'cpp': built}, self.out)
        startup.recovery_path().write_bytes(updated)
        combined, _ = startup.recovery_contract(self.rows, '{"services":{}}')
        resolved = json.loads(combined)
        self.assertEqual(resolved['services']['scene']['image'], 'sha256:' + '2' * 64)
        self.assertEqual(resolved['services']['gateway']['image'], 'sha256:' + '1' * 64)
        self.assertEqual(json.loads(updated)[subject.EXTENSION]['origin'], json.loads(raw)[subject.EXTENSION]['origin'])

    def test_modified_source_receipt_or_gm_proof_is_rejected(self):
        raw = self.promote()
        for path in (self.out / 'cpp/gate.json', self.root / gm.RELATIVE / 'source.json'):
            original = path.read_bytes()
            path.write_bytes(original + b' ')
            with self.subTest(path=path.name), self.assertRaises(RuntimeError):
                startup.recovery_contract(self.rows, '{"services":{}}')
            path.write_bytes(original)

    def test_volume_or_infra_substitution_is_rejected(self):
        raw = self.promote()
        document = json.loads(raw)
        document['volumes']['mysql-data']['name'] = 'empty'
        startup.recovery_path().write_text(json.dumps(document), encoding='utf-8')
        with self.assertRaisesRegex(RuntimeError, 'more than approved images'):
            startup.recovery_contract(self.rows, '{"services":{}}')
        startup.recovery_path().write_bytes(raw)
        self.baseline['services']['mysql']['Id'] = 'replacement'
        with self.assertRaisesRegex(RuntimeError, 'infrastructure identity'):
            startup.recovery_contract(self.rows, '{"services":{}}')

    def test_missing_live_service_cannot_use_history_exception(self):
        self.stack.enter_context(patch.object(startup, 'authored_overlay', return_value=(None, None, '{"services":{}}', {})))
        self.stack.enter_context(patch.object(startup, 'run', return_value=' '.join(r['Id'] for r in self.rows)))
        self.stack.enter_context(patch.object(startup, 'inspect', return_value=self.rows))
        # 为符合真实 inventory 格式，GM 新容器也使用完整身份。
        self.baseline['services']['scene']['Id'] = 'f' * 64
        config = {'services': {n: {} for n in set(startup.INFRA + startup.APPS) | startup.ONESHOTS}}
        self.rows[:] = [row for row in self.rows if row['Config']['Labels']['com.docker.compose.service'] != 'mysql']
        startup.run.return_value = ' '.join(r['Id'] for r in self.rows)
        with self.assertRaisesRegex(RuntimeError, 'service set is incomplete'):
            rebuild.current_containers(config)


class PostRebuildGmPromotionTest(unittest.TestCase):
    def setUp(self):
        fixture = recovery_fixture.RecoveryProvenanceTest()
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        self.fixture, self.root, self.rows = fixture, fixture.root, fixture.rows
        self.stack = fixture.stack
        for name in ('run', 'Popen'):
            self.stack.enter_context(patch.object(subprocess, name, side_effect=AssertionError('external process')))
        self.stack.enter_context(patch.object(startup, 'signature', side_effect=lambda row: row['testSignature']))
        compose = [self.root / 'deploy/debug/compose.game.yml', self.root / 'deploy/debug/compose.debug.yml']
        self.stack.enter_context(patch.object(startup, 'COMPOSE_FILES', compose))
        for path in compose:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('original-compose', encoding='utf-8')
        self.authored = self.root / 'build/verification/authored-festival-pet-acquisition-20261116/compose.authored.json'
        self.authored.parent.mkdir(parents=True, exist_ok=True)
        self.authored.write_text('{}', encoding='utf-8')
        for row in self.rows:
            name = row['Config']['Labels']['com.docker.compose.service']
            row['Image'] = fixture.overlay['services'][name]['image']
            row['testSignature'] = name + '-signature'
            row['Config']['Labels']['com.docker.compose.project.config_files'] = ','.join(map(str, [
                *compose, self.authored, startup.recovery_path()]))
        _, proof = startup.recovery_contract(self.rows, '{"services":{}}')
        baseline = {'services': {r['Config']['Labels']['com.docker.compose.service']: r for r in self.rows},
                    'historical': [], 'authoredOverlay': {'input': '{"services":{}}'},
                    'recoveryOverlay': {'raw': startup.recovery_path().read_bytes(), 'contract': proof}}
        out = self.root / 'build/verification/rebuild-before-gm'
        out.mkdir()
        self.cpp_image = 'sha256:' + '1' * 64
        builds = {}
        for language in ('cpp', 'go', 'java'):
            directory = out / language
            result = {'success': True, 'images': {}, 'manifestPaths': {}}
            for name in rebuild.SERVICES[language]:
                path = directory / (name + '.json')
                manifest = {'schema': 1, 'language': language, 'files': {'proto/scene/scene_admin.proto': 'a' * 64},
                            'artifacts': {} if language == 'cpp' else {'/app/test': 'b' * 64},
                            'build': {'type': 'Debug', 'sdkImageId': 'sha256:' + 'd' * 64,
                                      'sourceSha256': startup.sha(json.dumps({'proto/scene/scene_admin.proto': 'a' * 64}, sort_keys=True).encode())}}
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(json.dumps(manifest), encoding='utf-8')
                result['manifestPaths'][name] = str(path)
                result['images'][name] = {'image': 'local/' + name + ':new', 'imageId': 'sha256:' + '1' * 64}
            (directory / 'result.json').write_text(json.dumps(result), encoding='utf-8')
            builds[language] = result
        raw = subject.prepare(startup, baseline, builds, out)
        startup.recovery_path().write_bytes(raw)
        for row in self.rows:
            name = row['Config']['Labels']['com.docker.compose.service']
            if name not in startup.INFRA:
                row['Id'] = 'rebuilt-' + name
                row['Image'] = json.loads(raw)['services'][name]['image']
                row['testSignature'] = 'rebuilt-' + name + '-sig'
        files = [str(compose[0]), str(compose[1]), str(self.authored), str(startup.recovery_path())]
        before = {'passed': True, 'contract': {'project': startup.PROJECT,
            'ids': {r['Config']['Labels']['com.docker.compose.service']: r['Id'] for r in self.rows},
            'images': {r['Config']['Labels']['com.docker.compose.service']: r['Image'] for r in self.rows},
            'signatures': {r['Config']['Labels']['com.docker.compose.service']: r['testSignature'] for r in self.rows},
            'composeSha256': {files[0]: startup.sha(compose[0].read_bytes()), files[-1]: startup.sha(raw)}}}
        compose[0].write_text('gm-wired-compose', encoding='utf-8')
        gm_dir = self.root / gm.RELATIVE
        gm_dir.mkdir(parents=True, exist_ok=True)
        cpp_result = out / 'cpp/result.json'
        cpp_source = Path(builds['cpp']['manifestPaths']['scene'])
        complete = {**json.loads(cpp_source.read_bytes()), 'artifacts': {'/app/bin/scene': 'e' * 64}}
        scene = next(r for r in self.rows if r['Config']['Labels']['com.docker.compose.service'] == 'scene')
        old_scene = {'id': scene['Id'], 'image': scene['Image'], 'signature': scene['testSignature']}
        scene['Id'] = 'gm-wired-scene'
        receipt = {'schema': 1, 'project': startup.PROJECT, 'service': 'scene', 'afterRebuild': True,
            'composeFiles': files, 'baseline': self.save(gm_dir / 'before.json', before), 'oldScene': old_scene,
            'oldRecoverySha256': startup.sha(raw), 'newRecoverySha256': startup.sha(raw),
            'oldComposeSha256': before['contract']['composeSha256'][files[0]],
            'newComposeSha256': startup.sha(compose[0].read_bytes()),
            'buildResult': subject.bound(startup, cpp_result), 'sourceManifest': subject.bound(startup, cpp_source),
            'imageManifest': self.save(gm_dir / 'image.json', complete), 'imageId': self.cpp_image,
            'sourceSha256': complete['build']['sourceSha256'], 'sdkImageId': complete['build']['sdkImageId']}
        binding = self.save(gm_dir / 'promotion.json', receipt)
        startup.ENV.write_text(gm.KEY + '=' + binding['sha256'] + '\n', encoding='utf-8')
        scene['Config']['Labels'][gm.LABEL] = binding['sha256']
        self.fixture.config['services'] = {'scene': {'labels': {gm.LABEL: binding['sha256']},
            'ports': [{'target': 50001, 'published': '2355', 'host_ip': '127.0.0.1'}],
            'environment': {'MMORPG_GM_PLAYER_COMMAND_SECRET': 'test-only-' * 5,
                            'MMORPG_ASSET_OP_SECRET_MAIL': 'test-only-' * 5}}}

    def save(self, path, value):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(value), encoding='utf-8')
        return subject.bound(startup, path)

    def test_post_rebuild_gm_promotion_verifies_and_rejects_public_port_or_sibling_drift(self):
        combined, _ = startup.recovery_contract(self.rows, '{"services":{}}')
        self.assertEqual(json.loads(combined)['services']['scene']['image'], self.cpp_image)
        self.fixture.config['services']['scene']['ports'][0]['host_ip'] = '0.0.0.0'
        with self.assertRaisesRegex(RuntimeError, 'loopback endpoint'):
            startup.recovery_contract(self.rows, '{"services":{}}')
        self.fixture.config['services']['scene']['ports'][0]['host_ip'] = '127.0.0.1'
        gate = next(r for r in self.rows if r['Config']['Labels']['com.docker.compose.service'] == 'gate')
        gate['Id'] = 'tampered-gate'
        with self.assertRaisesRegex(RuntimeError, 'another service'):
            startup.recovery_contract(self.rows, '{"services":{}}')


if __name__ == '__main__':
    unittest.main()
