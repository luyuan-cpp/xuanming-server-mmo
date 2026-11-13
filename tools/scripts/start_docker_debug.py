"""恢复本机 Docker 调试栈；缺省只读预检，--execute 启动停止的原容器。"""
from contextlib import contextmanager, nullcontext
from datetime import datetime, timezone
import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import time

ROOT = Path(__file__).resolve().parents[2]
PROJECT = "xuanming-debug-current"
DOCKER = ["docker", "--context", "desktop-linux"]
ENV = ROOT / "build/docker-debug-current/.env"
COMPOSE_FILES = [ROOT / "deploy/debug/compose.game.yml", ROOT / "deploy/debug/compose.debug.yml"]
INFRA = ("mysql", "redis", "etcd", "kafka")
APPS = ("db", "data-service", "client-rpc-router", "scene-manager", "match", "player-inbox",
        "player-locator", "scene", "battle", "login", "gate", "gateway")
INIT = ("kafka-init", "sql-boot", "db-migrate", "data-migrate", "match-migrate", "player-inbox-migrate")
WRITERS = {"db", "data-service", "match", "player-inbox", "gateway"}
ONESHOTS = set(INIT) | {name + "-sql-boot" for name in WRITERS | {"login"}}
# 这些容器只保留成功历史，不参与日常恢复；业务镜像更新不会追溯更新旧迁移容器。
HISTORY_ONLY = set(INIT) | {"login-sql-boot"}
NODE_PORTS = {"gate": 10000, "scene": 20001, "battle": 20000}
AUTHORED_TABLE_TARGETS = {**dict.fromkeys(NODE_PORTS, '/app/generated/generated_tables'),
                          **dict.fromkeys(('login', 'scene-manager', 'player-locator'), '/generated/tables')}
OUT = None



def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def env_values(data):
    result = {}
    for line in data.decode("utf-8-sig").splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        key, sep, value = line.partition("=")
        require(sep and re.fullmatch(r"[A-Z][A-Z0-9_]*", key) and key not in result,
                "Unsupported or duplicate environment assignment")
        result[key] = value
    return result


def run(args, timeout=60, env=None, input_text=None):
    # 子进程的原始输出可能含配置秘密；不写回执、不回显错误文本。
    value = subprocess.run(args, cwd=ROOT, capture_output=True, text=True, encoding="utf-8",
                           errors="replace", timeout=timeout, env=env, input=input_text)
    require(value.returncode == 0, f"Command failed with exit code {value.returncode}")
    return value.stdout


def sha(data):
    return hashlib.sha256(data).hexdigest()


def hashed(value):
    return sha(json.dumps(value, sort_keys=True, separators=(',', ':')).encode())


def authored_overlay(rows):
    # 仅识别手绘节庆部署使用的第三份 Compose，不执行容器标签指定的任意配置。
    candidates = set()
    for row in rows:
        labels = row['Config']['Labels']
        files = [Path(p) for p in labels.get('com.docker.compose.project.config_files', '').split(',')]
        if not any(path.name == 'compose.authored.json' for path in files):
            continue
        require(files[:-1] == COMPOSE_FILES and files[-1].name == 'compose.authored.json',
                'Unexpected authored Compose file order')
        candidates.add(files[-1])
    if not candidates:
        return None, None, None, {}
    require(len(candidates) == 1, 'Multiple authored Compose overlays')
    overlay = candidates.pop()
    require(overlay.parent.parent == ROOT / 'build/verification'
            and re.fullmatch(r'authored-festival-[A-Za-z0-9_.-]+', overlay.parent.name)
            and overlay.is_file() and overlay.resolve() == overlay,
            'Unexpected authored Compose overlay path')

    tables = overlay.parent / 'tables/generated/tables'
    navigation = ROOT / 'data/scene_nav_bin/authored_maps'
    require(all(path.is_dir() and path.resolve() == path for path in (tables, navigation)),
            'Authored bind directory is missing or redirected')
    allowed = {name: {target: tables} for name, target in AUTHORED_TABLE_TARGETS.items()}
    for name in NODE_PORTS:
        allowed[name]['/app/data/scene_nav_bin/authored_maps'] = navigation
    expected = {'services': {name: {'volumes': [
        {'type': 'bind', 'source': path.as_posix(), 'target': target, 'read_only': True}
        for target, path in mounts.items()]} for name, mounts in allowed.items()}}
    original = overlay.read_bytes()
    require(json.loads(original) == expected, 'Unauthorized authored Compose overlay')
    # 将脚本构造的等价配置交给 Compose，避免重复解析磁盘原文。
    return overlay, original, json.dumps(expected), allowed


