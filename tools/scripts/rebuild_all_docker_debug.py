"""串行构建整套本机调试服，全部成功后刷新原配置并统一更新容器。"""
from __future__ import annotations

import argparse
from contextlib import contextmanager, nullcontext, redirect_stdout
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import threading
import time

import rebuild_docker_debug as rebuild
import start_docker_debug as startup
import docker_java_fixture as java_fixture

ROOT = Path(__file__).resolve().parents[2]
STATE = ROOT / 'build/docker-debug-current'
ORDER = ('cpp', 'go', 'java')
SERVICES = {'cpp': rebuild.CPP, 'go': rebuild.GO, 'java': ('gateway',)}


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def write_json(path, value):
    with path.open('x', encoding='utf-8') as stream:
        json.dump(value, stream, ensure_ascii=False, indent=2)
        stream.write('\n')


def select_languages(value):
    requested = value.split(',')
    require(requested and len(set(requested)) == len(requested) and set(requested) <= set(ORDER),
            'Languages must be a unique comma-separated selection of cpp,go,java')
    return tuple(language for language in ORDER if language in requested)


def local_requirements(languages, environment):
    """先检查不需要 Docker 的所有构建前提；错误只返回变量名，不返回变量值。"""
    problems = []
    tools = {'docker', 'git'}
    if 'go' in languages:
        tools.add('go')
    if 'java' in languages:
        import build_docker_java_debug as java
        tools.add('pwsh')
        problems.extend(java_fixture.local_requirements(environment))
        for label, path in (('Java', java.DEFAULT_JDK / 'bin/java.exe'),
                            ('javap', java.DEFAULT_JDK / 'bin/javap.exe'), ('Maven', java.DEFAULT_MAVEN),
                            ('pinned protoc', java.DEFAULT_PROTOC)):
            if not path.is_file():
                problems.append('Existing Java tool is unavailable: ' + label)
        if java.DEFAULT_PROTOC.is_file() and java.sha256(java.DEFAULT_PROTOC) != java.DEFAULT_PROTOC_SHA256:
            problems.append('Existing protoc does not match the Java builder SHA256 pin')
    for name in sorted(tools):
        if not shutil.which(name):
            problems.append('Existing tool is unavailable: ' + name)
    return problems


def child_environment(canonical, inherited):
    from prepare_docker_debug import DEV_INTERNAL_IDENTITY
    # Compose 必须以 canonical .env 为准；旧 .env 缺少的新开发身份键只在子进程内补齐。
    result = {key: value for key, value in inherited.items()
              if key not in canonical and not key.startswith('COMPOSE_')}
    result['DOCKER_CONTEXT'] = 'desktop-linux'
    for key, value in DEV_INTERNAL_IDENTITY.items():
        if key not in canonical:
            result[key] = value
    return result


def refresh_sources():
    from prepare_docker_debug import GO_CONFIGS, SQL_BOOT_SOURCES
    names = [f'go/{module}/etc/{name}' for module, name in GO_CONFIGS.items()]
    names += ['bin/etc/base_deploy_config.yaml', 'bin/etc/game_config.yaml', 'bin/etc/battle_rewards.json',
              'deploy/mysql-init/00_init_zone_dbs.sql', *SQL_BOOT_SOURCES,
              'deploy/debug/compose.game.yml', 'deploy/debug/compose.debug.yml',
              'deploy/debug/Dockerfile.cpp', 'deploy/debug/Dockerfile.go']
    names += ['tools/scripts/' + name + '.py' for name in
              ('prepare_docker_debug', 'start_docker_debug', 'rebuild_docker_debug', 'rebuild_all_docker_debug',
               'build_docker_cpp_debug', 'build_docker_go_debug', 'build_docker_java_debug')]
    names += list(java_fixture.source_inputs())
    return {name: rebuild.digest(ROOT / name) for name in sorted(set(names))}


def partial_refresh_guard(languages, canonical, config, services):
    """全量渲染也会触及未重编语言；不允许将变化的配置交给旧进程。"""
    from prepare_docker_debug import GO_CONFIGS, SQL_BOOT_SOURCES, DEV_INTERNAL_IDENTITY
    selected = set(languages)
    missing_identity = any(not canonical.get(key) for key in DEV_INTERNAL_IDENTITY)
    require(not missing_identity or {'cpp', 'go'} <= selected,
            'Internal identity migration requires C++ and Go together; use --languages cpp,go or the full command')
    if selected == set(ORDER):
        return
    recorded = json.loads((STATE / 'source-manifest.json').read_text('utf-8-sig'))
    renderer = 'tools/scripts/prepare_docker_debug.py'
    require(recorded.get(renderer) == rebuild.digest(ROOT / renderer),
            'Configuration renderer changed or has no recorded hash; use the full command')
    by_language = {
        'cpp': ['bin/etc/base_deploy_config.yaml', 'bin/etc/game_config.yaml', 'bin/etc/battle_rewards.json'],
        'go': [f'go/{module}/etc/{name}' for module, name in GO_CONFIGS.items()],
        'java': ['java/gateway_node/src/main/resources/application.yaml'],
    }
    for language in set(ORDER) - selected:
        for name in by_language[language]:
            require(recorded.get(name) == rebuild.digest(ROOT / name),
                    'Configuration changed for omitted language ' + language + ': ' + name
                    + '; include ' + language + ' in --languages or use the full command')
        for name in SERVICES[language]:
            if name in rebuild.ONE_SHOT:
                continue
            actual = dict(value.split('=', 1) for value in services[name]['Config'].get('Env', []))
            expected = config['services'][name].get('environment', {})
            require(all((key not in actual if value is None else actual.get(key) == str(value))
                        for key, value in expected.items()),
                    'Compose environment changed for omitted service ' + name
                    + '; include ' + language + ' in --languages or use the full command')
    # 共享注册代码和权限模板的刷新会影响全部写者，不能只替换其中一部分。
    shared = set(SQL_BOOT_SOURCES) - set(by_language['java'])
    require(all(recorded.get(name) == rebuild.digest(ROOT / name) for name in shared)
            or {'go', 'java'} <= selected,
            'Shared SQL writer inputs changed; include go,java in --languages or use the full command')


