"""离线复现手绘节庆部署后日常启动的挂载校验。"""
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import start_docker_debug as startup


@unittest.skipUnless(os.name == 'nt', 'Windows Docker Desktop bind paths')
class AuthoredComposeTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name).resolve()
        root_patch = patch.object(startup, 'ROOT', self.root)
        root_patch.start()
        self.addCleanup(root_patch.stop)
        self.batch = self.root / 'build/verification/authored-festival-test'
        self.tables = self.batch / 'tables/generated/tables'
        self.nav = self.root / 'data/scene_nav_bin/authored_maps'
        self.staged = self.root / 'build/docker-debug-current'
        for path in (self.tables, self.nav, self.staged):
            path.mkdir(parents=True)
            (path / 'test.bin').write_bytes(b'fixture')
        self.overlay = self.batch / 'compose.authored.json'
        self.mounts = {
            name: [(self.tables, '/app/generated/generated_tables'),
                   (self.nav, '/app/data/scene_nav_bin/authored_maps')]
            for name in ('gate', 'scene', 'battle')
        }
        self.mounts.update({name: [(self.tables, '/generated/tables')]
                            for name in ('login', 'scene-manager', 'player-locator')})
        self.config = {'services': {name: {'volumes': [
            dict(type='bind', source=source.as_posix(), target=target, read_only=True)
            for source, target in mounts]} for name, mounts in self.mounts.items()}}
        self.save_overlay()

    def save_overlay(self):
        self.overlay.write_text(json.dumps(self.config), encoding='utf-8')

    def container(self, name, mounts, *, overlay=True):
        files = [*startup.COMPOSE_FILES, self.overlay] if overlay else startup.COMPOSE_FILES
        return {'Config': {'Labels': {'com.docker.compose.project': startup.PROJECT,
                                     'com.docker.compose.service': name,
                                     'com.docker.compose.project.config_files': ','.join(map(str, files))}},
                'Mounts': mounts}

    @staticmethod
    def bind(source, target, *, writable=False):
        return dict(Type='bind', Source=source, Destination=target, RW=writable)

    def test_deployed_overlay_is_used_and_its_read_only_sources_are_hashed(self):
        mapped = '/run/desktop/mnt/host/' + self.tables.as_posix()[0].lower() + self.tables.as_posix()[2:]
        rows = [self.container('gate', [self.bind(mapped, '/app/generated/generated_tables'),
                                       self.bind(str(self.nav), '/app/data/scene_nav_bin/authored_maps')]),
                self.container('mysql', [self.bind(str(self.staged), '/mysql-config')], overlay=False)]
        overlay, content, compose_input, allowed = startup.authored_overlay(rows)
        self.assertEqual(overlay, self.overlay)
        self.assertEqual(content, self.overlay.read_bytes())
        self.assertEqual(json.loads(compose_input), self.config)
        hashes = startup.binding_hashes(rows, allowed)
        self.assertEqual(len(hashes), 3)
        (self.tables / 'test.bin').write_bytes(b'changed')
        self.assertNotEqual(startup.binding_hashes(rows, allowed), hashes)

    def test_no_overlay_preserves_the_staging_boundary(self):
        rows = [self.container('gate', [self.bind(str(self.staged), '/config')], overlay=False)]
        self.assertEqual(startup.authored_overlay(rows), (None, None, None, {}))
        self.assertEqual(len(startup.binding_hashes(rows)), 1)
        rows[0]['Mounts'][0]['Source'] = str(self.tables)
        with self.assertRaisesRegex(RuntimeError, 'Bind source outside staged directory'):
            startup.binding_hashes(rows)

    def test_overlay_cannot_add_other_compose_settings(self):
        self.config['services']['gate']['entrypoint'] = ['/unexpected']
        self.save_overlay()
        with self.assertRaisesRegex(RuntimeError, 'Unauthorized authored Compose overlay'):
            startup.authored_overlay([self.container('gate', [])])

    def test_overlay_outside_the_authored_batch_is_rejected(self):
        self.overlay = self.staged / self.overlay.name
        self.save_overlay()
        with self.assertRaisesRegex(RuntimeError, 'Unexpected authored Compose overlay path'):
            startup.authored_overlay([self.container('gate', [])])

    def test_overlay_sources_are_bound_to_the_service_target_and_read_only_mode(self):
        target = '/app/generated/generated_tables'
        _, _, _, allowed = startup.authored_overlay([self.container('gate', [])])
        for name, mount in [('mysql', self.bind(str(self.tables), target)),
                            ('gate', self.bind(str(self.tables), '/unexpected')),
                            ('gate', self.bind(str(self.tables), target, writable=True)),
                            ('gate', self.bind(str(self.nav), target))]:
            with self.subTest(name=name, mount=mount):
                with self.assertRaisesRegex(RuntimeError, 'Bind source outside staged directory'):
                    startup.binding_hashes([self.container(name, [mount])], allowed)



