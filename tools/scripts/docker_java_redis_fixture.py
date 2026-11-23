"""Java 限流测试的独占 Redis 单机与三主集群；失败保留，成功后只删除本次资源。"""
from __future__ import annotations

import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import socket
import subprocess
import time
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[2]
NAMES = ('GATEWAY_RATE_LIMIT_REDIS_URL', 'GATEWAY_RATE_LIMIT_REDIS_CLUSTER_NODES')
PASSWORD = 'GATEWAY_RATE_LIMIT_REDIS_PASSWORD'
IMAGE = 'redis:8.10.2@sha256:6f81e8915c60b065a524e6967e0ad1c639ba6efa84d669f823683ea04d9150ee'
LABEL = 'org.xuanming.java-redis-fixture.owner'
DOCKER = ['docker', '--context', 'desktop-linux']


class FixtureCommandError(RuntimeError):
    def __init__(self, code):
        self.code = code
        super().__init__('Java Redis fixture command failed: ' + code + '; retain evidence without retry')


def _require(ok, message):
    if not ok:
        raise RuntimeError(message)


def source_inputs():
    return ('tools/scripts/docker_java_redis_fixture.py',)


def mode(environment):
    return 'external' if any(name in environment for name in (*NAMES, PASSWORD)) else 'owned'


def _external(environment):
    # URI 只在内存中处理，所有错误只报变量名，不回显值或底层异常。
    try:
        uri = urlsplit(environment[NAMES[0]])
        _require(uri.scheme == 'redis' and uri.hostname in ('127.0.0.1', 'localhost')
                 and uri.port and not uri.query and not uri.fragment and uri.path in ('', '/', '/0'),
                 'Invalid Redis URL')
        single = ('127.0.0.1', uri.port, unquote(uri.password or ''), unquote(uri.username or ''))
        nodes = []
        for item in environment[NAMES[1]].split(','):
            match = re.fullmatch(r'(127\.0\.0\.1|localhost):([1-9][0-9]{0,4})', item.strip())
            _require(match is not None and 0 < int(match[2]) <= 65535, 'Invalid cluster seed')
            nodes.append(('127.0.0.1', int(match[2]), environment.get(PASSWORD, ''), ''))
        _require(nodes and len({node[:2] for node in nodes}) == len(nodes), 'Invalid cluster seeds')
        return single, nodes
    except (ValueError, KeyError, TypeError, RuntimeError):
        raise RuntimeError('External Java Redis fixture requires explicit loopback Redis URL and cluster seeds; check ' + ', '.join(NAMES)) from None


def local_requirements(environment):
    if mode(environment) == 'owned':
        return []
    missing = [name for name in NAMES if not environment.get(name, '').strip()]
    if missing:
        return ['Explicit Redis test fixtures are incomplete; missing variables: ' + ', '.join(missing)]
    try:
        _external(environment)
        return []
    except RuntimeError as error:
        return [str(error)]


def _docker(arguments, environment, timeout=60):
    # 不记录 Docker 原始 stdout/stderr，避免外部错误回显凭证。
    try:
        result = subprocess.run(DOCKER + arguments, env=environment, capture_output=True,
                                encoding='utf-8', errors='strict', timeout=timeout)
        if result.returncode:
            # 只分类已知错误，不保存可能包含外部凭证的原始 stderr。
            stderr = result.stderr.lower()
            code = 'docker-command-failed'
            if 'all predefined address pools have been fully subnetted' in stderr:
                code = 'network-address-pool-exhausted'
            elif 'overlap' in stderr:
                code = 'network-subnet-overlap'
            elif 'port is already allocated' in stderr or 'address already in use' in stderr:
                code = 'port-already-allocated'
            raise FixtureCommandError(code)
        return result.stdout.strip()
    except (OSError, subprocess.SubprocessError, UnicodeError):
        raise RuntimeError('Java Redis fixture Docker outcome is unknown; retain evidence without retry') from None


def _json(arguments, environment):
    try:
        return json.loads(_docker(arguments, environment))
    except (ValueError, TypeError):
        raise RuntimeError('Java Redis fixture Docker identity response is invalid') from None


def _read_resp(stream, depth=0):
    _require(depth < 8, 'Redis response nesting exceeded')
    line = stream.readline(65537)
    _require(line.endswith(b'\r\n') and len(line) <= 65536, 'Redis response is invalid')
    kind, value = line[:1], line[1:-2]
    if kind == b'+':
        return value
    if kind == b'-':
        raise RuntimeError('Redis fixture rejected a readiness command')
    if kind == b':':
        return int(value)
    if kind == b'$':
        length = int(value)
        _require(0 <= length <= 1048576, 'Redis bulk response is invalid')
        data = stream.read(length + 2)
        _require(len(data) == length + 2 and data[-2:] == b'\r\n', 'Redis bulk response is incomplete')
        return data[:-2]
    if kind == b'*':
        count = int(value)
        _require(0 <= count <= 32768, 'Redis array response is invalid')
        return [_read_resp(stream, depth + 1) for _ in range(count)]
    raise RuntimeError('Redis fixture response type is invalid')


