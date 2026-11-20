"""用隔离文件与模拟 Docker 验证 GM 原桥恢复，不启动真实容器。"""
import base64
from copy import deepcopy
import json
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import MagicMock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import gm_rpc_startup as gm
import start_docker_debug as startup


class RpcStartupTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.out = self.root / gm.RELATIVE
        (self.out / 'private').mkdir(parents=True)
        (self.out / 'context').mkdir()
        (self.out / 'context/bridge').write_bytes(b'test-only-bridge')
        self.evidence = self.root / 'build/test-start'
        self.evidence.mkdir()
        deploy = self.root / 'deploy/gm-admin'
        (deploy / 'rpc-bridge').mkdir(parents=True)
        production = Path(__file__).resolve().parents[3] / 'deploy/gm-admin/rpc-bridge.py'
        (deploy / 'rpc-bridge.py').write_bytes(production.read_bytes())
        for relative in ('rpc-bridge/main.go', 'rpc-bridge/go.mod', 'rpc-bridge-Dockerfile', 'rpc-bridge.compose.yml'):
            (deploy / relative).write_text('test-only source\n', encoding='utf-8')
        self.rows = {}
        for name, suffix, ip in [('data-service', 'data-service', '172.29.220.12'),
                                 ('player-locator', 'player-locator', '172.29.220.13'),
                                 ('bridge', 'bridge', '172.29.220.40')]:
            bridge = name == 'bridge'
            project = 'xuanming-gm-rpc' if bridge else startup.PROJECT
            label = str(deploy / 'rpc-bridge.compose.yml') if bridge else 'test-game-compose'
            self.rows[name] = {
                'Id': name + '-id', 'Image': 'sha256:' + name, 'Name': '/' + project + '-' + suffix + '-1',
                'Config': {'Labels': {'com.docker.compose.project': project,
                    'com.docker.compose.service': name, 'com.docker.compose.project.config_files': label},
                    'User': '10001:10001', 'Entrypoint': ['/app/gm-rpc-bridge'], 'Cmd': None},
                'HostConfig': {'ReadonlyRootfs': True, 'Dns': None, 'DnsOptions': None, 'DnsSearch': None,
                               'ExtraHosts': None}, 'Mounts': [], 'RestartCount': 0,
                'State': {'Status': 'running', 'ExitCode': 0, 'StartedAt': '2026-11-17T00:00:01Z',
                    'FinishedAt': '0001-01-01T00:00:00Z', 'OOMKilled': False, 'Health': {'Status': 'healthy'}},
                'NetworkSettings': {'Networks': {'xuanming-debug-current_default': {'IPAddress': ip}},
                    'Ports': {'50901/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '2356'}],
                              '50902/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '2357'}]}}}
        self.start_calls = []
        self.start_mode = 'success'
        self.fake = SimpleNamespace(PROJECT=startup.PROJECT, DOCKER=['docker', '--context', 'desktop-linux'],
            inspect=self.inspect, signature=startup.signature, state=startup.state, run=self.docker_run,
            require=startup.require, sha=startup.sha)
        sources = {str(path.relative_to(self.root)): gm.digest(path.read_bytes())
                   for path in sorted(deploy.rglob('*')) if path.is_file()}
        ready = {'schema': 1, 'passed': True, 'targets': gm.TARGETS,
                 'sources': sources, 'binarySha256': gm.digest(b'test-only-bridge'),
                 'imageId': self.rows['bridge']['Image'], 'bridge': self.snapshot('bridge'),
                 'upstreams': {name: self.snapshot(name) for name in ('data-service', 'player-locator')},
                 'existingGameServicesUnchanged': True, 'playerMutationPerformed': False}
        self.publish(ready)
        real_load = gm.load

        def loaded(root):
            module, approved, raw, seal = real_load(root)
            module.startup = self.fake
            module.protect = lambda path: None
            return module, approved, raw, seal

        loader_patch = patch.object(gm, 'load', side_effect=loaded)
        loader_patch.start()
        self.addCleanup(loader_patch.stop)
        connection_patch = patch.object(gm.socket, 'create_connection', return_value=MagicMock())
        self.connection = connection_patch.start()
        self.addCleanup(connection_patch.stop)

    def inspect(self, identities):
        return [deepcopy(next(row for row in self.rows.values()
                             if identity in (row['Id'], row['Name'].lstrip('/')))) for identity in identities]

    def snapshot(self, name):
        row = self.rows[name]
        result = {'id': row['Id'], 'imageId': row['Image'],
                  'signature': startup.signature(row), 'state': startup.state(row)}
        if name != 'bridge':
            port = 9000 if name == 'data-service' else 53200
            result.update(name=row['Name'].lstrip('/'), endpoint=
                          row['NetworkSettings']['Networks']['xuanming-debug-current_default']['IPAddress'] + ':' + str(port))
        return result

    def docker_run(self, args, **kwargs):
        if 'start' in args:
            self.start_calls.append(args)
            self.assertEqual(args[-2:], ['start', self.rows['bridge']['Id']])
            if self.start_mode == 'command-failure':
                raise RuntimeError('test startup failure')
            self.running('bridge', '2026-11-17T01:00:00Z')
            if self.start_mode == 'exit':
                self.stopped('bridge')
            if self.start_mode == 'timeout':
                self.rows['bridge']['State']['Health']['Status'] = 'starting'
            return args[-1]
        self.assertIn('etcdctl', args)
        key = args[args.index('get') + 1]
        name = 'data-service' if key == 'dataservice.rpc/' else 'player-locator'
        return json.dumps({'kvs': [{'value': base64.b64encode(self.snapshot(name)['endpoint'].encode()).decode()}]})

    def publish(self, ready):
        raw = (json.dumps(ready, indent=2) + '\n').encode()
        (self.out / 'ready.json').write_bytes(raw)
        (self.out / 'private/ready.sha256').write_text(gm.digest(raw) + '\n', encoding='ascii')

    def stopped(self, name):
        self.rows[name]['State'].update(Status='exited', FinishedAt='2026-11-17T00:30:00Z')

    def running(self, name, when):
        self.rows[name]['State'].update(Status='running', StartedAt=when, FinishedAt='0001-01-01T00:00:00Z')
        self.rows[name]['State']['Health']['Status'] = 'healthy'

    def evidence_bytes(self):
        return ((self.out / 'ready.json').read_bytes(), (self.out / 'private/ready.sha256').read_bytes())

    def test_running_is_idempotent_without_receipt_rewrite(self):
        original = self.evidence_bytes()
        result = gm.ensure_started(self.root, self.evidence, gm.preflight(self.root))
        self.assertTrue(result['passed'])
        self.assertFalse(result['started'])
        self.assertFalse(result['receiptRefreshed'])
        self.assertEqual(self.start_calls, [])
        self.assertEqual(original, self.evidence_bytes())
        self.assertFalse((self.evidence / 'gm-rpc-ready.before.json').exists())

    def test_stopped_original_bridge_starts_once_and_preserves_original_receipt(self):
        original = self.evidence_bytes()
        self.stopped('bridge')
        result = gm.ensure_started(self.root, self.evidence, gm.preflight(self.root))
        self.assertTrue(result['started'])
        self.assertTrue(result['receiptRefreshed'])
        self.assertEqual(len(self.start_calls), 1)
        self.assertEqual((self.evidence / 'gm-rpc-ready.before.json').read_bytes(), original[0])
        module, ready, _, _ = gm.load(self.root)
        self.assertEqual(module.verify_ready(), gm.TARGETS)
        self.assertEqual(ready['bridge']['id'], 'bridge-id')
        self.assertFalse(result['sceneConfigurationChanged'])

    def test_game_can_start_original_stopped_upstreams_between_phases(self):
        for name in self.rows:
            self.stopped(name)
        plan = gm.preflight(self.root)
        for name in ('data-service', 'player-locator'):
            self.running(name, '2026-11-17T00:59:00Z')
        result = gm.ensure_started(self.root, self.evidence, plan)
        self.assertTrue(result['passed'])
        self.assertEqual(len(self.start_calls), 1)

    def test_immutable_identity_drift_is_rejected_before_start(self):
        for name in self.rows:
            for field in ('Id', 'Image', 'HostConfig'):
                with self.subTest(service=name, field=field):
                    old = deepcopy(self.rows[name])
                    if field == 'HostConfig':
                        self.rows[name][field]['ReadonlyRootfs'] = False
                    else:
                        self.rows[name][field] += '-changed'
                    with self.assertRaises((RuntimeError, StopIteration)):
                        gm.preflight(self.root)
                    self.rows[name] = old
        self.assertEqual(self.start_calls, [])

    def test_source_and_private_digest_tampering_are_rejected(self):
        for path in (self.root / 'deploy/gm-admin/rpc-bridge.py', self.out / 'private/ready.sha256',
                     self.out / 'context/bridge'):
            with self.subTest(path=path.name):
                original = path.read_bytes()
                path.write_bytes(original + b' tampered')
                with self.assertRaises(RuntimeError):
                    gm.preflight(self.root)
                path.write_bytes(original)
        self.assertEqual(self.start_calls, [])

    def test_receipt_tampering_is_rejected_without_resigning(self):
        path = self.out / 'ready.json'
        path.write_bytes(path.read_bytes() + b' ')
        with self.assertRaisesRegex(RuntimeError, 'digest changed'):
            gm.preflight(self.root)

    def test_start_failure_or_timeout_never_refreshes_receipt(self):
        for mode in ('command-failure', 'exit', 'timeout'):
            with self.subTest(mode=mode):
                self.start_mode = mode
                self.stopped('bridge')
                plan = gm.preflight(self.root)
                original = self.evidence_bytes()
                calls = len(self.start_calls)
                with patch.object(gm, 'WAIT_SECONDS', 0), self.assertRaises(RuntimeError):
                    gm.ensure_started(self.root, self.evidence, plan)
                self.assertEqual(len(self.start_calls), calls + 1)
                self.assertEqual(self.evidence_bytes(), original)
                self.assertFalse((self.evidence / 'gm-rpc-ready.before.json').exists())

    def test_upstream_process_restarting_after_preflight_is_rejected(self):
        plan = gm.preflight(self.root)
        self.running('data-service', '2026-11-17T02:00:00Z')
        with self.assertRaisesRegex(RuntimeError, 'process changed'):
            gm.ensure_started(self.root, self.evidence, plan)
        self.assertEqual(self.start_calls, [])

    def test_sources_changing_after_plan_cannot_start_bridge(self):
        self.stopped('bridge')
        plan = gm.preflight(self.root)
        (self.root / 'deploy/gm-admin/rpc-bridge/main.go').write_text('changed')
        with self.assertRaisesRegex(RuntimeError, 'sources changed'):
            gm.ensure_started(self.root, self.evidence, plan)
        self.assertEqual(self.start_calls, [])

    def test_connection_failure_does_not_publish_readiness(self):
        self.stopped('bridge')
        plan = gm.preflight(self.root)
        original = self.evidence_bytes()
        self.connection.side_effect = OSError('test endpoint closed')
        with self.assertRaises(OSError):
            gm.ensure_started(self.root, self.evidence, plan)
        self.assertEqual(self.evidence_bytes(), original)

    def test_publication_rejects_concurrent_receipt_edit(self):
        path = self.evidence / 'sample.json'
        path.write_bytes(b'new concurrent data')
        with self.assertRaisesRegex(RuntimeError, 'changed before'):
            gm.atomic_replace(path, b'old approved data', b'candidate', lambda _: None)
        self.assertEqual(path.read_bytes(), b'new concurrent data')

    def test_proven_empty_dns_representation_preserves_identity_and_reseals_raw_signatures(self):
        original = self.evidence_bytes()
        for row in self.rows.values():
            for key in gm.DNS_FIELDS:
                row['HostConfig'][key] = []
        plan = gm.preflight(self.root)
        self.assertEqual(set(plan['signatureMigrations']), set(self.rows))
        for migration in plan['signatureMigrations'].values():
            self.assertEqual(migration['fields'], ['HostConfig.' + key for key in gm.DNS_FIELDS])
            self.assertNotEqual(migration['approvedSignature'], migration['runtimeSignature'])
        result = gm.ensure_started(self.root, self.evidence, plan)
        self.assertTrue(result['receiptRefreshed'])
        self.assertEqual(self.start_calls, [])
        self.assertEqual((self.evidence / 'gm-rpc-ready.before.json').read_bytes(), original[0])
        module, ready, _, _ = gm.load(self.root)
        self.assertEqual(module.verify_ready(), gm.TARGETS)
        self.assertEqual(ready['bridge']['signature'], startup.signature(self.rows['bridge']))
        self.assertEqual(gm.preflight(self.root)['signatureMigrations'], {})
        self.assertFalse(gm.ensure_started(self.root, self.evidence, gm.preflight(self.root))['receiptRefreshed'])

    def test_dns_compatibility_cannot_hide_nonempty_dns_or_extra_hosts_changes(self):
        for service in self.rows:
            for field, value in [('Dns', ['203.0.113.53']), ('ExtraHosts', []), ('ReadonlyRootfs', False)]:
                with self.subTest(service=service, field=field):
                    old = deepcopy(self.rows[service])
                    for key in gm.DNS_FIELDS:
                        self.rows[service]['HostConfig'][key] = []
                    self.rows[service]['HostConfig'][field] = value
                    with self.assertRaisesRegex(RuntimeError, 'signature changed'):
                        gm.preflight(self.root)
                    self.rows[service] = old
        self.assertEqual(self.start_calls, [])

    def test_partial_dns_representation_change_is_not_implicitly_approved(self):
        self.rows['bridge']['HostConfig']['Dns'] = []
        with self.assertRaisesRegex(RuntimeError, 'signature changed'):
            gm.preflight(self.root)

    def test_identity_or_endpoint_change_after_plan_prevents_start(self):
        self.stopped('bridge')
        plan = gm.preflight(self.root)
        for service in self.rows:
            with self.subTest(service=service):
                self.rows[service]['Image'] += '-changed'
                with self.assertRaises(RuntimeError):
                    gm.ensure_started(self.root, self.evidence, plan)
                self.rows[service]['Image'] = self.rows[service]['Image'].removesuffix('-changed')
        self.rows['data-service']['NetworkSettings']['Networks']['xuanming-debug-current_default']['IPAddress'] = '172.29.220.99'
        with self.assertRaisesRegex(RuntimeError, 'upstream identity or endpoint changed'):
            gm.ensure_started(self.root, self.evidence, plan)
        self.assertEqual(self.start_calls, [])

    def test_proven_dns_representation_can_change_during_planned_game_start(self):
        self.stopped('data-service')
        plan = gm.preflight(self.root)
        self.running('data-service', '2026-11-17T03:00:00Z')
        for key in gm.DNS_FIELDS:
            self.rows['data-service']['HostConfig'][key] = []
        result = gm.ensure_started(self.root, self.evidence, plan)
        self.assertEqual(set(result['signatureMigrations']), {'data-service'})
        self.assertEqual(self.start_calls, [])

    def test_upstream_restart_between_observation_and_registry_snapshot_is_rejected(self):
        plan = gm.preflight(self.root)
        original = self.evidence_bytes()
        original_inspect = self.fake.inspect
        once = [False]

        def race(identities):
            if identities == ['xuanming-debug-current-data-service-1'] and not once[0]:
                once[0] = True
                self.running('data-service', '2026-11-17T04:00:00Z')
            return original_inspect(identities)

        self.fake.inspect = race
        with self.assertRaisesRegex(RuntimeError, 'process changed'):
            gm.ensure_started(self.root, self.evidence, plan)
        self.assertEqual(self.evidence_bytes(), original)
        self.assertEqual(self.start_calls, [])

    def test_bridge_health_can_become_healthy_between_readonly_inspections(self):
        self.stopped('bridge')
        self.start_mode = 'timeout'
        plan = gm.preflight(self.root)
        original_inspect = self.fake.inspect

        def health_transition(identities):
            result = original_inspect(identities)
            if identities == ['bridge-id'] and self.rows['bridge']['State']['Health']['Status'] == 'starting':
                self.rows['bridge']['State']['Health']['Status'] = 'healthy'
            return result

        self.fake.inspect = health_transition
        self.assertTrue(gm.ensure_started(self.root, self.evidence, plan)['passed'])
        self.assertEqual(len(self.start_calls), 1)

    def test_proven_dns_representation_can_change_when_original_bridge_starts(self):
        self.stopped('bridge')
        plan = gm.preflight(self.root)
        original_run = self.fake.run

        def representation(args, **kwargs):
            result = original_run(args, **kwargs)
            if 'start' in args:
                for key in gm.DNS_FIELDS:
                    self.rows['bridge']['HostConfig'][key] = []
            return result

        self.fake.run = representation
        result = gm.ensure_started(self.root, self.evidence, plan)
        self.assertEqual(set(result['signatureMigrations']), {'bridge'})
        self.assertEqual(len(self.start_calls), 1)

    def test_refresh_upstreams_advances_verified_rebuilt_upstreams_and_rejects_mismatch(self):
        original = self.evidence_bytes()
        self.rows['data-service']['Id'] = 'rebuilt-data-id'
        self.rows['data-service']['Image'] = 'sha256:rebuilt-data'
        self.rows['player-locator']['Id'] = 'rebuilt-locator-id'
        self.rows['player-locator']['Image'] = 'sha256:rebuilt-locator'
        mismatched = {'passed': True, 'contract': {'project': startup.PROJECT,
            'ids': {'data-service': 'other-id', 'player-locator': 'rebuilt-locator-id'},
            'images': {'data-service': 'sha256:rebuilt-data', 'player-locator': 'sha256:rebuilt-locator'}}}
        with self.assertRaisesRegex(RuntimeError, 'verified game contract'):
            gm.refresh_upstreams(self.root, self.evidence, lambda: mismatched)
        self.assertEqual(self.evidence_bytes(), original)
        verified = {'passed': True, 'contract': {'project': startup.PROJECT,
            'ids': {'data-service': 'rebuilt-data-id', 'player-locator': 'rebuilt-locator-id'},
            'images': {'data-service': 'sha256:rebuilt-data', 'player-locator': 'sha256:rebuilt-locator'}}}
        result = gm.refresh_upstreams(self.root, self.evidence, lambda: verified)
        self.assertTrue(result['passed'] and result['receiptRefreshed'])
        self.assertEqual((self.evidence / 'gm-rpc-ready.before.json').read_bytes(), original[0])
        module, ready, _, _ = gm.load(self.root)
        self.assertEqual(module.verify_ready(), gm.TARGETS)
        self.assertEqual(ready['upstreams']['data-service']['id'], 'rebuilt-data-id')
        self.assertEqual(ready['bridge']['id'], 'bridge-id')


if __name__ == '__main__':
    unittest.main()
