"""恢复已封存身份的 GM 只读桥；调用方负责游戏启动锁，不创建或替换容器。"""
from copy import deepcopy
import hashlib
import json
import os
from pathlib import Path
import socket
import time
import types

RELATIVE = Path('build/verification/gm-admin-20261117/rpc-bridge')
TARGETS = {'GM_DATA_TARGET': '127.0.0.1:2356', 'GM_LOCATOR_TARGET': '127.0.0.1:2357'}
WAIT_SECONDS = 65
DNS_FIELDS = ('Dns', 'DnsOptions', 'DnsSearch')


def require(value, message):
    if not value:
        raise RuntimeError(message)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def read_file(path):
    require(path.is_file() and path.resolve() == path, 'GM RPC evidence is missing or redirected')
    return path.read_bytes()


def load(root):
    root = Path(root).resolve()
    out = root / RELATIVE
    raw = read_file(out / 'ready.json')
    seal = read_file(out / 'private/ready.sha256')
    require(digest(raw) == seal.decode('ascii').strip(), 'GM RPC ready receipt digest changed')
    ready = json.loads(raw)
    require(ready.get('schema') == 1 and ready.get('passed') is True and ready.get('targets') == TARGETS,
            'GM RPC readiness receipt is invalid')
    source = root / 'deploy/gm-admin/rpc-bridge'
    script = root / 'deploy/gm-admin/rpc-bridge.py'
    paths = [*sorted(source.glob('*.go')), source / 'go.mod',
             root / 'deploy/gm-admin/rpc-bridge-Dockerfile',
             root / 'deploy/gm-admin/rpc-bridge.compose.yml', script]
    contents = {path: read_file(path) for path in paths}
    hashes = {str(path.relative_to(root)): digest(data) for path, data in contents.items()}
    require(ready.get('sources') == hashes, 'GM RPC approved sources changed')
    require(ready.get('binarySha256') == digest(read_file(out / 'context/bridge')),
            'GM RPC approved binary changed')
    # 执行已经核验并冻结的脚本字节，避免验签与 import 再读之间接受并发修改。
    module = types.ModuleType('_gm_rpc_startup_verified_bridge')
    module.__file__ = str(script)
    exec(compile(contents[script], str(script), 'exec'), module.__dict__)
    return module, ready, raw, seal


def immutable(value):
    return {key: item for key, item in value.items() if key != 'state'}


def signature_compatibility(module, row, approved):
    actual = module.startup.signature(row)
    if actual == approved:
        return actual, None
    require(all(row['HostConfig'].get(key) == [] for key in DNS_FIELDS),
            'GM RPC immutable container signature changed')
    historical = deepcopy(row)
    for key in DNS_FIELDS:
        historical['HostConfig'][key] = None
    require(module.startup.signature(historical) == approved,
            'GM RPC immutable container signature changed')
    return actual, {'fields': ['HostConfig.' + key for key in DNS_FIELDS],
                    'approvedSignature': approved, 'runtimeSignature': actual}


def observe(module, ready, compatibility=None):
    require(set(ready.get('upstreams', {})) == set(module.UPSTREAMS), 'GM RPC upstream receipt is incomplete')
    require(ready.get('imageId') == ready['bridge']['imageId'], 'GM RPC image receipt is inconsistent')
    identities = {'bridge': ready['bridge'], **ready['upstreams']}
    result = {}
    for name, approved in identities.items():
        row = module.startup.inspect([approved['id']])[0]
        expected_name = module.NAME if name == 'bridge' else module.UPSTREAMS[name][0]
        expected_project = module.PROJECT if name == 'bridge' else module.startup.PROJECT
        labels = row['Config']['Labels']
        signature, migration = signature_compatibility(module, row, approved['signature'])
        require(row['Id'] == approved['id'] and row['Image'] == approved['imageId']
                and row['Name'] == '/' + expected_name
                and labels.get('com.docker.compose.project') == expected_project
                and labels.get('com.docker.compose.service') == name,
                'GM RPC immutable container identity changed: ' + name)
        state = module.startup.state(row)
        require(state['status'] in ('running', 'exited') and not state['oomKilled']
                and state['restartCount'] == 0, 'GM RPC container requires investigation: ' + name)
        if name == 'bridge':
            require(set(row['NetworkSettings']['Networks']) == {module.NETWORK},
                    'GM RPC bridge network changed')
        result[name] = {**immutable(approved), 'signature': signature, 'state': state}
        if compatibility is not None and migration:
            compatibility[name] = migration
    return result


