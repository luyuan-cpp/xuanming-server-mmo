"""Offline contracts for the orchestration only; no Docker, builds, SQL or third-party tests."""
from contextlib import ExitStack, redirect_stdout
import copy
import io
import json
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import rebuild_all_docker_debug as subject
import prepare_docker_debug as prepare


class RebuildAllTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.state = self.root / 'state'
        self.state.mkdir()
        self.canonical = {'COMPOSE_PROJECT_NAME': subject.startup.PROJECT, **prepare.DEV_INTERNAL_IDENTITY}
        (self.state / '.env').write_text(''.join(f'{k}={v}\n' for k, v in self.canonical.items()), encoding='utf-8')
        self.stack = ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.object(subject, 'STATE', self.state))
        self.stack.enter_context(patch.object(subject, 'ROOT', self.root))
        # No test may accidentally dispatch a real external process.
        self.popen = self.stack.enter_context(patch.object(subject.subprocess, 'Popen', side_effect=AssertionError('external process')))
        self.stack.enter_context(patch.object(subject.startup, 'run', side_effect=AssertionError('Docker command')))
        self.stack.enter_context(patch.object(subject.rebuild, 'execute', side_effect=AssertionError('Docker command')))
        # Fixture provisioning is independently tested. Orchestration tests never launch it.
        self.fixture_preflight = self.mock(subject.java_fixture, 'preflight')
        self.fixture_create = self.mock(subject.java_fixture, 'create',
                                       side_effect=lambda directory, env: ({**env, 'FIXTURE_CHILD': 'yes'}, {}))
        self.fixture_close = self.mock(subject.java_fixture, 'close')

    def mock(self, owner, name, **kwargs):
        return self.stack.enter_context(patch.object(owner, name, **kwargs))

    def pipeline_fixture(self):
        names = [name for language in subject.ORDER for name in subject.SERVICES[language]]
        names += [name + '-sql-boot' for name in subject.rebuild.SQL_WRITERS]
        names += list(subject.startup.INFRA)
        self.rows = {name: {'Id': name, 'Mounts': [], 'State': {'Status': 'exited' if name.endswith('-sql-boot') else 'running',
                                                            'ExitCode': 0}} for name in names}
        for name, row in self.rows.items():
            row['Image'] = 'built-' + ('sql-boot' if name.endswith('-sql-boot') else name)
        self.config = {'services': {name: {'image': 'local/' + name + ':old'} for name in names}}
        self.baseline = {'bases': {name: value['image'] for name, value in self.config['services'].items()},
                         'baseIds': {}, 'envSha256': 'digest', 'services': copy.deepcopy(self.rows),
                         'historical': [], 'projectVolumes': [], 'sqlBootImageId': 'existing-sql-boot'}
        self.mock(subject, 'local_requirements', return_value=[])
        self.preflight = self.mock(subject, 'preflight', return_value=(self.config, self.baseline))
        self.run_stage = self.mock(subject, 'run_stage', return_value=0)
        def built(language, directory):
            return {'success': True, 'images': {name: {'image': 'local/' + name + ':new', 'imageId': 'built-' + name} for name in subject.SERVICES[language]},
                    'manifestPaths': {}}
        self.validate = self.mock(subject, 'validate_build', side_effect=built)
        self.verify = self.mock(subject, 'verify_sources')
        self.unchanged = self.mock(subject, 'unchanged_before_apply')
        self.runtime = self.mock(subject, 'unchanged_runtime')
        self.refresh_contract = self.mock(subject, 'refreshed_contract', return_value=(self.canonical, {}))
        self.refreshed = self.mock(subject, 'unchanged_refreshed')
        self.update = self.mock(subject.rebuild, 'update_images')
        self.mock(subject, 'compose_config', return_value=self.config)
        self.mock(subject, 'current_containers', return_value=(self.rows, []))
        self.mock(subject.startup, 'project_volumes', return_value=[])
        self.mock(subject.startup, 'signature', side_effect=lambda row: row['Id'])
        self.mock(subject.startup, 'state', side_effect=lambda row: row['State'])
        self.mock(subject.startup, 'ports', return_value=[18085, 18086])
        self.mock(subject.startup, 'ready', return_value=True)
        self.check = self.mock(subject.rebuild, 'check', return_value=True)
        self.config_check = self.mock(subject.rebuild, 'check_config_inputs')
        self.result = {'success': False, 'stages': [], 'configurationWriteStarted': False, 'containerUpdateStarted': False}

    def run_pipeline(self, languages=subject.ORDER, check=False, inherited=None):
        with redirect_stdout(io.StringIO()):
            subject.pipeline(languages, self.root, inherited or {}, self.result, check)

    def test_selection_fixed_order_and_rejects_duplicates_or_empty(self):
        self.assertEqual(subject.select_languages('java,cpp,go'), subject.ORDER)
        self.assertEqual(subject.select_languages('go,cpp'), ('cpp', 'go'))
        for value in ('', 'cpp,cpp', 'ruby', 'cpp,', 'CPP'):
            with self.subTest(value=value), self.assertRaises(RuntimeError):
                subject.select_languages(value)

    def test_missing_fixture_stops_before_preflight_or_build(self):
        self.pipeline_fixture()
        self.mock(subject, 'local_requirements', return_value=['Missing Java NORMAL authority fixture'])
        with self.assertRaisesRegex(RuntimeError, 'No build, configuration refresh or container update'):
            self.run_pipeline()
        self.preflight.assert_not_called()
        self.run_stage.assert_not_called()
        self.update.assert_not_called()
        self.fixture_preflight.assert_not_called()
        self.fixture_create.assert_not_called()

    def test_real_fixture_probe_reports_names_not_values(self):
        java = SimpleNamespace(DEFAULT_JDK=self.root, DEFAULT_MAVEN=self.root / 'mvn',
                               DEFAULT_PROTOC=self.root / 'protoc', DEFAULT_PROTOC_SHA256='0' * 64)
        self.mock(subject.java_fixture, 'local_requirements',
                  return_value=['Missing external fixture variables: MMORPG_AUTHORITY_IT_JDBC_URL'])
        with patch.dict(sys.modules, {'build_docker_java_debug': java}), patch.object(subject.shutil, 'which', return_value='tool'):
            issues = subject.local_requirements(('java',), {'MMORPG_SQL_PASSWORD': 'private-password'})
        message = '\n'.join(issues)
        self.assertIn('MMORPG_AUTHORITY_IT_JDBC_URL', message)
        self.assertNotIn('private-password', message)

    def test_empty_environment_uses_fixture_prerequisites_without_manual_ten_variables(self):
        (self.root / 'bin').mkdir()
        for name in ('bin/java.exe', 'bin/javap.exe', 'mvn', 'protoc'):
            (self.root / name).write_text('tool', encoding='utf-8')
        java = SimpleNamespace(DEFAULT_JDK=self.root, DEFAULT_MAVEN=self.root / 'mvn',
                               DEFAULT_PROTOC=self.root / 'protoc', DEFAULT_PROTOC_SHA256='0' * 64,
                               sha256=lambda path: '0' * 64)
        requirements = self.mock(subject.java_fixture, 'local_requirements', return_value=[])
        with patch.dict(sys.modules, {'build_docker_java_debug': java}), patch.object(subject.shutil, 'which', return_value='tool'):
            self.assertEqual(subject.local_requirements(('java',), {}), [])
        requirements.assert_called_once_with({})

    def test_check_has_no_build_refresh_or_update(self):
        self.pipeline_fixture()
        self.run_pipeline(check=True)
        self.assertTrue(self.result['success'])
        self.assertFalse(self.result['applied'])
        self.run_stage.assert_not_called()
        self.update.assert_not_called()
        self.fixture_preflight.assert_called_once()
        self.fixture_create.assert_not_called()
        self.fixture_close.assert_not_called()
        report = json.loads((self.root / 'preflight.json').read_text('utf-8'))
        self.assertEqual(report['javaFixture'], {'mode': 'owned', 'readOnlyChecksPassed': True, 'created': False})
        self.assertNotIn('javaFixturePresenceOnly', report)

    def test_success_builds_serially_then_refreshes_and_recreates_only_targets(self):
        self.pipeline_fixture()
        self.run_pipeline()
        calls = self.run_stage.call_args_list
        self.assertEqual(len(calls), 5)
        for language, call in zip(subject.ORDER, calls[:3]):
            command = call.args[0]
            self.assertTrue(command[4].endswith('build_docker_' + language + '_debug.py'))
            self.assertNotIn('db-migrate', ' '.join(command))
            self.assertNotIn('skipTests', ' '.join(command))
            if language != 'java':
                self.assertEqual(command[-2:], ['--jobs', '1'])
        self.assertEqual(calls[3].args[0][-1], '--refresh-existing')
        command = calls[4].args[0]
        for flag in ('--no-deps', '--no-build', '--force-recreate', '--wait'):
            self.assertIn(flag, command)
        self.assertEqual(command[command.index('--pull') + 1], 'never')
        targets = command[command.index('--wait-timeout') + 2:]
        self.assertEqual(set(targets), set(subject.startup.APPS) | {s + '-sql-boot' for s in subject.rebuild.SQL_WRITERS})
        self.assertFalse(set(targets) & (set(subject.startup.INFRA) | set(subject.startup.INIT)))
        self.update.assert_called_once()
        self.assertEqual(self.verify.call_count, 3)
        self.assertEqual(self.runtime.call_count, 2)
        self.assertTrue(self.result['success'])
        self.fixture_close.assert_called_once()
        self.assertTrue(self.fixture_close.call_args.args[0]['javaBuildSucceeded'])
        self.assertEqual(self.result['javaFixture'], {'mode': 'owned', 'status': 'closed'})

    def test_fixture_preflight_failure_stops_before_any_build(self):
        self.pipeline_fixture()
        self.fixture_preflight.side_effect = RuntimeError('Pinned fixture image missing')
        with self.assertRaisesRegex(RuntimeError, 'Pinned fixture image missing'):
            self.run_pipeline()
        self.preflight.assert_not_called()
        self.run_stage.assert_not_called()
        self.fixture_create.assert_not_called()
        self.fixture_close.assert_not_called()
        self.update.assert_not_called()
        self.assertEqual(self.result['javaFixture']['status'], 'preflight-failed')

    def test_fixture_created_only_at_java_and_closed_after_verification_before_refresh(self):
        self.pipeline_fixture()
        def create(directory, environment):
            self.assertEqual(directory, self.root / 'java-fixture')
            self.assertEqual(self.run_stage.call_count, 2)
            self.assertEqual(self.validate.call_count, 2)
            self.assertFalse(self.result['configurationWriteStarted'])
            return {**environment, 'FIXTURE_CHILD': 'yes'}, {}
        def close(state, environment):
            self.assertEqual(self.run_stage.call_count, 3)
            self.assertEqual(self.validate.call_count, 3)
            self.verify.assert_called_once()
            self.unchanged.assert_called_once()
            self.assertTrue(state['javaBuildSucceeded'])
            self.assertFalse(self.result['configurationWriteStarted'])
        self.fixture_create.side_effect = create
        self.fixture_close.side_effect = close
        self.run_pipeline()
        self.fixture_create.assert_called_once()
        self.fixture_close.assert_called_once()
        for index, call in enumerate(self.run_stage.call_args_list):
            self.assertEqual(call.args[2].get('FIXTURE_CHILD'), 'yes' if index == 2 else None)

    def test_fixture_creation_failure_preserves_state_and_never_builds_java_or_cleans(self):
        self.pipeline_fixture()
        self.fixture_create.side_effect = RuntimeError('Fixture outcome UNKNOWN; inspect evidence')
        with self.assertRaisesRegex(RuntimeError, 'UNKNOWN'):
            self.run_pipeline()
        self.assertEqual(self.run_stage.call_count, 2)
        self.fixture_create.assert_called_once()
        self.fixture_close.assert_not_called()
        self.update.assert_not_called()
        self.assertEqual(self.result['javaFixture']['status'], 'create-failed')
        self.assertFalse(self.result['configurationWriteStarted'])

    def test_java_invalid_build_receipt_keeps_fixture_and_does_not_apply(self):
        self.pipeline_fixture()
        original = self.validate.side_effect
        self.validate.side_effect = lambda language, directory: (subject.require(False, 'Incomplete Java build')
            if language == 'java' else original(language, directory))
        with self.assertRaisesRegex(RuntimeError, 'Incomplete Java build'):
            self.run_pipeline()
        self.assertEqual(self.run_stage.call_count, 3)
        self.fixture_close.assert_not_called()
        self.update.assert_not_called()
        self.assertEqual(self.result['javaFixture']['status'], 'build-failed-preserved')

    def test_fixture_close_failure_never_retries_refreshes_or_deploys(self):
        self.pipeline_fixture()
        self.fixture_close.side_effect = RuntimeError('Retirement outcome UNKNOWN')
        with self.assertRaisesRegex(RuntimeError, 'UNKNOWN'):
            self.run_pipeline()
        self.assertEqual(self.run_stage.call_count, 3)
        self.fixture_close.assert_called_once()
        self.update.assert_not_called()
        self.assertFalse(self.result['configurationWriteStarted'])
        self.assertFalse(self.result['containerUpdateStarted'])
        self.assertEqual(self.result['javaFixture']['status'], 'close-failed')

    def test_external_fixture_is_never_closed_and_java_has_no_management_credentials(self):
        self.pipeline_fixture()
        private = {'COMMERCE_IT_JDBC_URL': 'private-commerce', 'COMMERCE_IT_PASSWORD': 'private-password',
                   'MMORPG_SQL_SCHEMA_DSN': 'private-schema', 'MMORPG_SQL_REGISTRAR_DSN': 'private-registrar',
                   'MMORPG_SQL_MAINTENANCE_DSN': 'private-maintenance'}
        external = {'MMORPG_AUTHORITY_IT_JDBC_URL': 'jdbc:mysql://127.0.0.1:24000/mmorpg',
                    'MMORPG_SQL_USERNAME': 'normal-writer', 'MMORPG_SQL_PASSWORD': 'private-writer',
                    'MMORPG_SQL_WRITER_GRANT_FILE': str(self.root / 'grant.json'),
                    'MMORPG_SQL_WRITER_GRANT_SHA256': 'a' * 64, 'MMORPG_SQL_DEPLOYMENT_ID': 'external-deployment',
                    'MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256': 'b' * 64,
                    'MMORPG_SQL_WRITER_CONSUME_DIR': str(self.root / 'consume'),
                    'MMORPG_SQL_PRIVATE_FILE_HELPER': str(self.root / 'helper.exe'),
                    'MMORPG_SQL_PRIVATE_FILE_HELPER_SHA256': 'c' * 64}
        self.run_pipeline(inherited={**private, **external})
        child = self.run_stage.call_args_list[2].args[2]
        self.assertFalse(set(child) & set(private))
        self.assertEqual(child['MMORPG_SQL_USERNAME'], 'normal-writer')
        self.fixture_close.assert_not_called()
        self.assertEqual(self.result['javaFixture'], {'mode': 'external', 'status': 'external-preserved'})
        public = json.dumps(self.result) + (self.root / 'preflight.json').read_text('utf-8')
        for value in private.values():
            self.assertNotIn(value, public)

    def test_private_helper_configuration_alone_keeps_owned_mode(self):
        self.pipeline_fixture()
        self.run_pipeline(check=True, inherited={'MMORPG_SQL_PRIVATE_FILE_HELPER': str(self.root / 'helper.exe'),
                                                'MMORPG_SQL_PRIVATE_FILE_HELPER_SHA256': 'c' * 64})
        self.assertEqual(self.result['javaFixture']['mode'], 'owned')
        self.fixture_create.assert_not_called()

    def test_owned_java_environment_also_drops_inherited_management_credentials(self):
        self.pipeline_fixture()
        self.run_pipeline(inherited={'COMMERCE_IT_PASSWORD': 'private', 'MMORPG_SQL_SCHEMA_DSN': 'private'})
        child = self.run_stage.call_args_list[2].args[2]
        self.assertNotIn('COMMERCE_IT_PASSWORD', child)
        self.assertNotIn('MMORPG_SQL_SCHEMA_DSN', child)

    def test_refresh_source_guard_includes_all_public_fixture_inputs(self):
        scan = self.mock(subject.rebuild, 'digest', return_value='digest')
        public = ('tools/scripts/docker_java_fixture.py', 'fixture/public.ps1', 'fixture/compose.yml')
        inputs = self.mock(subject.java_fixture, 'source_inputs', return_value=public)
        sources = subject.refresh_sources()
        self.assertTrue(set(public) <= set(sources))
        self.assertIn('tools/scripts/build_docker_java_debug.py', sources)
        self.assertEqual(scan.call_count, len(sources))
        inputs.assert_called_once_with()
        inputs.return_value = public + ('go/shared/new-source.go',)
        self.assertIn('go/shared/new-source.go', subject.refresh_sources())

    def test_each_build_failure_stops_without_apply_or_retry(self):
        self.pipeline_fixture()
        for failed_index in range(3):
            with self.subTest(failed_index=failed_index):
                self.run_stage.reset_mock(side_effect=True)
                self.fixture_create.reset_mock()
                self.run_stage.side_effect = [0] * failed_index + [7]
                with self.assertRaisesRegex(RuntimeError, 'Build failed'):
                    self.run_pipeline()
                self.assertEqual(self.run_stage.call_count, failed_index + 1)
                self.update.assert_not_called()
                self.assertFalse(self.result['configurationWriteStarted'])
                self.fixture_close.assert_not_called()
                self.assertEqual(self.fixture_create.call_count, int(failed_index == 2))
                # The next subtest represents a new invocation/evidence directory.
                for path in self.root.glob('*-base-images.json'):
                    path.unlink()
                (self.root / 'preflight.json').unlink()

    def test_source_drift_before_refresh_prevents_any_apply(self):
        self.pipeline_fixture()
        self.verify.side_effect = RuntimeError('source drift')
        with self.assertRaisesRegex(RuntimeError, 'source drift'):
            self.run_pipeline()
        self.assertEqual(self.run_stage.call_count, 3)
        self.update.assert_not_called()
        self.fixture_close.assert_not_called()

    def test_source_drift_after_refresh_prevents_image_update(self):
        self.pipeline_fixture()
        self.verify.side_effect = [None, RuntimeError('late source drift')]
        with self.assertRaisesRegex(RuntimeError, 'late source drift'):
            self.run_pipeline()
        self.assertEqual(self.run_stage.call_count, 4)
        self.update.assert_not_called()
        self.assertTrue(self.result['configurationWriteStarted'])
        self.assertFalse(self.result['containerUpdateStarted'])

    def test_source_drift_after_image_update_prevents_recreate(self):
        self.pipeline_fixture()
        self.verify.side_effect = [None, None, RuntimeError('last source drift')]
        with self.assertRaisesRegex(RuntimeError, 'last source drift'):
            self.run_pipeline()
        self.assertEqual(self.run_stage.call_count, 4)
        self.update.assert_called_once()
        self.assertFalse(self.result['containerUpdateStarted'])

    def test_canonical_drift_after_refresh_prevents_image_update(self):
        self.pipeline_fixture()
        self.refreshed.side_effect = RuntimeError('canonical drift')
        with self.assertRaisesRegex(RuntimeError, 'canonical drift'):
            self.run_pipeline()
        self.update.assert_not_called()
        self.assertEqual(self.run_stage.call_count, 4)

    def test_refreshed_file_drift_after_image_update_prevents_recreate(self):
        self.pipeline_fixture()
        self.refreshed.side_effect = [None, RuntimeError('refreshed file drift')]
        with self.assertRaisesRegex(RuntimeError, 'refreshed file drift'):
            self.run_pipeline()
        self.update.assert_called_once()
        self.assertEqual(self.run_stage.call_count, 4)

    def test_refreshed_guard_checks_actual_env_and_bind_digest(self):
        rows = {'services': {}, 'historical': []}
        self.mock(subject.startup, 'binding_hashes', return_value={'config': 'new'})
        with self.assertRaisesRegex(RuntimeError, 'Runtime files changed after refresh'):
            subject.unchanged_refreshed(self.canonical, {'config': 'old'}, rows)
        with self.assertRaisesRegex(RuntimeError, 'Canonical environment changed'):
            subject.unchanged_refreshed({**self.canonical, 'GATEWAY_PORT': 'wrong'}, {'config': 'new'}, rows)

    def test_refreshed_contract_rejects_changed_port_before_apply(self):
        canonical = {**self.canonical, 'GATEWAY_PORT': '18086'}
        with self.assertRaisesRegex(RuntimeError, 'preserved canonical setting'):
            subject.refreshed_contract(canonical, {}, {}, {})

    def test_refresh_failure_stops_and_marks_partial_write(self):
        self.pipeline_fixture()
        self.run_stage.side_effect = [0, 0, 0, 9]
        with self.assertRaisesRegex(RuntimeError, 'Configuration refresh failed'):
            self.run_pipeline()
        self.update.assert_not_called()
        self.assertTrue(self.result['configurationWriteStarted'])
        self.assertFalse(self.result['containerUpdateStarted'])

    def test_recreate_failure_preserves_receipt_without_retry(self):
        self.pipeline_fixture()
        self.run_stage.side_effect = [0, 0, 0, 0, 12]
        with self.assertRaisesRegex(RuntimeError, 'completed changes remain applied'):
            self.run_pipeline()
        self.assertEqual(self.run_stage.call_count, 5)
        self.assertTrue(self.result['containerUpdateStarted'])
        self.check.assert_not_called()

    def test_partial_cpp_go_never_builds_or_recreates_java(self):
        self.pipeline_fixture()
        self.run_pipeline(('cpp', 'go'))
        self.assertEqual(self.run_stage.call_count, 4)
        targets = self.run_stage.call_args.args[0]
        self.assertNotIn('gateway', targets)
        self.assertNotIn('gateway-sql-boot', targets)
        self.fixture_preflight.assert_not_called()
        self.fixture_create.assert_not_called()
        self.fixture_close.assert_not_called()

    def test_environment_discards_compose_and_canonical_overrides(self):
        canonical = {'GATEWAY_PORT': '18086', 'MYSQL_ROOT_PASSWORD': 'canonical', **prepare.DEV_INTERNAL_IDENTITY}
        result = subject.child_environment(canonical, {'GATEWAY_PORT': '9999', 'MYSQL_ROOT_PASSWORD': 'override',
                                            'COMPOSE_FILE': 'wrong', 'COMPOSE_PROFILES': 'unsafe',
                                            'DOCKER_CONTEXT': 'wrong', 'PATH': 'tool-path'})
        self.assertNotIn('COMPOSE_FILE', result)
        self.assertNotIn('COMPOSE_PROFILES', result)
        self.assertNotIn('GATEWAY_PORT', result)
        self.assertNotIn('MYSQL_ROOT_PASSWORD', result)
        self.assertEqual(result['DOCKER_CONTEXT'], 'desktop-linux')
        self.assertEqual(result['PATH'], 'tool-path')

    def test_final_runtime_rejects_other_image_even_if_source_check_would_pass(self):
        self.pipeline_fixture()
        self.rows['scene']['Image'] = 'old-source-consistent-image'
        with self.assertRaisesRegex(RuntimeError, 'expected image.*scene'):
            self.run_pipeline()
        self.check.assert_not_called()
        self.assertFalse(self.result['success'])

    def test_final_registration_rejects_previous_sql_boot_image(self):
        self.pipeline_fixture()
        self.rows['gateway-sql-boot']['Image'] = self.baseline['sqlBootImageId']
        with self.assertRaisesRegex(RuntimeError, 'expected image.*gateway-sql-boot'):
            self.run_pipeline()
        self.assertFalse(self.result['success'])

    def test_java_only_registration_uses_preflight_sql_boot_id_without_resolving_tag_again(self):
        self.pipeline_fixture()
        self.rows['gateway-sql-boot']['Image'] = self.baseline['sqlBootImageId']
        self.run_pipeline(('java',))
        self.assertTrue(self.result['success'])
        subject.startup.run.assert_not_called()

    def test_java_only_registration_rejects_tag_drift_after_preflight(self):
        self.pipeline_fixture()
        self.rows['gateway-sql-boot']['Image'] = 'different-image-behind-same-tag'
        with self.assertRaisesRegex(RuntimeError, 'expected image.*gateway-sql-boot'):
            self.run_pipeline(('java',))

    def test_capture_redacts_secrets_and_propagates_log_failures(self):
        for failure in (None, 'write', 'flush', 'read'):
            with self.subTest(failure=failure):
                class BrokenLog(io.BytesIO):
                    def write(self, value):
                        if failure == 'write':
                            raise OSError('private-password')
                        return super().write(value)
                    def flush(self):
                        if failure == 'flush':
                            raise OSError('private-password')
                        return super().flush()
                    def close(self):
                        pass
                class ChildOutput(io.BytesIO):
                    def readline(self):
                        if failure == 'read':
                            raise OSError('private-password')
                        return super().readline()
                log = BrokenLog()
                process = SimpleNamespace(stdout=ChildOutput(b'private-password\nprivate-dsn\n'), wait=lambda timeout: 0)
                self.popen.side_effect = None
                self.popen.return_value = process
                output = SimpleNamespace(open=lambda mode: log)
                if failure:
                    with self.assertRaisesRegex(RuntimeError, 'Stage log capture failed') as caught:
                        subject.run_stage(['mock'], output, {'SQL_PASSWORD': 'private-password', 'SQL_SCHEMA_DSN': 'private-dsn'}, 1)
                    self.assertNotIn('private-password', str(caught.exception))
                else:
                    self.assertEqual(subject.run_stage(['mock'], output,
                                                     {'SQL_PASSWORD': 'private-password', 'SQL_SCHEMA_DSN': 'private-dsn'}, 1), 0)
                    self.assertEqual(log.getvalue(), b'[REDACTED]\n[REDACTED]\n')

    def partial_fixture(self):
        names = ['bin/etc/base_deploy_config.yaml', 'bin/etc/game_config.yaml', 'bin/etc/battle_rewards.json',
                 'tools/scripts/prepare_docker_debug.py',
                 *prepare.SQL_BOOT_SOURCES,
                 *(f'go/{module}/etc/{name}' for module, name in prepare.GO_CONFIGS.items())]
        recorded = {}
        for name in names:
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('original', encoding='utf-8')
            recorded[name] = subject.rebuild.digest(path)
        (self.state / 'source-manifest.json').write_text(json.dumps(recorded), encoding='utf-8')
        rows = {name: {'Config': {'Env': ['KEY=value']}} for group in subject.SERVICES.values() for name in group}
        config = {'services': {name: {'environment': {'KEY': 'value'}} for name in rows}}
        return config, rows

    def test_partial_identity_migration_requires_cpp_and_go(self):
        config, rows = self.partial_fixture()
        with self.assertRaisesRegex(RuntimeError, r'requires C\+\+ and Go together'):
            subject.partial_refresh_guard(('cpp',), {}, config, rows)
        subject.partial_refresh_guard(('cpp', 'go'), {}, config, rows)

    def test_partial_cannot_refresh_omitted_language_configuration(self):
        config, rows = self.partial_fixture()
        (self.root / 'java/gateway_node/src/main/resources/application.yaml').write_text('changed', encoding='utf-8')
        with self.assertRaisesRegex(RuntimeError, 'omitted language java'):
            subject.partial_refresh_guard(('cpp', 'go'), self.canonical, config, rows)

    def test_partial_cannot_mix_changed_compose_environment(self):
        config, rows = self.partial_fixture()
        config['services']['gateway']['environment']['KEY'] = 'changed'
        with self.assertRaisesRegex(RuntimeError, 'omitted service gateway'):
            subject.partial_refresh_guard(('cpp', 'go'), self.canonical, config, rows)

    def test_partial_shared_sql_changes_require_all_writers(self):
        config, rows = self.partial_fixture()
        (self.root / prepare.SQL_BOOT_SOURCES[0]).write_text('changed', encoding='utf-8')
        with self.assertRaisesRegex(RuntimeError, 'include go,java'):
            subject.partial_refresh_guard(('cpp', 'go'), self.canonical, config, rows)

    def test_partial_renderer_changes_or_missing_record_require_full_build(self):
        config, rows = self.partial_fixture()
        renderer = 'tools/scripts/prepare_docker_debug.py'
        (self.root / renderer).write_text('changed', encoding='utf-8')
        with self.assertRaisesRegex(RuntimeError, 'renderer changed'):
            subject.partial_refresh_guard(('cpp', 'go'), self.canonical, config, rows)
        recorded = json.loads((self.state / 'source-manifest.json').read_text('utf-8'))
        del recorded[renderer]
        (self.state / 'source-manifest.json').write_text(json.dumps(recorded), encoding='utf-8')
        with self.assertRaisesRegex(RuntimeError, 'no recorded hash'):
            subject.partial_refresh_guard(('cpp', 'go'), self.canonical, config, rows)
        subject.partial_refresh_guard(subject.ORDER, self.canonical, config, rows)

    def test_final_source_scan_keeps_sdk_validation_path(self):
        manifest = self.root / 'manifest.json'
        manifest.write_text(json.dumps({'language': 'cpp', 'schema': 1, 'files': {'source.cpp': 'before'}}), encoding='utf-8')
        scan = self.mock(subject.rebuild, 'current_sources', side_effect=RuntimeError('SDK mismatch'))
        with self.assertRaisesRegex(RuntimeError, 'SDK mismatch'):
            subject.verify_sources({'cpp': {'manifestPaths': {'scene': str(manifest)}}})
        scan.assert_called_once_with('cpp', 'scene')

    def test_legacy_config_error_points_to_unified_entry(self):
        self.mock(subject.rebuild, 'STATE', new=self.state)
        (self.state / 'source-manifest.json').write_text('{}', encoding='utf-8')
        self.mock(subject.rebuild, 'digest', return_value='changed')
        with self.assertRaisesRegex(RuntimeError, 'rebuild_all_docker_debug.py'):
            subject.rebuild.check_config_inputs(['scene'])


if __name__ == '__main__':
    unittest.main()
