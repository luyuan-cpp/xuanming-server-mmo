"""Offline fixture safety boundaries. Never talks to Docker, SQL or external services."""
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import docker_java_fixture as fixture


PRODUCER_INPUTS = (
    'tools/scripts/lib/sql_boot_catalog_local.ps1', 'tools/scripts/lib/release_common.ps1',
    'tools/scripts/maintenance_drain.ps1', 'deploy/sql/authority_runtime_role.sql',
    'deploy/sql/authority_operator_role.sql', 'java/mall_game/src/main/resources/application.yaml',
    'java/mall_game/src/main/resources/db/schema.sql',
)


class GreetingConnection:
    def __init__(self, chunks):
        self.chunks = list(chunks)
        self.timeouts = []

    def __enter__(self):
        return self

    def __exit__(self, *args):
        return False

    def settimeout(self, timeout):
        self.timeouts.append(timeout)

    def recv(self, size):
        if not self.chunks:
            return b''
        chunk = self.chunks.pop(0)
        if len(chunk) > size:
            self.chunks.insert(0, chunk[size:])
        return chunk[:size]


def greeting(version=b'8.0.11-TiDB-v8.5.8'):
    payload = (b'\x0a' + version + b'\0' + b'\x01\0\0\0' + b'abcdefgh' + b'\0' + b'\xff\xf7' +
               b'\x21' + b'\x02\0' + b'\xff\x81' + b'\x15' + b'\0' * 10 +
               b'ijklmnopqrst\0mysql_native_password\0')
    return len(payload).to_bytes(3, 'little') + b'\0' + payload