def host_bind_path(source, allowed_source=None):
    # Docker Desktop 可能返回宿主路径，也可能返回其 Linux 挂载映射。
    match = re.fullmatch(r'/run/desktop/mnt/host/([a-zA-Z])/(.+)', source)
    if match:
        relative = match.group(2)
        require('\\' not in relative and all(part not in ('', '.', '..') for part in relative.split('/')),
                'Unsupported mapped bind path')
        source = match.group(1).upper() + ':/' + relative
    require(re.match(r'^[a-zA-Z]:[/\\]', source) is not None, 'Unsupported host bind path')
    path = Path(source)
    require(path.is_absolute() and path.exists(), 'Bind source unavailable')
    resolved = path.resolve()
    require(resolved == allowed_source or resolved.is_relative_to((ROOT / 'build/docker-debug-current').resolve()),
            'Bind source outside staged directory')
    return resolved


def binding_hashes(rows, allowed=None):
    # 原件只读入内存求摘要；不记录文件内容、环境变量或凭据。
    sources = {}
    for row in rows:
        targets = allowed.get(row['Config']['Labels'].get('com.docker.compose.service'), {}) if allowed else {}
        for mount in row['Mounts']:
            if mount['Type'] == 'bind':
                source = mount['Source']
                allowed_source = targets.get(mount['Destination']) if mount['RW'] is False else None
                sources[source] = host_bind_path(source, allowed_source)
    result = {}
    for source, path in sources.items():
        if path.is_file():
            files = [('.', sha(path.read_bytes()))]
        else:
            entries = sorted(path.rglob('*'))
            require(all(not p.is_symlink() and p.resolve() == p for p in entries), 'Redirected entry in bind source')
            files = [(str(p.relative_to(path)), sha(p.read_bytes())) for p in entries if p.is_file()]
        result[sha(source.encode())] = hashed(files)
    return result


def signature(row):
    # 配置、调用参数和挂载作为不可变契约，回执仅保留摘要。
    return hashed({key: row[key] for key in ('Id', 'Image', 'Config', 'HostConfig', 'Mounts')})


def now():
    return datetime.now(timezone.utc).isoformat()


def state(row):
    current = row["State"]
    return {"id": row["Id"], "imageId": row["Image"], "status": current["Status"],
            "exitCode": current["ExitCode"], "startedAt": current["StartedAt"],
            "finishedAt": current["FinishedAt"], "oomKilled": current["OOMKilled"],
            "health": current.get("Health", {}).get("Status"), "restartCount": row["RestartCount"]}


def inspect(ids):
    return json.loads(run(DOCKER + ["inspect", *ids], timeout=30))


def node_ready(identity, name):
    shell = ('test "$(readlink /proc/1/exe)" = "$1" || { echo pending; exit 0; }; '
             'awk -v p=":$2" \'$2 ~ p "$" && $4 == "0A" {ok=1} '
             'END {print ok ? "ready" : "pending"}\' /proc/1/net/tcp /proc/1/net/tcp6')
    result = run(DOCKER + ["exec", identity, "sh", "-ec", shell, "--", f"/app/bin/{name}",
                          f"{NODE_PORTS[name]:04X}"], timeout=10).strip()
    require(result in {"ready", "pending"}, "Unexpected node probe response")
    return result == "ready"