@contextmanager
def process_environment(environment):
    original = dict(os.environ)
    os.environ.clear()
    os.environ.update(environment)
    try:
        yield
    finally:
        os.environ.clear()
        os.environ.update(original)


def compose_command():
    return startup.DOCKER + ['compose', '--project-name', startup.PROJECT, '--env-file', str(STATE / '.env'),
                            '-f', str(startup.COMPOSE_FILES[0]), '-f', str(startup.COMPOSE_FILES[1])]


def compose_config(environment):
    return json.loads(startup.run(compose_command() + ['config', '--format', 'json'], env=environment))


def current_containers(config):
    identities = startup.run(startup.DOCKER + ['ps', '-aq', '--no-trunc', '--filter',
                                              'label=com.docker.compose.project=' + startup.PROJECT]).split()
    require(identities, 'Existing debug stack is missing; this command does not initialize a stack')
    rows = startup.inspect(identities)
    require(all(re.fullmatch(r'[0-9a-f]{64}', identity) for identity in identities)
            and len(rows) == len(set(identities)) == len(identities)
            and {row['Id'] for row in rows} == set(identities), 'Incomplete project container identity inventory')
    services, historical = {}, []
    for row in rows:
        labels = row['Config']['Labels']
        name = labels.get('com.docker.compose.service')
        require(labels.get('com.docker.compose.project') == startup.PROJECT and name in config['services'],
                'Unexpected container in the selected project')
        if labels.get('com.docker.compose.oneoff', '').lower() == 'true':
            require(name == 'db-migrate' and row['State']['Status'] == 'exited' and row['State']['ExitCode'] == 0,
                    'Unexpected historical container')
            historical.append(row)
        else:
            require(labels.get('com.docker.compose.container-number') == '1' and name not in services,
                    'Duplicate or unsupported service container: ' + name)
            services[name] = row
    require(set(services) == set(config['services']) and len(historical) <= 1,
            'Existing initialized service set is incomplete')
    return services, historical


def mounted_volumes(names):
    details = json.loads(startup.run(startup.DOCKER + ['volume', 'inspect', *sorted(names)]))
    by_name = {volume['Name']: volume for volume in details}
    require(set(by_name) == set(names) and len(details) == len(by_name), 'Incomplete infrastructure volume inventory')
    return by_name


def infrastructure_plan(config, services, base_ids):
    """Qualify only existing infrastructure against the resolved Compose configuration."""
    names = {mount['Name'] for name in startup.INFRA for mount in services[name]['Mounts'] if mount['Type'] == 'volume'}
    require(names, 'Infrastructure data volumes are missing')
    volumes = mounted_volumes(names)
    pending = []
    for name in startup.INFRA:
        row, service = services[name], config['services'][name]
        container, host, state = row['Config'], row['HostConfig'], row['State']
        labels = container['Labels']
        require(labels.get('com.docker.compose.oneoff', '').lower() == 'false'
                and labels.get('com.docker.compose.service') == name and row['Image'] == base_ids[service['image']]
                and host['RestartPolicy']['Name'] == 'no' and row['RestartCount'] == 0
                and not any(state.get(key) for key in ('OOMKilled', 'Restarting', 'Dead', 'Paused')),
                'Unsupported infrastructure identity or lifecycle: ' + name)
        image = json.loads(startup.run(startup.DOCKER + ['image', 'inspect', '--format', '{{json .Config}}', row['Image']]))
        for field, key in (('Cmd', 'command'), ('Entrypoint', 'entrypoint'), ('User', 'user')):
            expected = service.get(key)
            if expected is None:
                expected = image.get(field)
            if field == 'User' and expected is None:
                expected = ''
            require(container.get(field) == expected, 'Infrastructure process configuration differs: ' + name)
        health_test = [part.replace('$$', '$') for part in service['healthcheck']['test']]
        require(container.get('Healthcheck', {}).get('Test') == health_test, 'Infrastructure healthcheck differs: ' + name)
        actual_env = dict(entry.split('=', 1) for entry in container['Env'])
        require(all((key not in actual_env if value is None else actual_env.get(key) == str(value))
                    for key, value in service.get('environment', {}).items()), 'Infrastructure environment differs: ' + name)
        expected_ports = sorted((str(p['target']) + '/' + p.get('protocol', 'tcp'), p.get('host_ip', ''), str(p['published']))
                                for p in service.get('ports', []))
        actual_ports = sorted((target, p['HostIp'], p['HostPort'])
                              for target, bindings in (host.get('PortBindings') or {}).items() for p in bindings or [])
        require(actual_ports == expected_ports, 'Infrastructure published ports differ: ' + name)
        expected_binds = sorted((v['target'], startup.host_bind_path(v['source']), bool(v.get('read_only')))
                                for v in service['volumes'] if v['type'] == 'bind')
        actual_binds = sorted((m['Destination'], startup.host_bind_path(m['Source']), not m['RW'])
                              for m in row['Mounts'] if m['Type'] == 'bind')
        require(actual_binds == expected_binds and all(m['Type'] in ('bind', 'volume') for m in row['Mounts']),
                'Infrastructure bind mounts differ: ' + name)
        startup.validate_mounts(name, row, service, config['volumes'], image.get('Volumes') or {}, volumes)
        if state['Status'] == 'exited' and not state['Running']:
            require(state.get('StartedAt') and not state['StartedAt'].startswith('0001-'),
                    'Infrastructure container has never run: ' + name)
            pending.append(name)
        else:
            require(state['Running'] and state.get('Health', {}).get('Status') == 'healthy' and startup.ready(name, row, {}),
                    'Infrastructure is not healthy: ' + name)
    return pending, {name: startup.hashed(value) for name, value in volumes.items()}


