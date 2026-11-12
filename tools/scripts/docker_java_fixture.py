"""Provision one disposable TiDB NORMAL fixture through the formal local producer.

No write is retried. A failed/unknown action leaves its private originals and
owned infrastructure intact. Only the caller's confirmed successful Java build
may retire and remove an owned fixture; external fixtures are never cleaned up.
"""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import subprocess
import time

ROOT = Path(__file__).resolve().parents[2]
CORE_NAMES = ('MMORPG_AUTHORITY_IT_JDBC_URL', 'MMORPG_SQL_USERNAME', 'MMORPG_SQL_PASSWORD',
              'MMORPG_SQL_WRITER_GRANT_FILE', 'MMORPG_SQL_WRITER_GRANT_SHA256',
              'MMORPG_SQL_DEPLOYMENT_ID', 'MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256',
              'MMORPG_SQL_WRITER_CONSUME_DIR')
HELPER_NAMES = ('MMORPG_SQL_PRIVATE_FILE_HELPER', 'MMORPG_SQL_PRIVATE_FILE_HELPER_SHA256')
FIXTURE_NAMES = CORE_NAMES + HELPER_NAMES
DEFAULT_HELPER_SHA256 = '5e38b54f2960324dfc5bc6191e483322a543e35831341478454430b302c0d131'
PUBLIC_INPUTS = ('tools/scripts/docker_java_fixture.py', 'tools/scripts/docker_java_fixture.ps1',
                 'deploy/debug/java-fixture/compose.yml', 'deploy/debug/java-fixture/tikv.toml',
                 'go/shared/cmd/sql_file_surface/main.go',
                 'tools/scripts/lib/sql_boot_catalog_local.ps1', 'tools/scripts/lib/release_common.ps1',
                 'tools/scripts/maintenance_drain.ps1', 'deploy/sql/authority_runtime_role.sql',
                 'deploy/sql/authority_operator_role.sql', 'java/mall_game/src/main/resources/application.yaml',
                 'java/mall_game/src/main/resources/db/schema.sql')
IMAGES = {'pd': 'sha256:507b401ece53aba15db9628a90a1e350c3cd1b9b39c086c708e2e88541b0e7fd',
          'tikv': 'sha256:cdb681d570c490424d69ecc1ecda6865999f2b86ae3b2d2cb070989bf4d2701d',
          'tidb': 'sha256:3378c71dc8b3dd39742dc0ba546c80880a5d8b0da67a9f9d116cfc33505505c3'}
OWNER_LABEL = 'org.xuanming.java-fixture.owner'
COMPOSE = ROOT / 'deploy/debug/java-fixture/compose.yml'
RUNNER = ROOT / 'tools/scripts/docker_java_fixture.ps1'
DOCKER = ['docker', '--context', 'desktop-linux']
CONTROL_NAMES = {'MMORPG_SQL_SCHEMA_DSN', 'MMORPG_SQL_REGISTRAR_DSN', 'MMORPG_SQL_MAINTENANCE_DSN',
                 'MMORPG_SQL_SCHEMA_PRINCIPAL', 'MMORPG_SQL_OPERATOR_PRINCIPAL',
                 'SQL_SCHEMA_DSN', 'SQL_REGISTRAR_DSN', 'SQL_MAINTENANCE_DSN'}


class _FixtureFailure(RuntimeError):
    """Only fixed, public diagnostics produced by this module may enter HOLD."""


def _require(ok, message):
    if not ok:
        raise _FixtureFailure(message)


def _sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def source_inputs():
    """Rescan each time: new/deleted local Go inputs must also trip source guards."""
    return tuple(sorted(set(PUBLIC_INPUTS) | {
        str(path.relative_to(ROOT)).replace('\\', '/')
        for module in ('data_service', 'shared', 'proto')
        for path in (ROOT / 'go' / module).rglob('*')
        if path.is_file() and (path.name in ('go.mod', 'go.sum') or
                              path.suffix == '.go' and not path.name.endswith('_test.go'))
    }))


def mode(environment):
    # Presence, including an explicitly empty value, must never opt into auto creation.
    return 'external' if any(name in environment for name in CORE_NAMES) else 'owned'