def gateway_ready(ports):
    for port in ports:
        connection = http.client.HTTPConnection("127.0.0.1", port, timeout=2)
        try:
            connection.request("GET", "/actuator/health")
            response = connection.getresponse()
            body = response.read(32769)
            if response.status != 200 or len(body) > 32768 or json.loads(body).get("status") != "UP":
                return False
        except (OSError, ValueError, http.client.HTTPException):
            return False
        finally:
            connection.close()
    return True


def save(name, value):
    with (OUT / name).open('x', encoding='utf-8') as stream:
        json.dump(value, stream, indent=2)
        stream.write('\n')


def event(kind, **data):
    value = {'utc': now(), 'event': kind, **data}
    with (OUT / 'events.jsonl').open('a', encoding='utf-8') as stream:
        stream.write(json.dumps(value) + '\n')
    print(json.dumps(value), flush=True)


def compose_contract(overlay_input=None):
    original = ENV.read_bytes()
    values = env_values(original)
    require(values.get('COMPOSE_PROJECT_NAME', PROJECT) == PROJECT, 'Unexpected configured project')
    # 宿主环境不能覆盖这次明确选择的 .env；完整配置只保存在内存。
    child_env = {k: v for k, v in os.environ.items() if k not in values and not k.startswith('COMPOSE_')}
    command = DOCKER + ['compose', '--project-name', PROJECT, '--env-file', str(ENV)]
    for path in COMPOSE_FILES:
        command += ['-f', str(path)]
    options = {'env': child_env}
    if overlay_input is not None:
        command += ['-f', '-']
        options['input_text'] = overlay_input
    config = json.loads(run(command + ['config', '--format', 'json'], **options))
    require(config.get('name') == PROJECT, 'Unexpected resolved project')
    require(set(config['services']) == set(INFRA) | set(APPS) | ONESHOTS
            and len(config['services']) == 28, 'Unexpected Compose service set')
    require(ENV.read_bytes() == original, 'Environment changed during configuration read')
    return config, sha(original)


def project_volumes():
    return sorted(run(DOCKER + ['volume', 'ls', '--filter',
        'label=com.docker.compose.project=' + PROJECT, '--format', '{{.Name}}']).splitlines())


def ports(config, name, target):
    selected = [p for p in config['services'][name].get('ports', []) if int(p['target']) == target]
    require(selected and all(p.get('host_ip') == '127.0.0.1' and p.get('protocol', 'tcp') == 'tcp'
                            for p in selected), 'Unexpected published endpoint: ' + name)
    result = sorted({int(p['published']) for p in selected})
    require(all(0 < p <= 65535 for p in result), 'Invalid published port: ' + name)
    return result


def validate_mounts(name, row, service, volume_definitions, image_volumes, volume_details):
    # 历史任务只核实际挂载；将执行的容器还必须精确匹配当前 Compose 显式挂载。
    configured = service.get('volumes', []) if service is not None else []
    explicit = {v['target']: v for v in configured}
    mounts = {v['Destination']: v for v in row['Mounts']}
    require(len(mounts) == len(row['Mounts']), 'Duplicate mount destination: ' + name)
    for target, value in explicit.items():
        if value['type'] != 'volume':
            continue
        actual = mounts.get(target, {})
        require(actual.get('Type') == 'volume'
                and actual.get('Name') == volume_definitions[value['source']]['name']
                and actual.get('RW') == (not value.get('read_only', False)),
                'Named volume bindings differ from Compose: ' + name)
    result = []
    for target, actual in mounts.items():
        if actual['Type'] != 'volume':
            continue
        identity = actual['Name']
        volume = volume_details.get(identity)
        require(volume is not None and volume.get('Name') == identity, 'Mounted volume is missing: ' + name)
        labels = volume.get('Labels') or {}
        project = labels.get('com.docker.compose.project')
        require(project in (None, PROJECT), 'Mounted volume belongs to another project: ' + name)
        require(actual.get('Driver') == volume.get('Driver'), 'Mounted volume driver differs: ' + name)
        if target in explicit:
            require(explicit[target]['type'] == 'volume', 'Volume overrides an explicit mount: ' + name)
            require(project == PROJECT, 'Named volume lacks project ownership: ' + name)
            anonymous = False
        elif service is None and project == PROJECT and 'com.docker.volume.anonymous' not in labels:
            anonymous = False
        else:
            # 镜像 VOLUME 会自动生成无 Compose 项目标签的匿名卷，必须保留其原身份。
            require(target in image_volumes and re.fullmatch(r'[0-9a-f]{64}', identity)
                    and 'com.docker.volume.anonymous' in labels and actual.get('RW') is True,
                    'Unexpected implicit volume: ' + name)
            anonymous = True
        result.append({'target': target, 'name': identity, 'readWrite': actual['RW'],
                       'driver': actual['Driver'], 'anonymous': anonymous,
                       'volumeSignature': hashed({k: volume.get(k) for k in
                           ('Name', 'Driver', 'Scope', 'CreatedAt', 'Labels', 'Options', 'Mountpoint')})})
    return sorted(result, key=lambda value: value['target'])


