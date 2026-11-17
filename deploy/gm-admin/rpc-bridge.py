"""独立部署两条 GM 本机 RPC 桥，并绑定真实上游、源码与容器身份；不修改游戏服务。"""
from __future__ import annotations
import argparse
import base64
import csv
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'tools/scripts'))
import start_docker_debug as startup

OUT = ROOT / 'build/verification/gm-admin-20261117/rpc-bridge'
SOURCE = ROOT / 'deploy/gm-admin/rpc-bridge'
COMPOSE = ROOT / 'deploy/gm-admin/rpc-bridge.compose.yml'
RECIPE = ROOT / 'deploy/gm-admin/rpc-bridge-Dockerfile'
PROJECT = 'xuanming-gm-rpc'
NETWORK = 'xuanming-debug-current_default'
NAME = 'xuanming-gm-rpc-bridge-1'
TARGETS = {'GM_DATA_TARGET': '127.0.0.1:2356', 'GM_LOCATOR_TARGET': '127.0.0.1:2357'}
UPSTREAMS = {
    'data-service': ('xuanming-debug-current-data-service-1', 'dataservice.rpc', 9000),
    'player-locator': ('xuanming-debug-current-player-locator-1', 'playerlocator.rpc', 53200),
}


def source_hashes():
    paths = [*sorted(SOURCE.glob('*.go')), SOURCE / 'go.mod', RECIPE, COMPOSE, Path(__file__).resolve()]
    return {str(path.relative_to(ROOT)): startup.sha(path.read_bytes()) for path in paths}


def protect(path):
    if os.name == 'nt':
        sid = next(csv.reader([startup.run(['whoami', '/user', '/fo', 'csv', '/nh']).strip()]))[-1]
        startup.run(['icacls', str(path), '/inheritance:r', '/grant:r', f'*{sid}:(F)', '*S-1-5-18:(F)'])
    else:
        path.chmod(0o700 if path.is_dir() else 0o600)