def confirm_infrastructure_rows(originals):
    original_ids = {row['Id']: row for row in originals}
    observed = startup.inspect(list(original_ids))
    require(len(observed) == len(original_ids) and {row['Id'] for row in observed} == set(original_ids)
            and all(startup.signature(row) == startup.signature(original_ids[row['Id']])
                    and startup.state(row) == startup.state(original_ids[row['Id']]) for row in observed),
            'Infrastructure changed after inventory')


def preflight(languages, environment):
    from prepare_docker_debug import refresh_existing_options
    # 此接口只读 canonical/run 路径与原环境，不产生配置或随机密钥。
    refresh_existing_options()
    env_digest = startup.sha((STATE / '.env').read_bytes())
    config = compose_config(environment)
    require(config.get('name') == startup.PROJECT, 'Only the existing local debug project is supported')
    services, historical = current_containers(config)
    selected_apps = {name for language in languages for name in SERVICES[language]} - set(rebuild.ONE_SHOT)
    for name in startup.APPS:
        row = services[name]
        require(row['State']['Status'] in {'running', 'exited'} and not row['State'].get('Paused'),
                'Unsupported application state: ' + name)
        if name not in selected_apps:
            require(startup.ready(name, row, {'gatewayPorts': startup.ports(config, 'gateway', 8081)}),
                    'Omitted application is not ready: ' + name + '; include its language or start it first')
    for name in startup.ONESHOTS:
        row = services[name]
        require(row['State']['Status'] == 'exited' and row['State']['ExitCode'] == 0,
                'Initialization or writer registration did not complete: ' + name)
    canonical = startup.env_values((STATE / '.env').read_bytes())
    partial_refresh_guard(languages, canonical, config, services)
    expected = [name for language in languages for name in SERVICES[language]]
    bases = {name: config['services'][name]['image'] for name in expected}
    base_ids = {}
    sql_boot_reference = config['services']['sql-boot']['image']
    infra_references = {config['services'][name]['image'] for name in startup.INFRA}
    for reference in set(bases.values()) | {sql_boot_reference} | infra_references:
        identity = startup.run(startup.DOCKER + ['image', 'inspect', '--format', '{{.Id}}', reference]).strip()
        require(re.fullmatch(r'sha256:[0-9a-f]{64}', identity), 'A required local base image is unavailable')
        base_ids[reference] = identity
    pending, volumes = infrastructure_plan(config, services, base_ids)
    if 'cpp' in languages:
        from build_docker_cpp_debug import inputs, validate_sdk
        validate_sdk()
        if pending:
            inputs()
    if 'go' in languages:
        from build_docker_go_debug import GO_VERSION, CATALOGUE, build_environment, runtime_resource_plan, snapshot
        version = startup.run(['go', 'version'], env=build_environment(network=False)).strip()
        require(version.startswith('go version ' + GO_VERSION + ' '), 'Existing Go version does not match the builder')
        for module in sorted({CATALOGUE[name][0] for name in SERVICES['go']}):
            runtime_resource_plan(module)
            if pending:
                snapshot(module)
    if 'java' in languages:
        import build_docker_java_debug as java
        java.source_files()
        identity = base_ids[bases['gateway']]
        user = startup.run(startup.DOCKER + ['image', 'inspect', '--format', '{{.Config.User}}', identity]).strip()
        entrypoint = json.loads(startup.run(startup.DOCKER + ['image', 'inspect', '--format', '{{json .Config.Entrypoint}}', identity]))
        require(user == '10001:10001' and entrypoint == ['java', '-jar', '/app/service.jar'],
                'Java gateway base image contract differs from the builder')
    all_rows = list(services.values()) + historical
    mounts = {row['Id']: startup.signature(row) for row in all_rows}
    baseline = {'services': services, 'historical': historical, 'bases': bases, 'baseIds': base_ids,
                'pendingResume': pending, 'infraVolumes': volumes, 'sqlBootImageId': base_ids[sql_boot_reference],
                'signatures': mounts, 'envSha256': env_digest, 'states': {row['Id']: startup.state(row) for row in all_rows},
                'refreshInputs': refresh_sources(), 'bindHashes': startup.binding_hashes(all_rows),
                'projectVolumes': startup.project_volumes()}
    require(env_digest == startup.sha((STATE / '.env').read_bytes()), 'Canonical environment changed during preflight')
    confirm_infrastructure_rows([services[name] for name in startup.INFRA])
    return config, baseline


def builder_command(language, output, bases):
    command = [sys.executable, '-X', 'utf8', '-B', str(ROOT / f'tools/scripts/build_docker_{language}_debug.py'),
               '--output', str(output), '--base-images', str(bases)]
    if language != 'java':
        command += ['--services', ','.join(SERVICES[language]), '--jobs', '1']
    return command