def inventory():
    selected = run(DOCKER + ['ps', '-aq', '--no-trunc', '--filter',
        'label=com.docker.compose.project=' + PROJECT]).split()
    require(len(selected) in (28, 29) and len(set(selected)) == len(selected), 'Unexpected project container count')
    rows = inspect(selected)
    overlay, overlay_bytes, overlay_input, allowed = authored_overlay(rows)
    config, env_sha = compose_contract(overlay_input)
    image_volumes = {}
    for identity in sorted({row['Image'] for row in rows}):
        declared = json.loads(run(DOCKER + ['image', 'inspect', '--format', '{{json .Config.Volumes}}', identity]))
        require(declared is None or isinstance(declared, dict), 'Invalid image volume declaration')
        image_volumes[identity] = sorted(declared or {})
    mounted_names = sorted({m['Name'] for row in rows for m in row['Mounts'] if m['Type'] == 'volume'})
    details = json.loads(run(DOCKER + ['volume', 'inspect', *mounted_names])) if mounted_names else []
    volume_details = {value['Name']: value for value in details}
    require(set(volume_details) == set(mounted_names) and len(volume_details) == len(details),
            'Mounted volume inventory is incomplete')
    services = {}
    extra = []
    mount_contracts = {}
    image_ids = {}
    for name, value in config['services'].items():
        if name in HISTORY_ONLY:
            continue
        reference = value['image']
        if reference not in image_ids:
            identity = run(DOCKER + ['image', 'inspect', '--format', '{{.Id}}', reference]).strip()
            require(re.fullmatch(r'sha256:[0-9a-f]{64}', identity), 'Invalid image identity: ' + name)
            image_ids[reference] = identity
    for row in rows:
        labels = row['Config']['Labels']
        name = labels.get('com.docker.compose.service')
        require(labels.get('com.docker.compose.project') == PROJECT and name in config['services'],
                'Unknown project container')
        require(row['HostConfig']['RestartPolicy']['Name'] == 'no' and row['RestartCount'] == 0,
                'Unexpected restart policy or restart count: ' + name)
        normal = labels.get('com.docker.compose.oneoff', '').lower() == 'false' and labels.get('com.docker.compose.container-number') == '1'
        if not normal:
            require(name == 'db-migrate' and labels.get('com.docker.compose.oneoff', '').lower() == 'true'
                    and state(row)['status'] == 'exited' and state(row)['exitCode'] == 0,
                    'Unexpected extra container')
            mount_contracts['storage-migration'] = validate_mounts(name, row, None, config['volumes'],
                                                                  image_volumes[row['Image']], volume_details)
            extra.append(row)
            continue
        require(name not in services, 'Duplicate service container: ' + name)
        mount_contracts[name] = validate_mounts(name, row,
            None if name in HISTORY_ONLY else config['services'][name], config['volumes'],
            image_volumes[row['Image']], volume_details)
        if name in HISTORY_ONLY:
            current = state(row)
            require(current['status'] == 'exited' and current['exitCode'] == 0
                    and not current['startedAt'].startswith('0001-'),
                    'Prior initialization did not complete: ' + name)
            # 实际签名、挂载路径和项目卷仍由下面的完整基线守卫；不与新运行配方追溯比较。
            services[name] = row
            continue
        require(row['Image'] == image_ids[config['services'][name]['image']], 'Current image differs from container: ' + name)
        targets = allowed.get(name, {})
        expected_binds = sorted(
            (v['target'], host_bind_path(v['source'], targets.get(v['target'])), bool(v.get('read_only', False)))
            for v in config['services'][name].get('volumes', []) if v['type'] == 'bind')
        actual_binds = sorted(
            (v['Destination'], host_bind_path(v['Source'], targets.get(v['Destination'])), not v['RW'])
            for v in row['Mounts'] if v['Type'] == 'bind')
        require(actual_binds == expected_binds, 'Bind mounts differ from Compose: ' + name)
        # 比较只在内存中进行；错误、日志和回执都不保存环境变量内容。
        actual_env = dict(value.split('=', 1) for value in row['Config'].get('Env', []))
        expected_env = config['services'][name].get('environment', {})
        require(all((key not in actual_env if value is None else actual_env.get(key) == str(value))
                    for key, value in expected_env.items()), 'Environment differs from Compose: ' + name)
        expected_ports = sorted((str(p['target']) + '/' + p.get('protocol', 'tcp'),
                                 p.get('host_ip', ''), str(p['published']))
                                for p in config['services'][name].get('ports', []))
        actual_ports = sorted((target, p['HostIp'], p['HostPort'])
                              for target, bindings in (row['HostConfig'].get('PortBindings') or {}).items()
                              for p in (bindings or []))
        require(actual_ports == expected_ports, 'Published ports differ from Compose: ' + name)
        services[name] = row
    require(set(services) == set(config['services']) and len(extra) <= 1, 'Incomplete current service inventory')
    if extra:
        services['storage-migration'] = extra[0]
    volume_names = project_volumes()
    named = {v['name'] for entries in mount_contracts.values() for v in entries if not v['anonymous']}
    require(named <= set(volume_names), 'Named volume is not in the project volume baseline')
    node_ports = {name: int(config['services'][name]['environment']['RPC_PORT']) for name in NODE_PORTS}
    require(node_ports == NODE_PORTS, 'Current node ports differ from the qualified readiness probe')
    binds = binding_hashes(list(services.values()), allowed)
    require(binds == binding_hashes(list(services.values()), allowed), 'Bind files changed during inventory')
    compose_sha = {str(path): sha(path.read_bytes()) for path in COMPOSE_FILES}
    if overlay is not None:
        require(overlay.read_bytes() == overlay_bytes, 'Authored Compose overlay changed during inventory')
        compose_sha[str(overlay)] = sha(overlay_bytes)
    contract = {'project': PROJECT, 'envSha256': env_sha, 'composeSha256': compose_sha,
                'scriptSha256': sha(Path(__file__).read_bytes()),
                'configSha256': hashed(config), 'ids': {n: r['Id'] for n, r in services.items()},
                'images': {n: r['Image'] for n, r in services.items()},
                'signatures': {n: signature(r) for n, r in services.items()}, 'bindHashes': binds,
                'mountedVolumes': mount_contracts, 'imageVolumeDeclarations': image_volumes,
                'projectVolumes': volume_names, 'gatewayPorts': ports(config, 'gateway', 8081),
                'gatePorts': ports(config, 'gate', node_ports['gate']), 'nodePorts': node_ports}
    return contract, services


