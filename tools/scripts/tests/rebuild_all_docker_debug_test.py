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
                         'refreshInputs': {name: 'recipe' for lang in ('cpp', 'go') for name in subject.recipe_paths(lang)},
                         'historical': [], 'projectVolumes': [], 'pendingResume': [], 'sqlBootImageId': 'existing-sql-boot'}
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

    def test_check_reports_pending_services_without_recovering(self):
        self.pipeline_fixture()
        self.baseline['pendingResume'] = ['mysql', 'kafka']
        recover = self.mock(subject, 'resume_infrastructure')
        with redirect_stdout(io.StringIO()) as console:
            subject.pipeline(subject.ORDER, self.root, {}, self.result, check_only=True)
        report = json.loads((self.root / 'preflight.json').read_text('utf-8'))
        self.assertFalse(report['infraReady'])
        self.assertEqual(report['pendingResume'], ['mysql', 'kafka'])
        self.assertIn('infraReady=false pendingResume=["mysql", "kafka"]', console.getvalue())
        recover.assert_not_called()
        self.update.assert_not_called()
        self.run_stage.assert_not_called()
        self.fixture_create.assert_not_called()

    def test_static_sdk_failure_prevents_recovery(self):
        self.pipeline_fixture()
        self.preflight.side_effect = RuntimeError('SDK mismatch')
        recover = self.mock(subject, 'resume_infrastructure')
        with self.assertRaisesRegex(RuntimeError, 'SDK mismatch'):
            self.run_pipeline()
        self.fixture_preflight.assert_called_once()
        recover.assert_not_called()
        self.run_stage.assert_not_called()

    def test_resume_still_preflights_creates_new_java_and_applies_only_after_success(self):
        self.pipeline_fixture()
        old = self.root / 'old'
        prefix = {lang: {**self.validate.side_effect(lang, old / lang), 'reused_from': str(old)}
                  for lang in ('cpp', 'go')}
        resume = self.mock(subject, 'resumed_builds', return_value=prefix)
        subject.pipeline(subject.ORDER, self.root, {}, self.result, resume_from=old)
        resume.assert_called_once_with(old, subject.ORDER, self.root, self.baseline)
        self.preflight.assert_called_once()
        self.fixture_preflight.assert_called_once()
        self.fixture_create.assert_called_once()
        self.fixture_close.assert_called_once()
        cmds = [call.args[0] for call in self.run_stage.call_args_list]
        self.assertEqual(len(cmds), 3)
        self.assertTrue(cmds[0][4].endswith('build_docker_java_debug.py'))
        self.assertEqual([s['language'] for s in self.result['stages']], list(subject.ORDER))
        self.assertEqual(self.result['reused_from'], {'cpp': str(old), 'go': str(old)})
        self.update.assert_called_once()

    def test_resume_check_verifies_candidates_without_recovery_or_fixture(self):
        self.pipeline_fixture()
        resume = self.mock(subject, 'resumed_builds', return_value={'cpp': {'reused_from': 'old'}})
        recover = self.mock(subject, 'resume_infrastructure')
        subject.pipeline(subject.ORDER, self.root, {}, self.result, True, 'old')
        resume.assert_called_once()
        self.fixture_create.assert_not_called()
        recover.assert_not_called()
        self.run_stage.assert_not_called()
        self.assertEqual(self.result['stages'], [])

    def test_resume_rejection_precedes_infra_or_fixture_changes(self):
        self.pipeline_fixture()
        self.mock(subject, 'resumed_builds', side_effect=RuntimeError('source drift'))
        recover = self.mock(subject, 'resume_infrastructure')
        with self.assertRaisesRegex(RuntimeError, 'source drift'):
            subject.pipeline(subject.ORDER, self.root, {}, self.result, resume_from='old')
        recover.assert_not_called()
        self.fixture_create.assert_not_called()
        self.run_stage.assert_not_called()
        self.update.assert_not_called()

    def test_success_builds_serially_then_refreshes_and_recreates_only_targets(self):
        self.pipeline_fixture()
        self.run_pipeline()
        calls = self.run_stage.call_args_list
        self.assertEqual(len(calls), 5)
        for language, call in zip(subject.ORDER, calls[:3]):
            command = call.args[0]
            self.assertTrue(command[4].endswith('build_docker_' + language + '_debug.py'))
            if language == 'go':
                services = command[command.index('--services') + 1].split(',')
                self.assertIn('db-migrate', services)
                self.assertIn('sql-boot', services)
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
        self.assertEqual(self.update.call_args.args[0]['db-migrate']['image'], 'local/db-migrate:new')
        self.assertEqual(self.verify.call_count, 3)
        self.assertEqual(self.runtime.call_count, 2)
        self.assertTrue(self.result['success'])
        self.fixture_close.assert_called_once()
        self.assertTrue(self.fixture_close.call_args.args[0]['javaBuildSucceeded'])
        self.assertEqual(self.result['javaFixture'], {'mode': 'owned', 'status': 'closed'})

    def test_go_build_requires_migration_image_and_manifest(self):
        result = {'success': True, 'images': dict.fromkeys(subject.rebuild.GO, {}),
                  'manifestPaths': dict.fromkeys(subject.rebuild.GO, 'manifest.json')}
        for field in ('images', 'manifestPaths'):
            with self.subTest(field=field):
                incomplete = copy.deepcopy(result)
                del incomplete[field]['db-migrate']
                (self.root / 'result.json').write_text(json.dumps(incomplete), encoding='utf-8')
                with self.assertRaisesRegex(RuntimeError, 'Incomplete build result: go'):
                    subject.validate_build('go', self.root)

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


class ResumeBuildTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.repo = Path(tmp.name).resolve()
        self.old = self.repo / 'build/verification/docker-rebuild-all-20261112T040700.029055Z'
        self.new = self.old.parent / 'docker-rebuild-all-20261112T050000.000000Z'
        self.state = self.repo / 'state'
        self.old.mkdir(parents=True)
        self.state.mkdir()
        stack = ExitStack()
        self.addCleanup(stack.close)
        stack.enter_context(patch.object(subject, 'ROOT', self.repo))
        stack.enter_context(patch.object(subject, 'STATE', self.state))
        stack.enter_context(patch.object(subject.subprocess, 'Popen', side_effect=AssertionError('external process')))
        self.run = stack.enter_context(patch.object(subject.startup, 'run', side_effect=self.inspect))
        self.files = {'src/node.cpp': '1' * 64}
        self.scan = stack.enter_context(patch.object(subject.rebuild, 'current_sources', return_value=self.files))
        self.sdk = 'sha256:' + '2' * 64
        self.identity = 'sha256:' + '3' * 64
        self.base_id = 'sha256:' + '4' * 64
        self.baseline = {'bases': {name: 'local/base:v1' for name in subject.SERVICES['cpp']},
                         'baseIds': {'local/base:v1': self.base_id}}
        self.stamp = '20261112T040701.000000Z'
        sha = subject.startup.sha(json.dumps(self.files, sort_keys=True).encode())
        self.commit = 'a' * 12 + '-dirty.source-' + sha[:12]
        self.put(self.state / 'cpp-toolchain.json', {'imageId': self.sdk})
        self.manifest = self.old / 'cpp/source_meta/source-manifest.json'
        self.body = {'schema': 1, 'language': 'cpp', 'sourceRoot': '/src', 'files': self.files, 'build':
                     {'sourceSha256': sha, 'type': 'Debug', 'commit': self.commit,
                      'createdUtc': self.stamp, 'sdkImageId': self.sdk}}
        self.put(self.manifest, self.body)
        image = {'image': 'local/xuanming-node:debug-' + self.stamp.lower(), 'imageId': self.identity}
        self.built = {'success': True, 'images': dict.fromkeys(subject.SERVICES['cpp'], image),
                      'manifestPaths': dict.fromkeys(subject.SERVICES['cpp'], str(self.manifest))}
        self.put(self.old / 'cpp/result.json', self.built)
        (self.old / 'cpp/image.iid').write_text(self.identity)
        (self.old / 'cpp/build.log').write_text(
            'local/xuanming-independent-sdk:pin-' + self.sdk[7:] + '@' + self.sdk + '\n'
            + 'local/xuanming-cpp-debug-system:pin-' + self.base_id[7:] + '@' + self.base_id + '\n'
            + '{"files": 1, "artifacts": 6, "buildType": "Debug", "complete": true}\n')
        (self.old / 'cpp.log').write_text('jobs=1\n')
        self.put(self.old / 'cpp-base-images.json', self.baseline['bases'])
        self.result = {'applied': False, 'configurationWriteStarted': False, 'containerUpdateStarted': False,
                       'stages': [{'language': 'cpp', 'exitCode': 0, 'log': str(self.old / 'cpp.log'),
                                   'output': str(self.old / 'cpp')}, {'language': 'go', 'exitCode': 1}]}
        self.put(self.old / 'result.json', self.result)
        self.put(self.old / 'preflight.json', {'passed': True, 'languages': list(subject.ORDER),
                                               'baseIds': self.baseline['baseIds']})

    @staticmethod
    def put(path, body):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(body), encoding='utf-8')

    def inspect(self, args):
        if args[0] == 'git':
            return '5' * 40
        self.assertEqual(args[:len(subject.startup.DOCKER) + 2], subject.startup.DOCKER + ['image', 'inspect'])
        if args[-2] == '{{.Id}}':
            return self.identity
        self.assertEqual(args[-2], '{{json .Config.Env}}')
        return json.dumps(['MMORPG_BUILD_COMMIT=' + self.commit, 'MMORPG_BUILD_VERSION=debug-' + self.stamp])

    def resume(self):
        return subject.resumed_builds(self.old, subject.ORDER, self.new, self.baseline)

    def test_reuses_successful_cpp_only_and_continues_at_failed_go(self):
        builds = self.resume()
        self.assertEqual(list(builds), ['cpp'])
        self.assertEqual(builds['cpp']['reused_from'], str(self.old))
        self.assertEqual(builds['cpp']['manifestDigests'][str(self.manifest)], subject.rebuild.digest(self.manifest))
        self.scan.assert_called_once()

    def test_rejects_applied_unknown_or_partially_applied_receipts(self):
        for key in ('applied', 'configurationWriteStarted', 'containerUpdateStarted'):
            for value in (True, None):
                with self.subTest(key=key, value=value):
                    self.put(self.old / 'result.json', {**self.result, key: value})
                    with self.assertRaisesRegex(RuntimeError, 'applying'):
                        self.resume()

    def test_requires_local_run_and_direct_artifact_paths(self):
        with self.assertRaisesRegex(RuntimeError, 'previous unified'):
            subject.resumed_builds(self.repo, subject.ORDER, self.new, self.baseline)
        stage = {**self.result['stages'][0], 'output': str(self.repo / 'cpp')}
        self.put(self.old / 'result.json', {**self.result, 'stages': [stage]})
        with self.assertRaisesRegex(RuntimeError, 'origin'):
            self.resume()
        self.put(self.old / 'result.json', self.result)
        self.built['manifestPaths']['scene'] = str(self.repo / 'source-manifest.json')
        self.put(self.old / 'cpp/result.json', self.built)
        with self.assertRaisesRegex(RuntimeError, 'manifest path'):
            self.resume()

    def test_failed_builder_receipt_or_out_of_order_stages_rejected(self):
        self.put(self.old / 'cpp/result.json', {**self.built, 'success': False})
        with self.assertRaisesRegex(RuntimeError, 'Incomplete'):
            self.resume()
        self.put(self.old / 'result.json', {**self.result, 'stages': self.result['stages'][::-1]})
        with self.assertRaisesRegex(RuntimeError, 'stage order'):
            self.resume()

    def test_rejects_changed_sdk_recipe_base_or_incomplete_debug_contract(self):
        self.put(self.state / 'cpp-toolchain.json', {'imageId': 'other'})
        with self.assertRaisesRegex(RuntimeError, 'SDK'):
            self.resume()
        self.put(self.state / 'cpp-toolchain.json', {'imageId': self.sdk})
        self.run.side_effect = lambda args: 'other' if 'hash-object' in args else self.inspect(args)
        with self.assertRaisesRegex(RuntimeError, 'recipe'):
            self.resume()
        self.run.side_effect = self.inspect
        self.baseline['baseIds']['local/base:v1'] = 'wrong'
        with self.assertRaisesRegex(RuntimeError, 'base'):
            self.resume()
        self.baseline['baseIds']['local/base:v1'] = self.base_id
        (self.old / 'cpp/build.log').write_text('')
        with self.assertRaisesRegex(RuntimeError, 'completion'):
            self.resume()

    def test_source_manifest_and_image_are_rechecked_after_initial_acceptance(self):
        builds = self.resume()
        for changed in ({}, {**self.files, 'added.cpp': '9'}, {'src/node.cpp': 'changed'}):
            self.scan.return_value = changed
            with self.assertRaisesRegex(RuntimeError, 'Source files changed'):
                subject.verify_sources(builds)
        self.scan.return_value = self.files
        self.run.side_effect = lambda args: 'changed' if '{{.Id}}' in args else self.inspect(args)
        with self.assertRaisesRegex(RuntimeError, 'image ID'):
            subject.verify_sources(builds)
        self.run.side_effect = self.inspect
        self.put(self.manifest, {**self.body, 'build': {}})
        with self.assertRaisesRegex(RuntimeError, 'manifest changed'):
            subject.verify_sources(builds)

    def test_recorded_recipe_hashes_allow_matching_uncommitted_build_recipe(self):
        recorded = subject.read_receipt(self.old, 'preflight.json')
        hashes = {}
        for name in subject.recipe_paths('cpp'):
            path = self.repo / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(b'local build recipe')
            hashes[name] = subject.rebuild.digest(path)
        self.put(self.old / 'preflight.json', {**recorded, 'buildRecipeHashes': {'cpp': hashes}})
        self.resume()
        self.assertFalse(any(c.args[0][0] == 'git' for c in self.run.call_args_list))
        path.write_bytes(b'changed')
        with self.assertRaisesRegex(RuntimeError, 'recipe changed'):
            self.resume()

    def test_go_service_manifest_requires_exact_flags_resources_receipt_and_base(self):
        import build_docker_go_debug as go
        directory = self.old / 'go/login'
        base = 'local/base:v1'
        self.baseline['bases']['login'] = base
        source = self.repo / 'generated/tables/One.json'
        recipe = self.repo / 'deploy/debug/Dockerfile.go'
        recipe.parent.mkdir(parents=True)
        recipe.write_bytes(b'go recipe')
        recipe_hash = subject.rebuild.digest(recipe)
        revision = 'a' * 40
        self.put(self.old / 'go/preflight.json', {'jobs': 1, 'services': ['login'], 'sourceRoot': str(self.repo),
                 'goVersion': 'go version ' + go.GO_VERSION + ' windows/amd64', 'dockerfileSHA256': recipe_hash, 'commit': revision})
        self.put(self.old / 'go/delve-receipt.json', {'version': go.DELVE_VERSION, 'goVersion': go.GO_VERSION,
                 'goVersionCheckDisabled': False, 'sha256': 'd' * 64})
        files = {'generated/tables/One.json': 'b' * 64}
        digest = subject.startup.hashed(files)
        build = {'service': 'login', 'module': 'login', 'entry': 'login.go', 'goVersion': go.GO_VERSION,
                 'gcflags': 'all=-N -l', 'trimpath': False, 'stripSymbols': False, 'goos': 'linux',
                 'goarch': 'amd64', 'cgoEnabled': False, 'delveVersion': go.DELVE_VERSION, 'goVersionCheckDisabled': False,
                 'runtimeAssetsFrozen': True, 'tablesLowercase': True, 'dockerfileSHA256': recipe_hash,
                 'sourceDigest': digest, 'commit': revision[:12] + '-dirty+' + digest[:12], 'version': 'debug-v1'}
        dest = '/generated/tables/one.json'
        manifest = {'sourceRoot': str(self.repo), 'build': build, 'files': files, 'runtimeResources': {dest: 'generated/tables/One.json'},
                    'artifacts': {dest: 'b' * 64, '/app/service': 'f' * 64, '/usr/local/bin/dlv': 'd' * 64}}
        path = directory / 'source_meta/source-manifest.json'
        self.put(path, manifest)
        image = {'image': 'local/mmorpg-login:debug-v1', 'imageId': self.identity}
        receipt = {**image, 'manifestSHA256': subject.rebuild.digest(path), 'runtimeResourceFiles': 1,
                   'baseImage': base, 'baseImageId': self.base_id}
        built = {'manifestPaths': {'login': str(path)}, 'images': {'login': image}}
        self.put(directory / 'image-receipt.json', receipt)
        with patch.dict(subject.SERVICES, {'go': ('login',)}), patch.object(go, 'runtime_resource_plan', return_value={dest: source}):
            subject.go_resume_contract(self.old, built, self.baseline)
            for change in ({'build': {**build, 'gcflags': '-O2'}}, {'runtimeResources': {}},
                           {'artifacts': {**manifest['artifacts'], dest: 'x'}}):
                self.put(path, {**manifest, **change})
                with self.subTest(change=next(iter(change))), self.assertRaises(RuntimeError):
                    subject.go_resume_contract(self.old, built, self.baseline)
            self.put(path, manifest)
            for change in ({'baseImageId': 'wrong'}, {'manifestSHA256': 'wrong'}, {'imageId': 'wrong'}):
                self.put(directory / 'image-receipt.json', {**receipt, **change})
                with self.subTest(change=next(iter(change))), self.assertRaisesRegex(RuntimeError, 'image receipt'):
                    subject.go_resume_contract(self.old, built, self.baseline)