def run_stage(command, output, environment, timeout):
    from build_docker_go_debug import stop_owned_process
    canonical = startup.env_values((STATE / '.env').read_bytes())
    private = {value.encode('utf-8') for key, value in {**canonical, **environment}.items()
               if value and re.search(r'PASSWORD|PASSWD|SECRET|SIGNING_KEY|JDBC_URL|DSN|SQL_USERNAME|ADMIN_API_KEY', key)}
    private = sorted(private, key=len, reverse=True)
    with output.open('xb') as log:
        process = subprocess.Popen(command, cwd=ROOT, env=environment, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        capture_errors = []
        def capture():
            try:
                with process.stdout:
                    for line in iter(process.stdout.readline, b''):
                        for value in private:
                            line = line.replace(value, b'[REDACTED]')
                        log.write(line)
                        log.flush()
            except BaseException:
                # Never expose exception text: stream errors may contain private output.
                capture_errors.append(True)
        reader = threading.Thread(target=capture, daemon=True)
        reader.start()
        try:
            return process.wait(timeout=timeout)
        except BaseException:
            stop_owned_process(process)
            raise
        finally:
            reader.join(timeout=30)
            require(not reader.is_alive(), 'Stage log capture did not finish')
            require(not capture_errors, 'Stage log capture failed; raw details suppressed')


def validate_build(language, directory):
    value = json.loads((directory / 'result.json').read_text('utf-8-sig'))
    expected = set(SERVICES[language])
    require(value.get('success') is True and set(value.get('images', {})) == expected
            and set(value.get('manifestPaths', {})) == expected, 'Incomplete build result: ' + language)
    return value


def receipt_file(old, relative):
    path = old / relative
    require(path.resolve() == path and path.is_file(), 'Missing or redirected resume evidence: ' + str(relative))
    return path


def read_receipt(old, relative):
    return json.loads(receipt_file(old, relative).read_text('utf-8-sig'))


def recipe_paths(language):
    return ('deploy/debug/Dockerfile.' + language, 'tools/scripts/build_docker_' + language + '_debug.py')


def check_build_recipe(old, commit, language):
    recorded = read_receipt(old, 'preflight.json')
    if 'buildRecipeHashes' in recorded:
        expected = {name: rebuild.digest(ROOT / name) for name in recipe_paths(language)}
        require(recorded['buildRecipeHashes'].get(language) == expected, 'Build recipe changed: ' + language)
        return
    # Legacy receipts recorded the build commit. Check the exact normalized Git blobs.
    require(re.fullmatch(r'[0-9a-f]{12,40}', commit), 'Invalid build commit in old receipt')
    for name in recipe_paths(language):
        original = startup.run(['git', 'rev-parse', '--verify', commit + ':' + name]).strip()
        current = startup.run(['git', 'hash-object', '--path=' + name, '--', name]).strip()
        require(original == current, 'Build recipe changed: ' + name)


def cpp_resume_contract(old, built, baseline):
    path = receipt_file(old, 'cpp/source_meta/source-manifest.json')
    manifest = json.loads(path.read_text('utf-8-sig'))
    require(all(Path(p) == path for p in built['manifestPaths'].values()), 'Unexpected C++ manifest path')
    details = manifest['build']
    source_sha = startup.sha(json.dumps(manifest['files'], sort_keys=True).encode())
    require(manifest.get('sourceRoot') == '/src' and details['type'] == 'Debug' and details['sourceSha256'] == source_sha
            and re.fullmatch(r'[0-9a-f]{12}-dirty\.source-' + source_sha[:12], details['commit']),
            'C++ Debug source contract mismatch')
    check_build_recipe(old, details['commit'][:12], 'cpp')
    sdk = read_receipt(STATE, 'cpp-toolchain.json')['imageId']
    stamp = details['createdUtc']
    require(re.fullmatch(r'\d{8}T\d{6}\.\d{6}Z', stamp) and details['sdkImageId'] == sdk,
            'C++ SDK or build stamp mismatch')
    base = baseline['baseIds'][baseline['bases']['scene']]
    identity = receipt_file(old, 'cpp/image.iid').read_text('utf-8').strip()
    ref = 'local/xuanming-node:debug-' + stamp.lower()
    require(all(image == {'image': ref, 'imageId': identity} for image in built['images'].values()),
            'C++ image batch mismatch')
    log = receipt_file(old, 'cpp/build.log').read_text('utf-8', errors='replace')
    completion = json.dumps({'files': len(manifest['files']), 'artifacts': 6, 'buildType': 'Debug', 'complete': True})
    require(completion in log and 'local/xuanming-independent-sdk:pin-' + sdk[7:] + '@' + sdk in log
            and 'local/xuanming-cpp-debug-system:pin-' + base[7:] + '@' + base in log
            and 'jobs=1' in receipt_file(old, 'cpp.log').read_text('utf-8', errors='replace'),
            'C++ build completion, pinned images or serial build evidence missing')


def go_resume_contract(old, built, baseline):
    from build_docker_go_debug import CATALOGUE, DELVE_VERSION, GO_VERSION, runtime_resource_plan
    recorded = read_receipt(old, 'go/preflight.json')
    recipe = rebuild.digest(ROOT / 'deploy/debug/Dockerfile.go')
    require(recorded.get('jobs') == 1 and set(recorded['services']) == set(SERVICES['go'])
            and Path(recorded['sourceRoot']) == ROOT and recorded['goVersion'].startswith('go version ' + GO_VERSION + ' ')
            and recorded['dockerfileSHA256'] == recipe, 'Go build tool or recipe mismatch')
    check_build_recipe(old, recorded['commit'], 'go')
    delve = read_receipt(old, 'go/delve-receipt.json')
    require(delve['version'] == DELVE_VERSION and delve['goVersion'] == GO_VERSION
            and delve['goVersionCheckDisabled'] is False, 'Go debugger contract mismatch')
    for service, image in built['images'].items():
        path = receipt_file(old, Path('go') / service / 'source_meta/source-manifest.json')
        require(Path(built['manifestPaths'][service]) == path, 'Unexpected Go manifest path: ' + service)
        manifest = json.loads(path.read_text('utf-8-sig'))
        module, entry = CATALOGUE[service]
        files = manifest['files']
        source_sha = startup.hashed(files)
        build = manifest['build']
        flags = {'service': service, 'module': module, 'entry': entry, 'goVersion': GO_VERSION,
                 'gcflags': 'all=-N -l', 'trimpath': False, 'stripSymbols': False, 'goos': 'linux',
                 'goarch': 'amd64', 'cgoEnabled': False, 'delveVersion': DELVE_VERSION,
                 'goVersionCheckDisabled': False, 'runtimeAssetsFrozen': True, 'tablesLowercase': True,
                 'dockerfileSHA256': recipe, 'sourceDigest': source_sha,
                 'commit': recorded['commit'][:12] + '-dirty+' + source_sha[:12]}
        require(all(build.get(k) == v for k, v in flags.items()), 'Go debug build flags or source digest mismatch: ' + service)
        resources = {dest: source.relative_to(ROOT).as_posix() for dest, source in runtime_resource_plan(module).items()}
        artifacts = manifest['artifacts']
        require(Path(manifest['sourceRoot']) == ROOT and manifest['runtimeResources'] == resources
                and set(artifacts) == set(resources) | {'/app/service', '/usr/local/bin/dlv'}
                and all(artifacts[dest] == files.get(source) for dest, source in resources.items())
                and re.fullmatch(r'[0-9a-f]{64}', artifacts['/app/service']) and artifacts['/usr/local/bin/dlv'] == delve['sha256'],
                'Go frozen runtime resource contract mismatch: ' + service)
        receipt = read_receipt(old, Path('go') / service / 'image-receipt.json')
        base = baseline['bases'][service]
        require(image['image'] == 'local/mmorpg-' + service + ':' + build['version']
                and receipt['image'] == image['image'] and receipt['imageId'] == image['imageId']
                and receipt['manifestSHA256'] == rebuild.digest(path) and receipt['runtimeResourceFiles'] == len(resources)
                and receipt['baseImage'] == base
                and receipt['baseImageId'] == baseline['baseIds'][base], 'Go image receipt mismatch: ' + service)


def resumed_builds(requested, languages, root, baseline):
    verification = root.parent
    old = Path(requested)
    if not old.is_absolute():
        old = (verification if len(old.parts) == 1 else ROOT) / old
    require(old.resolve() == old and old.parent == verification and old != root
            and re.fullmatch(r'docker-rebuild-all-\d{8}T\d{6}\.\d{6}Z', old.name),
            'Resume requires a previous unified run under build/verification')
    result = read_receipt(old, 'result.json')
    require(all(result.get(key) is False for key in ('applied', 'configurationWriteStarted', 'containerUpdateStarted')),
            'Old run already began applying configuration or updating containers')
    old_preflight = read_receipt(old, 'preflight.json')
    require(old_preflight.get('passed') is True, 'Old run has no passed preflight')
    stages = result.get('stages', [])
    stage_names = [stage['language'] for stage in stages]
    selected = old_preflight['languages']
    require(list(select_languages(','.join(selected))) == selected and stage_names == selected[:len(stages)],
            'Invalid old stage order')
    builds = {}
    for stage, language in zip(stages, languages):
        if language == 'java' or stage['language'] != language or type(stage.get('exitCode')) is not int or stage['exitCode'] != 0:
            break
        require(Path(stage['output']) == old / language and Path(stage['log']) == receipt_file(old, language + '.log')
                and not stage.get('reused_from') and language in old_preflight['languages'], 'Unexpected old stage origin')
        bases = {service: baseline['bases'][service] for service in SERVICES[language]}
        require(read_receipt(old, language + '-base-images.json') == bases and all(
            old_preflight['baseIds'].get(ref) == baseline['baseIds'][ref] for ref in bases.values()),
            'Old base references or image IDs differ: ' + language)
        receipt_file(old, Path(language) / 'result.json')
        built = validate_build(language, old / language)
        if language == 'cpp':
            cpp_resume_contract(old, built, baseline)
        else:
            go_resume_contract(old, built, baseline)
        built['reused_from'] = str(old)
        built['manifestDigests'] = {p: rebuild.digest(Path(p)) for p in set(built['manifestPaths'].values())}
        builds[language] = built
    require(builds, 'No successful C++ or Go prefix to resume')
    verify_sources(builds)
    return builds


def verify_sources(builds):
    scanned = {}
    inspected = set()
    for language, built in builds.items():
        for service, path in built['manifestPaths'].items():
            raw = Path(path).read_bytes()
            manifest = json.loads(raw)
            reused = 'reused_from' in built
            if reused:
                require(startup.sha(raw) == built['manifestDigests'][path], 'Reused manifest changed: ' + service)
                image = built['images'][service]
                ref, identity = image['image'], image['imageId']
                require(re.fullmatch(r'sha256:[0-9a-f]{64}', identity), 'Invalid reused image ID: ' + service)
                if ref not in inspected:
                    actual = startup.run(startup.DOCKER + ['image', 'inspect', '--format', '{{.Id}}', ref]).strip()
                    require(actual == identity, 'Reused image ID changed: ' + service)
                    details = manifest['build']
                    if language == 'cpp':
                        require(details['sdkImageId'] == read_receipt(STATE, 'cpp-toolchain.json')['imageId'],
                                'Reused C++ SDK changed')
                        env = json.loads(startup.run(startup.DOCKER + ['image', 'inspect', '--format', '{{json .Config.Env}}', identity]))
                        require('MMORPG_BUILD_COMMIT=' + details['commit'] in env
                                and 'MMORPG_BUILD_VERSION=debug-' + details['createdUtc'] in env,
                                'C++ image build metadata mismatch')
                    else:
                        labels = json.loads(startup.run(startup.DOCKER + ['image', 'inspect', '--format', '{{json .Config.Labels}}', identity]))
                        require(all(labels.get('org.opencontainers.image.' + k) == details[v]
                                    for k, v in [('version', 'version'), ('revision', 'commit'), ('created', 'buildTime')]),
                                'Go image build metadata mismatch: ' + service)
                    inspected.add(ref)
            key = (language, 'nodes' if language == 'cpp' else service)
            if key not in scanned:
                scanned[key] = rebuild.current_sources(language, service)
            expected = manifest.get('files', {})
            require(manifest.get('language') == language and manifest.get('schema') == 1
                    and (expected == scanned[key] if reused else
                         rebuild.runtime_sources(language, expected) == rebuild.runtime_sources(language, scanned[key])),
                    'Source files changed after build: ' + service)


def unchanged_runtime(baseline, environment):
    services, historical = current_containers(compose_config(environment))
    rows = list(services.values()) + historical
    require({r['Id']: startup.signature(r) for r in rows} == baseline['signatures']
            and {r['Id']: startup.state(r) for r in rows} == baseline['states']
            and startup.project_volumes() == baseline['projectVolumes'], 'Runtime changed during builds or refresh')
    require(refresh_sources() == baseline['refreshInputs'], 'Configuration or build tools changed during this invocation')
    volumes = mounted_volumes(baseline['infraVolumes'])
    require({name: startup.hashed(value) for name, value in volumes.items()} == baseline['infraVolumes'],
            'Infrastructure volume identity changed')
    return rows


def unchanged_before_apply(config, baseline, environment):
    rows = unchanged_runtime(baseline, environment)
    require(startup.binding_hashes(rows) == baseline['bindHashes']
            and startup.sha((STATE / '.env').read_bytes()) == baseline['envSha256'],
            'Runtime configuration changed during builds')
    require(compose_config(environment) == config, 'Resolved Compose configuration changed during builds')
    for reference, expected in baseline['baseIds'].items():
        require(startup.run(startup.DOCKER + ['image', 'inspect', '--format', '{{.Id}}', reference]).strip() == expected,
                'A base image reference changed during builds')


def wait_infrastructure(name, original, signature):
    identity = original['Id']
    deadline = time.monotonic() + 360
    while (remaining := deadline - time.monotonic()) > 0:
        rows = json.loads(startup.run(startup.DOCKER + ['inspect', identity], timeout=min(30, remaining)))
        require(len(rows) == 1 and rows[0]['Id'] == identity and startup.signature(rows[0]) == signature,
                'Infrastructure container changed after start: ' + name)
        row = rows[0]
        state = row['State']
        require(state['Running'] and state['Status'] == 'running' and row['RestartCount'] == 0
                and not any(state.get(key) for key in ('OOMKilled', 'Restarting', 'Dead', 'Paused'))
                and state.get('StartedAt') and state['StartedAt'] != original['State']['StartedAt']
                and state.get('Health', {}).get('Status') in ('starting', 'healthy'),
                'Infrastructure failed after start: ' + name)
        if state['Health']['Status'] == 'healthy' and time.monotonic() <= deadline:
            return row
        time.sleep(max(0, min(1, deadline - time.monotonic())))
    raise RuntimeError('Infrastructure readiness timed out: ' + name)


def resume_infrastructure(config, baseline, environment, root, result):
    pending = baseline['pendingResume']
    if not pending:
        return
    require(pending == [name for name in startup.INFRA if name in pending], 'Invalid infrastructure recovery plan')
    result['phase'] = 'resume-infrastructure'
    events = result['infraRecovery'] = []
    unchanged_before_apply(config, baseline, environment)
    for index, name in enumerate(pending):
        original = baseline['services'][name]
        identity = original['Id']
        require(re.fullmatch(r'[0-9a-f]{64}', identity) and original['State']['Status'] == 'exited'
                and not original['State']['Running'], 'Invalid infrastructure start target: ' + name)
        confirm_infrastructure_rows([original])
        event = {'service': name, 'containerId': identity, 'status': 'start-intent'}
        write_json(root / ('infra-' + name + '-start.json'), event)
        events.append(event)
        event['status'] = 'start-outcome-unconfirmed'
        startup.run(startup.DOCKER + ['start', identity], timeout=45)
        event['status'] = 'readiness-unconfirmed'
        row = wait_infrastructure(name, original, baseline['signatures'][identity])
        baseline['states'][identity] = startup.state(row)
        baseline['services'][name] = row
        event['status'] = 'healthy'
        event['startedAt'] = row['State']['StartedAt']
        write_json(root / ('infra-' + name + '-healthy.json'), event)
        result['pendingResume'] = pending[index + 1:]
        unchanged_before_apply(config, baseline, environment)
    result['infraReady'] = True


def refreshed_contract(original, config, baseline, environment):
    from prepare_docker_debug import ADDITIVE_SECRET_NAMES, DEV_INTERNAL_IDENTITY, SERVICE_IP_OFFSETS
    actual = startup.env_values((STATE / '.env').read_bytes())
    derived = {'KAFKA_COMMAND_TOPIC_GENERATION', 'KAFKA_COMMAND_TOPIC_PARTITIONS'}
    require(all(actual.get(key) == value for key, value in original.items()
                if key not in derived | {'INTERNAL_AUTH_SECRET'}),
            'Configuration refresh changed a preserved canonical setting')
    allowed_additions = set(ADDITIVE_SECRET_NAMES) | set(DEV_INTERNAL_IDENTITY) | derived
    allowed_additions.update(name.upper() + '_IP' for name in SERVICE_IP_OFFSETS)
    require(set(actual) - set(original) <= allowed_additions, 'Unexpected canonical setting after refresh')
    current = compose_config(child_environment(actual, environment))
    require(current.get('volumes') == config.get('volumes') and current.get('networks') == config.get('networks'),
            'Refresh changed the existing volume or network contract')
    for name, service in config['services'].items():
        require(all(current['services'][name].get(key) == service.get(key) for key in ('volumes', 'ports')),
                'Refresh changed existing mounts or ports: ' + name)
    rows = list(baseline['services'].values()) + baseline['historical']
    return actual, startup.binding_hashes(rows)


def unchanged_refreshed(expected_environment, expected_binds, baseline):
    require(startup.env_values((STATE / '.env').read_bytes()) == expected_environment,
            'Canonical environment changed after refresh')
    rows = list(baseline['services'].values()) + baseline['historical']
    require(startup.binding_hashes(rows) == expected_binds, 'Runtime files changed after refresh')


def java_test_environment(environment):
    """Integration tests receive the NORMAL writer only, never inherited management credentials."""
    management = {'MMORPG_SQL_SCHEMA_DSN', 'MMORPG_SQL_REGISTRAR_DSN', 'MMORPG_SQL_MAINTENANCE_DSN'}
    return {key: value for key, value in environment.items()
            if not key.startswith('COMMERCE_IT_') and key not in management}


def fixture_operation(result, phase, operation, *args):
    result['phase'] = 'java-fixture-' + phase
    result['javaFixture']['status'] = phase
    try:
        return operation(*args)
    except BaseException:
        # Persist only the public phase; private producer context stays in its own directory.
        result['javaFixture']['status'] = phase + '-failed'
        raise


def pipeline(languages, root, inherited, result, check_only=False, resume_from=None):
    problems = local_requirements(languages, inherited)
    require(not problems, '\n'.join(problems) + '\nNo build, configuration refresh or container update was started; '
            'Java uses a real NORMAL TiDB fixture (no test skipping or old JAR fallback).')
    canonical = startup.env_values((STATE / '.env').read_bytes())
    require(canonical.get('COMPOSE_PROJECT_NAME') == startup.PROJECT, 'Unexpected canonical project')
    environment = child_environment(canonical, inherited)
    with process_environment(environment):
        fixture_state = None
        if 'java' in languages:
            result['javaFixture'] = {'mode': java_fixture.mode(environment), 'status': 'pending'}
            fixture_operation(result, 'preflight', java_fixture.preflight, environment)
            result['javaFixture']['status'] = 'preflight-complete'
        config, baseline = preflight(languages, environment)
        if resume_from:
            result['phase'] = 'verify-reused-builds'
        builds = resumed_builds(resume_from, languages, root, baseline) if resume_from else {}
        if builds:
            result['reused_from'] = {lang: built['reused_from'] for lang, built in builds.items()}
        pending = baseline['pendingResume']
        result.update(infraReady=not pending, pendingResume=pending)
        write_json(root / 'preflight.json', {'passed': True, 'languages': languages, 'services': SERVICES,
                   'envSha256': baseline['envSha256'], 'baseIds': baseline['baseIds'],
                   'infraReady': not pending, 'pendingResume': pending,
                   'configurationRefreshAfterBuilds': True, 'reused_from': result.get('reused_from', {}),
                   'buildRecipeHashes': {
                       lang: {name: baseline['refreshInputs'][name] for name in recipe_paths(lang)}
                       for lang in languages if lang in ('cpp', 'go')},
                   'javaFixture': ({'mode': result['javaFixture']['mode'], 'readOnlyChecksPassed': True,
                                    'created': False} if 'java' in languages else None)})
        if check_only:
            print('infraReady=' + str(not pending).lower() + ' pendingResume=' + json.dumps(pending), flush=True)
            result.update(success=True, phase='preflight-complete', applied=False)
            return
        resume_infrastructure(config, baseline, environment, root, result)
        for language in languages:
            if language in builds:
                old = builds[language]['reused_from']
                result['stages'].append({'language': language, 'exitCode': 0, 'reused_from': old,
                                         'output': str(Path(old) / language), 'log': str(Path(old) / (language + '.log'))})
                print('Reusing verified ' + language + ' build from ' + old, flush=True)
                continue
            stage_environment = environment
            if language == 'java':
                stage_environment, fixture_state = fixture_operation(
                    result, 'create', java_fixture.create, root / 'java-fixture', environment)
                stage_environment = java_test_environment(stage_environment)
                result['javaFixture']['status'] = 'ready'
            result['phase'] = 'build-' + language
            base_file = root / (language + '-base-images.json')
            write_json(base_file, {service: baseline['bases'][service] for service in SERVICES[language]})
            directory = root / language
            log = root / (language + '.log')
            print('Building ' + language + '; log: ' + str(log), flush=True)
            try:
                code = run_stage(builder_command(language, directory, base_file), log, stage_environment, 21600)
                result['stages'].append({'language': language, 'exitCode': code, 'log': str(log), 'output': str(directory)})
                require(code == 0, 'Build failed: ' + language + '; no configuration or container update was applied')
                builds[language] = validate_build(language, directory)
            except BaseException:
                if language == 'java':
                    result['javaFixture']['status'] = 'build-failed-preserved'
                raise
            if language == 'java':
                result['javaFixture']['status'] = 'built-awaiting-verification'
        result['phase'] = 'verify-built-sources'
        verify_sources(builds)
        unchanged_before_apply(config, baseline, environment)
        if fixture_state is not None:
            if result['javaFixture']['mode'] == 'owned':
                fixture_state['javaBuildSucceeded'] = True
                fixture_operation(result, 'close', java_fixture.close, fixture_state, environment)
                result['javaFixture']['status'] = 'closed'
            else:
                result['javaFixture']['status'] = 'external-preserved'
        result['phase'] = 'refresh-configuration'
        result['configurationWriteStarted'] = True
        code = run_stage([sys.executable, '-X', 'utf8', '-B', str(ROOT / 'tools/scripts/prepare_docker_debug.py'),
                          '--refresh-existing'], root / 'refresh.log', environment, 300)
        require(code == 0, 'Configuration refresh failed; inspect refresh.log for rollback status; no container update started')
        unchanged_runtime(baseline, environment)
        verify_sources(builds)
        refreshed, refreshed_binds = refreshed_contract(canonical, config, baseline, environment)
        unchanged_refreshed(refreshed, refreshed_binds, baseline)
        result['phase'] = 'update-images'
        images = {service: image for built in builds.values() for service, image in built['images'].items()}
        rebuild.update_images(images)
        canonical = dict(refreshed)
        for service, image in images.items():
            canonical[rebuild.IMAGE_KEYS[service]] = image['image']
        unchanged_refreshed(canonical, refreshed_binds, baseline)
        environment = child_environment(canonical, inherited)
        with process_environment(environment):
            unchanged_runtime(baseline, environment)
            verify_sources(builds)
            running = [service for service in images if service not in rebuild.ONE_SHOT]
            rebuild.check_config_inputs(running)
            unchanged_refreshed(canonical, refreshed_binds, baseline)
            targets = [name for service in running for name in
                       ((service + '-sql-boot', service) if service in rebuild.SQL_WRITERS else (service,))]
            result['phase'] = 'recreate-services'
            result['containerUpdateStarted'] = True
            code = run_stage(compose_command() + ['up', '-d', '--no-build', '--pull', 'never', '--no-deps',
                             '--force-recreate', '--wait', '--wait-timeout', '300', *targets],
                             root / 'recreate.log', environment, 600)
            require(code == 0, 'Container update failed; inspect recreate.log; completed changes remain applied')
            result['phase'] = 'verify-runtime'
            final_config = compose_config(environment)
            final, historical = current_containers(final_config)
            require(startup.project_volumes() == baseline['projectVolumes'], 'Project volume set changed')
            for service, row in final.items():
                original = baseline['services'][service]
                if service not in targets:
                    require(startup.signature(row) == startup.signature(original)
                            and startup.state(row) == startup.state(original), 'Untargeted container changed: ' + service)
                require(row['Mounts'] == original['Mounts'], 'Existing volume or bind mounts changed: ' + service)
            require({row['Id']: startup.signature(row) for row in historical} ==
                    {row['Id']: startup.signature(row) for row in baseline['historical']}, 'Historical migration changed')
            readiness = {'gatewayPorts': startup.ports(final_config, 'gateway', 8081)}
            sql_boot_id = images.get('sql-boot', {}).get('imageId', baseline['sqlBootImageId'])
            for name in targets:
                expected_id = sql_boot_id if name.endswith('-sql-boot') else images[name]['imageId']
                require(final[name].get('Image') == expected_id,
                        'Container does not use the expected image from this invocation: ' + name)
                if name.endswith('-sql-boot'):
                    require(final[name]['State']['Status'] == 'exited' and final[name]['State']['ExitCode'] == 0,
                            'Writer registration failed: ' + name)
            require(all(startup.ready(service, final[service], readiness) for service in startup.INFRA + startup.APPS),
                    'A service did not pass final runtime readiness')
            result['phase'] = 'verify-sources'
            with (root / 'source-check.log').open('x', encoding='utf-8') as log, redirect_stdout(log):
                require(rebuild.check(running, root / 'source-check.json'), 'Final source check failed; inspect source-check.json')
            result.update(success=True, phase='complete', applied=True, services=running)


def main():
    parser = argparse.ArgumentParser(description='Build all debug servers serially, then apply them together.')
    parser.add_argument('--check', action='store_true', help='Read-only preflight; do not build, refresh or update containers.')
    parser.add_argument('--languages', default=','.join(ORDER), help='Selected languages; default cpp,go,java. Always built in that order.')
    parser.add_argument('--resume-builds-from', type=Path, metavar='RUN',
                        help='Verify and reuse the successful C++/Go prefix of this unapplied unified run.')
    args = parser.parse_args()
    root = ROOT / 'build/verification' / ('docker-rebuild-all-' + datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ'))
    root.mkdir(parents=True, exist_ok=False)
    print('Evidence directory: ' + str(root), flush=True)
    result = {'success': False, 'phase': 'preflight', 'applied': False, 'stages': [],
              'configurationWriteStarted': False, 'containerUpdateStarted': False}
    try:
        languages = select_languages(args.languages)
        # 与日常启动互斥；锁只影响本项目，不停止其他进程或服务。
        with nullcontext() if args.check else startup.startup_lock():
            pipeline(languages, root, dict(os.environ), result, args.check, args.resume_builds_from)
    except (Exception, KeyboardInterrupt) as error:
        result.update(errorType=type(error).__name__,
                      error=str(error) if isinstance(error, RuntimeError) else 'Operation failed; raw details suppressed')
        print('Stopped: ' + result['error'], file=sys.stderr)
    finally:
        write_json(root / 'result.json', result)
    if result['success']:
        print('Preflight completed. No build or update performed.' if args.check else 'All selected debug servers rebuilt and updated.')
    return 0 if result['success'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