def ready(name, row, contract):
    current = state(row)
    if current['status'] != 'running' or current['oomKilled'] or current['restartCount']:
        return False
    if 'Health' in row['State'] and current['health'] != 'healthy':
        return False
    if name in NODE_PORTS:
        return node_ready(row['Id'], name)
    if name == 'gateway':
        return gateway_ready(contract['gatewayPorts'])
    return True


def preflight():
    contract, rows = inventory()
    states = {name: state(row) for name, row in rows.items()}
    plan = []
    for name, current in states.items():
        require(not current['oomKilled'], 'Prior OOM requires investigation: ' + name)
        if name in ONESHOTS or name == 'storage-migration':
            require(current['status'] == 'exited' and current['exitCode'] == 0
                    and not current['startedAt'].startswith('0001-'),
                    'Prior initialization did not complete: ' + name)
        else:
            require(current['status'] in {'running', 'exited'}, 'Unsupported service state: ' + name)
            if current['status'] == 'running':
                require(ready(name, rows[name], contract), 'Running service is not ready: ' + name)
    for name in INFRA + APPS:
        if states[name]['status'] == 'running':
            plan.append({'service': name, 'action': 'check-running'})
        else:
            if name in WRITERS:
                plan.append({'service': name + '-sql-boot', 'action': 'register-writer'})
            plan.append({'service': name, 'action': 'start-existing'})
    return {'passed': True, 'phase': 'preflight-complete', 'utc': now(), 'contract': contract,
            'states': states, 'plan': plan, 'priorExitCodes': {n: s['exitCode'] for n, s in states.items()},
            'sourceCheckExecuted': False, 'priorCrashCauseResolved': False}