class InventoryDiagnosticsTest(unittest.TestCase):
    @staticmethod
    def row(name, identity, *, oneoff=False, status='exited', exit_code=0):
        return {'Id': identity, 'Image': 'fixture-image', 'RestartCount': 0,
                'Config': {'Labels': {'com.docker.compose.project': startup.PROJECT,
                    'com.docker.compose.service': name,
                    'com.docker.compose.oneoff': str(oneoff).lower(),
                    'com.docker.compose.container-number': '1'}},
                'State': {'Status': status, 'ExitCode': exit_code, 'StartedAt': '2026-11-16T00:00:00Z',
                          'FinishedAt': '2026-11-16T00:01:00Z', 'OOMKilled': False}}

    def complete(self):
        return [self.row(name, startup.sha(name.encode()))
                for name in sorted(set(startup.INFRA) | set(startup.APPS) | startup.ONESHOTS)]

    def test_zero_inventory_reports_preserved_volume_possibility_without_inspect_or_writes(self):
        with patch.object(startup, 'run', return_value='') as run, patch.object(startup, 'inspect') as inspect:
            with self.assertRaises(startup.InventoryError) as caught:
                startup.inventory()
        self.assertIn('Data volumes may still exist', str(caught.exception))
        self.assertEqual(caught.exception.diagnostics['containerCount'], 0)
        self.assertIn('mysql', caught.exception.diagnostics['missingServices'])
        inspect.assert_not_called()
        self.assertEqual(len(run.call_args_list), 1)
        self.assertIn('ps', run.call_args.args[0])

    def test_missing_duplicate_and_unknown_are_reported_together_without_secret_labels(self):
        rows = [row for row in self.complete() if row['Config']['Labels']['com.docker.compose.service'] != 'mysql']
        rows += [self.row('redis', 'a' * 64), self.row('credential-do-not-print', 'b' * 64)]
        with self.assertRaises(startup.InventoryError) as caught:
            startup.validate_service_inventory(rows)
        details = caught.exception.diagnostics
        self.assertEqual(details['missingServices'], ['mysql'])
        self.assertEqual(details['duplicateServices'], {'redis': 2})
        self.assertEqual(len(details['extraContainers']), 1)
        self.assertNotIn('credential-do-not-print', str(caught.exception) + json.dumps(details))

    def test_multiple_successful_migrations_are_preserved_and_never_planned(self):
        rows = self.complete() + [self.row('db-migrate', 'a' * 64, oneoff=True),
                                  self.row('db-migrate', 'b' * 64, oneoff=True)]
        startup.validate_service_inventory(rows)
        services = {row['Config']['Labels']['com.docker.compose.service']: row for row in rows[:-2]}
        services.update({startup.history_key(row): row for row in rows[-2:]})
        self.assertEqual(len(services), len(rows))
        with patch.object(startup, 'inventory', return_value=({}, services)), patch.object(startup, 'run') as run:
            plan = startup.preflight()['plan']
        self.assertFalse(any(step['service'].startswith('storage-migration') for step in plan))
        self.assertEqual([step['service'] for step in plan if step['action'] == 'start-existing'],
                         list(startup.INFRA + startup.APPS))
        run.assert_not_called()

    def test_failed_or_running_oneoff_is_rejected_without_command(self):
        for status, code in [('running', 0), ('exited', 1)]:
            with self.subTest(status=status), patch.object(startup, 'run') as run:
                with self.assertRaises(startup.InventoryError):
                    startup.validate_service_inventory(self.complete() +
                        [self.row('db-migrate', 'c' * 64, oneoff=True, status=status, exit_code=code)])
                run.assert_not_called()

    def test_invalid_inventory_stops_before_compose_or_image_reads(self):
        rows = self.complete()[1:]
        with patch.object(startup, 'run', return_value=' '.join(r['Id'] for r in rows)) as run, \
                patch.object(startup, 'inspect', return_value=rows), patch.object(startup, 'compose_contract') as compose:
            with self.assertRaises(startup.InventoryError):
                startup.inventory()
        compose.assert_not_called()
        self.assertEqual(run.call_count, 1)

    def test_inventory_keeps_each_historical_container_in_identity_and_mount_contracts(self):
        from contextlib import ExitStack
        rows = self.complete() + [self.row('db-migrate', 'a' * 64, oneoff=True),
                                  self.row('db-migrate', 'b' * 64, oneoff=True)]
        image = 'sha256:' + 'd' * 64
        for row in rows:
            row.update(Image=image, Mounts=[], HostConfig={'RestartPolicy': {'Name': 'no'}})
        names = set(startup.INFRA) | set(startup.APPS) | startup.ONESHOTS
        config = {'services': {name: {'image': 'fixture', 'environment': {}} for name in names}, 'volumes': {}}
        for name, port in startup.NODE_PORTS.items():
            config['services'][name]['environment']['RPC_PORT'] = port
            next(r for r in rows if r['Config']['Labels']['com.docker.compose.service'] == name)['Config']['Env'] = [f'RPC_PORT={port}']

        def command(args, **kwargs):
            if 'ps' in args:
                return ' '.join(r['Id'] for r in rows)
            if 'image' in args:
                return 'null' if args[-2] == '{{json .Config.Volumes}}' else image
            self.fail('Unexpected external command')

        with ExitStack() as stack:
            for name, value in [('inspect', rows), ('authored_overlay', (None, None, None, {})),
                                ('compose_contract', (config, 'fixture-hash')), ('project_volumes', []),
                                ('binding_hashes', {}), ('ports', [10000])]:
                stack.enter_context(patch.object(startup, name, return_value=value))
            stack.enter_context(patch.object(startup, 'COMPOSE_FILES', []))
            stack.enter_context(patch.object(startup, 'run', side_effect=command))
            contract, services = startup.inventory()
        for row in rows[-2:]:
            key = startup.history_key(row)
            self.assertEqual(contract['ids'][key], row['Id'])
            self.assertIn(key, contract['signatures'])
            self.assertIn(key, contract['mountedVolumes'])
            self.assertEqual(services[key], row)

    def test_main_persists_redacted_inventory_diagnostics(self):
        import contextlib
        import io
        with tempfile.TemporaryDirectory() as directory, patch.object(startup, 'ROOT', Path(directory)), \
                patch.object(startup, 'OUT'), patch.object(startup, 'run', return_value='') as run, \
                patch.object(sys, 'argv', ['start_docker_debug.py']), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(startup.main(), 1)
            result = json.loads(next(Path(directory).rglob('result.json')).read_text())
        self.assertFalse(result['passed'])
        self.assertEqual(result['phase'], 'preflight')
        self.assertEqual(result['inventoryDiagnostics']['containerCount'], 0)
        self.assertEqual(run.call_count, 1)


