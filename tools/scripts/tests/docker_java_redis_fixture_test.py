"""只测本项目 Redis fixture 合同；禁止执行真实 Docker 或网络命令。"""
import copy
import io
import ipaddress
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import docker_java_redis_fixture as fixture


class RedisFixtureTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.patch(fixture, 'ROOT', new=self.root)
        self.patch(fixture.subprocess, 'run', side_effect=AssertionError('Real process forbidden'))
        self.patch(fixture.socket, 'create_connection', side_effect=AssertionError('Real network forbidden'))

    def patch(self, owner, name, **kwargs):
        patcher = patch.object(owner, name, **kwargs)
        self.addCleanup(patcher.stop)
        return patcher.start()

    def external(self):
        return {fixture.NAMES[0]: 'redis://127.0.0.1:25100',
                fixture.NAMES[1]: '127.0.0.1:25101,127.0.0.1:25102,127.0.0.1:25103'}

    def test_empty_environment_automatically_owns_fixtures(self):
        self.assertEqual(fixture.mode({}), 'owned')
        self.assertEqual(fixture.local_requirements({}), [])

    def test_partial_or_empty_explicit_input_never_falls_back_to_owned(self):
        for name in (*fixture.NAMES, fixture.PASSWORD):
            env = {name: ''}
            self.assertEqual(fixture.mode(env), 'external')
            self.assertTrue(fixture.local_requirements(env))

    def test_external_rejects_remote_or_invalid_endpoints_without_secrets(self):
        for value in ('redis://:sensitive-value@remote:1234', 'redis://127.0.0.1:70000',
                      'redis://127.0.0.1:1234?password=sensitive-value'):
            env = self.external()
            env[fixture.NAMES[0]] = value
            errors = fixture.local_requirements(env)
            self.assertTrue(errors)
            self.assertNotIn('sensitive-value', str(errors))
        env = self.external()
        env[fixture.NAMES[1]] = 'remote:1234'
        self.assertTrue(fixture.local_requirements(env))

    def test_preflight_pins_existing_image_without_pull(self):
        self.patch(fixture, '_subnet', return_value='10.254.1.0/28')
        inspect = self.patch(fixture, '_json', return_value=[{'Id': 'sha256:' + 'a' * 64,
                             'RepoDigests': ['redis@' + fixture.IMAGE.split('@')[1]]}])
        fixture.preflight({})
        inspect.assert_called_once_with(['image', 'inspect', fixture.IMAGE], {})
        inspect.return_value[0]['RepoDigests'] = []
        with self.assertRaisesRegex(RuntimeError, 'automatic pull is disabled'):
            fixture.preflight({})

    def test_external_preflight_runs_real_readiness_interface(self):
        ready = self.patch(fixture, '_ready')
        fixture.preflight(self.external())
        ready.assert_called_once()

    def test_readiness_rejects_unreachable_remote_or_incomplete_cluster(self):
        slots = [[0, 5460, [b'127.0.0.1', 25101]], [5461, 10922, [b'127.0.0.1', 25102]],
                 [10923, 16383, [b'127.0.0.1', 25103]]]
        def query(endpoint, *parts):
            if parts == ('PING',):
                return b'PONG'
            if parts == ('CLUSTER', 'INFO'):
                return b'cluster_state:ok\r\ncluster_slots_assigned:16384\r\n'
            return slots
        self.patch(fixture, '_query', side_effect=query)
        fixture._ready(self.external())
        slots[0][2][0] = b'172.18.0.2'
        with self.assertRaisesRegex(RuntimeError, 'non-loopback'):
            fixture._ready(self.external())
        slots[0][2][0] = b'127.0.0.1'
        slots[1][0] += 1
        with self.assertRaisesRegex(RuntimeError, 'coverage'):
            fixture._ready(self.external())

    def test_resp_parser_does_not_echo_redis_error(self):
        with self.assertRaisesRegex(RuntimeError, 'rejected') as error:
            fixture._read_resp(io.BytesIO(b'-ERR sensitive-value\r\n'))
        self.assertNotIn('sensitive-value', str(error.exception))
        self.assertEqual(fixture._read_resp(io.BytesIO(b'*2\r\n:1\r\n$4\r\nPONG\r\n')), [1, b'PONG'])

    def prepared(self):
        self.patch(fixture, 'preflight')
        self.patch(fixture, '_subnet', return_value='10.254.1.0/28')
        self.patch(fixture, '_json', return_value=[{'Id': 'sha256:' + 'a' * 64}])
        self.patch(fixture, '_ports', return_value=list(range(25000, 25007)))
        self.patch(fixture, '_query', return_value=b'PONG')
        self.patch(fixture, '_ready')
        self.patch(fixture, '_identity', return_value={'verified': True})
        calls = []
        def docker(args, environment, timeout=60):
            calls.append(args)
            if args[:2] in (['container', 'ls'], ['network', 'ls']):
                return ''
            if args[:2] == ['network', 'create']:
                return 'b' * 64
            if args[0] == 'run':
                return str(sum(call[0] == 'run' for call in calls)) * 64
            return ''
        self.patch(fixture, '_docker', side_effect=docker)
        return calls

    def test_owned_creation_is_loopback_pinned_bounded_and_volume_free(self):
        calls = self.prepared()
        child, state = fixture.create(self.root / 'build/verification/run/redis', {'INHERITED': 'yes'})
        self.assertEqual(child['INHERITED'], 'yes')
        self.assertEqual(child[fixture.NAMES[1]], '127.0.0.1:25001,127.0.0.1:25002,127.0.0.1:25003')
        runs = [call for call in calls if call[0] == 'run']
        self.assertEqual(len(runs), 4)
        for args in runs:
            self.assertIn('--pull=never', args)
            self.assertIn('--read-only', args)
            self.assertIn('--restart=no', args)
            self.assertIn('sha256:' + 'a' * 64, args)
            self.assertNotIn('--volume', args)
        for i, arg in enumerate(runs[0]):
            if arg == '--publish':
                self.assertTrue(runs[0][i + 1].startswith('127.0.0.1:'))
        for args in runs[1:]:
            self.assertIn('container:' + state['containers']['single'], args)
            self.assertNotIn('--publish', args)
        self.assertEqual(sum(call[0] == 'exec' for call in calls), 1)

    def test_build_failure_cannot_cleanup_and_success_removes_only_recorded_ids(self):
        calls = self.prepared()
        _, state = fixture.create(self.root / 'build/verification/run/redis', {})
        with self.assertRaisesRegex(RuntimeError, 'successful Java build'):
            fixture.close(state, {})
        self.assertFalse(any(call[:2] == ['container', 'stop'] for call in calls))
        state['javaBuildSucceeded'] = True
        fixture.close(state, {})
        removed = [call[2] for call in calls if call[:2] == ['container', 'rm']]
        self.assertEqual(removed, [state['containers'][name] for name in ('cluster2', 'cluster1', 'cluster0', 'single')])
        self.assertIn(['network', 'rm', state['networkId']], calls)

    def test_identity_drift_preserves_resources(self):
        calls = self.prepared()
        _, state = fixture.create(self.root / 'build/verification/run/redis', {})
        state['javaBuildSucceeded'] = True
        state['identity'] = {'changed': True}
        with self.assertRaisesRegex(RuntimeError, 'retain'):
            fixture.close(state, {})
        self.assertFalse(any(call[:2] == ['container', 'stop'] for call in calls))

    def test_creation_failure_is_recorded_without_retry_or_cleanup(self):
        calls = self.prepared()
        self.patch(fixture, '_query', side_effect=RuntimeError('unready'))
        self.patch(fixture.time, 'monotonic', side_effect=[0, 100])
        directory = self.root / 'build/verification/run/redis'
        with self.assertRaisesRegex(RuntimeError, 'retain'):
            fixture.create(directory, {})
        self.assertTrue((directory / 'create.hold.json').exists())
        self.assertFalse(any(call[:2] in (['container', 'rm'], ['network', 'rm']) for call in calls))

    def test_external_never_creates_or_deletes_resources(self):
        self.patch(fixture, 'preflight')
        docker = self.patch(fixture, '_docker')
        env = self.external()
        child, state = fixture.create(self.root / 'never-created', env)
        self.assertEqual(child, env)
        fixture.close(state, env)
        docker.assert_not_called()
        self.assertFalse((self.root / 'never-created').exists())

    def test_subnet_excludes_docker_and_host_routes_without_mutation(self):
        self.patch(fixture.secrets, 'randbelow', return_value=0)
        self.patch(fixture, '_occupied_subnets', return_value=[ipaddress.ip_network('10.254.0.0/24')])
        self.assertEqual(fixture._subnet({}), '10.254.1.0/28')
        self.patch(fixture, '_occupied_subnets', return_value=[ipaddress.ip_network('10.0.0.0/8')])
        with self.assertRaisesRegex(RuntimeError, 'host routes'):
            fixture._subnet({})

    def test_docker_error_classification_never_echoes_stderr(self):
        from types import SimpleNamespace
        self.patch(fixture.subprocess, 'run', return_value=SimpleNamespace(returncode=1, stdout='',
                   stderr='all predefined address pools have been fully subnetted; sensitive-value'))
        with self.assertRaises(fixture.FixtureCommandError) as caught:
            fixture._docker(['network', 'create', 'fixture'], {})
        self.assertEqual(caught.exception.code, 'network-address-pool-exhausted')
        self.assertNotIn('sensitive-value', str(caught.exception))

    def test_host_and_none_network_null_ipam_are_accepted(self):
        from types import SimpleNamespace
        self.patch(fixture, '_docker', return_value='network-id')
        self.patch(fixture, '_json', return_value=[{'IPAM': {'Config': None}}])
        self.patch(fixture.os, 'name', new='nt')
        self.patch(fixture.subprocess, 'run', return_value=SimpleNamespace(returncode=0, stdout='["0.0.0.0/0","10.20.0.0/16"]'))
        self.assertEqual(fixture._occupied_subnets({}), [ipaddress.ip_network('10.20.0.0/16')])


if __name__ == '__main__':
    unittest.main()
