"""GM 一键启动的持久数据边界、幂等和失败退出回归。"""
from contextlib import nullcontext
from copy import deepcopy
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

SCRIPTS = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(SCRIPTS))
import gm_admin_startup as gm
import start_docker_debug as startup


class DependenciesTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        runtime = self.root / 'build/gm-admin-runtime'
        (runtime / 'sql').mkdir(parents=True)
        self.values = {name: 'test-' + name for name in (
            'GM_MYSQL_ROOT_PASSWORD', 'GM_DB_PASSWORD', 'GM_REDIS_PASSWORD')}
        (runtime / 'private.json').write_text(json.dumps(self.values), encoding='utf-8')
        (runtime / 'redis.conf').write_text('bind 0.0.0.0\nprotected-mode yes\nappendonly yes\nrequirepass '
                                           + self.values['GM_REDIS_PASSWORD'] + '\n', encoding='utf-8')
        (runtime / 'sql/001-jeecg.sql').write_text('SELECT 1;', encoding='utf-8')
        for relative in ('deploy/gm-admin/compose.yml', 'java/gm_admin/run_local.py'):
            path = self.root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('fixture', encoding='utf-8')
        self.rows = {}
        self.calls = []
        self.fail_health = False
        self.on_start = lambda name: None
        for name in gm.SERVICES:
            target, port = gm.PORTS[name]
            environment = {'REDISCLI_AUTH': self.values['GM_REDIS_PASSWORD']} if name == 'redis' else {
                'MYSQL_ROOT_PASSWORD': self.values['GM_MYSQL_ROOT_PASSWORD'], 'MYSQL_DATABASE': 'gm_admin',
                'MYSQL_USER': 'gm_admin', 'MYSQL_PASSWORD': self.values['GM_DB_PASSWORD']}
            self.rows[name] = {
                'Id': 'id-' + name, 'Image': 'image-' + name, 'Name': '/' + gm.PROJECT + '-' + name + '-1',
                'Config': {'Labels': {'com.docker.compose.project': gm.PROJECT, 'com.docker.compose.service': name,
                    'com.docker.compose.project.config_files': str(self.root / 'deploy/gm-admin/compose.yml')},
                    'Env': [k + '=' + v for k, v in environment.items()]},
                'HostConfig': {'PortBindings': {target: [{'HostIp': '127.0.0.1', 'HostPort': port}]}},
                'Mounts': [
                    {'Destination': '/data' if name == 'redis' else '/var/lib/mysql', 'Type': 'volume',
                     'Name': gm.PROJECT + '-' + name + '-data', 'RW': True},
                    {'Destination': '/usr/local/etc/redis/redis.conf' if name == 'redis' else '/docker-entrypoint-initdb.d',
                     'Type': 'bind', 'Source': str(runtime / ('redis.conf' if name == 'redis' else 'sql')), 'RW': False}],
                'State': {'Status': 'exited', 'ExitCode': 0, 'StartedAt': 'old-start', 'FinishedAt': 'old-stop',
                          'OOMKilled': False, 'Health': {'Status': 'starting'}}, 'RestartCount': 0}
        self.addCleanup(patch.stopall)
        patch.object(startup, 'run', side_effect=self.run_command).start()
        patch.object(startup, 'inspect', side_effect=self.inspect).start()

    def inspect(self, ids):
        result = []
        for value in ids:
            row = next((r for r in self.rows.values() if r['Id'] == value), None)
            if row is None:
                row = {'Id': 'image-' + next(n for n, image in gm.IMAGES.items() if image == value)}
            result.append(deepcopy(row))
        return result

    def run_command(self, command, **kwargs):
        args = command[len(startup.DOCKER):]
        self.calls.append(args)
        if args[:2] == ['ps', '-aq']:
            return '\n'.join(r['Id'] for r in self.rows.values())
        if args[:2] == ['volume', 'inspect']:
            return json.dumps([{'Name': args[2], 'Driver': 'local', 'Scope': 'local', 'CreatedAt': 'original',
                'Labels': {'com.docker.compose.project': gm.PROJECT}, 'Options': None, 'Mountpoint': '/original'}])
        if args[0] == 'start':
            row = next(r for r in self.rows.values() if r['Id'] == args[1])
            row['State'].update(Status='running', StartedAt='new-start', FinishedAt='')
            row['State']['Health']['Status'] = 'unhealthy' if self.fail_health else 'healthy'
            self.on_start(row['Config']['Labels']['com.docker.compose.service'])
            return row['Id']
        raise AssertionError('Unexpected Docker mutation or command: ' + str(args))

    def test_stopped_dependencies_start_original_ids_once_and_keep_volumes(self):
        result = gm.ensure_infra(self.root, gm.infra_preflight(self.root))
        self.assertTrue(result['existingIdsImagesVolumesUnchanged'])
        self.assertEqual([c for c in self.calls if c[0] == 'start'], [['start', 'id-mysql'], ['start', 'id-redis']])
        self.assertEqual(result['actions'], {'mysql': 'started', 'redis': 'started'})
        self.calls.clear()
        result = gm.ensure_infra(self.root, gm.infra_preflight(self.root))
        self.assertEqual(result['actions'], {'mysql': 'already-running', 'redis': 'already-running'})
        self.assertFalse(any(c[0] == 'start' for c in self.calls))

    def test_missing_database_does_not_create_a_replacement(self):
        del self.rows['mysql']
        with self.assertRaisesRegex(RuntimeError, 'missing or duplicated'):
            gm.infra_preflight(self.root)
        self.assertFalse(any(c[0] == 'start' for c in self.calls))

    def test_changed_credentials_rejected_before_start(self):
        self.rows['mysql']['Config']['Env'][0] = 'MYSQL_ROOT_PASSWORD=changed'
        with self.assertRaisesRegex(RuntimeError, 'credentials'):
            gm.infra_preflight(self.root)

    def test_public_port_rejected(self):
        self.rows['redis']['HostConfig']['PortBindings']['6379/tcp'][0]['HostIp'] = '0.0.0.0'
        with self.assertRaisesRegex(RuntimeError, 'loopback'):
            gm.infra_preflight(self.root)

    def test_replaced_image_rejected(self):
        self.rows['mysql']['Image'] = 'replacement'
        with self.assertRaisesRegex(RuntimeError, 'image differs'):
            gm.infra_preflight(self.root)

    def test_writable_configuration_bind_rejected(self):
        self.rows['redis']['Mounts'][1]['RW'] = True
        with self.assertRaisesRegex(RuntimeError, 'mount changed'):
            gm.infra_preflight(self.root)

    def test_drift_after_preflight_prevents_any_start(self):
        plan = gm.infra_preflight(self.root)
        (self.root / 'build/gm-admin-runtime/sql/001-jeecg.sql').write_text('changed', encoding='utf-8')
        with self.assertRaisesRegex(RuntimeError, 'changed after preflight'):
            gm.ensure_infra(self.root, plan)
        self.assertFalse(any(c[0] == 'start' for c in self.calls))

    def test_failed_health_has_no_retry_and_does_not_start_next_service(self):
        plan = gm.infra_preflight(self.root)
        self.fail_health = True
        with self.assertRaisesRegex(RuntimeError, 'startup failed'):
            gm.ensure_infra(self.root, plan)
        self.assertEqual([c for c in self.calls if c[0] == 'start'], [['start', 'id-mysql']])

    def test_concurrent_start_of_next_dependency_is_rejected(self):
        plan = gm.infra_preflight(self.root)
        def start_other(name):
            if name == 'mysql':
                self.rows['redis']['State'].update(Status='running', StartedAt='external-start', FinishedAt='')
                self.rows['redis']['State']['Health']['Status'] = 'healthy'
        self.on_start = start_other
        with self.assertRaisesRegex(RuntimeError, 'state changed before its startup step'):
            gm.ensure_infra(self.root, plan)
        self.assertEqual([c for c in self.calls if c[0] == 'start'], [['start', 'id-mysql']])

    def test_restart_of_confirmed_dependency_is_rejected(self):
        self.rows['mysql']['State'].update(Status='running', FinishedAt='')
        self.rows['mysql']['State']['Health']['Status'] = 'healthy'
        plan = gm.infra_preflight(self.root)
        def restart_other(name):
            if name == 'redis':
                self.rows['mysql']['State']['StartedAt'] = 'external-restart'
        self.on_start = restart_other
        with self.assertRaisesRegex(RuntimeError, 'Confirmed GM dependency state changed'):
            gm.ensure_infra(self.root, plan)

    def test_restart_while_waiting_for_dependency_health_is_rejected(self):
        plan = gm.infra_preflight(self.root)
        self.on_start = lambda name: self.rows[name]['State']['Health'].update(Status='starting')
        reads = []
        def restarting_inspect(ids):
            if ids == ['id-mysql'] and self.rows['mysql']['State']['Status'] == 'running':
                reads.append(True)
                if len(reads) == 2:
                    self.rows['mysql']['State']['StartedAt'] = 'external-restart'
                    self.rows['mysql']['State']['Health']['Status'] = 'healthy'
            return self.inspect(ids)
        with patch.object(startup, 'inspect', side_effect=restarting_inspect), patch.object(gm.time, 'sleep'):
            with self.assertRaisesRegex(RuntimeError, 'restarted during readiness checks'):
                gm.ensure_infra(self.root, plan)
        self.assertEqual([c for c in self.calls if c[0] == 'start'], [['start', 'id-mysql']])


