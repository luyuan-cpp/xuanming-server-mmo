"""封存、审阅并在显式执行时仅替换 Scene；准备阶段不构建、不重启、不写玩家资产。"""
from __future__ import annotations
import argparse
import base64
import csv
from datetime import datetime, timezone
import difflib
import json
import os
from pathlib import Path
import secrets
import socket
import sys
import time

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'tools/scripts'))
import start_docker_debug as startup
import build_docker_cpp_debug as builder
import rebuild_docker_debug as source_check
import gm_scene_promotion as promotion

OUT = ROOT / promotion.RELATIVE
PRIVATE = OUT / 'private'
MAIL = ROOT / 'build/verification/gm-admin-20261117/mail'
AUTHORED = ROOT / 'build/verification/authored-festival-pet-acquisition-20261116'
FILES = [*startup.COMPOSE_FILES, AUTHORED / 'compose.authored.json', startup.recovery_path()]
SECRET_KEYS = ('MMORPG_GM_PLAYER_COMMAND_SECRET', 'MMORPG_ASSET_OP_SECRET_MAIL')
GM_REQUIRED_SOURCES = (
    'cpp/nodes/scene/handler/grpc/gm_player_command_security.h',
    'proto/scene/scene_admin.proto',
)


def active_authored(receipt):
    overlays = [Path(key) for key in receipt['contract']['composeSha256'] if Path(key).name == 'compose.authored.json']
    startup.require(len(overlays) == 1, 'Expected exactly one active authored Compose overlay')
    overlay = overlays[0]
    startup.require(overlay.is_file() and overlay.resolve() == overlay, 'Active authored overlay is missing or redirected')
    if overlay != startup.authored_origin_overlay():
        startup.require(startup.authored_origin_overlay() in startup.authored_manifest_chain(overlay),
                        'Active authored overlay lacks approved ancestry')
    return overlay.parent


def compose_files(authored_dir):
    return [*startup.COMPOSE_FILES, authored_dir / 'compose.authored.json', startup.recovery_path()]


def is_rebuilt_scene_pin(recovery, build_path, image_id, current_scene_image):
    evidence = recovery.get('x-xuanming-rebuild')
    if not isinstance(evidence, dict):
        return False
    update = evidence.get('updates', {}).get('scene', {})
    build_sha = startup.sha(build_path.read_bytes())
    return (update.get('build') == build_sha and build_sha in evidence.get('builds', {})
            and recovery.get('services', {}).get('scene', {}).get('image') == image_id == current_scene_image)


def verify_container_artifacts(row, complete, runtime_data):
    raw = startup.run(startup.DOCKER + ['exec', row['Id'], 'cat', '/app/DEBUG_SOURCE.json'], timeout=60)
    startup.require(json.loads(raw) == complete, 'Running Scene debug manifest differs from the verified image')
    artifacts = source_check.runtime_table_artifacts('scene', row['Id'], complete['artifacts'], runtime_data)
    hashes = startup.run(startup.DOCKER + ['exec', row['Id'], 'sha256sum', '--', *artifacts], timeout=60).splitlines()
    actual = {line[66:]: line[:64] for line in hashes if len(line) > 66}
    startup.require(actual == artifacts, 'Running Scene binary, symbol or mounted table digest differs')
    save(OUT / 'source-check.json', {'success': True, 'services': [{
        'service': 'scene', 'success': True, 'imageId': row['Image'], 'sourceCount': len(complete['files']),
        'differences': [], 'artifactVerificationOnly': True, 'workspaceSourcesChecked': False,
        'runtimeDataManifestSha256': runtime_data['manifestSha256'],
        'officialTableManifestSha256': runtime_data['officialTableManifestSha256']}]})