class FixtureTests(unittest.TestCase):
    def test_source_guard_covers_create_close_producer_inputs_without_unused_migrations(self):
        names = set(fixture.source_inputs())
        self.assertTrue(set(PRODUCER_INPUTS) <= names)
        for name in ('V3__catalog_admin.sql', 'V4__mock_payment.sql', 'V5__rmb_consignment.sql'):
            self.assertNotIn('java/mall_game/src/main/resources/db/migration/' + name, names)

    def test_changed_or_deleted_producer_input_blocks_close_before_external_operations(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            # Copy the actual public files, then exercise the real dynamic scan
            # and file hashes. No mocks replace source_inputs or _sha.
            for relative in fixture.PUBLIC_INPUTS:
                target = root / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(fixture.ROOT / relative, target)
            with patch.object(fixture, 'ROOT', root):
                baseline = {name: fixture._sha(root / name) for name in fixture.source_inputs()}
                for index, relative in enumerate(PRODUCER_INPUTS):
                    target = root / relative
                    original = target.read_bytes()
                    for change in ('edit', 'delete'):
                        with self.subTest(input=relative, change=change):
                            if change == 'edit':
                                target.write_bytes(original + b'\n# offline input guard change\n')
                            else:
                                target.unlink()
                            evidence = root / 'evidence' / (str(index) + '-' + change)
                            evidence.mkdir(parents=True)
                            (evidence / 'create.completed.json').write_text('{}')
                            state = {'mode': 'owned', 'javaBuildSucceeded': True, 'directory': str(evidence),
                                     'inputs': baseline, 'nonce': 'a' * 32, 'project': 'owned-unit', 'infrastructure': {}}
                            try:
                                with patch.object(fixture, '_inventory') as inventory, \
                                     patch.object(fixture, '_runner') as runner, patch.object(fixture, '_docker') as docker:
                                    with self.assertRaisesRegex(RuntimeError, 'retain remaining evidence'):
                                        fixture.close(state, {})
                                inventory.assert_not_called()
                                runner.assert_not_called()
                                docker.assert_not_called()
                                self.assertEqual(json.loads((evidence / 'close.hold.json').read_text())['status'], 'HOLD')
                            finally:
                                target.write_bytes(original)

    def test_readiness_retries_proxy_eof_and_short_greeting_without_sending(self):
        valid = greeting()
        connections = [GreetingConnection([]), GreetingConnection([valid[:2]]),
                       GreetingConnection([valid[:8]]), GreetingConnection([valid[:2], valid[2:9], valid[9:]])]
        with patch.object(fixture.time, 'monotonic', return_value=0), patch.object(fixture.time, 'sleep') as sleep, \
             patch.object(fixture.socket, 'create_connection', side_effect=connections) as connect:
            fixture._ready({}, seconds=10)
        self.assertEqual(connect.call_count, 4)
        self.assertEqual(sleep.call_count, 3)
        self.assertTrue(all(0 < timeout <= 1 for connection in connections for timeout in connection.timeouts))
        self.assertEqual(connect.call_args.args[0], ('127.0.0.1', 24000))

    def test_greeting_requires_sequence_protocol_exact_version_and_bounded_packet(self):
        valid = greeting()
        invalid = [valid[:3] + b'\x01' + valid[4:], valid[:4] + b'\x09' + valid[5:],
                   (16385).to_bytes(3, 'little') + b'\0', (33).to_bytes(3, 'little') + b'\0',
                   greeting(b'8.0.11-TiDB-v8.5.80'), greeting(b'8.0.11-TiDB-v8.5.8.1'), greeting(b'8.0.40-MySQL')]
        with patch.object(fixture.time, 'monotonic', return_value=0):
            self.assertTrue(fixture._read_greeting(GreetingConnection([valid]), 10))
            for packet in invalid:
                self.assertFalse(fixture._read_greeting(GreetingConnection([packet]), 10))

    def test_greeting_readiness_stops_at_deadline_without_sql_or_authentication(self):
        clock = [0.0]
        def sleep(seconds):
            clock[0] += seconds
        with patch.object(fixture.time, 'monotonic', side_effect=lambda: clock[0]), \
             patch.object(fixture.time, 'sleep', side_effect=sleep), \
             patch.object(fixture.socket, 'create_connection', side_effect=OSError('PRIVATE_NETWORK_DETAILS')) as connect, \
             patch.object(fixture, '_runner') as runner:
            with self.assertRaisesRegex(RuntimeError, 'MySQL greeting readiness deadline') as caught:
                fixture._ready({}, seconds=1.2)
        self.assertAlmostEqual(clock[0], 1.2)
        self.assertEqual(connect.call_count, 3)
        self.assertNotIn('PRIVATE_NETWORK_DETAILS', str(caught.exception))
        runner.assert_not_called()

    def test_source_guard_detects_new_and_deleted_go_files(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / 'go/shared/new.go'
            source.parent.mkdir(parents=True)
            with patch.object(fixture, 'ROOT', root), patch.object(fixture, 'PUBLIC_INPUTS', ()):
                self.assertEqual(fixture.source_inputs(), ())
                source.write_text('package shared')
                self.assertEqual(fixture.source_inputs(), ('go/shared/new.go',))
                source.unlink()
                self.assertEqual(fixture.source_inputs(), ())

    def test_partial_or_empty_core_is_external(self):
        for name in fixture.CORE_NAMES:
            self.assertEqual(fixture.mode({name: ''}), 'external')
            self.assertEqual(fixture.mode({name: 'sentinel'}), 'external')
        self.assertEqual(fixture.mode({}), 'owned')
        self.assertEqual(fixture.mode({name: 'sentinel' for name in fixture.HELPER_NAMES}), 'owned')

    def test_external_reports_names_never_values(self):
        environment = {'MMORPG_SQL_PASSWORD': 'NEVER_PRINT_PASSWORD'}
        with patch.object(fixture.shutil, 'which', return_value='tool'):
            errors = '\n'.join(fixture.local_requirements(environment))
        self.assertIn('MMORPG_SQL_WRITER_GRANT_FILE', errors)
        self.assertNotIn('NEVER_PRINT_PASSWORD', errors)

    def test_incomplete_explicit_helper_does_not_fall_back(self):
        self.assertEqual(fixture._helper({'MMORPG_SQL_PRIVATE_FILE_HELPER': 'explicit'}), ('explicit', ''))

    def test_external_jdbc_accepts_only_unambiguous_loopback_authority(self):
        for value in ('jdbc:mysql://127.0.0.1:24000/mall_it_unit', 'jdbc:mysql://localhost:65535/mall_1'):
            self.assertTrue(fixture._loopback_jdbc(value))
        for value in ('jdbc:mysql://remote.example:24000/mall', 'jdbc:mysql://127.0.0.1/mall',
                      'jdbc:mysql://127.0.0.1:24000/', 'jdbc:mysql://127.0.0.1:65536/mall',
                      'jdbc:mysql://127.0.0.1:024000/mall', 'jdbc:mysql://user@127.0.0.1:24000/mall',
                      'jdbc:mysql://127.0.0.1:24000,remote.example:3306/mall',
                      'jdbc:mysql://127.0.0.1:24000/mall?socketFactory=remote',
                      'jdbc:mysql://localhost.evil:24000/mall', 'jdbc:mysql://127.0.0.1:24000/mall#fragment',
                      'jdbc:mysql://localhost:24000/mall%2Fother', 'jdbc:mysql://127.0.0.1:24000/mall\n'):
            self.assertFalse(fixture._loopback_jdbc(value), value)

    def test_external_remote_url_rejected_without_exposing_url(self):
        private_url = 'jdbc:mysql://PRIVATE_REMOTE_USER@private.remote:3306/private_schema'
        with patch.object(fixture.shutil, 'which', return_value='tool'):
            errors = '\n'.join(fixture.local_requirements({'MMORPG_AUTHORITY_IT_JDBC_URL': private_url}))
        self.assertIn('one explicit loopback host', errors)
        self.assertNotIn(private_url, errors)
        self.assertNotIn('PRIVATE_REMOTE_USER', errors)

    def test_child_removes_case_insensitive_control_capabilities(self):
        environment = {'COMMERCE_IT_SCHEMA_DSN_FILE': 'private', 'commerce_it_other': 'private',
                       'MMORPG_SQL_SCHEMA_DSN': 'secret', 'MMORPG_SQL_REGISTRAR_DSN': 'secret',
                       'MMORPG_SQL_MAINTENANCE_DSN': 'secret', 'SQL_SCHEMA_DSN': 'secret',
                       'PATH': 'safe', 'MMORPG_SQL_PASSWORD': 'normal'}
        self.assertEqual(fixture._clean(environment), {'PATH': 'safe', 'MMORPG_SQL_PASSWORD': 'normal'})

    def test_child_sql_environment_is_only_normal_and_cf3_allowlist(self):
        allowed = {name: 'normal' for name in fixture.FIXTURE_NAMES}
        environment = {**allowed, 'PATH': 'safe', 'MMORPG_SQL_BOOT_CATALOG': 'old-private-catalog',
                       'MMORPG_SQL_BACKEND': 'old', 'mmorpg_sql_other_capability': 'private',
                       'COMMERCE_IT_OTHER': 'private'}
        self.assertEqual(fixture._clean(environment), {**allowed, 'PATH': 'safe'})

    def test_external_create_and_close_do_not_manage_resources(self):
        environment = {'MMORPG_SQL_PASSWORD': 'normal', 'COMMERCE_IT_SCHEMA_DSN_FILE': 'secret'}
        with patch.object(fixture, 'preflight') as preflight, patch.object(fixture, '_docker') as docker:
            child, state = fixture.create(Path('must-not-create'), environment)
            fixture.close(state, environment)
        preflight.assert_called_once_with(environment)
        docker.assert_not_called()
        self.assertEqual(state, {'mode': 'external'})
        self.assertEqual(child, {'MMORPG_SQL_PASSWORD': 'normal'})

    def test_command_errors_never_expose_output_or_argument(self):
        failed = subprocess.CompletedProcess(['tool'], 1, 'PASSWORD_OUTPUT', 'DSN_OUTPUT')
        with patch.object(fixture.subprocess, 'run', return_value=failed):
            with self.assertRaisesRegex(RuntimeError, 'retain evidence') as caught:
                fixture._run(['tool', 'PRIVATE_ARG'], {})
        self.assertNotIn('PASSWORD_OUTPUT', str(caught.exception))
        with patch.object(fixture.subprocess, 'run', side_effect=subprocess.TimeoutExpired('PRIVATE_ARG', 1, 'PRIVATE_OUTPUT')):
            with self.assertRaisesRegex(RuntimeError, 'unconfirmed') as caught:
                fixture._run(['tool'], {})
        self.assertNotIn('PRIVATE', str(caught.exception))

    def test_preflight_is_read_only_and_never_pulls(self):
        calls = []
        def docker(arguments, environment, timeout=60):
            calls.append(arguments)
            return arguments[-1] if arguments[:2] == ['image', 'inspect'] else 'Docker Compose'
        with patch.object(fixture, 'local_requirements', return_value=[]), patch.object(fixture, '_runner') as runner, \
             patch.object(fixture, '_port_available') as port, patch.object(fixture, '_docker', side_effect=docker), \
             patch.object(fixture, '_run', return_value='go version go1.27.2 windows/amd64\n'), \
             patch.object(fixture, '_receipt') as write:
            fixture.preflight({})
        runner.assert_called_once_with('check-helper', {})
        port.assert_called_once()
        write.assert_not_called()
        self.assertEqual(len(calls), 4)
        self.assertTrue(all(args[0] == 'image' or args == ['compose', 'version'] for args in calls))

    def test_port_conflict_prevents_docker_and_build(self):
        with patch.object(fixture, 'local_requirements', return_value=[]), patch.object(fixture, '_runner'), \
             patch.object(fixture, '_port_available', side_effect=RuntimeError('occupied')), \
             patch.object(fixture, '_docker') as docker, patch.object(fixture, '_run') as run:
            with self.assertRaisesRegex(RuntimeError, 'occupied'):
                fixture.preflight({})
        docker.assert_not_called()
        run.assert_not_called()

    def test_inventory_requires_full_container_ids_from_docker_list(self):
        state = {'project': 'owned-unit', 'nonce': 'a' * 32}
        labels = {'com.docker.compose.project': state['project'], fixture.OWNER_LABEL: state['nonce']}
        ids = dict(zip(('pd', 'tikv', 'tidb'), ('b' * 64, 'c' * 64, 'd' * 64)))
        rows = {}
        for service, cid in ids.items():
            mounts = [] if service == 'tidb' else [{'Type': 'volume', 'Name': state['project'] + '_' + service + '-data',
                                                   'Destination': '/data', 'RW': True}]
            if service == 'tikv':
                mounts.append({'Type': 'bind', 'Source': str(fixture.COMPOSE.parent / 'tikv.toml'),
                               'Destination': '/etc/tikv-fixture.toml', 'RW': False})
            rows[cid] = {'Id': cid, 'Config': {'Labels': {**labels, 'com.docker.compose.service': service}},
                         'Image': fixture.IMAGES[service], 'State': {'Running': True}, 'Mounts': mounts,
                         'NetworkSettings': {'Networks': {state['project'] + '_default': {}}},
                         'HostConfig': {'RestartPolicy': {'Name': 'no'},
                                        'Memory': (512 if service == 'pd' else 1536) * 1024 * 1024,
                                        'NanoCpus': 500000000 if service == 'pd' else 1000000000,
                                        'PortBindings': {'4000/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '24000'}]} if service == 'tidb' else {}}}
        def inspect(arguments, environment):
            if arguments[0] == 'container':
                return [rows[arguments[-1]]]
            if arguments[0] == 'network':
                return [{'Id': 'e' * 64, 'Labels': labels, 'Containers': dict.fromkeys(ids.values(), {})}]
            return [{'Name': arguments[-1], 'Driver': 'local', 'Options': {}, 'Labels': labels}]
        for force_short in (False, True):
            with self.subTest(force_short=force_short):
                def docker(arguments, environment):
                    if arguments[0] == 'compose':
                        return ids[arguments[-1]]
                    # Docker CLI defaults to twelve characters unless --no-trunc is requested.
                    values = ids.values() if '--no-trunc' in arguments and not force_short else [cid[:12] for cid in ids.values()]
                    return '\n'.join(values)
                with patch.object(fixture, '_docker', side_effect=docker), patch.object(fixture, '_json_docker', side_effect=inspect):
                    if force_short:
                        with self.assertRaisesRegex(RuntimeError, 'Unexpected container'):
                            fixture._inventory(state, {})
                    else:
                        self.assertEqual(fixture._inventory(state, {})['containers'], ids)

    def test_failed_create_retains_owned_resources_without_cleanup(self):
        evidence = fixture.ROOT / 'build/verification'
        evidence.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix='fixture-unit-', dir=evidence) as parent:
            directory = Path(parent) / 'new'
            with patch.object(fixture, 'preflight'), patch.object(fixture, '_run'), \
                 patch.object(fixture, '_sha', return_value='a' * 64), patch.object(fixture, 'source_inputs', return_value=()), \
                 patch.object(fixture, '_assert_absent'), patch.object(fixture, '_port_available'), \
                 patch.object(fixture, '_docker') as docker, \
                 patch.object(fixture, '_inventory', side_effect=RuntimeError('PRIVATE_SQL_ERROR')), \
                 patch.object(fixture, 'close') as close:
                with self.assertRaisesRegex(RuntimeError, 'no automatic retry or cleanup') as caught:
                    fixture.create(directory, {})
            self.assertNotIn('PRIVATE_SQL_ERROR', str(caught.exception))
            close.assert_not_called()
            self.assertTrue((directory / 'create.hold.json').is_file())
            hold = json.loads((directory / 'create.hold.json').read_text())
            self.assertEqual(hold['phase'], 'verify-infrastructure')
            self.assertNotIn('PRIVATE_SQL_ERROR', json.dumps(hold))
            self.assertTrue((directory / 'state.json').is_file())
            commands = [call.args[0] for call in docker.call_args_list]
            self.assertEqual(sum('up' in command for command in commands), 1)
            self.assertFalse(any(token in command for command in commands for token in ('down', 'rm', 'stop')))

    def test_create_hold_records_only_controlled_public_failure_reason(self):
        evidence = fixture.ROOT / 'build/verification'
        evidence.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix='fixture-unit-', dir=evidence) as parent:
            directory = Path(parent) / 'new'
            reason = 'Unexpected container in the Java fixture project'
            with patch.object(fixture, 'preflight'), patch.object(fixture, '_run'), \
                 patch.object(fixture, '_sha', return_value='a' * 64), patch.object(fixture, 'source_inputs', return_value=()), \
                 patch.object(fixture, '_assert_absent'), patch.object(fixture, '_port_available'), patch.object(fixture, '_docker'), \
                 patch.object(fixture, '_inventory', side_effect=fixture._FixtureFailure(reason)):
                with self.assertRaisesRegex(RuntimeError, 'no automatic retry or cleanup'):
                    fixture.create(directory, {})
            hold = json.loads((directory / 'create.hold.json').read_text())
            self.assertEqual(hold['phase'], 'verify-infrastructure')
            self.assertEqual(hold['reason'], reason)
            self.assertFalse(hold['automaticRetry'])
            self.assertFalse(hold['automaticCleanup'])

    def test_close_requires_confirmed_build(self):
        for success in (None, False, 1, 'true'):
            with patch.object(fixture, '_docker') as docker:
                with self.assertRaisesRegex(RuntimeError, 'successful Java build'):
                    fixture.close({'mode': 'owned', 'javaBuildSucceeded': success}, {})
            docker.assert_not_called()

    def test_changed_ownership_prevents_retirement_and_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'create.completed.json').write_text('{}')
            state = {'mode': 'owned', 'javaBuildSucceeded': True, 'directory': directory,
                     'inputs': {}, 'nonce': 'a' * 32, 'project': 'test', 'infrastructure': {'original': True}}
            with patch.object(fixture, 'source_inputs', return_value=()), patch.object(fixture, '_inventory', return_value={'changed': True}), \
                 patch.object(fixture, '_runner') as runner, patch.object(fixture, '_docker') as docker:
                with self.assertRaisesRegex(RuntimeError, 'unconfirmed'):
                    fixture.close(state, {})
            runner.assert_not_called()
            docker.assert_not_called()
            self.assertTrue((root / 'close.hold.json').exists())

    def test_failed_formal_retirement_never_removes_infrastructure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'create.completed.json').write_text('{}')
            inventory = {'containers': {'pd': 'a' * 64}, 'networkId': 'b' * 64, 'volumes': {}}
            state = {'mode': 'owned', 'javaBuildSucceeded': True, 'directory': directory,
                     'inputs': {}, 'nonce': 'c' * 32, 'project': 'test', 'infrastructure': inventory}
            with patch.object(fixture, 'source_inputs', return_value=()), patch.object(fixture, '_inventory', return_value=inventory), \
                 patch.object(fixture, '_runner', side_effect=RuntimeError('UNKNOWN')), patch.object(fixture, '_docker') as docker:
                with self.assertRaisesRegex(RuntimeError, 'unconfirmed'):
                    fixture.close(state, {})
            docker.assert_not_called()

    def test_success_close_uses_exact_recorded_ids_and_no_compose_down(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'create.completed.json').write_text('{}')
            (root / 'producer-close.completed.json').write_text('{}')
            inventory = {'containers': dict(zip(('pd', 'tikv', 'tidb'), ('a' * 64, 'b' * 64, 'c' * 64))),
                         'networkId': 'd' * 64, 'volumes': {'owned-pd': {'Name': 'owned-pd', 'CreatedAt': 'original'}}}
            state = {'mode': 'owned', 'javaBuildSucceeded': True, 'directory': directory, 'inputs': {},
                     'nonce': 'e' * 32, 'project': 'owned', 'infrastructure': inventory}
            with patch.object(fixture, 'source_inputs', return_value=()), patch.object(fixture, '_inventory', return_value=inventory), patch.object(fixture, '_runner') as runner, \
                 patch.object(fixture, '_json_docker', return_value=[inventory['volumes']['owned-pd']]), \
                 patch.object(fixture, '_docker') as docker:
                fixture.close(state, {})
            runner.assert_called_once_with('close', {}, root / 'state.json')
            self.assertEqual([call.args[0] for call in docker.call_args_list], [
                ['container', 'stop', '--time', '30', 'a' * 64, 'b' * 64, 'c' * 64],
                ['container', 'rm', 'a' * 64, 'b' * 64, 'c' * 64],
                ['network', 'rm', 'd' * 64], ['volume', 'rm', 'owned-pd']])

    def test_context_only_returns_normal_and_cf3_and_rejects_escape(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            nonce = 'a' * 32
            state = {'directory': directory, 'nonce': nonce, 'fixtureRoot': str(root), 'contextFile': str(root / 'context.json')}
            context = {'format': 'JAVA_OWNED_TIDB_CONTEXT_V1', 'nonce': nonce,
                       'ownerFile': str(root / 'owner.json'), 'ownerSha256': 'b' * 64,
                       'jdbcUrl': 'jdbc:mysql://127.0.0.1:24000/mall_it_' + nonce,
                       'username': 'normal', 'password': 'PRIVATE_PASSWORD',
                       'environment': {name: 'value' for name in fixture.CORE_NAMES[3:]}}
            context['environment']['MMORPG_SQL_WRITER_GRANT_FILE'] = str(root / 'grant.json')
            context['environment']['MMORPG_SQL_WRITER_CONSUME_DIR'] = str(root / 'consume')
            def read_context():
                raw = json.dumps(context)
                (root / 'producer-create.completed.json').write_text(json.dumps({'contextSha256': hashlib.sha256(raw.encode()).hexdigest()}))
                with patch.object(fixture, '_run', return_value=raw):
                    return fixture._read_context(state, {'COMMERCE_IT_SCHEMA_DSN_FILE': 'secret',
                                                        'MMORPG_SQL_SCHEMA_DSN': 'secret'})
            child = read_context()
            self.assertEqual(set(child), set(fixture.FIXTURE_NAMES))
            self.assertEqual(child['MMORPG_SQL_PASSWORD'], 'PRIVATE_PASSWORD')
            context['environment']['MMORPG_SQL_WRITER_GRANT_FILE'] = str(root.parent / 'foreign.json')
            with self.assertRaisesRegex(RuntimeError, 'escaped'):
                read_context()


if __name__ == '__main__':
    unittest.main()