def _helper(environment):
    if any(name in environment for name in HELPER_NAMES):
        return tuple(environment.get(name, '') for name in HELPER_NAMES)
    document_copy = Path(environment.get('USERPROFILE', '')) / 'Documents/XuanmingDebugHelpers/private_control_file.exe'
    if document_copy.is_file():
        return str(document_copy), DEFAULT_HELPER_SHA256
    return (str(Path(environment.get('LOCALAPPDATA', '')) / 'mmorpg/sql-boot-local/private_control_file.exe'), DEFAULT_HELPER_SHA256)


def _clean(environment):
    return {key: value for key, value in environment.items()
            if not key.upper().startswith('COMMERCE_IT_') and key.upper() not in CONTROL_NAMES
            and (not key.upper().startswith('MMORPG_SQL_') or key.upper() in FIXTURE_NAMES)}


def _loopback_jdbc(value):
    # An exact authority and one unescaped schema exclude userinfo, multi-host
    # forms, fragments, query overrides and parser-dependent host ambiguity.
    match = re.fullmatch(r'jdbc:mysql://(?:127\.0\.0\.1|localhost):([1-9][0-9]{0,4})/[A-Za-z_][A-Za-z0-9_]{0,63}', value)
    return match is not None and int(match.group(1)) <= 65535


def _public_file(path):
    path = Path(path)
    return path.is_absolute() and path.is_file() and not path.is_symlink() and not path.is_junction()


def local_requirements(environment):
    """Static local checks; returns only fixed messages and public variable names."""
    problems = []
    if os.name != 'nt':
        problems.append('Automatic Java fixture requires the existing Windows NTFS/CF3 toolchain')
    external = mode(environment) == 'external'
    if external:
        missing = [name for name in FIXTURE_NAMES if not environment.get(name)]
        if missing:
            problems.append('External Java NORMAL fixture is incomplete; missing variables: ' + ', '.join(missing))
        if environment.get('MMORPG_AUTHORITY_IT_JDBC_URL') and not _loopback_jdbc(environment['MMORPG_AUTHORITY_IT_JDBC_URL']):
            problems.append('External Java fixture JDBC URL must name one explicit loopback host, port and schema without URL options')
    for name in (('pwsh',) if external else ('pwsh', 'docker', 'go')):
        if not shutil.which(name, path=environment.get('PATH')):
            problems.append('Existing Java fixture tool is unavailable: ' + name)
    helper, pin = _helper(environment)
    pairs = [(helper, pin, 'MMORPG_SQL_PRIVATE_FILE_HELPER')]
    if external and environment.get('MMORPG_SQL_WRITER_GRANT_FILE'):
        pairs.append((environment['MMORPG_SQL_WRITER_GRANT_FILE'],
                      environment.get('MMORPG_SQL_WRITER_GRANT_SHA256', ''), 'MMORPG_SQL_WRITER_GRANT_FILE'))
    for path, expected, name in pairs:
        try:
            valid = (_public_file(path) and re.fullmatch('[0-9a-fA-F]{64}', expected)
                     and _sha(path) == expected.lower())
        except (OSError, ValueError):
            valid = False
        if not valid:
            problems.append('Java fixture file or SHA256 pin is invalid: ' + name)
    if external:
        if environment.get('MMORPG_SQL_WRITER_CONSUME_DIR'):
            path = Path(environment['MMORPG_SQL_WRITER_CONSUME_DIR'])
            if not path.is_absolute() or not path.is_dir() or path.is_symlink() or path.is_junction():
                problems.append('External Java fixture consume directory is unavailable')
        for name in ('MMORPG_SQL_WRITER_GRANT_SHA256', 'MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256'):
            if environment.get(name) and not re.fullmatch('[0-9a-fA-F]{64}', environment[name]):
                problems.append('External Java fixture checksum is invalid: ' + name)
    else:
        for relative in PUBLIC_INPUTS + ('go/data_service/cmd/writer_authority/main.go',):
            if not (ROOT / relative).is_file():
                problems.append('Formal Java fixture source is unavailable: ' + relative)
    return problems


def _run(command, environment, timeout=60, cwd=ROOT):
    # stdout may contain CF3 context only in _read_context; callers never log it.
    try:
        result = subprocess.run(command, cwd=cwd, env=environment, capture_output=True,
                                timeout=timeout, encoding='utf-8', errors='strict')
    except (OSError, subprocess.SubprocessError, UnicodeError):
        raise _FixtureFailure('Java fixture command outcome is unconfirmed; retain evidence without retry') from None
    _require(result.returncode == 0, 'Java fixture command failed; retain evidence without retry')
    return result.stdout