def _send(stream, *parts):
    values = [str(part).encode('utf-8') for part in parts]
    stream.write(b'*' + str(len(values)).encode() + b'\r\n' + b''.join(
        b'$' + str(len(value)).encode() + b'\r\n' + value + b'\r\n' for value in values))
    stream.flush()
    return _read_resp(stream)


def _query(endpoint, *parts):
    try:
        host, port, password, username = endpoint
        with socket.create_connection((host, port), timeout=3) as connection:
            connection.settimeout(3)
            with connection.makefile('rwb') as stream:
                if password or username:
                    auth = ('AUTH', username, password) if username else ('AUTH', password)
                    _require(_send(stream, *auth) == b'OK', 'Redis fixture authentication failed')
                return _send(stream, *parts)
    except (OSError, ValueError, RuntimeError):
        raise RuntimeError('Java Redis fixture readiness or authentication failed') from None


def _ready(environment):
    single, seeds = _external(environment)
    _require(_query(single, 'PING') == b'PONG', 'Standalone Redis fixture is not ready')
    for seed in seeds:
        info = _query(seed, 'CLUSTER', 'INFO')
        _require(b'cluster_state:ok\r\n' in info and b'cluster_slots_assigned:16384\r\n' in info,
                 'Redis cluster fixture is not ready')
        slots = _query(seed, 'CLUSTER', 'SLOTS')
        cursor, masters = 0, set()
        for row in sorted(slots, key=lambda value: value[0]):
            _require(len(row) >= 3 and row[0] == cursor and row[0] <= row[1] <= 16383,
                     'Redis cluster fixture slot coverage is invalid')
            cursor = row[1] + 1
            for node in row[2:]:
                _require(node[0] in (b'127.0.0.1', b'localhost') and 0 < node[1] <= 65535,
                         'Redis cluster fixture advertises a non-loopback endpoint')
            master = ('127.0.0.1', row[2][1], seed[2], seed[3])
            _require(_query(master, 'PING') == b'PONG', 'Redis cluster fixture master is unreachable')
            masters.add(master[:2])
        _require(cursor == 16384 and len(masters) >= 3, 'Redis fixture requires three reachable cluster masters')


def preflight(environment):
    problems = local_requirements(environment)
    _require(not problems, '\n'.join(problems))
    if mode(environment) == 'external':
        _ready(environment)
        return
    try:
        rows = _json(['image', 'inspect', IMAGE], environment)
    except RuntimeError:
        raise RuntimeError('Pinned local Redis fixture image is unavailable; automatic pull is disabled') from None
    _require(len(rows) == 1 and re.fullmatch(r'sha256:[0-9a-f]{64}', rows[0]['Id'])
             and ('redis@' + IMAGE.split('@')[1]) in rows[0].get('RepoDigests', []),
             'Pinned local Redis fixture image is unavailable or changed; automatic pull is disabled')
    _subnet(environment)


def _occupied_subnets(environment):
    ids = _docker(['network', 'ls', '-q'], environment).split()
    networks = _json(['network', 'inspect', *ids], environment) if ids else []
    prefixes = [config['Subnet'] for network in networks for config in (network.get('IPAM', {}).get('Config') or [])
                if config.get('Subnet')]
    # 宿主 VPN/静态路由同样排除，不能仅检查 Docker。默认路由不占专用地址空间。
    _require(os.name == 'nt', 'Owned Redis fixture requires Windows host route inspection')
    try:
        result = subprocess.run(['pwsh', '-NoLogo', '-NoProfile', '-NonInteractive', '-Command',
                                 '$ErrorActionPreference="Stop"; @((Get-NetRoute -AddressFamily IPv4).DestinationPrefix) | ConvertTo-Json -Compress'],
                                env=environment, capture_output=True, encoding='utf-8', timeout=30)
        _require(result.returncode == 0, 'Host route inspection failed')
        routes = json.loads(result.stdout)
        _require(isinstance(routes, list) and all(isinstance(value, str) for value in routes), 'Invalid host routes')
        prefixes.extend(routes)
        return [network for value in prefixes if (network := ipaddress.ip_network(value, strict=False)).version == 4
                and network.prefixlen != 0]
    except (OSError, subprocess.SubprocessError, ValueError, RuntimeError):
        raise RuntimeError('Java Redis fixture cannot verify host routes; no network is created') from None