class InfraRecoveryTests(unittest.TestCase):
    def setUp(self):
        stack = self.enterContext(ExitStack())
        self.root = Path(stack.enter_context(tempfile.TemporaryDirectory()))
        self.env_file = self.root / '.env'
        self.env_file.write_bytes(b'original')
        self.live, self.config, self.volumes, self.base_ids = {}, {'services': {}, 'volumes': {}}, {}, {}
        self.starts = []
        self.after_start = lambda name: None
        self.result = {}
        image = {'Cmd': [], 'Entrypoint': ['server'], 'User': '', 'Volumes': {}}
        self.image = image
        for index, name in enumerate((*subject.startup.INFRA, 'gateway', 'db-migrate'), 1):
            identity = format(index, '064x')
            image_id = 'sha256:' + format(index + 10, '064x')
            ref, volume_name = name + ':local', 'project-' + name
            service = {'image': ref, 'volumes': [{'source': name, 'type': 'volume', 'target': '/data'}],
                       'environment': {'MODE': 'original'}, 'healthcheck': {'test': ['CMD', 'ping']}}
            self.config['services'][name] = service
            self.config['volumes'][name] = {'name': volume_name}
            self.base_ids[ref] = image_id
            self.volumes[volume_name] = {'Name': volume_name, 'CreatedAt': 'original', 'Driver': 'local',
                                         'Labels': {'com.docker.compose.project': subject.startup.PROJECT}}
            self.live[name] = {'Id': identity, 'Image': image_id, 'RestartCount': 0,
                               'State': {'Status': 'exited', 'Running': False, 'StartedAt': 'original',
                                         'FinishedAt': 'old', 'OOMKilled': False, 'ExitCode': 255},
                               'Config': {**image, 'Labels': {'com.docker.compose.service': name,
                                                              'com.docker.compose.oneoff': 'False'},
                                          'Env': ['MODE=original'], 'Healthcheck': {'Test': ['CMD', 'ping']}},
                               'HostConfig': {'RestartPolicy': {'Name': 'no'}},
                               'Mounts': [{'Type': 'volume', 'Destination': '/data', 'Name': volume_name,
                                           'Driver': 'local', 'RW': True}]}
        for owner, name, kwargs in (
            (subject, 'STATE', {'new': self.root}),
            (subject.startup, 'run', {'side_effect': self.docker}),
            (subject, 'current_containers', {'side_effect': lambda _: (copy.deepcopy(self.live), [])}),
            (subject, 'compose_config', {'return_value': self.config}),
            (subject, 'refresh_sources', {'return_value': {'main.py': 'sha'}}),
            (subject.startup, 'project_volumes', {'return_value': sorted(self.volumes)}),
            (subject.subprocess, 'Popen', {'side_effect': AssertionError('unexpected real process')}),
        ):
            stack.enter_context(patch.object(owner, name, **kwargs))

    def docker(self, command, **kwargs):
        self.assertEqual(command[:len(subject.startup.DOCKER)], subject.startup.DOCKER)
        args = command[len(subject.startup.DOCKER):]
        if args[:2] == ['volume', 'inspect']:
            return json.dumps([self.volumes[name] for name in args[2:]])
        if args[:3] == ['image', 'inspect', '--format']:
            return self.base_ids[args[4]] if args[3] == '{{.Id}}' else json.dumps(self.image)
        if args[0] == 'inspect':
            by_id = {row['Id']: row for row in self.live.values()}
            return json.dumps([by_id[identity] for identity in args[1:]])
        name, row = next((name, row) for name, row in self.live.items() if row['Id'] == args[1])
        self.assertEqual(args, ['start', row['Id']])
        self.assertIn(name, subject.startup.INFRA)
        self.starts.append(name)
        self.healthy(name)
        self.after_start(name)
        return row['Id']

    def healthy(self, name):
        self.live[name]['State'].update(Status='running', Running=True, ExitCode=0, StartedAt='new',
                                        Health={'Status': 'healthy'})

    def baseline(self):
        pending, volumes = subject.infrastructure_plan(self.config, self.live, self.base_ids)
        return {'pendingResume': pending, 'infraVolumes': volumes, 'services': copy.deepcopy(self.live),
                'signatures': {row['Id']: subject.startup.signature(row) for row in self.live.values()},
                'states': {row['Id']: subject.startup.state(row) for row in self.live.values()},
                'envSha256': subject.startup.sha(self.env_file.read_bytes()), 'refreshInputs': subject.refresh_sources(),
                'bindHashes': {}, 'projectVolumes': sorted(self.volumes), 'baseIds': self.base_ids.copy()}

    def resume(self, baseline):
        subject.resume_infrastructure(self.config, baseline, {}, self.root, self.result)

    def test_start_each_original_once_in_order_and_advance_only_their_states(self):
        baseline = self.baseline()
        original = copy.deepcopy(baseline)
        self.resume(baseline)
        self.assertEqual(self.starts, list(subject.startup.INFRA))
        self.assertEqual([event['status'] for event in self.result['infraRecovery']], ['healthy'] * 4)
        self.assertTrue(self.result['infraReady'])
        self.assertEqual(self.result['pendingResume'], [])
        self.assertEqual(baseline['signatures'], original['signatures'])
        for name in self.live:
            self.assertEqual(baseline['services'][name], self.live[name])
            self.assertEqual(baseline['states'][self.live[name]['Id']], subject.startup.state(self.live[name]))
        subject.unchanged_before_apply(self.config, baseline, {})

    def test_skip_running_healthy(self):
        for name in ('mysql', 'etcd'):
            self.healthy(name)
        self.resume(self.baseline())
        self.assertEqual(self.starts, ['redis', 'kafka'])
        self.resume(self.baseline())
        self.assertEqual(self.starts, ['redis', 'kafka'])

    def test_running_unhealthy_is_rejected_even_with_other_services_stopped(self):
        self.healthy('kafka')
        self.live['kafka']['State']['Health']['Status'] = 'unhealthy'
        with self.assertRaisesRegex(RuntimeError, 'not healthy: kafka'):
            self.baseline()
        self.assertEqual(self.starts, [])

    def test_reject_container_id_env_file_volume_or_peer_state_drift_before_start(self):
        baseline = self.baseline()
        for name, modify, restore in (
            ('ID', lambda: self.live['mysql'].update(Id='a' * 64), lambda: self.live['mysql'].update(Id=format(1, '064x'))),
            ('env', lambda: self.env_file.write_bytes(b'changed'), lambda: self.env_file.write_bytes(b'original')),
            ('volume', lambda: self.volumes['project-etcd'].update(CreatedAt='changed'),
             lambda: self.volumes['project-etcd'].update(CreatedAt='original')),
            ('peer', lambda: self.healthy('gateway'),
             lambda: self.live['gateway'].update(State=copy.deepcopy(baseline['services']['gateway']['State']))),
            ('external start', lambda: self.healthy('mysql'),
             lambda: self.live['mysql'].update(State=copy.deepcopy(baseline['services']['mysql']['State']))),
        ):
            with self.subTest(name=name):
                modify()
                with self.assertRaises(RuntimeError):
                    self.resume(baseline)
                self.assertEqual(self.starts, [])
                restore()

    def test_runtime_contract_refuses_wrong_mount_image_environment_or_port(self):
        for name in ('mount', 'image', 'env', 'port', 'command'):
            row = copy.deepcopy(self.live['mysql'])
            with self.subTest(name=name):
                if name == 'mount':
                    self.live['mysql']['Mounts'][0]['Name'] = 'project-kafka'
                elif name == 'image':
                    self.live['mysql']['Image'] = self.live['kafka']['Image']
                elif name == 'env':
                    self.live['mysql']['Config']['Env'] = ['MODE=changed']
                elif name == 'port':
                    self.live['mysql']['HostConfig']['PortBindings'] = {'1/tcp': [{'HostIp': '', 'HostPort': '2'}]}
                else:
                    self.live['mysql']['Config']['Cmd'] = ['unexpected']
                with self.assertRaises(RuntimeError):
                    self.baseline()
                self.assertEqual(self.starts, [])
            self.live['mysql'] = row

    def test_preflight_rechecks_health_after_slow_sdk_validation(self):
        import build_docker_cpp_debug as cpp
        for name in subject.startup.INFRA:
            self.healthy(name)
        self.config['name'] = subject.startup.PROJECT
        self.config['services']['sql-boot'] = self.config['services']['gateway']
        self.env_file.write_bytes(b'COMPOSE_PROJECT_NAME=xuanming-debug-current\n')
        with ExitStack() as stack:
            for owner, name, kwargs in (
                    (prepare, 'refresh_existing_options', {}), (subject.startup, 'APPS', {'new': ()}),
                    (subject.startup, 'ONESHOTS', {'new': set()}), (subject, 'SERVICES', {'new': {'cpp': ['gateway']}}),
                    (subject, 'partial_refresh_guard', {}),
                    (cpp, 'validate_sdk', {'side_effect': lambda: self.live['mysql']['State']['Health'].update(Status='unhealthy')})):
                stack.enter_context(patch.object(owner, name, **kwargs))
            with self.assertRaisesRegex(RuntimeError, 'Infrastructure changed after inventory'):
                subject.preflight(('cpp',), {})
        self.assertEqual(self.starts, [])

    def test_null_compose_process_uses_image_defaults_and_empty_user(self):
        self.image.update(Cmd=['server'], User=None)
        for name in subject.startup.INFRA:
            self.config['services'][name].update(command=None, entrypoint=None)
            self.live[name]['Config'].update(Cmd=['server'], User='')
        self.config['services']['redis']['command'] = []
        self.live['redis']['Config']['Cmd'] = []
        self.config['services']['kafka']['user'] = 'root:root'
        self.live['kafka']['Config']['User'] = 'root:root'
        self.assertEqual(self.baseline()['pendingResume'], list(subject.startup.INFRA))
        self.live['mysql']['Config']['User'] = 'different'
        with self.assertRaisesRegex(RuntimeError, 'process configuration differs: mysql'):
            self.baseline()

    def test_external_start_during_guard_is_detected_before_start_intent(self):
        baseline = self.baseline()
        with patch.object(subject, 'unchanged_before_apply', side_effect=lambda *_: self.healthy('mysql')):
            with self.assertRaisesRegex(RuntimeError, 'Infrastructure changed after inventory'):
                self.resume(baseline)
        self.assertFalse((self.root / 'infra-mysql-start.json').exists())
        self.assertEqual(self.starts, [])

    def test_peer_drift_during_mysql_recovery_stops_before_redis(self):
        baseline = self.baseline()
        self.after_start = lambda _: self.healthy('gateway')
        with self.assertRaisesRegex(RuntimeError, 'Runtime changed'):
            self.resume(baseline)
        self.assertEqual(self.starts, ['mysql'])
        self.assertTrue((self.root / 'infra-mysql-healthy.json').is_file())

    def test_second_command_failure_preserves_first_and_unconfirmed_start(self):
        def fail_second(name):
            if name == 'redis':
                raise RuntimeError('Command failed')
        self.after_start = fail_second
        baseline = self.baseline()
        with self.assertRaisesRegex(RuntimeError, 'Command failed'):
            self.resume(baseline)
        self.assertEqual(self.starts, ['mysql', 'redis'])
        self.assertEqual([e['status'] for e in self.result['infraRecovery']], ['healthy', 'start-outcome-unconfirmed'])
        self.assertTrue((self.root / 'infra-redis-start.json').is_file())
        self.assertFalse((self.root / 'infra-redis-healthy.json').exists())
        self.assertEqual(baseline['services']['redis']['State']['Status'], 'exited')

    def test_unconfirmed_health_deadline_never_restarts_or_continues(self):
        baseline = self.baseline()
        self.after_start = lambda name: self.live[name]['State'].update(Health={'Status': 'starting'})
        with patch.object(subject.time, 'monotonic', side_effect=[0, 0, 359, 360]), patch.object(subject.time, 'sleep') as sleep:
            with self.assertRaisesRegex(RuntimeError, 'readiness timed out: mysql'):
                self.resume(baseline)
        sleep.assert_called_once_with(1)
        self.assertEqual(self.starts, ['mysql'])
        self.assertEqual(self.result['infraRecovery'][0]['status'], 'readiness-unconfirmed')
        self.assertFalse((self.root / 'infra-mysql-healthy.json').exists())


if __name__ == '__main__':
    unittest.main()