def _docker(arguments, environment, timeout=60):
    return _run(DOCKER + arguments, environment, timeout)


def _json_docker(arguments, environment):
    try:
        return json.loads(_docker(arguments, environment))
    except (ValueError, TypeError):
        raise _FixtureFailure('Java fixture Docker identity response is invalid') from None


def _receipt(path, value):
    with Path(path).open('x', encoding='utf-8') as stream:
        json.dump(value, stream, indent=2)
        stream.write('\n')


def _port_available():
    # An exclusive bind detects an existing listener on loopback or wildcard without connecting to it.
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
            if hasattr(socket, 'SO_EXCLUSIVEADDRUSE'):
                probe.setsockopt(socket.SOL_SOCKET, socket.SO_EXCLUSIVEADDRUSE, 1)
            probe.bind(('127.0.0.1', 24000))
    except OSError:
        raise _FixtureFailure('Java fixture loopback port 24000 is occupied; no existing listener is adopted or stopped') from None


def _runner(action, environment, state_file=None):
    helper, pin = _helper(environment)
    command = ['pwsh', '-NoLogo', '-NoProfile', '-NonInteractive', '-File', str(RUNNER),
               '-Action', action, '-PrivateHelper', helper, '-PrivateHelperSha256', pin]
    if state_file is not None:
        command += ['-StateFile', str(state_file)]
    _run(command, _clean(environment), 240)


def preflight(environment):
    """Read-only validation; never creates a directory, grant, SQL schema or container."""
    problems = local_requirements(environment)
    _require(not problems, '\n'.join(problems))
    clean = _clean(environment)
    _runner('check-helper', clean)
    if mode(environment) == 'external':
        helper, _ = _helper(environment)
        _run([helper, '--command', 'validate-directory', '--path', environment['MMORPG_SQL_WRITER_CONSUME_DIR'],
              '--max-bytes', '16777216'], clean)
        raw = _run([helper, '--command', 'read-file', '--path', environment['MMORPG_SQL_WRITER_GRANT_FILE'],
                    '--max-bytes', '16777216'], clean)
        _require(hashlib.sha256(raw.encode('utf-8')).hexdigest() == environment['MMORPG_SQL_WRITER_GRANT_SHA256'].lower(),
                 'External Java fixture grant pin changed during private validation')
        return
    _port_available()
    _require(_run(['go', 'version'], {**clean, 'GOTOOLCHAIN': 'local', 'GOWORK': 'off', 'GOENV': 'off'}).strip()
             == 'go version go1.27.2 windows/amd64',
             'Java fixture requires the existing Go 1.27.2 Windows toolchain')
    _docker(['compose', 'version'], clean)
    for expected in IMAGES.values():
        _require(_docker(['image', 'inspect', '--format', '{{.Id}}', expected], clean).strip() == expected,
                 'Pinned local Java fixture image is missing or changed; automatic pull is disabled')


def _compose(state):
    return ['compose', '-p', state['project'], '-f', str(COMPOSE)]


def _owned_environment(environment, state):
    result = {key: value for key, value in _clean(environment).items()
              if not key.upper().startswith('COMPOSE_') and key.upper() not in FIXTURE_NAMES}
    result.update(DOCKER_CONTEXT='desktop-linux', JAVA_FIXTURE_OWNER=state['nonce'])
    return result


def _assert_absent(state, environment):
    for kind in ('container', 'network', 'volume'):
        arguments = [kind, 'ls', '-q', '--filter', 'label=com.docker.compose.project=' + state['project']]
        if kind == 'container':
            arguments.insert(2, '-a')
        _require(not _docker(arguments, environment).strip(), 'Random Java fixture project already exists; refusing adoption')
    for name in (state['project'] + '_pd-data', state['project'] + '_tikv-data'):
        _require(not _docker(['volume', 'ls', '-q', '--filter', 'name=^' + name + '$'], environment).strip(),
                 'Random Java fixture volume name already exists; refusing adoption')
    _require(not _docker(['network', 'ls', '-q', '--filter', 'name=^' + state['project'] + '_default$'], environment).strip(),
             'Random Java fixture network name already exists; refusing adoption')


def _labels(labels, state):
    return labels.get(OWNER_LABEL) == state['nonce'] and labels.get('com.docker.compose.project') == state['project']