def make_plan(ready, raw, observed, compatibility):
    return {'schema': 1, 'receiptSha256': digest(raw), 'targets': dict(TARGETS),
            'bridge': observed['bridge'],
            'signatureMigrations': compatibility,
            'upstreams': {name: value for name, value in observed.items() if name != 'bridge'},
            'action': 'start-existing' if observed['bridge']['state']['status'] == 'exited' else 'check-running'}


def preflight(root):
    """只读检查旧回执及不可变身份；游戏上游尚未启动时也可生成计划。"""
    module, ready, raw, _ = load(root)
    compatibility = {}
    observed = observe(module, ready, compatibility)
    return make_plan(ready, raw, observed, compatibility)


def validate_transition(before, after, *, upstream, compatibility=None):
    before = deepcopy(before)
    if compatibility and before['signature'] == compatibility['approvedSignature']:
        before['signature'] = compatibility['runtimeSignature']
    require(immutable(before) == immutable(after), 'GM RPC planned container identity changed')
    previous, current = before['state'], after['state']
    if upstream and previous['status'] == 'exited':
        require(current['status'] == 'running' and current['startedAt'] != previous['startedAt'],
                'GM RPC upstream has not completed the planned startup')
    else:
        # 桥的健康状态会随游戏上游启动自然恢复；它的进程身份和起止时间不能变。
        require({key: value for key, value in previous.items() if key != 'health'} ==
                {key: value for key, value in current.items() if key != 'health'},
                'GM RPC process changed after preflight')


def atomic_replace(path, expected, replacement, protect):
    require(read_file(path) == expected, 'GM RPC receipt changed before publication')
    temporary = path.with_name(path.name + '.startup-' + str(os.getpid()) + '.tmp')
    with temporary.open('xb') as output:
        protect(temporary)
        output.write(replacement)
        output.flush()
        os.fsync(output.fileno())
    require(read_file(path) == expected, 'GM RPC receipt changed during publication')
    os.replace(temporary, path)


