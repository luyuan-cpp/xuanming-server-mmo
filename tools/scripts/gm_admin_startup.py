"""日常启动只恢复已准备的 GM 依赖与网页，不部署游戏或重新初始化管理库。"""
from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import re
import sys
import time

import start_docker_debug as startup
import gm_rpc_startup


PROJECT = 'xuanming-gm-admin'
SERVICES = ('mysql', 'redis')
PORTS = {'mysql': ('3306/tcp', '13391'), 'redis': ('6379/tcp', '16391')}
IMAGES = {'mysql': 'container-registry.oracle.com/mysql/community-server:8.4', 'redis': 'redis:7.2'}


def web_module(root):
    path = root / 'java/gm_admin/run_local.py'
    spec = importlib.util.spec_from_file_location('gm_local_web_runtime', path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def host_source(value):
    match = re.fullmatch(r'/run/desktop/mnt/host/([A-Za-z])/(.+)', value)
    if match:
        value = match.group(1).upper() + ':/' + match.group(2)
    return Path(value).resolve()


def input_hashes(root):
    runtime = root / 'build/gm-admin-runtime'
    paths = [root / 'deploy/gm-admin/compose.yml', runtime / 'private.json', runtime / 'redis.conf',
             root / 'java/gm_admin/run_local.py']
    sql = runtime / 'sql'
    startup.require(sql.is_dir() and (sql / '001-jeecg.sql').is_file(),
                    'GM prepared SQL is missing; prepare the existing GM installation first')
    paths.extend(sorted(p for p in sql.rglob('*') if p.is_file()))
    startup.require(all(p.is_file() and not p.is_symlink() for p in paths), 'GM startup input is missing or redirected')
    return {str(p.relative_to(root)): startup.sha(p.read_bytes()) for p in paths}


def infra_preflight(root):
    runtime = root / 'build/gm-admin-runtime'
    inputs = input_hashes(root)
    values = json.loads((runtime / 'private.json').read_text('utf-8'))
    expected_env = {
        'mysql': {'MYSQL_ROOT_PASSWORD': values['GM_MYSQL_ROOT_PASSWORD'], 'MYSQL_DATABASE': 'gm_admin',
                  'MYSQL_USER': 'gm_admin', 'MYSQL_PASSWORD': values['GM_DB_PASSWORD']},
        'redis': {'REDISCLI_AUTH': values['GM_REDIS_PASSWORD']},
    }
    expected_redis = 'bind 0.0.0.0\nprotected-mode yes\nappendonly yes\nrequirepass ' + values['GM_REDIS_PASSWORD'] + '\n'
    startup.require((runtime / 'redis.conf').read_text('utf-8') == expected_redis,
                    'GM Redis configuration differs from the existing private credentials')
    ids = startup.run(startup.DOCKER + ['ps', '-aq', '--filter', 'label=com.docker.compose.project=' + PROJECT]).split()
    startup.require(len(ids) == 2, 'GM MySQL/Redis containers are missing or duplicated; restore the existing installation first')
    rows = startup.inspect(ids)
    result = {}
    volumes = {}
    for row in rows:
        labels = row['Config'].get('Labels', {})
        name = labels.get('com.docker.compose.service')
        startup.require(name in SERVICES and name not in result and labels.get('com.docker.compose.project') == PROJECT
                        and row['Name'] == '/' + PROJECT + '-' + name + '-1'
                        and labels.get('com.docker.compose.project.config_files') == str(root / 'deploy/gm-admin/compose.yml'),
                        'GM dependency ownership changed')
        actual_env = dict(item.split('=', 1) for item in row['Config'].get('Env', []))
        startup.require(all(actual_env.get(k) == v for k, v in expected_env[name].items()),
                        'GM dependency credentials or database configuration changed: ' + name)
        image = startup.inspect([IMAGES[name]])[0]
        startup.require(row['Image'] == image['Id'], 'GM dependency image differs from its prepared local image: ' + name)
        target, port = PORTS[name]
        startup.require(row['HostConfig']['PortBindings'] == {target: [{'HostIp': '127.0.0.1', 'HostPort': port}]},
                        'GM dependency loopback port configuration changed: ' + name)
        volume_name = PROJECT + '-' + name + '-data'
        volume_target = '/var/lib/mysql' if name == 'mysql' else '/data'
        bind_target = '/docker-entrypoint-initdb.d' if name == 'mysql' else '/usr/local/etc/redis/redis.conf'
        bind_source = runtime / ('sql' if name == 'mysql' else 'redis.conf')
        mounts = {m['Destination']: m for m in row['Mounts']}
        startup.require(len(row['Mounts']) == 2 and set(mounts) == {volume_target, bind_target},
                        'GM dependency mount inventory changed: ' + name)
        volume, binding = mounts[volume_target], mounts[bind_target]
        startup.require(volume['Type'] == 'volume' and volume['Name'] == volume_name and volume['RW'] is True
                        and binding['Type'] == 'bind' and binding['RW'] is False
                        and host_source(binding['Source']) == bind_source.resolve(), 'GM dependency mount changed: ' + name)
        details = json.loads(startup.run(startup.DOCKER + ['volume', 'inspect', volume_name]))[0]
        startup.require(details.get('Labels', {}).get('com.docker.compose.project') == PROJECT,
                        'GM data volume ownership changed: ' + name)
        volumes[name] = startup.volume_signature(details)
        current = startup.state(row)
        startup.require(current['status'] in ('running', 'exited') and not current['oomKilled']
                        and current['restartCount'] == 0, 'GM dependency requires investigation: ' + name)
        if current['status'] == 'running':
            startup.require(current['health'] == 'healthy', 'Running GM dependency is not healthy: ' + name)
        result[name] = {'id': row['Id'], 'imageId': row['Image'], 'signature': startup.signature(row),
                        'state': current, 'action': 'check-running' if current['status'] == 'running' else 'start-existing'}
    startup.require(set(result) == set(SERVICES), 'GM dependency inventory is incomplete')
    return {'services': result, 'volumes': volumes, 'inputs': inputs}


def preflight(root):
    """只读：缺少既有安装时明确失败，不下载、构建、建库或生成新口令。"""
    web = web_module(root).web_preflight()
    return {'schema': 1, 'infra': infra_preflight(root), 'bridge': gm_rpc_startup.preflight(root), 'web': web}


def ensure_infra(root, plan):
    startup.require(infra_preflight(root) == plan, 'GM dependencies changed after preflight')
    actions = {}
    confirmed = {}

    def checked():
        fresh = infra_preflight(root)
        startup.require(fresh['inputs'] == plan['inputs'] and fresh['volumes'] == plan['volumes'],
                        'GM startup inputs or data volume identity changed')
        for service in SERVICES:
            startup.require(fresh['services'][service]['signature'] == plan['services'][service]['signature'],
                            'GM dependency identity changed: ' + service)
        for service, expected in confirmed.items():
            startup.require(fresh['services'][service]['state'] == expected,
                            'Confirmed GM dependency state changed: ' + service)
        return fresh

    for name in SERVICES:
        saved = plan['services'][name]
        fresh = checked()
        startup.require(fresh['services'][name]['state'] == saved['state'],
                        'GM dependency state changed before its startup step: ' + name)
        if saved['action'] == 'check-running':
            actions[name] = 'already-running'
            confirmed[name] = saved['state']
            continue
        print('start-existing: gm-' + name, flush=True)
        startup.run(startup.DOCKER + ['start', saved['id']], timeout=45)
        deadline = time.monotonic() + 240
        observed_start = None
        while time.monotonic() < deadline:
            row = startup.inspect([saved['id']])[0]
            startup.require(startup.signature(row) == saved['signature'], 'GM dependency identity changed: ' + name)
            current = startup.state(row)
            startup.require(not current['oomKilled'] and current['restartCount'] == 0
                            and current['status'] not in ('exited', 'dead', 'removing', 'restarting')
                            and current['health'] != 'unhealthy', 'GM dependency startup failed: ' + name)
            startup.require(current['startedAt'] != saved['state']['startedAt'],
                            'GM dependency start was not observed: ' + name)
            if observed_start is None:
                observed_start = current['startedAt']
            startup.require(current['startedAt'] == observed_start,
                            'GM dependency restarted during readiness checks: ' + name)
            if current['status'] == 'running' and current['health'] == 'healthy':
                break
            time.sleep(1)
        else:
            raise RuntimeError('GM dependency readiness deadline exceeded: ' + name)
        actions[name] = 'started'
        confirmed[name] = current
    final = checked()
    for name in SERVICES:
        startup.require(final['services'][name]['signature'] == plan['services'][name]['signature']
                        and final['services'][name]['state']['status'] == 'running'
                        and final['services'][name]['state']['health'] == 'healthy',
                        'GM dependency final readiness failed: ' + name)
    return {'actions': actions, 'states': {n: final['services'][n]['state'] for n in SERVICES},
            'existingIdsImagesVolumesUnchanged': True}


def ensure_started(root, evidence_dir, plan):
    """调用者持有游戏启动锁，且游戏已经就绪；此处不再次获取该锁。"""
    dependencies = ensure_infra(root, plan['infra'])
    bridge = gm_rpc_startup.ensure_started(root, evidence_dir, plan['bridge'])
    web = web_module(root).ensure_web_started()
    return {'passed': True, 'url': 'http://127.0.0.1:3101/', 'dependencies': dependencies,
            'readBridge': bridge, 'web': web, 'sceneDeploymentPerformed': False}