class EntrypointTest(unittest.TestCase):
    def run_main(self, execute, gm_error=None):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        reports = {}
        baseline = {'plan': [], 'contract': {'id': 'same'}, 'states': {'scene': 'same'}}
        with patch.object(startup, 'ROOT', Path(temp.name)), patch.object(startup, 'preflight', return_value=baseline), \
             patch.object(startup, 'startup_lock', return_value=nullcontext()), \
             patch.object(startup, 'save', side_effect=lambda name, value: reports.update({name: deepcopy(value)})), \
             patch.object(startup, 'event'), patch.object(gm, 'preflight', return_value={}), \
             patch.object(gm, 'ensure_started', side_effect=gm_error, return_value={'passed': True}) as gm_start, \
             patch.object(startup, 'execute', side_effect=lambda saved, result: result.update(passed=True, phase='complete')) as game_start, \
             patch.object(sys, 'argv', ['start_docker_debug.py'] + (['--execute'] if execute else [])):
            code = startup.main()
        return code, reports, game_start, gm_start

    def test_readonly_preflight_starts_neither_game_nor_gm(self):
        code, reports, game, web = self.run_main(False)
        self.assertEqual(code, 0)
        game.assert_not_called()
        web.assert_not_called()
        self.assertIn('gm-preflight.json', reports)

    def test_gm_failure_cannot_leave_game_success_as_overall_success(self):
        code, reports, game, web = self.run_main(True, RuntimeError('GM backend readiness failed'))
        self.assertEqual(code, 1)
        game.assert_called_once()
        web.assert_called_once()
        self.assertFalse(reports['result.json']['passed'])
        self.assertEqual(reports['result.json']['phase'], 'gm-startup')

    def test_complete_entrypoint_reports_gm_and_unchanged_game(self):
        code, reports, game, web = self.run_main(True)
        self.assertEqual(code, 0)
        self.assertTrue(reports['result.json']['passed'])
        self.assertTrue(reports['result.json']['gmGameRuntimeUnchanged'])
        game.assert_called_once()
        web.assert_called_once()