class RecoveryProvenanceTest(unittest.TestCase):
    def setUp(self):
        from contextlib import ExitStack
        self.stack = ExitStack()
        self.addCleanup(self.stack.close)
        self.root = Path(self.stack.enter_context(tempfile.TemporaryDirectory())).resolve()
        self.stack.enter_context(patch.object(startup, 'ROOT', self.root))
        selected = set(startup.INFRA + startup.APPS) | {n + '-sql-boot' for n in startup.WRITERS}
        self.rows = [InventoryDiagnosticsTest.row(name, startup.sha(name.encode())) for name in selected]
        authored = self.root / 'build/verification/authored-festival-pet-acquisition-20261116/compose.authored.json'
        for row in self.rows:
            row['Config']['Labels']['com.docker.compose.project.config_files'] = ','.join(
                map(str, [*startup.COMPOSE_FILES, authored, startup.recovery_path()]))
        self.volume = {'Name': 'a' * 64, 'Driver': 'local', 'Scope': 'local', 'CreatedAt': 'original',
                       'Labels': {'com.docker.volume.anonymous': ''}, 'Options': None, 'Mountpoint': '/fixture'}
        self.mounts = [dict(target='/etc/kafka/secrets', name='a' * 64, anonymous=True,
                            volumeSignature=startup.volume_signature(self.volume)),
                       dict(target='/mnt/shared/config', name='b' * 64, anonymous=True, volumeSignature='second')]
        history = {'passed': True, 'states': {name: {'status': 'exited', 'exitCode': 0, 'startedAt': '2026-11-15'}
                                             for name in startup.HISTORY_ONLY},
                   'contract': {'project': startup.PROJECT, 'ids': {n: n for n in startup.INFRA},
                                'mountedVolumes': {'kafka': self.mounts,
                                    'mysql': [{'target': '/var/lib/mysql', 'name': 'original-mysql',
                                               'anonymous': False, 'volumeSignature': 'original-mysql-signature'}]}}}
        latest = {name: {'id': name, 'imageId': 'sha256:' + 'c' * 64} for name in selected}
        sources = {}
        fixtures = [history, {'services': dict.fromkeys(startup.INIT, 0)}, latest, {'success': True}]
        for relative, value in zip(startup.RECOVERY_SOURCES, fixtures):
            path = self.root / 'build/verification' / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(json.dumps(value), encoding='utf-8')
            sources[relative] = startup.sha(path.read_bytes())
        self.stack.enter_context(patch.object(startup, 'RECOVERY_SOURCES', sources))
        self.config = {'volumes': {'mysql-data': {'name': 'original-mysql'}}}
        self.stack.enter_context(patch.object(startup, 'compose_contract', return_value=(self.config, 'env-hash')))
        self.overlay = {'services': {name: {'image': latest[name]['imageId'], 'pull_policy': 'never'} for name in selected},
                        'volumes': {'mysql-data': {'name': 'original-mysql', 'external': True}}}
        for index, mount in enumerate(self.mounts):
            key = 'recovery-kafka-' + str(index)
            self.overlay['volumes'][key] = {'name': mount['name'], 'external': True}
            self.overlay['services']['kafka'].setdefault('volumes', []).append(
                {'type': 'volume', 'source': key, 'target': mount['target']})
        startup.recovery_path().parent.mkdir(parents=True, exist_ok=True)
        self.save_overlay()

    def save_overlay(self):
        startup.recovery_path().write_text(json.dumps(self.overlay), encoding='utf-8')

    def test_verified_provenance_allows_only_absent_historical_services(self):
        combined, proof = startup.recovery_contract(self.rows, '{"services":{}}')
        self.assertEqual(json.loads(combined), self.overlay)
        self.assertEqual(proof['anonymousVolumes']['/etc/kafka/secrets']['name'], 'a' * 64)
        details = startup.validate_service_inventory(self.rows, startup.HISTORY_ONLY)
        self.assertEqual(set(details['missingServices']), startup.HISTORY_ONLY)
        with self.assertRaises(startup.InventoryError):
            startup.validate_service_inventory(self.rows)
        with self.assertRaises(startup.InventoryError):
            startup.validate_service_inventory([r for r in self.rows if
                r['Config']['Labels']['com.docker.compose.service'] != 'mysql'], startup.HISTORY_ONLY)

    def test_history_evidence_cannot_be_replaced_with_a_new_success_claim(self):
        source = self.root / 'build/verification' / next(iter(startup.RECOVERY_SOURCES))
        source.write_text('{"passed":true}', encoding='utf-8')
        with self.assertRaisesRegex(RuntimeError, 'Approved recovery evidence changed'):
            startup.recovery_contract(self.rows, '{"services":{}}')

    def test_recovery_overlay_cannot_substitute_empty_data_volumes(self):
        self.overlay['volumes']['mysql-data']['name'] = 'empty-mysql'
        self.save_overlay()
        with self.assertRaisesRegex(RuntimeError, 'Unauthorized recovery Compose overlay'):
            startup.recovery_contract(self.rows, '{"services":{}}')

    def test_explicit_old_anonymous_volume_requires_original_creation_identity(self):
        target = '/etc/kafka/secrets'
        row = {'Mounts': [{'Destination': target, 'Name': 'a' * 64, 'Type': 'volume', 'RW': True, 'Driver': 'local'}]}
        service = {'volumes': [{'type': 'volume', 'source': 'recovery-kafka-0', 'target': target}]}
        definitions = {'recovery-kafka-0': {'name': 'a' * 64, 'external': True}}
        details = {'a' * 64: self.volume}
        proof = {'anonymousVolumes': {target: self.mounts[0]}}
        with self.assertRaisesRegex(RuntimeError, 'Named volume lacks project ownership'):
            startup.validate_mounts('kafka', row, service, definitions, [target], details)
        checked = startup.validate_mounts('kafka', row, service, definitions, [target], details, proof)
        self.assertTrue(checked[0]['anonymous'])
        self.volume['CreatedAt'] = 'recreated'
        with self.assertRaisesRegex(RuntimeError, 'Recovered anonymous volume identity changed'):
            startup.validate_mounts('kafka', row, service, definitions, [target], details, proof)

    def test_other_recovery_file_orders_are_rejected(self):
        row = self.rows[0]
        row['Config']['Labels']['com.docker.compose.project.config_files'] = ','.join(
            map(str, [*startup.COMPOSE_FILES, startup.recovery_path()]))
        with self.assertRaisesRegex(RuntimeError, 'Inconsistent recovery Compose provenance'):
            startup.recovery_contract(self.rows, '{"services":{}}')

    def test_new_rank_registrar_retains_shared_private_volume_at_original_target_only(self):
        # 旧完整快照尚无 rank，但共享私有卷已由旧注册器使用并记录真实身份。
        relative = next(iter(startup.RECOVERY_SOURCES))
        path = self.root / 'build/verification' / relative
        history = json.loads(path.read_text())
        history['contract']['mountedVolumes']['db-sql-boot'] = [
            {'target': '/run/sql-boot/private', 'name': 'original-private', 'anonymous': False,
             'volumeSignature': 'original-private-signature'}]
        path.write_text(json.dumps(history), encoding='utf-8')
        startup.RECOVERY_SOURCES[relative] = startup.sha(path.read_bytes())
        self.config['volumes']['sql-boot-private'] = {'name': 'original-private'}
        private_mount = {'type': 'volume', 'source': 'sql-boot-private', 'target': '/run/sql-boot/private'}
        self.config['services'] = {'rank-sql-boot': {'volumes': [private_mount]}}
        self.overlay['volumes']['sql-boot-private'] = {'name': 'original-private', 'external': True}
        self.save_overlay()
        _, proof = startup.recovery_contract(self.rows, '{"services":{}}')
        self.assertEqual(proof['volumeSignatures']['original-private'], 'original-private-signature')
        private_mount['target'] = '/run/sql-boot/unexpected'
        with self.assertRaisesRegex(RuntimeError, 'Recovery service volume differs from historical binding: rank-sql-boot'):
            startup.recovery_contract(self.rows, '{"services":{}}')


if __name__ == '__main__':
    unittest.main()
