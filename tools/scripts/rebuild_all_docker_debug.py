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

import rebuild_docker_debug as rebuild
import start_docker_debug as startup
import docker_java_fixture as java_fixture

ROOT = Path(__file__).resolve().parents[2]
STATE = ROOT / 'build/docker-debug-current'
ORDER = ('cpp', 'go', 'java')
GO_SERVICES = ('login', 'db', 'data-service', 'scene-manager', 'player-locator',
               'client-rpc-router', 'match', 'player-inbox', 'sql-boot')
SERVICES = {'cpp': rebuild.CPP, 'go': GO_SERVICES, 'java': ('gateway',)}


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
              'deploy/debug/compose.game.yml', 'deploy/debug/compose.debug.yml']
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


def preflight(languages, environment):
    from prepare_docker_debug import refresh_existing_options
    # 此接口只读 canonical/run 路径与原环境，不产生配置或随机密钥。
    refresh_existing_options()
    config = compose_config(environment)
    require(config.get('name') == startup.PROJECT, 'Only the existing local debug project is supported')
    services, historical = current_containers(config)
    for name in startup.INFRA:
        row = services[name]
        require(row['State']['Running'] and not row['State'].get('OOMKilled')
                and ('Health' not in row['State'] or row['State']['Health']['Status'] == 'healthy'),
                'Service is not ready: ' + name + '; run start_docker_debug.py --execute first')
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
    for reference in set(bases.values()) | {sql_boot_reference}:
        identity = startup.run(startup.DOCKER + ['image', 'inspect', '--format', '{{.Id}}', reference]).strip()
        require(re.fullmatch(r'sha256:[0-9a-f]{64}', identity), 'A required local base image is unavailable')
        base_ids[reference] = identity
    if 'cpp' in languages:
        from build_docker_cpp_debug import validate_sdk
        validate_sdk()
    if 'go' in languages:
        from build_docker_go_debug import GO_VERSION, CATALOGUE, build_environment, runtime_resource_plan
        version = startup.run(['go', 'version'], env=build_environment(network=False)).strip()
        require(version.startswith('go version ' + GO_VERSION + ' '), 'Existing Go version does not match the builder')
        for name in SERVICES['go']:
            runtime_resource_plan(CATALOGUE[name][0])
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
    return config, {'services': services, 'historical': historical, 'bases': bases, 'baseIds': base_ids,
                    'sqlBootImageId': base_ids[sql_boot_reference],
                    'signatures': mounts, 'envSha256': startup.sha((STATE / '.env').read_bytes()),
                    'states': {row['Id']: startup.state(row) for row in all_rows},
                    'refreshInputs': refresh_sources(),
                    'bindHashes': startup.binding_hashes(all_rows), 'projectVolumes': startup.project_volumes()}


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


def verify_sources(builds):
    scanned = {}
    for language, built in builds.items():
        for service, path in built['manifestPaths'].items():
            manifest = json.loads(Path(path).read_text('utf-8-sig'))
            key = (language, 'nodes' if language == 'cpp' else service)
            if key not in scanned:
                scanned[key] = rebuild.runtime_sources(language, rebuild.current_sources(language, service))
            require(manifest.get('language') == language and manifest.get('schema') == 1
                    and rebuild.runtime_sources(language, manifest.get('files', {})) == scanned[key],
                    'Source files changed after build: ' + service)


def unchanged_runtime(baseline, environment):
    services, historical = current_containers(compose_config(environment))
    rows = list(services.values()) + historical
    require({r['Id']: startup.signature(r) for r in rows} == baseline['signatures']
            and {r['Id']: startup.state(r) for r in rows} == baseline['states']
            and startup.project_volumes() == baseline['projectVolumes'], 'Runtime changed during builds or refresh')
    require(refresh_sources() == baseline['refreshInputs'], 'Configuration or build tools changed during this invocation')
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


def pipeline(languages, root, inherited, result, check_only=False):
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
        write_json(root / 'preflight.json', {'passed': True, 'languages': languages, 'services': SERVICES,
                   'envSha256': baseline['envSha256'], 'baseIds': baseline['baseIds'],
                   'configurationRefreshAfterBuilds': True,
                   'javaFixture': ({'mode': result['javaFixture']['mode'], 'readOnlyChecksPassed': True,
                                    'created': False} if 'java' in languages else None)})
        if check_only:
            result.update(success=True, phase='preflight-complete', applied=False)
            return
        builds = {}
        for language in languages:
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
            pipeline(languages, root, dict(os.environ), result, args.check)
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