def ensure_started(root, evidence_dir, plan):
    """调用方已持 startup_lock 且游戏就绪；仅一次 start 原桥，失败不刷新回执。"""
    root, evidence_dir = Path(root).resolve(), Path(evidence_dir).resolve()
    require(evidence_dir.is_relative_to(root / 'build') and evidence_dir.is_dir(),
            'GM RPC startup evidence directory is invalid')
    module, ready, raw, seal = load(root)
    compatibility = {}
    observed = observe(module, ready, compatibility)
    current = make_plan(ready, raw, observed, compatibility)
    require(set(plan) == set(current) and plan['schema'] == 1
            and plan['receiptSha256'] == current['receiptSha256'] and plan['targets'] == TARGETS
            and plan['action'] == current['action'], 'GM RPC startup plan changed')
    require(all(current['signatureMigrations'].get(name) == value for name, value in plan['signatureMigrations'].items()),
            'GM RPC approved signature representation changed after preflight')
    validate_transition(plan['bridge'], current['bridge'], upstream=False, compatibility=compatibility.get('bridge'))
    require(set(plan['upstreams']) == set(current['upstreams']), 'GM RPC planned upstream set changed')
    for name in current['upstreams']:
        validate_transition(plan['upstreams'][name], current['upstreams'][name], upstream=True,
                            compatibility=compatibility.get(name))
    upstreams = module.upstreams()
    require(set(upstreams) == set(current['upstreams'])
            and all(immutable(upstreams[name]) == immutable(current['upstreams'][name]) for name in upstreams),
            'GM RPC authoritative upstream identity or endpoint changed')
    for name in upstreams:
        validate_transition(current['upstreams'][name], upstreams[name], upstream=False)
    started = current['action'] == 'start-existing'
    if started:
        module.startup.run(module.startup.DOCKER + ['start', ready['bridge']['id']], timeout=45)
    deadline = time.monotonic() + WAIT_SECONDS
    observed_start = None
    while True:
        observed_compatibility = {}
        observed = observe(module, ready, observed_compatibility)
        compatibility.update(observed_compatibility)
        state = observed['bridge']['state']
        require(state['status'] == 'running', 'GM RPC bridge startup failed')
        validate_transition(current['bridge'], observed['bridge'], upstream=started,
                            compatibility=compatibility.get('bridge'))
        if observed_start is None:
            observed_start = state['startedAt']
        require(state['startedAt'] == observed_start, 'GM RPC bridge restarted during readiness checks')
        row = module.startup.inspect([ready['bridge']['id']])[0]
        latest_state = module.startup.state(row)
        require({key: value for key, value in latest_state.items() if key != 'health'} ==
                {key: value for key, value in state.items() if key != 'health'},
                'GM RPC bridge state changed during inspection')
        state = latest_state
        if state['health'] == 'healthy':
            bridge = module.bridge_snapshot(row)
            require(immutable(bridge) == immutable(observed['bridge']), 'GM RPC bridge identity changed after startup')
            break
        require(time.monotonic() < deadline, 'GM RPC bridge readiness deadline exceeded')
        time.sleep(1)
    require(module.upstreams() == upstreams, 'GM RPC upstream changed during bridge startup')
    for port in (2356, 2357):
        with socket.create_connection(('127.0.0.1', port), timeout=3):
            pass
    # 只更新状态；再次验证旧封印，防止启动等待期间源码、凭据或容器被替换。
    checked_module, checked_ready, checked_raw, checked_seal = load(root)
    require(checked_ready == ready and checked_raw == raw and checked_seal == seal,
            'GM RPC approved receipt changed during startup')
    require(checked_module.upstreams() == upstreams
            and checked_module.bridge_snapshot(checked_module.startup.inspect([ready['bridge']['id']])[0]) == bridge,
            'GM RPC runtime changed before publication')
    refreshed = deepcopy(ready)
    refreshed['upstreams'], refreshed['bridge'] = upstreams, bridge
    changed = refreshed != ready
    before_path = evidence_dir / 'gm-rpc-ready.before.json'
    if changed:
        with before_path.open('xb') as output:
            output.write(raw)
        with (evidence_dir / 'gm-rpc-ready.before.sha256').open('xb') as output:
            output.write(seal)
        replacement = (json.dumps(refreshed, ensure_ascii=False, indent=2) + '\n').encode('utf-8')
        out = root / RELATIVE
        # 先发布数据后发布封印；两步之间及任一步失败时 verify_ready 均保持关闭。
        atomic_replace(out / 'ready.json', raw, replacement, module.protect)
        atomic_replace(out / 'private/ready.sha256', seal, (digest(replacement) + '\n').encode('ascii'), module.protect)
    require(module.verify_ready() == TARGETS, 'GM RPC restored bridge verification failed')
    return {'passed': True, 'action': current['action'], 'started': started,
            'bridgeId': bridge['id'], 'bridgeImageId': bridge['imageId'],
            'previousReceiptSha256': digest(raw), 'receiptSha256': digest(read_file(root / RELATIVE / 'ready.json')),
            'receiptRefreshed': changed, 'backupPath': str(before_path) if changed else None,
            'signatureMigrations': compatibility,
            'targets': dict(TARGETS), 'sceneConfigurationChanged': False, 'containerRecreated': False}