def _subnet(environment):
    # 始终避开 TiDB fixture 的专用 10.254.254.0/24 网段，即使 TiDB fixture 尚未创建。
    occupied = [*_occupied_subnets(environment), ipaddress.ip_network('10.254.254.0/24')]
    # 临时范围与现有 TiDB fixture 均显式排重；随机起点只用于选择，不重试创建。
    start = secrets.randbelow(4096)
    base = int(ipaddress.IPv4Address('10.254.0.0'))
    for offset in range(4096):
        candidate = ipaddress.ip_network((base + ((start + offset) % 4096) * 16, 28))
        if not any(candidate.overlaps(existing) for existing in occupied):
            return str(candidate)
    raise RuntimeError('No isolated Redis fixture subnet is available outside Docker networks and host routes')


def _receipt(path, value):
    with Path(path).open('x', encoding='utf-8') as stream:
        json.dump(value, stream, indent=2)
        stream.write('\n')


def _ports():
    # 只探测空闲端口，不停止现有监听者；释放与 Docker bind 间的竞争由 Docker 失败封闭。
    sockets = []
    try:
        for _ in range(7):
            probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            sockets.append(probe)
            if hasattr(socket, 'SO_EXCLUSIVEADDRUSE'):
                probe.setsockopt(socket.SOL_SOCKET, socket.SO_EXCLUSIVEADDRUSE, 1)
            probe.bind(('127.0.0.1', 0))
        return [probe.getsockname()[1] for probe in sockets]
    finally:
        for probe in sockets:
            probe.close()


def _identity(state, environment):
    result = {}
    for name, cid in state['containers'].items():
        rows = _json(['container', 'inspect', cid], environment)
        _require(len(rows) == 1, 'Redis fixture container identity is missing')
        row = rows[0]
        _require(row['Id'] == cid and row['Config']['Labels'].get(LABEL) == state['nonce']
                 and row['Name'] == '/' + state['project'] + '-' + name
                 and row['Image'] == state['imageId'] and row['State']['Running'],
                 'Redis fixture container ownership or running identity changed')
        result[name] = {key: row[key] for key in ('Id', 'Image', 'Name', 'Config', 'HostConfig', 'Mounts')}
    networks = _json(['network', 'inspect', state['networkId']], environment)
    _require(len(networks) == 1 and networks[0]['Id'] == state['networkId']
             and networks[0]['Labels'].get(LABEL) == state['nonce']
             and networks[0]['Name'] == state['project']
             and set(networks[0]['Containers']) == {state['containers']['single']},
             'Redis fixture network ownership or attachments changed')
    result['network'] = {key: networks[0][key] for key in ('Id', 'Name', 'Labels', 'IPAM', 'Driver', 'Internal')}
    return result