def save(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')


def bound(path):
    return {'path': str(path), 'sha256': startup.sha(path.read_bytes())}


def protect(path):
    if os.name == 'nt':
        sid = next(csv.reader([startup.run(['whoami', '/user', '/fo', 'csv', '/nh']).strip()]))[-1]
        startup.run(['icacls', str(path), '/inheritance:r', '/grant:r', f'*{sid}:(F)', '*S-1-5-18:(F)'])
    else:
        path.chmod(0o700 if path.is_dir() else 0o600)


def private_write(path, raw):
    path.touch(exist_ok=True)
    protect(path)
    path.write_bytes(raw)


def free_port():
    with socket.socket() as probe:
        probe.bind(('127.0.0.1', 2355))


def read_mail():
    row = startup.inspect(['xuanming-gm-mail-mail-1'])[0]
    startup.require(row['State'].get('Health', {}).get('Status') == 'healthy', 'Mail is not healthy')
    values = dict(value.split('=', 1) for value in row['Config']['Env'])
    local = startup.env_values((MAIL / 'private/scene.env').read_bytes())
    startup.require(local.get(SECRET_KEYS[1]) == values.get(SECRET_KEYS[1]) and len(local[SECRET_KEYS[1]]) >= 32,
                    'Running mail asset secret differs from the private Scene bridge')
    admin = startup.env_values((MAIL / 'private/admin.env').read_bytes())
    startup.require(admin.get('MAIL_PRODUCER_SECRET_ADMIN') == values.get('MAIL_PRODUCER_SECRET_ADMIN'),
                    'Running mail admin secret differs from the private GM bridge')
    return row, local, admin


def command(files=None):
    result = startup.DOCKER + ['compose', '--project-name', startup.PROJECT, '--env-file', str(startup.ENV)]
    for path in (FILES if files is None else files):
        result += ['-f', str(path)]
    return result + ['up', '-d', '--no-deps', '--no-build', '--pull', 'never', '--force-recreate', 'scene']


def bridge():
    _, local, admin = read_mail()
    scene = startup.env_values((PRIVATE / 'scene.env').read_bytes())
    runtime = startup.env_values((PRIVATE / 'gm-runtime.env').read_bytes())
    startup.require(set(scene) == set(SECRET_KEYS) and len(scene[SECRET_KEYS[0]]) >= 32
                    and scene[SECRET_KEYS[1]] == local[SECRET_KEYS[1]], 'Private Scene bridge changed')
    startup.require(runtime == {'GM_SCENE_ADMIN_SECRET': scene[SECRET_KEYS[0]],
        'GM_SCENE_TARGETS_JSON': '{"1":"127.0.0.1:2355"}', 'GM_MAIL_TARGET': '127.0.0.1:2354',
        'GM_MAIL_ADMIN_SECRET': admin['MAIL_PRODUCER_SECRET_ADMIN']}, 'Private GM runtime bridge changed')
    return scene


def environment_bytes(original, values):
    existing = startup.env_values(original)
    startup.require(all(key not in existing or existing[key] == value for key, value in values.items()),
                    'Original environment contains conflicting Scene settings')
    append = ''.join(key + '=' + value + '\n' for key, value in values.items() if key not in existing)
    return original + (b'' if original.endswith(b'\n') else b'\n') + append.encode()


def prepare():
    OUT.mkdir(parents=True, exist_ok=True)
    PRIVATE.mkdir(exist_ok=True)
    protect(PRIVATE)
    startup.require(not (OUT / 'ready.json').exists(), 'A completed Scene deployment already exists')
    receipt = startup.preflight()
    startup.require(promotion.KEY not in startup.env_values(startup.ENV.read_bytes()), 'Scene promotion is already applied')
    free_port()
    mail, local, admin = read_mail()
    authored_dir = active_authored(receipt)
    files = compose_files(authored_dir)
    path = PRIVATE / 'scene.env'
    prior = startup.env_values(path.read_bytes()) if path.exists() else {}
    gm = prior.get(SECRET_KEYS[0]) or secrets.token_urlsafe(48)
    startup.require(len(gm) >= 32 and (SECRET_KEYS[1] not in prior or prior[SECRET_KEYS[1]] == local[SECRET_KEYS[1]]),
                    'Existing private Scene bridge is inconsistent')
    private_write(path, ''.join(key + '=' + value + '\n' for key, value in
                  ((SECRET_KEYS[0], gm), (SECRET_KEYS[1], local[SECRET_KEYS[1]]))).encode())
    runtime = {'GM_SCENE_ADMIN_SECRET': gm, 'GM_SCENE_TARGETS_JSON': '{"1":"127.0.0.1:2355"}',
               'GM_MAIL_TARGET': '127.0.0.1:2354', 'GM_MAIL_ADMIN_SECRET': admin['MAIL_PRODUCER_SECRET_ADMIN']}
    private_write(PRIVATE / 'gm-runtime.env', ''.join(key + '=' + value + '\n' for key, value in runtime.items()).encode())
    recovery = json.loads(files[-1].read_bytes())
    rebuilt = isinstance(recovery.get('x-xuanming-rebuild'), dict)
    if not rebuilt:
        source_check.check_config_inputs(['scene'])
    source_check.load_runtime_data_manifest(authored_dir / 'runtime-data-manifest.json')
    save(OUT / 'runtime-before.json', receipt)
    save(OUT / 'base-images.json', {name: receipt['contract']['images'][name] for name in builder.NODES})
    build_command = [sys.executable, '-X', 'utf8', '-B', str(ROOT / 'tools/scripts/build_docker_cpp_debug.py'),
                     '--output', str(OUT / 'cpp-build'), '--base-images', str(OUT / 'base-images.json'),
                     '--services', 'gate,scene,battle', '--jobs', '1']
    stage_build = str(OUT / 'cpp-build/result.json')
    if rebuilt:
        scene_build = recovery['x-xuanming-rebuild'].get('updates', {}).get('scene', {}).get('build')
        stage_build = recovery['x-xuanming-rebuild'].get('builds', {}).get(scene_build, {}).get('result', {}).get('path', stage_build)
    save(OUT / 'plan.json', {'schema': 1, 'preparedAt': startup.now(), 'executed': False,
        'afterRebuild': rebuilt,
        'buildCommand': build_command, 'stageCommand': [sys.executable, str(Path(__file__).resolve()), 'stage',
                                                      '--build-result', stage_build],
        'applyCommand': [sys.executable, str(Path(__file__).resolve()), 'apply', '--execute'],
        'composeCommand': command(files), 'composeFiles': [str(file) for file in files],
        'affectedServices': ['scene'], 'affectedSceneContainers': 1, 'disconnectsCurrentScenePlayers': True,
        'unchangedOtherRunningServicesRequired': 16, 'sourceSnapshotAndSdkValidationRequired': not rebuilt,
        'readinessDoesNotProvePlayerMutationOrMailClaim': True, 'mailContainerId': mail['Id'],
        'privateRuntimePath': str(PRIVATE / 'gm-runtime.env'), 'readyReceiptPath': str(OUT / 'ready.json')})
    print('Scene deployment plan prepared; no build or container change performed.')


def proposed_compose(raw):
    text = raw.decode('utf-8')
    newline = '\r\n' if '\r\n' in text else '\n'
    start = text.index(newline + '  scene:' + newline)
    stop = text.index(newline + '  battle:' + newline, start)
    scene = text[start:stop]
    startup.require(all(key not in scene for key in (*SECRET_KEYS, promotion.LABEL)) and '    ports:' not in scene,
                    'Scene Compose already contains an unreviewed admin configuration')
    anchor = '      SCENE_RUN_MODE: dev' + newline
    startup.require(scene.count(anchor) == 1, 'Scene development gate is not the expected existing value')
    scene = scene.replace(anchor, anchor + ''.join('      ' + key + ': ${' + key + ':?Load the private GM Scene configuration}'
                                                 + newline for key in SECRET_KEYS))
    anchor = '    volumes:' + newline
    startup.require(scene.count(anchor) == 1, 'Unexpected Scene Compose structure')
    scene = scene.replace(anchor, '    labels:' + newline + '      ' + promotion.LABEL + ': ${'
        + promotion.KEY + ':?Stage and approve the GM Scene promotion}' + newline
        + '    ports: ["127.0.0.1:2355:50001"]' + newline + anchor)
    return (text[:start] + scene + text[stop:]).encode('utf-8')


def stage(path):
    before = json.loads((OUT / 'runtime-before.json').read_text('utf-8'))
    promotion.same_runtime(before, startup.preflight())
    free_port()
    bridge()
    promotion.verified_read_bridge(ROOT)
    read_bridge = bound(ROOT / promotion.READ_RECEIPT)
    authored_dir = active_authored(before)
    files = compose_files(authored_dir)
    source_check.load_runtime_data_manifest(authored_dir / 'runtime-data-manifest.json')
    path = path.resolve()
    startup.require(path.is_relative_to(ROOT / 'build'), 'Build receipt is outside the workspace build directory')
    build = json.loads(path.read_text('utf-8'))
    startup.require(build.get('success') is True and set(build['images']) == set(builder.NODES), 'C++ build is incomplete')
    image = build['images']['scene']['imageId']
    startup.require(all(value['imageId'] == image and builder.image_id(value['image']) == image
                        for value in build['images'].values()), 'C++ batch image identities differ')
    source_path = Path(build['manifestPaths']['scene'])
    source = json.loads(source_path.read_text('utf-8'))
    promotion.require_table_coverage(source['files'], authored_dir / 'tables/generated/tables')
    old_recovery = files[-1].read_bytes()
    recovery = json.loads(old_recovery)
    after_rebuild = is_rebuilt_scene_pin(recovery, path, image, before['contract']['images']['scene'])
    if after_rebuild:
        startup.require(all(name in source['files'] for name in GM_REQUIRED_SOURCES),
                        'Rebuilt Scene source manifest is missing GM player command sources')
        sdk_image_id = source['build']['sdkImageId']
        new_recovery = old_recovery
    else:
        source_check.check_config_inputs(['scene'])
        sdk = builder.validate_sdk()
        sdk_image_id = sdk['imageId']
        startup.require(source['build']['sdkImageId'] == sdk_image_id, 'C++ SDK identity changed')
        builder.assert_sources(source['files'])
        startup.require(recovery['services']['scene']['image'] == before['contract']['images']['scene'], 'Scene baseline image changed')
        recovery['services']['scene']['image'] = image
        new_recovery = (json.dumps(recovery, ensure_ascii=False, indent=2) + '\n').encode()
    # 临时进程只读镜像内清单和摘要，网络关闭，既不启动节点也不连接游戏。
    raw = startup.run(startup.DOCKER + ['run', '--rm', '--network', 'none', '--entrypoint', 'cat', image,
                                      '/app/DEBUG_SOURCE.json'], timeout=60)
    complete = json.loads(raw)
    startup.require(complete['files'] == source['files'] and complete.get('artifacts'), 'Image source manifest differs from builder')
    artifacts = complete['artifacts']
    startup.require(all(name.startswith('/app/') and '..' not in Path(name).parts for name in artifacts),
                    'Unexpected C++ artifact path')
    hashes = startup.run(startup.DOCKER + ['run', '--rm', '--network', 'none', '--entrypoint', 'sha256sum', image,
                                         '--', *artifacts], timeout=60).splitlines()
    startup.require({line[66:]: line[:64] for line in hashes if len(line) > 66} == artifacts, 'Image artifact digest differs')
    (OUT / 'image-manifest.json').write_text(raw, encoding='utf-8')
    old_compose = files[0].read_bytes()
    new_compose = proposed_compose(old_compose)
    (OUT / 'compose.game.proposed.yml').write_bytes(new_compose)
    (OUT / 'compose.recovery.proposed.json').write_bytes(new_recovery)
    private_write(PRIVATE / 'original.env', startup.ENV.read_bytes())
    receipt = {'schema': 1, 'project': startup.PROJECT, 'service': 'scene', 'stagedAt': startup.now(),
        'baseline': bound(OUT / 'runtime-before.json'), 'oldScene': {
            'id': before['contract']['ids']['scene'], 'image': before['contract']['images']['scene'],
            'signature': before['contract']['signatures']['scene']},
        'buildResult': bound(path), 'sourceManifest': bound(source_path), 'imageManifest': bound(OUT / 'image-manifest.json'),
        'imageId': image, 'sourceSha256': source['build']['sourceSha256'], 'sdkImageId': sdk_image_id,
        'composeFiles': [str(file) for file in files], 'oldRecoverySha256': startup.sha(old_recovery),
        'newRecoverySha256': startup.sha(new_recovery), 'oldComposeSha256': startup.sha(old_compose),
        'newComposeSha256': startup.sha(new_compose), 'deploymentScriptSha256': startup.sha(Path(__file__).read_bytes())}
    if after_rebuild:
        receipt['afterRebuild'] = True
    receipt['readBridge'] = read_bridge
    save(OUT / 'promotion.json', receipt)
    approval = startup.sha((OUT / 'promotion.json').read_bytes())
    values = bridge()
    values[promotion.KEY] = approval
    original = (PRIVATE / 'original.env').read_bytes()
    private_write(PRIVATE / 'proposed.env', environment_bytes(original, values))
    patch = ''.join(difflib.unified_diff(old_compose.decode().splitlines(True), new_compose.decode().splitlines(True),
                                       fromfile='a/deploy/debug/compose.game.yml', tofile='b/deploy/debug/compose.game.yml'))
    patch += ''.join(difflib.unified_diff(old_recovery.decode().splitlines(True), new_recovery.decode().splitlines(True),
                                        fromfile='a/' + files[-1].relative_to(ROOT).as_posix(),
                                        tofile='b/' + files[-1].relative_to(ROOT).as_posix()))
    (OUT / 'review.patch').write_text(patch, encoding='utf-8')
    if not after_rebuild:
        builder.assert_sources(source['files'])
    promotion.same_runtime(before, startup.preflight())
    promotion.read_bound(ROOT, read_bridge)
    print('Scene promotion staged with exact image, source, runtime and Compose identities; not applied.')


def replace_exact(path, old, new):
    startup.require(path.read_bytes() == old, 'Configuration changed before replacement')
    temporary = path.with_name(path.name + '.gm-scene.tmp')
    startup.require(not temporary.exists(), 'An earlier Scene replacement is still pending')
    with temporary.open('xb') as output:
        output.write(new)
    if path == startup.ENV:
        protect(temporary)
    startup.require(path.read_bytes() == old, 'Configuration changed during replacement')
    temporary.replace(path)


def apply():
    receipt_path = OUT / 'promotion.json'
    receipt = json.loads(receipt_path.read_text('utf-8'))
    startup.require(receipt['deploymentScriptSha256'] == startup.sha(Path(__file__).read_bytes()), 'Scene deployment script changed after review')
    before = promotion.read_bound(ROOT, receipt['baseline'])
    after_rebuild = receipt.get('afterRebuild') is True
    with startup.startup_lock():
        promotion.same_runtime(before, startup.preflight())
        free_port()
        mail, _, _ = read_mail()
        promotion.read_bound(ROOT, receipt['readBridge'])
        promotion.verified_read_bridge(ROOT)
        authored_dir = active_authored(before)
        files = compose_files(authored_dir)
        startup.require([str(file) for file in files] == receipt['composeFiles'], 'Staged Compose file provenance changed')
        runtime_data = source_check.load_runtime_data_manifest(authored_dir / 'runtime-data-manifest.json')
        source = promotion.read_bound(ROOT, receipt['sourceManifest'])
        promotion.require_table_coverage(source['files'], authored_dir / 'tables/generated/tables')
        build = promotion.read_bound(ROOT, receipt['buildResult'])
        complete = promotion.read_bound(ROOT, receipt['imageManifest'])
        if not after_rebuild:
            sdk = builder.validate_sdk()
            startup.require(source['build']['sdkImageId'] == sdk['imageId'] == receipt['sdkImageId'], 'C++ SDK identity changed')
            builder.assert_sources(source['files'])
        else:
            startup.require(source['build']['sdkImageId'] == receipt['sdkImageId'], 'Staged SDK identity changed')
        startup.require(build.get('success') is True and set(build.get('images', {})) == set(builder.NODES)
            and all(value['imageId'] == receipt['imageId'] for value in build['images'].values())
            and build['manifestPaths']['scene'] == receipt['sourceManifest']['path']
            and source['build']['sourceSha256'] == receipt['sourceSha256']
            and complete['files'] == source['files'] and complete.get('artifacts'), 'Staged C++ build chain changed')
        startup.require(builder.image_id(receipt['imageId']) == receipt['imageId'], 'Staged Scene image changed')
        raw = startup.run(startup.DOCKER + ['run', '--rm', '--network', 'none', '--entrypoint', 'cat',
                         receipt['imageId'], '/app/DEBUG_SOURCE.json'], timeout=60)
        startup.require(json.loads(raw) == complete, 'Staged image manifest differs from the immutable image')
        original_env = (PRIVATE / 'original.env').read_bytes()
        startup.require(startup.ENV.read_bytes() == original_env, 'Original environment changed after review')
        private_values = bridge()
        private_values[promotion.KEY] = startup.sha(receipt_path.read_bytes())
        reviewed_env = (PRIVATE / 'proposed.env').read_bytes()
        startup.require(reviewed_env == environment_bytes(original_env, private_values),
                        'Proposed private environment changed after review')
        changes = [(files[0], 'oldComposeSha256', 'newComposeSha256', OUT / 'compose.game.proposed.yml'),
                   (files[-1], 'oldRecoverySha256', 'newRecoverySha256', OUT / 'compose.recovery.proposed.json')]
        reviewed = []
        for path, old_key, new_key, proposed in changes:
            old_bytes, new_bytes = path.read_bytes(), proposed.read_bytes()
            startup.require(startup.sha(old_bytes) == receipt[old_key] and startup.sha(new_bytes) == receipt[new_key],
                            'Reviewed Compose inputs changed')
            reviewed.append((path, old_bytes, new_bytes))
        # SDK/快照/镜像核对可能耗时；在真正修改前再确认完整运行身份，拒绝窗口内的并发重建。
        if not after_rebuild:
            builder.assert_sources(source['files'])
        promotion.same_runtime(before, startup.preflight())
        # 先留存所有原字节，再进行一次明确的 Scene-only 重建；失败保留现场，不自动重试或回滚容器。
        for path, old_bytes, _ in reviewed:
            (OUT / (path.name + '.before')).write_bytes(old_bytes)
        save(OUT / 'apply-intent.json', {'utc': startup.now(), 'promotionSha256': startup.sha(receipt_path.read_bytes()),
                                       'oldSceneId': before['contract']['ids']['scene'], 'command': command(files)})
        for path, old_bytes, new_bytes in reviewed:
            if old_bytes != new_bytes:
                replace_exact(path, old_bytes, new_bytes)
        replace_exact(startup.ENV, original_env, reviewed_env)
        env_values = startup.env_values(startup.ENV.read_bytes())
        child_env = {key: value for key, value in os.environ.items() if key not in env_values and not key.startswith('COMPOSE_')}
        startup.run(command(files), timeout=180, env=child_env)
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            row = startup.inspect(['xuanming-debug-scene'])[0]
            startup.require(row['State']['Status'] == 'running' and row['RestartCount'] == 0,
                            'New Scene exited or restarted; automatic retry is prohibited')
            if startup.node_ready(row['Id'], 'scene'):
                break
            time.sleep(1)
        else:
            raise RuntimeError('Scene readiness deadline exceeded')
        after = startup.preflight()
        promotion.unchanged_other_states(before, after)
        save(OUT / 'runtime-after.json', after)
        if after_rebuild:
            verify_container_artifacts(row, complete, runtime_data)
        else:
            startup.require(source_check.check(['scene'], OUT / 'source-check.json', authored_dir / 'runtime-data-manifest.json'),
                            'Scene artifact/source check failed after deployment')
        actual = dict(value.split('=', 1) for value in startup.run(startup.DOCKER + ['exec', row['Id'], 'cat', '/proc/1/environ']).split('\0') if '=' in value)
        private = startup.env_values((PRIVATE / 'scene.env').read_bytes())
        startup.require(all(actual.get(key) == private[key] for key in SECRET_KEYS), 'Scene process secret bridge mismatch')
        with socket.create_connection(('127.0.0.1', 2355), timeout=5):
            pass
        startup.require(startup.inspect([mail['Id']])[0]['State']['Health']['Status'] == 'healthy', 'Mail health changed')
        promotion.read_bound(ROOT, receipt['readBridge'])
        promotion.verified_read_bridge(ROOT)
        registry = json.loads(startup.run(startup.DOCKER + ['exec', 'xuanming-debug-current-etcd-1', 'etcdctl',
            'get', 'SceneNodeService.rpc/zone/1/', '--prefix', '--write-out=json']))
        nodes = [json.loads(base64.b64decode(item['value'])) for item in registry.get('kvs', [])]
        startup.require(any(item.get('nodeId') == 1 and item.get('grpcEndpoint') == {'ip': actual['NODE_IP'], 'port': 50001}
                            for item in nodes), 'Scene trusted registry identity or gRPC endpoint changed')
        save(OUT / 'ready.json', {'schema': 1, 'passed': True, 'utc': startup.now(), 'project': startup.PROJECT,
            'sceneContainerId': row['Id'], 'sceneImageId': row['Image'], 'mailContainerId': mail['Id'],
            'promotion': bound(receipt_path), 'runtime': bound(OUT / 'runtime-after.json'),
            'readBridge': receipt['readBridge'],
            'sourceCheck': bound(OUT / 'source-check.json'), 'privateRuntimePath': str(PRIVATE / 'gm-runtime.env'),
            'privateRuntimeSha256': startup.sha((PRIVATE / 'gm-runtime.env').read_bytes()),
            'sceneTarget': '127.0.0.1:2355', 'sceneNodeId': 1, 'mailTarget': '127.0.0.1:2354',
            'playerMutationTested': False, 'mailAttachmentClaimTested': False})
        private_write(PRIVATE / 'ready.sha256', (startup.sha((OUT / 'ready.json').read_bytes()) + '\n').encode('ascii'))
        print('Scene runtime promotion verified; no player command or mail claim was performed.')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('prepare', 'stage', 'apply'))
    parser.add_argument('--build-result', type=Path)
    parser.add_argument('--execute', action='store_true')
    args = parser.parse_args()
    if args.action == 'prepare':
        prepare()
    elif args.action == 'stage':
        startup.require(args.build_result is not None, 'Stage requires a successful C++ build receipt')
        stage(args.build_result)
    else:
        startup.require(args.execute, 'Apply requires the explicit --execute flag')
        apply()


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        if OUT.exists():
            save(OUT / ('failure-' + datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S%fZ') + '.json'),
                 {'passed': False, 'kind': type(error).__name__, 'error': str(error) if isinstance(error, RuntimeError)
                  else 'Operation failed; raw details suppressed'})
        print('Scene deployment stopped; no automatic retry was performed. Review the private task evidence.')
        raise SystemExit(1)