class MailRecoveryTest(unittest.TestCase):
    def setUp(self):
        self.calls = []
        self.rows = {
            'mail': {
                'Id': 'id-mail', 'Image': 'img-mail', 'Name': '/xuanming-gm-mail-mail-1',
                'Config': {'Labels': {'com.docker.compose.project': gm.MAIL_PROJECT, 'com.docker.compose.service': 'mail'}},
                'HostConfig': {'PortBindings': deepcopy(gm.MAIL_PORTS)}, 'Mounts': [], 'RestartCount': 0,
                'State': {'Status': 'exited', 'ExitCode': 255, 'StartedAt': 'old-mail', 'FinishedAt': 'old-stop',
                          'OOMKilled': False, 'Health': {'Status': 'unhealthy'}},
            },
            'mail-register': {
                'Id': 'id-reg', 'Image': 'img-boot', 'Name': '/xuanming-gm-mail-mail-register-1',
                'Config': {'Labels': {'com.docker.compose.project': gm.MAIL_PROJECT, 'com.docker.compose.service': 'mail-register'}},
                'HostConfig': {'PortBindings': {}}, 'Mounts': [], 'RestartCount': 0,
                'State': {'Status': 'exited', 'ExitCode': 0, 'StartedAt': 'old-reg', 'FinishedAt': 'old-reg-end',
                          'OOMKilled': False},
            },
        }
        self.addCleanup(patch.stopall)
        patch.object(startup, 'run', side_effect=self.run_command).start()
        patch.object(startup, 'inspect', side_effect=self.inspect).start()
        patch.object(gm.time, 'sleep').start()

    def inspect(self, ids):
        return [deepcopy(next(r for r in self.rows.values() if r['Id'] == value)) for value in ids]

    def run_command(self, command, **kwargs):
        args = command[len(startup.DOCKER):]
        self.calls.append(args)
        if args[:2] == ['ps', '-aq']:
            return '\n'.join(r['Id'] for r in self.rows.values())
        if args[0] == 'start':
            row = next(r for r in self.rows.values() if r['Id'] == args[1])
            if row['Id'] == 'id-reg':
                row['State'].update(Status='exited', ExitCode=0, StartedAt='new-reg', FinishedAt='new-reg-end')
            else:
                row['State'].update(Status='running', ExitCode=0, StartedAt='new-mail', FinishedAt='')
                row['State']['Health']['Status'] = 'healthy'
            return row['Id']
        raise AssertionError('Unexpected Docker command: ' + str(args))

    def test_stopped_mail_registers_writer_before_starting_existing_container(self):
        plan = gm.mail_preflight(Path('.'))
        self.assertEqual(plan['action'], 'start-existing')
        result = gm.ensure_mail(Path('.'), plan)
        self.assertEqual(result['action'], 'started')
        self.assertEqual([c for c in self.calls if c[0] == 'start'], [['start', 'id-reg'], ['start', 'id-mail']])
        self.calls.clear()
        again = gm.ensure_mail(Path('.'), gm.mail_preflight(Path('.')))
        self.assertEqual(again['action'], 'already-running')
        self.assertFalse(any(c[0] == 'start' for c in self.calls))

    def test_public_mail_port_is_rejected(self):
        self.rows['mail']['HostConfig']['PortBindings']['50900/tcp'][0]['HostIp'] = '0.0.0.0'
        with self.assertRaisesRegex(RuntimeError, 'loopback port'):
            gm.mail_preflight(Path('.'))


if __name__ == '__main__':
    unittest.main()