def create(directory, environment):
    preflight(environment)
    if mode(environment) == 'external':
        return dict(environment), {'mode': 'external'}
    directory = Path(directory).resolve()
    _require(directory.is_relative_to((ROOT / 'build/verification').resolve()) and not directory.exists(),
             'Redis fixture evidence must be a new directory under build/verification')
    directory.mkdir(parents=True)
    nonce = secrets.token_hex(16)
    state = {'mode': 'owned', 'directory': str(directory), 'nonce': nonce,
             'project': 'xuanming-java-redis-' + nonce, 'containers': {},
             'sourceSha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}
    _receipt(directory / 'create.started.json', {'status': 'started', 'project': state['project']})
    try:
        state['phase'] = 'resolve-image'
        image = _json(['image', 'inspect', IMAGE], environment)[0]['Id']
        state['imageId'] = image
        ports = _ports()
        state['ports'] = ports
        state['phase'] = 'select-isolated-subnet'
        state['subnet'] = _subnet(environment)
        # 先证明随机命名空间无资源，绝不接管已有对象。
        for kind in ('container', 'network'):
            state['phase'] = 'check-absent-' + kind
            _require(not _docker([kind, 'ls', '-q', '--filter', 'name=' + state['project']], environment),
                     'Random Redis fixture namespace already exists')
        state['phase'] = 'create-network'
        state['networkId'] = _docker(['network', 'create', '--subnet', state['subnet'],
                                     '--label', LABEL + '=' + nonce, state['project']], environment)
        _receipt(directory / 'network.created.json', {'id': state['networkId']})
        for index, name in enumerate(('single', 'cluster0', 'cluster1', 'cluster2')):
            state['phase'] = 'create-' + name
            args = ['run', '--detach', '--pull=never', '--name', state['project'] + '-' + name,
                    '--label', LABEL + '=' + nonce, '--restart=no', '--memory=128m', '--cpus=0.5',
                    '--pids-limit=64', '--read-only', '--user=999:999', '--cap-drop=ALL',
                    '--security-opt=no-new-privileges', '--tmpfs', '/data:rw,noexec,nosuid,size=64m,mode=1777',
                    '--tmpfs', '/tmp:rw,noexec,nosuid,size=8m,mode=1777']
            if index == 0:
                args += ['--network', state['networkId']]
                for port in ports[:4]:
                    args += ['--publish', f'127.0.0.1:{port}:{port}']
            else:
                args += ['--network', 'container:' + state['containers']['single']]
            args += [image, 'redis-server', '--port', str(ports[index]), '--save', '', '--appendonly', 'no',
                     '--protected-mode', 'no', '--maxmemory', '64mb', '--maxmemory-policy', 'noeviction']
            if index:
                args += ['--cluster-enabled', 'yes', '--cluster-config-file', '/data/nodes.conf',
                         '--cluster-node-timeout', '5000', '--cluster-announce-ip', '127.0.0.1',
                         '--cluster-announce-port', str(ports[index]), '--cluster-port', str(ports[index + 3]),
                         '--cluster-announce-bus-port', str(ports[index + 3])]
            cid = _docker(args, environment)
            _require(re.fullmatch('[0-9a-f]{64}', cid), 'Redis fixture container creation is unconfirmed')
            state['containers'][name] = cid
            _receipt(directory / (name + '.created.json'), {'id': cid})
        child = dict(environment)
        child[NAMES[0]] = f'redis://127.0.0.1:{ports[0]}'
        child[NAMES[1]] = ','.join(f'127.0.0.1:{port}' for port in ports[1:4])
        # 仅只读 readiness 可以在单调截止时间内重查；建群写命令只发一次。
        deadline = time.monotonic() + 60
        state['phase'] = 'wait-node-readiness'
        while True:
            try:
                for port in ports[:4]:
                    _require(_query(('127.0.0.1', port, '', ''), 'PING') == b'PONG', 'Redis is not ready')
                break
            except RuntimeError:
                _require(time.monotonic() < deadline, 'Redis fixture startup readiness deadline expired')
                time.sleep(0.25)
        state['phase'] = 'create-cluster'
        _docker(['exec', state['containers']['cluster0'], 'redis-cli', '--cluster', 'create',
                 *[f'127.0.0.1:{port}' for port in ports[1:4]], '--cluster-replicas', '0', '--cluster-yes'], environment)
        state['phase'] = 'wait-cluster-readiness'
        while True:
            try:
                _ready(child)
                break
            except RuntimeError:
                _require(time.monotonic() < deadline, 'Redis fixture cluster readiness deadline expired')
                time.sleep(0.25)
        state['phase'] = 'verify-resource-identity'
        state['identity'] = _identity(state, environment)
        _receipt(directory / 'state.json', state)
        _receipt(directory / 'create.completed.json', {'status': 'confirmed', 'project': state['project']})
        return child, state
    except BaseException as error:
        _receipt(directory / 'create.hold.json', {'status': 'HOLD', 'automaticRetry': False,
                                                'automaticCleanup': False, 'phase': state.get('phase'),
                                                'errorCode': getattr(error, 'code', type(error).__name__), 'resources': state})
        raise RuntimeError('Java Redis fixture creation failed; retain owned resources and evidence without retry') from None


def close(state, environment):
    if state.get('mode') == 'external':
        return
    _require(state.get('mode') == 'owned' and state.get('javaBuildSucceeded') is True,
             'Redis fixture close requires a confirmed successful Java build')
    directory = Path(state['directory'])
    _require((directory / 'create.completed.json').is_file(), 'Redis fixture creation is not confirmed')
    _receipt(directory / 'close.started.json', {'status': 'started'})
    try:
        _require(state['sourceSha256'] == hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                 'Redis fixture producer source changed; retain resources')
        _require(_identity(state, environment) == state['identity'], 'Redis fixture resource contract changed')
        # 先删共享命名空间的从属进程，最后删持有者；只用创建时记录的完整 CID。
        for name in ('cluster2', 'cluster1', 'cluster0', 'single'):
            _docker(['container', 'stop', '--time', '10', state['containers'][name]], environment)
            _docker(['container', 'rm', state['containers'][name]], environment)
        _docker(['network', 'rm', state['networkId']], environment)
        _receipt(directory / 'close.completed.json', {'status': 'confirmed', 'ownedInfrastructureRemoved': True})
    except BaseException:
        _receipt(directory / 'close.hold.json', {'status': 'HOLD', 'automaticRetry': False, 'automaticCleanup': False})
        raise RuntimeError('Redis fixture close is unconfirmed; retain remaining resources without retry') from None