def refresh_upstreams(root, evidence_dir, preflight_fn):
    """上游容器经正式重编替换后，在核验原桥与现役游戏合同不变的前提下前移只读桥回执。"""
    root, evidence_dir = Path(root).resolve(), Path(evidence_dir).resolve()
    require(evidence_dir.is_relative_to(root / 'build') and evidence_dir.is_dir(),
            'GM RPC refresh evidence directory is invalid')
    module, ready, raw, seal = load(root)
    game = preflight_fn()
    require(game.get('passed') is True and game.get('contract', {}).get('project') == module.startup.PROJECT,
            'Game runtime preflight is not verified')
    bridge_row = module.startup.inspect([ready['bridge']['id']])[0]
    bridge_sig, bridge_migration = signature_compatibility(module, bridge_row, ready['bridge']['signature'])
    bridge = module.bridge_snapshot(bridge_row)
    require(bridge['id'] == ready['bridge']['id'] and bridge['imageId'] == ready['bridge']['imageId']
            and bridge['signature'] == bridge_sig, 'GM RPC immutable bridge identity changed')
    upstreams = module.upstreams()
    require(set(upstreams) == set(ready['upstreams']), 'GM RPC upstream inventory changed')
    for name, prior in ready['upstreams'].items():
        actual = upstreams[name]
        require(actual['id'] == game['contract']['ids'].get(name)
                and actual['imageId'] == game['contract']['images'].get(name)
                and actual['name'] == prior['name'] and actual['endpoint'] == prior['endpoint'],
                'GM RPC upstream does not match the verified game contract: ' + name)
    for port in (2356, 2357):
        with socket.create_connection(('127.0.0.1', port), timeout=3):
            pass
    refreshed = deepcopy(ready)
    refreshed['upstreams'], refreshed['bridge'] = upstreams, bridge
    changed = refreshed != ready
    before_path = evidence_dir / 'gm-rpc-ready.before.json'
    if changed:
        with before_path.open('xb') as output:
            output.write(raw)
        with (evidence_dir / 'gm-rpc-ready.before.sha256').open('xb') as output:
            output.write(seal)
        replacement = (json.dumps(refreshed, ensure_ascii=False, indent=2) + '\n').encode('utf-8')
        out = root / RELATIVE
        atomic_replace(out / 'ready.json', raw, replacement, module.protect)
        atomic_replace(out / 'private/ready.sha256', seal, (digest(replacement) + '\n').encode('ascii'), module.protect)
    require(module.verify_ready() == TARGETS, 'GM RPC refreshed bridge verification failed')
    return {'passed': True, 'receiptRefreshed': changed, 'bridgeId': bridge['id'],
            'bridgeMigration': bridge_migration, 'upstreams': {k: v['id'] for k, v in upstreams.items()},
            'previousReceiptSha256': digest(raw), 'receiptSha256': digest(read_file(root / RELATIVE / 'ready.json')),
            'backupPath': str(before_path) if changed else None, 'targets': dict(TARGETS)}


def main():
    import argparse
    from datetime import datetime, timezone
    import sys

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('refresh-upstreams',))
    parser.add_argument('--execute', action='store_true')
    args = parser.parse_args()
    require(args.execute, 'Refresh requires the explicit --execute flag')
    scripts = str(Path(__file__).resolve().parent)
    if scripts not in sys.path:
        sys.path.insert(0, scripts)
    import start_docker_debug as startup

    out = startup.ROOT / 'build/verification' / ('gm-rpc-refresh-' + datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ'))
    out.mkdir(parents=True, exist_ok=False)
    with startup.startup_lock():
        result = refresh_upstreams(startup.ROOT, out, startup.preflight)
    (out / 'result.json').write_text(json.dumps(result, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print('GM RPC bridge upstream receipt refreshed and verified. Evidence: ' + str(out))


if __name__ == '__main__':
    main()