def _inventory(state, environment):
    containers = {}
    for service, expected in IMAGES.items():
        cid = _docker(_compose(state) + ['ps', '-aq', service], environment).strip()
        _require(re.fullmatch('[a-f0-9]{64}', cid), 'Expected exactly one Java fixture container per service')
        rows = _json_docker(['container', 'inspect', cid], environment)
        _require(len(rows) == 1, 'Java fixture container inspection is incomplete')
        row = rows[0]
        labels = row['Config']['Labels'] or {}
        host = row['HostConfig']
        _require(row['Id'] == cid and _labels(labels, state) and labels.get('com.docker.compose.service') == service
                 and row['Image'] == expected and row['State']['Running'] is True
                 and host['RestartPolicy']['Name'] == 'no', 'Java fixture container ownership or running identity changed')
        bindings = host.get('PortBindings') or {}
        _require(bindings == ({'4000/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '24000'}]} if service == 'tidb' else {}),
                 'Java fixture host ports differ from the isolated loopback contract')
        _require(host['Memory'] == (512 if service == 'pd' else 1536) * 1024 * 1024
                 and host['NanoCpus'] == (500000000 if service == 'pd' else 1000000000),
                 'Java fixture resource limits differ from the bounded contract')
        mounts = row.get('Mounts', [])
        expected_volume = state['project'] + '_' + service + '-data'
        expected_mounts = [] if service == 'tidb' else [('volume', expected_volume, '/data', True)]
        if service == 'tikv':
            expected_mounts.append(('bind', str(COMPOSE.parent / 'tikv.toml').replace('\\', '/').lower(), '/etc/tikv-fixture.toml', False))
        actual = []
        for mount in mounts:
            source = mount.get('Name') if mount['Type'] == 'volume' else mount['Source'].replace('\\', '/').lower()
            # Docker Desktop may report its normalized host bind through /run/desktop/mnt/host/d/...
            if mount['Type'] == 'bind' and source.startswith('/run/desktop/mnt/host/'):
                suffix = source[len('/run/desktop/mnt/host/'):]
                source = suffix[0] + ':' + suffix[1:]
            actual.append((mount['Type'], source, mount['Destination'], mount['RW']))
        _require(sorted(actual) == sorted(expected_mounts), 'Java fixture mounts differ from its new owned volumes and pinned config')
        _require(set(row['NetworkSettings']['Networks']) == {state['project'] + '_default'},
                 'Java fixture joined an unexpected network')
        containers[service] = cid
    listed = set(_docker(['container', 'ls', '-aq', '--no-trunc', '--filter',
                         'label=com.docker.compose.project=' + state['project']], environment).split())
    _require(listed == set(containers.values()), 'Unexpected container in the Java fixture project')
    networks = _json_docker(['network', 'inspect', state['project'] + '_default'], environment)
    _require(len(networks) == 1, 'Java fixture network identity is incomplete')
    network = networks[0]
    _require(_labels(network.get('Labels') or {}, state) and set(network.get('Containers') or {}) == set(containers.values()),
             'Java fixture network has changed ownership or foreign attachments')
    volumes = {}
    for service in ('pd', 'tikv'):
        name = state['project'] + '_' + service + '-data'
        rows = _json_docker(['volume', 'inspect', name], environment)
        _require(len(rows) == 1 and _labels(rows[0].get('Labels') or {}, state), 'Java fixture volume ownership changed')
        volume = rows[0]
        _require(volume['Name'] == name and volume['Driver'] == 'local' and not volume.get('Options'),
                 'Java fixture volume is not a new local volume')
        volumes[name] = {key: volume.get(key) for key in ('Name', 'CreatedAt', 'Driver', 'Mountpoint', 'Labels', 'Options')}
    return {'containers': containers, 'networkId': network['Id'], 'volumes': volumes}


def _read_greeting(connection, deadline):
    """Read only one bounded protocol-10 packet; never send credentials or SQL."""
    def receive_exact(size):
        data = bytearray()
        while len(data) < size:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError('Greeting deadline expired')
            connection.settimeout(min(1.0, remaining))
            chunk = connection.recv(size - len(data))
            if not chunk:
                raise OSError('Incomplete greeting')
            data.extend(chunk)
        return bytes(data)

    header = receive_exact(4)
    size = int.from_bytes(header[:3], 'little')
    if header[3] != 0 or not 34 <= size <= 16384:
        return False
    payload = receive_exact(size)
    if payload[0] != 10:
        return False
    version_end = payload.find(b'\0', 1, 257)
    # Require the fixed 4.1 greeting fields after the terminated version. The
    # authentication challenge remains in memory and is never logged or saved.
    if version_end < 2 or len(payload) < version_end + 32:
        return False
    version = payload[1:version_end]
    return re.search(rb'TiDB-v8[.]5[.]8(?:$|[^0-9.])', version) is not None


def _ready(environment, seconds=120):
    deadline = time.monotonic() + seconds
    while (remaining := deadline - time.monotonic()) > 0:
        try:
            with socket.create_connection(('127.0.0.1', 24000), timeout=min(1.0, remaining)) as connection:
                if _read_greeting(connection, deadline):
                    return
        except OSError:
            pass
        remaining = deadline - time.monotonic()
        if remaining > 0:
            time.sleep(min(0.5, remaining))
    raise _FixtureFailure('Java fixture MySQL greeting readiness deadline expired; retain owned resources without SQL retry')


def _read_context(state, environment):
    helper, _ = _helper(environment)
    raw = _run([helper, '--command', 'read-file', '--path', state['contextFile'], '--max-bytes', '16777216'], _clean(environment))
    receipt = json.loads((Path(state['directory']) / 'producer-create.completed.json').read_text('utf-8-sig'))
    _require(hashlib.sha256(raw.encode('utf-8')).hexdigest() == receipt['contextSha256'], 'Java fixture context original changed')
    context = json.loads(raw)
    _require(context.get('format') == 'JAVA_OWNED_TIDB_CONTEXT_V1' and context.get('nonce') == state['nonce']
             and context.get('jdbcUrl') == 'jdbc:mysql://127.0.0.1:24000/mall_it_' + state['nonce']
             and context.get('ownerFile') == str(Path(state['fixtureRoot']) / 'owner.json')
             and re.fullmatch('[a-f0-9]{64}', context.get('ownerSha256', '')),
             'Java fixture context does not match this invocation owner')
    allowed = set(CORE_NAMES) - {'MMORPG_AUTHORITY_IT_JDBC_URL', 'MMORPG_SQL_USERNAME', 'MMORPG_SQL_PASSWORD'}
    _require(set(context.get('environment', {})) == allowed and all(context['environment'].values())
             and context.get('username') and context.get('password'), 'Java fixture NORMAL context is incomplete')
    for name in ('MMORPG_SQL_WRITER_GRANT_FILE', 'MMORPG_SQL_WRITER_CONSUME_DIR'):
        _require(Path(context['environment'][name]).resolve().is_relative_to(Path(state['fixtureRoot']).resolve()),
                 'Java fixture NORMAL path escaped this invocation')
    child = _clean(environment)
    child.update(context['environment'])
    child.update(MMORPG_AUTHORITY_IT_JDBC_URL=context['jdbcUrl'], MMORPG_SQL_USERNAME=context['username'],
                 MMORPG_SQL_PASSWORD=context['password'])
    child.update(zip(HELPER_NAMES, _helper(environment)))
    return child


def create(directory: Path, environment: dict):
    preflight(environment)
    if mode(environment) == 'external':
        return _clean(environment), {'mode': 'external'}
    directory = Path(directory).absolute()
    _require(directory.resolve().is_relative_to((ROOT / 'build/verification').resolve()),
             'Owned Java fixture evidence must be in the ignored build/verification directory')
    _require(not directory.exists() and not directory.is_symlink(), 'Java fixture directory already exists; no automatic replay')
    directory.mkdir(parents=True, exist_ok=False)
    nonce = secrets.token_hex(16)
    private = directory / 'private'
    fixture = private / ('java-fixture-' + nonce)
    state = {'mode': 'owned', 'directory': str(directory), 'nonce': nonce,
             'project': 'xuanming-java-fixture-' + nonce, 'privateRoot': str(private),
             'fixtureRoot': str(fixture), 'contextFile': str(fixture / 'context.json')}
    _receipt(directory / 'create.started.json', {'status': 'started', 'project': state['project'], 'automaticRetry': False})
    phase = 'prepare-tool-directory'
    try:
        clean = _owned_environment(environment, state)
        tools = directory / 'tools'
        tools.mkdir()
        phase = 'snapshot-source-inputs'
        inputs = {name: _sha(ROOT / name) for name in source_inputs()}
        go_environment = {**clean, 'GOWORK': 'off', 'GOPROXY': 'off', 'GOSUMDB': 'off', 'GOTOOLCHAIN': 'local',
                          'GOENV': 'off', 'GOFLAGS': '', 'GOOS': 'windows', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'}
        built = {}
        for module, name in (('data_service', 'writer_authority'), ('shared', 'sql_file_surface')):
            phase = 'build-' + name
            executable = tools / (name + '.exe')
            _run(['go', 'build', '-mod=readonly', '-trimpath', '-o', str(executable), './cmd/' + name],
                 go_environment, 600, ROOT / 'go' / module)
            built[name] = {'path': str(executable), 'sha256': _sha(executable)}
        state['tools'] = built
        phase = 'verify-source-inputs'
        _require(inputs == {name: _sha(ROOT / name) for name in source_inputs()},
                 'Java fixture helper sources changed during offline build')
        state['inputs'] = inputs
        phase = 'record-inputs'
        _receipt(directory / 'state.json', state)
        phase = 'verify-infrastructure-absent'
        _assert_absent(state, clean)
        phase = 'verify-port'
        _port_available()
        phase = 'validate-compose'
        _docker(_compose(state) + ['config', '--quiet'], clean)
        # No --force-recreate and no retry: the random project was just proved absent.
        phase = 'start-infrastructure'
        _docker(_compose(state) + ['up', '-d', '--no-build', '--pull', 'never'], clean, 180)
        phase = 'verify-infrastructure'
        inventory = _inventory(state, clean)
        phase = 'record-infrastructure'
        _receipt(directory / 'infra.completed.json', inventory)
        state['infrastructure'] = inventory
        phase = 'mysql-greeting-readiness'
        _ready(clean)
        phase = 'producer-create'
        _runner('create', environment, directory / 'state.json')
        phase = 'read-normal-context'
        child = _read_context(state, environment)
        phase = 'record-create'
        _receipt(directory / 'create.completed.json', {'status': 'confirmed', 'project': state['project'],
                 'contextFile': state['contextFile'], 'localQualificationOnly': True})
        return child, state
    except BaseException as error:
        reason = str(error) if isinstance(error, _FixtureFailure) else 'Unexpected local failure; raw details suppressed'
        _receipt(directory / 'create.hold.json', {'status': 'HOLD', 'phase': phase, 'reason': reason,
                 'automaticRetry': False, 'automaticCleanup': False})
        raise RuntimeError('Owned Java fixture preparation failed; retain this invocation evidence and resources; no automatic retry or cleanup') from None


def close(state, environment):
    if state.get('mode') == 'external':
        return
    _require(state.get('mode') == 'owned' and state.get('javaBuildSucceeded') is True,
             'Owned Java fixture may close only after a confirmed successful Java build')
    directory = Path(state['directory'])
    _require((directory / 'create.completed.json').is_file(), 'Java fixture creation is not confirmed')
    _receipt(directory / 'close.started.json', {'status': 'started', 'automaticRetry': False})
    try:
        _require(state['inputs'] == {name: _sha(ROOT / name) for name in source_inputs()},
                 'Java fixture formal inputs changed; retain fixture')
        clean = _owned_environment(environment, state)
        _require(_inventory(state, clean) == state['infrastructure'], 'Owned Java fixture infrastructure identity changed')
        _runner('close', environment, directory / 'state.json')
        _require((directory / 'producer-close.completed.json').is_file(), 'Java fixture formal retirement is not confirmed')
        _require(_inventory(state, clean) == state['infrastructure'], 'Owned Java fixture infrastructure changed after retirement')
        ids = list(state['infrastructure']['containers'].values())
        _docker(['container', 'stop', '--time', '30', *ids], clean, 120)
        _docker(['container', 'rm', *ids], clean)
        _docker(['network', 'rm', state['infrastructure']['networkId']], clean)
        for name, expected in state['infrastructure']['volumes'].items():
            rows = _json_docker(['volume', 'inspect', name], clean)
            _require(len(rows) == 1 and {key: rows[0].get(key) for key in expected} == expected,
                     'Owned Java fixture volume identity changed before removal')
            _docker(['volume', 'rm', name], clean)
        _receipt(directory / 'close.completed.json', {'status': 'confirmed', 'retired': True, 'ownedInfrastructureRemoved': True})
    except BaseException:
        _receipt(directory / 'close.hold.json', {'status': 'HOLD', 'automaticRetry': False, 'automaticCleanup': False})
        raise RuntimeError('Owned Java fixture close is unconfirmed; retain remaining evidence and resources without retry') from None