def save(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')


def upstreams():
    result = {}
    for service, (name, registry_key, port) in UPSTREAMS.items():
        row = startup.inspect([name])[0]
        labels = row['Config']['Labels']
        startup.require(row['Name'] == '/' + name and labels.get('com.docker.compose.project') == startup.PROJECT
                        and labels.get('com.docker.compose.service') == service
                        and row['State']['Status'] == 'running', 'GM RPC upstream identity is invalid')
        ip = row['NetworkSettings']['Networks'][NETWORK]['IPAddress']
        endpoint = f'{ip}:{port}'
        registry = json.loads(startup.run(startup.DOCKER + ['exec', 'xuanming-debug-current-etcd-1',
            'etcdctl', 'get', registry_key + '/', '--prefix', '--write-out=json']))
        endpoints = [base64.b64decode(item['value']).decode('utf-8') for item in registry.get('kvs', [])]
        startup.require(endpoints == [endpoint], 'GM RPC registry must identify the exact existing upstream')
        result[service] = {'name': name, 'id': row['Id'], 'imageId': row['Image'],
            'signature': startup.signature(row), 'state': startup.state(row), 'endpoint': endpoint}
    return result


def bridge_snapshot(row):
    labels = row['Config']['Labels']
    startup.require(row['Name'] == '/' + NAME and labels.get('com.docker.compose.project') == PROJECT
        and labels.get('com.docker.compose.service') == 'bridge'
        and labels.get('com.docker.compose.project.config_files') == str(COMPOSE), 'Unexpected GM RPC bridge owner')
    startup.require(row['State']['Status'] == 'running' and row['State'].get('Health', {}).get('Status') == 'healthy'
        and row['RestartCount'] == 0, 'GM RPC bridge is not healthy')
    ports = row['NetworkSettings']['Ports']
    startup.require(ports == {'50901/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '2356'}],
                             '50902/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '2357'}]},
                    'GM RPC bridge ports are not the two approved loopback endpoints')
    startup.require(set(row['NetworkSettings']['Networks']) == {NETWORK}
        and row['Config']['User'] == '10001:10001' and row['HostConfig']['ReadonlyRootfs'] is True
        and row['Config']['Entrypoint'] == ['/app/gm-rpc-bridge'] and not row['Config'].get('Cmd')
        and not row['Mounts'], 'GM RPC bridge runtime shape changed')
    return {'id': row['Id'], 'imageId': row['Image'], 'signature': startup.signature(row), 'state': startup.state(row)}


def verify_ready():
    """失败抛异常；调用方只可在成功时采用返回的两个固定只读目标。"""
    raw = (OUT / 'ready.json').read_bytes()
    startup.require(startup.sha(raw) == (OUT / 'private/ready.sha256').read_text('ascii').strip(),
                    'GM RPC ready receipt digest changed')
    ready = json.loads(raw)
    startup.require(ready.get('schema') == 1 and ready.get('passed') is True
                    and ready.get('targets') == TARGETS and ready.get('sources') == source_hashes(),
                    'GM RPC ready receipt or sources changed')
    startup.require(ready['upstreams'] == upstreams(), 'GM RPC upstream runtime changed')
    row = startup.inspect([NAME])[0]
    startup.require(ready['bridge'] == bridge_snapshot(row), 'GM RPC bridge identity changed')
    startup.require(ready['binarySha256'] == startup.sha((OUT / 'context/bridge').read_bytes())
                    and ready['imageId'] == row['Image'], 'GM RPC build identity changed')
    for port in (2356, 2357):
        with socket.create_connection(('127.0.0.1', port), timeout=3):
            pass
    return dict(TARGETS)


def deploy():
    startup.require(not (OUT / 'ready.json').exists(), 'GM RPC bridge already has a ready receipt; verify it instead')
    OUT.mkdir(parents=True, exist_ok=True)
    private = OUT / 'private'
    private.mkdir(exist_ok=True)
    protect(private)
    with startup.startup_lock():
        before = startup.preflight()
        upstream_before = upstreams()
        for port in (2356, 2357):
            with socket.socket() as listener:
                listener.bind(('127.0.0.1', port))
        existing = startup.run(startup.DOCKER + ['ps', '-aq', '--filter', 'label=com.docker.compose.project=' + PROJECT]).strip()
        startup.require(not existing, 'GM RPC project already exists; refusing to recreate it')
        sources = source_hashes()
        context = OUT / 'context'
        context.mkdir(exist_ok=True)
        env = os.environ.copy()
        env.update({'CGO_ENABLED': '0', 'GOOS': 'linux', 'GOARCH': 'amd64',
                    'GOTOOLCHAIN': 'local', 'GOPROXY': 'off', 'GOSUMDB': 'off'})
        compiled = subprocess.run(['go', 'build', '-trimpath', '-ldflags=-s -w', '-o', str(context / 'bridge'), '.'],
            cwd=SOURCE, env=env, capture_output=True, text=True, timeout=180)
        (OUT / 'go-build.log').write_text(compiled.stdout + compiled.stderr, encoding='utf-8')
        startup.require(compiled.returncode == 0, 'GM RPC bridge compilation failed')
        binary_sha = startup.sha((context / 'bridge').read_bytes())
        shutil.copyfile(RECIPE, context / 'Dockerfile')
        image = 'local/xuanming-gm-rpc:' + binary_sha[:16]
        build = startup.run(startup.DOCKER + ['build', '--network=none', '--pull=false', '--iidfile', str(OUT / 'image.iid'),
            '-t', image, str(context)], timeout=180)
        (OUT / 'image-build.log').write_text(build, encoding='utf-8')
        image_id = (OUT / 'image.iid').read_text('utf-8').strip()
        startup.require(startup.inspect([image])[0]['Id'] == image_id, 'GM RPC image identity changed')
        startup.require(source_hashes() == sources and upstreams() == upstream_before, 'GM RPC inputs changed during build')
        fresh = startup.preflight()
        startup.require(fresh['contract'] == before['contract'] and fresh['states'] == before['states'],
                        'Game runtime changed during RPC bridge preparation')
        launch_env = {key: value for key, value in os.environ.items() if not key.startswith(('COMPOSE_', 'GM_RPC_'))}
        launch_env['GM_RPC_BRIDGE_IMAGE'] = image_id
        command = startup.DOCKER + ['compose', '--project-name', PROJECT, '-f', str(COMPOSE),
            'up', '-d', '--no-build', '--pull', 'never', '--wait', '--wait-timeout', '60']
        startup.run(command, timeout=90, env=launch_env)
        after = startup.preflight()
        startup.require(after['contract'] == before['contract'] and after['states'] == before['states'],
                        'RPC bridge deployment changed an existing game service')
        startup.require(source_hashes() == sources and upstreams() == upstream_before, 'RPC bridge source or upstream changed')
        row = startup.inspect([NAME])[0]
        receipt = {'schema': 1, 'passed': True, 'utc': startup.now(), 'targets': TARGETS,
            'sources': sources, 'binarySha256': binary_sha, 'imageId': image_id,
            'upstreams': upstream_before, 'bridge': bridge_snapshot(row),
            'existingGameServicesUnchanged': True, 'playerMutationPerformed': False}
        save(OUT / 'runtime-before.json', before)
        save(OUT / 'runtime-after.json', after)
        save(OUT / 'ready.json', receipt)
        checksum = private / 'ready.sha256'
        checksum.touch(exist_ok=True)
        protect(checksum)
        checksum.write_text(startup.sha((OUT / 'ready.json').read_bytes()) + '\n', encoding='ascii')
        verify_ready()
        print('GM RPC bridge verified on loopback ports 2356 and 2357; existing game services are unchanged.')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('verify', 'deploy'))
    parser.add_argument('--execute', action='store_true')
    args = parser.parse_args()
    if args.action == 'verify':
        print(json.dumps(verify_ready()))
    else:
        startup.require(args.execute, 'Deploy requires explicit --execute')
        deploy()


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print('GM RPC bridge stopped: ' + (str(error) if isinstance(error, RuntimeError) else type(error).__name__), file=sys.stderr)
        raise SystemExit(1)