@contextmanager
def startup_lock():
    # 操作系统锁随进程退出释放；防止两个手动启动同时替换同一写者的授权。
    with ENV.with_name('.server-start.lock').open('a+b') as stream:
        stream.seek(0, 2)
        if stream.tell() == 0:
            stream.write(b'0')
            stream.flush()
        stream.seek(0)
        try:
            if os.name == 'nt':
                import msvcrt
                msvcrt.locking(stream.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl
                fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            raise RuntimeError('Another server startup is active') from None
        try:
            yield
        finally:
            stream.seek(0)
            if os.name == 'nt':
                msvcrt.locking(stream.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                fcntl.flock(stream.fileno(), fcntl.LOCK_UN)


def execute(saved, result):
    fresh = preflight()
    require(all(saved[key] == fresh[key] for key in fresh if key != 'utc'),
            'Preflight changed; refusing execution')
    contract = saved['contract']
    ids = contract['ids']
    confirmed = {}
    result.update(priorExitCodes=saved['priorExitCodes'], sourceCheckExecuted=False,
                  priorCrashCauseResolved=False, startedServices=[], checkedRunningServices=[])

    def unchanged_inputs():
        actual, rows = inventory()
        require(actual == contract, 'Container, image, volume or staged input contract changed')
        return rows

    def checked(name):
        row = inspect([ids[name]])[0]
        require(signature(row) == contract['signatures'][name], 'Container contract changed: ' + name)
        return row

    for step in saved['plan']:
        name, action = step['service'], step['action']
        result.update(activeService=name, phase='pre-start')
        rows = unchanged_inputs()
        require(state(rows[name]) == saved['states'][name], 'Service changed since preflight: ' + name)
        for done, expected in confirmed.items():
            require(state(rows[done]) == expected, 'Earlier service changed: ' + done)
        if action == 'check-running':
            require(ready(name, rows[name], contract), 'Running service is not ready: ' + name)
            confirmed[name] = state(rows[name])
            result['checkedRunningServices'].append(name)
            event('already-running', service=name)
            continue
        if action == 'register-writer':
            writer = name.removesuffix('-sql-boot')
            require(state(rows[writer]) == saved['states'][writer] and state(rows[writer])['status'] == 'exited',
                    'Writer is no longer stopped: ' + writer)
        result['phase'] = 'start'
        event('start-intent', service=name, id=ids[name])
        # 一次明确调用只启动一次；就绪轮询不是重新启动或重试失败命令。
        run(DOCKER + ['start', ids[name]], timeout=45)
        result['startedServices'].append(name)
        result['phase'] = 'readiness'
        deadline = time.monotonic() + (360 if name in INFRA else 240)
        while time.monotonic() < deadline:
            row = checked(name)
            current = state(row)
            require(not current['oomKilled'] and current['restartCount'] == 0, 'Container failed: ' + name)
            require(current['startedAt'] != saved['states'][name]['startedAt'], 'Container start not observed: ' + name)
            if action == 'register-writer' and current['status'] == 'exited' and current['exitCode'] == 0:
                break
            require(current['status'] not in {'exited', 'dead', 'removing', 'restarting'}
                    and current['health'] != 'unhealthy', 'Startup failed: ' + name)
            if action != 'register-writer' and ready(name, row, contract):
                break
            time.sleep(min(1, max(0, deadline - time.monotonic())))
        else:
            raise RuntimeError('Readiness deadline exceeded: ' + name)
        confirmed[name] = current
        result['confirmed'] = dict(confirmed)
        event('ready', service=name, state=current)

    result['phase'] = 'final-verification'
    rows = unchanged_inputs()
    for name, expected in confirmed.items():
        require(state(rows[name]) == expected, 'Final service state changed: ' + name)
    for name in set(ids) - set(confirmed):
        require(state(rows[name]) == saved['states'][name], 'Skipped initializer changed: ' + name)
    require(all(ready(name, rows[name], contract) for name in INFRA + APPS), 'Final readiness failed')
    counts = {}
    for port in contract['gatewayPorts']:
        connection = http.client.HTTPConnection('127.0.0.1', port, timeout=5)
        try:
            connection.request('GET', '/api/server-list')
            response = connection.getresponse()
            body = response.read(32769)
            require(response.status == 200 and len(body) <= 32768, 'Server list HTTP failure')
            zones = json.loads(body).get('zones')
            require(isinstance(zones, list) and any(z.get('status') == 'OPEN' for z in zones), 'No open zone')
            counts[str(port)] = len(zones)
        finally:
            connection.close()
    for port in contract['gatePorts']:
        with socket.create_connection(('127.0.0.1', port), timeout=5):
            pass
    result.update(passed=True, phase='complete', activeService=None, runtimeReadiness=True,
                  gatewayZoneCounts=counts, unchangedIdsImagesVolumes=True,
                  nextStep='Run the existing VS Code preLaunch source check before attaching.')


def main():
    global OUT
    parser = argparse.ArgumentParser(description='Start existing local Docker debug servers without replaying migrations.')
    parser.add_argument('--execute', action='store_true', help='Start stopped services; otherwise only perform read-only preflight.')
    args = parser.parse_args()
    stamp = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    OUT = ROOT / 'build/verification' / ('docker-start-' + stamp)
    OUT.mkdir(parents=True, exist_ok=False)
    print('Evidence directory: ' + str(OUT), flush=True)
    result = {'passed': False, 'startedUtc': now(), 'phase': 'preflight', 'activeService': None}
    try:
        with startup_lock() if args.execute else nullcontext():
            saved = preflight()
            save('preflight.json', saved)
            for step in saved['plan']:
                print(step['action'] + ': ' + step['service'], flush=True)
            if args.execute:
                execute(saved, result)
            else:
                result.update(passed=True, phase='preflight-complete', sourceCheckExecuted=False)
    except (Exception, KeyboardInterrupt) as error:
        result.update(errorType=type(error).__name__,
                      error=str(error) if isinstance(error, RuntimeError) else 'Operation failed; raw details suppressed')
        event('stopped', phase=result['phase'], service=result.get('activeService'), errorType=type(error).__name__)
    finally:
        result['finishedUtc'] = now()
        save('result.json', result)
    if result['passed']:
        print('Server startup completed.' if args.execute else 'Preflight completed. No containers started.', flush=True)
    else:
        print('Stopped: ' + result['error'] + '. Review the receipt; no automatic retry was performed.', flush=True)
    return 0 if result['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
