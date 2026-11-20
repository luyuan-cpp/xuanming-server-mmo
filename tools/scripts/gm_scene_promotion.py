"""仅接受原恢复栈上显式批准的 GM Scene 镜像前移；不放宽历史或其他服务。"""
from copy import deepcopy
import hashlib
import importlib.util
import json
from pathlib import Path
import re
import sys


RELATIVE = Path('build/verification/gm-admin-20261117/scene')
KEY = 'GM_SCENE_PROMOTION_SHA256'
LABEL = 'com.xuanming.gm.scene-promotion'
RUNTIME_KEYS = ('GM_SCENE_ADMIN_SECRET', 'GM_SCENE_TARGETS_JSON', 'GM_MAIL_TARGET', 'GM_MAIL_ADMIN_SECRET')
READ_TARGETS = {'GM_DATA_TARGET': '127.0.0.1:2356', 'GM_LOCATOR_TARGET': '127.0.0.1:2357'}
READ_RECEIPT = Path('build/verification/gm-admin-20261117/rpc-bridge/ready.json')


def require(value, message):
    if not value:
        raise RuntimeError(message)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def read_bound(root, record):
    path = Path(record['path'])
    require(path.is_absolute() and path.resolve() == path and path.is_relative_to(root / 'build'),
            'GM Scene evidence path is outside the build directory')
    raw = path.read_bytes()
    require(digest(raw) == record['sha256'], 'GM Scene evidence digest changed')
    return json.loads(raw)


def same_runtime(saved, fresh):
    """执行前必须是同一份完整合同；不以只比较容器名替代身份检查。"""
    require(saved['contract'] == fresh['contract'], 'GM Scene preflight identity or input changed')
    require(saved['states'] == fresh['states'], 'GM Scene runtime state changed')


def same_contract_and_liveness(saved, fresh):
    """就绪读取核对完整合同与运行态；允许宿主机重启原地启动同一容器带来的起止时间变化。"""
    require(saved['contract'] == fresh['contract'], 'GM Scene preflight identity or input changed')
    require(set(saved['states']) == set(fresh['states']), 'GM Scene runtime state changed')
    volatile = {'startedAt', 'finishedAt'}
    for name, prior in saved['states'].items():
        current = fresh['states'][name]
        require({k: v for k, v in prior.items() if k not in volatile}
                == {k: v for k, v in current.items() if k not in volatile},
                'GM Scene runtime state changed')


def unchanged_other_states(before, after):
    """仅部署窗口使用；日常启动仍保留原来恢复停止容器的能力。"""
    require(set(before['states']) == set(after['states']), 'GM Scene post-deploy service inventory changed')
    require(all(state == after['states'][name] for name, state in before['states'].items() if name != 'scene'),
            'Another service changed runtime state during GM Scene deployment')


def require_table_coverage(source_files, runtime_directory):
    """新节点必读表不能被旧目录挂载遮住；只核覆盖，不批准新表或替代统一验收。"""
    required = {Path(name).name.lower() for name in source_files
                if name.startswith('generated/tables/') and name.lower().endswith('.json')
                and Path(name).name.lower() != 'manifest.json'}
    require(required, 'New Scene source manifest has no runtime table inputs')
    # 镜像构建会把源表文件名转成小写；Linux 运行挂载必须真的有这些小写名称。
    present = {path.name for path in runtime_directory.glob('*.json') if path.is_file()}
    missing = sorted(required - present)
    require(not missing, 'Authored runtime is missing new Scene tables: ' + ', '.join(missing)
            + '; deployment is blocked pending unified validation. Review build/verification/gm-admin-20261117/scene/table-compatibility-review.json')


def environment(data):
    values = {}
    for line in data.decode('utf-8-sig').splitlines():
        if not line.strip() or line.lstrip().startswith('#'):
            continue
        key, separator, value = line.partition('=')
        require(separator and re.fullmatch(r'[A-Z][A-Z0-9_]*', key) and key not in values,
                'Invalid GM Scene environment file')
        values[key] = value
    return values


def verified_read_bridge(root):
    path = root / 'deploy/gm-admin/rpc-bridge.py'
    require(path.resolve() == path, 'Redirected GM read bridge verifier')
    spec = importlib.util.spec_from_file_location('gm_verified_rpc_bridge', path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    values = module.verify_ready()
    require(values == READ_TARGETS, 'GM read bridge returned unexpected endpoints')
    return values


def ready_runtime(root, preflight, inspect, process_environment, read_bridge):
    """仅从通过真实部署验收的私有桥加载四个明确配置；调用者失败时保持禁用。"""
    out = root / RELATIVE
    raw = (out / 'ready.json').read_bytes()
    require(digest(raw) == (out / 'private/ready.sha256').read_text('ascii').strip(),
            'GM Scene ready receipt digest changed')
    ready = json.loads(raw)
    require(ready.get('schema') == 1 and ready.get('passed') is True
            and ready.get('project') == 'xuanming-debug-current', 'GM Scene readiness is incomplete')
    for field, filename in (('promotion', 'promotion.json'), ('runtime', 'runtime-after.json'), ('sourceCheck', 'source-check.json')):
        require(ready[field]['path'] == str(out / filename), 'GM Scene ready evidence path changed')
    promotion = read_bound(root, ready['promotion'])
    after = read_bound(root, ready['runtime'])
    checked = read_bound(root, ready['sourceCheck'])
    configured = environment((root / 'build/docker-debug-current/.env').read_bytes())
    require(configured.get(KEY) == ready['promotion']['sha256'], 'GM Scene readiness is not the approved promotion')
    require(ready['readBridge']['path'] == str(root / READ_RECEIPT), 'GM Scene read bridge receipt path changed')
    read_bound(root, ready['readBridge'])
    require(read_bridge() == READ_TARGETS, 'GM Scene read bridge is not verified')
    require(after.get('passed') is True and checked.get('success') is True
            and len(checked.get('services', [])) == 1 and checked['services'][0].get('service') == 'scene'
            and checked['services'][0].get('success') is True
            and checked['services'][0].get('imageId') == promotion['imageId'], 'GM Scene artifact verification is incomplete')
    require(ready['sceneImageId'] == promotion['imageId'] == after['contract']['images']['scene']
            and ready['sceneContainerId'] == after['contract']['ids']['scene'], 'GM Scene ready identity is inconsistent')
    fresh = preflight()
    same_contract_and_liveness(after, fresh)
    require(ready['sceneTarget'] == '127.0.0.1:2355' and ready['sceneNodeId'] == 1
            and ready['mailTarget'] == '127.0.0.1:2354', 'GM Scene ready endpoint changed')
    path = out / 'private/gm-runtime.env'
    require(ready['privateRuntimePath'] == str(path) and path.resolve() == path, 'GM runtime bridge path changed')
    private_raw = path.read_bytes()
    require(digest(private_raw) == ready['privateRuntimeSha256'], 'GM runtime bridge bytes changed')
    values = environment(private_raw)
    require(set(values) == set(RUNTIME_KEYS), 'Unexpected GM runtime bridge field')
    scene_values = environment((out / 'private/scene.env').read_bytes())
    actual = process_environment(ready['sceneContainerId'])
    mail = inspect([ready['mailContainerId']])[0]
    mail_values = dict(line.split('=', 1) for line in mail['Config']['Env'])
    require(mail['Id'] == ready['mailContainerId'] and mail['Name'] == '/xuanming-gm-mail-mail-1'
            and mail['State']['Status'] == 'running' and mail['State'].get('Health', {}).get('Status') == 'healthy',
            'GM Mail readiness is stale')
    require(len(values['GM_SCENE_ADMIN_SECRET']) >= 32 and values['GM_SCENE_ADMIN_SECRET'] ==
            actual.get('MMORPG_GM_PLAYER_COMMAND_SECRET') == scene_values.get('MMORPG_GM_PLAYER_COMMAND_SECRET'),
            'GM Scene runtime secret differs from the approved bridge')
    require(len(values['GM_MAIL_ADMIN_SECRET']) >= 32 and values['GM_MAIL_ADMIN_SECRET'] == mail_values.get('MAIL_PRODUCER_SECRET_ADMIN')
            and len(actual.get('MMORPG_ASSET_OP_SECRET_MAIL', '')) >= 32
            and actual.get('MMORPG_ASSET_OP_SECRET_MAIL') == mail_values.get('MMORPG_ASSET_OP_SECRET_MAIL') == scene_values.get('MMORPG_ASSET_OP_SECRET_MAIL'),
            'GM mail runtime secrets differ from the approved bridge')
    require(values['GM_SCENE_TARGETS_JSON'] == '{"1":"127.0.0.1:2355"}' and values['GM_MAIL_TARGET'] == '127.0.0.1:2354',
            'GM runtime bridge endpoint changed')
    return values


def _load_approved_receipt(root, values):
    approval = values.get(KEY)
    if approval is None:
        return None, None
    require(re.fullmatch(r'[0-9a-f]{64}', approval or '') is not None, 'Invalid GM Scene approval digest')
    path = root / RELATIVE / 'promotion.json'
    require(path.resolve() == path, 'Redirected GM Scene promotion evidence')
    raw = path.read_bytes()
    require(digest(raw) == approval, 'GM Scene promotion receipt does not match explicit approval')
    return approval, json.loads(raw)


def is_post_rebuild(root, values):
    approval, receipt = _load_approved_receipt(root, values)
    return bool(approval is not None and receipt.get('afterRebuild') is True)


def _verify_chain_and_config(root, approval, receipt, config, *, require_image_change):
    old = receipt['oldScene']
    build = read_bound(root, receipt['buildResult'])
    image = receipt['imageId']
    require(re.fullmatch(r'sha256:[0-9a-f]{64}', image or '') is not None
            and (image != old['image'] if require_image_change else image == old['image']),
            'GM Scene promoted image is invalid')
    require(build.get('success') is True and set(build.get('images', {})) == {'gate', 'scene', 'battle'}
            and all(item['imageId'] == image for item in build['images'].values()),
            'GM Scene image is not from one successful canonical C++ batch')
    source = read_bound(root, receipt['sourceManifest'])
    complete = read_bound(root, receipt['imageManifest'])
    require(source.get('schema') == 1 and source.get('language') == 'cpp' and source.get('files')
            and complete.get('files') == source['files'] and complete.get('artifacts'),
            'GM Scene source and artifact manifests do not match')
    source_hash = digest(json.dumps(source['files'], sort_keys=True).encode())
    require(source['build']['type'] == 'Debug' and source['build']['sourceSha256'] == source_hash
            and receipt['sourceSha256'] == source_hash
            and receipt['sdkImageId'] == source['build']['sdkImageId'], 'GM Scene source build identity changed')
    require(receipt['sourceManifest']['path'] == build['manifestPaths']['scene'],
            'GM Scene source manifest is not the builder output')
    scene = config['services']['scene']
    require(scene.get('labels', {}).get(LABEL) == approval, 'GM Scene Compose approval label mismatch')
    ports = [(p['target'], str(p['published']), p.get('host_ip'), p.get('protocol', 'tcp')) for p in scene.get('ports', [])]
    require(ports == [(50001, '2355', '127.0.0.1', 'tcp')], 'GM Scene admin port is not the approved loopback endpoint')
    require(all(len(scene.get('environment', {}).get(key, '')) >= 32 for key in
                ('MMORPG_GM_PLAYER_COMMAND_SECRET', 'MMORPG_ASSET_OP_SECRET_MAIL')), 'GM Scene secrets are missing')
    return image


def expected_overlay(root, values, expected, rows, config, signature, *, original_recovery_raw=None):
    approval, receipt = _load_approved_receipt(root, values)
    if approval is None:
        return expected
    if receipt.get('afterRebuild') is True:
        require(original_recovery_raw is not None, 'Post-rebuild GM Scene promotion requires rebuild recovery overlay')
        return expected
    require(receipt.get('schema') == 1 and receipt.get('project') == 'xuanming-debug-current'
            and receipt.get('service') == 'scene', 'Invalid GM Scene promotion scope')
    files = [str(root / name) for name in ('deploy/debug/compose.game.yml', 'deploy/debug/compose.debug.yml',
        'build/verification/authored-festival-pet-acquisition-20261116/compose.authored.json',
        'build/verification/container-recovery-20261117/compose.recovery.json')]
    require(receipt.get('composeFiles') == files, 'GM Scene Compose provenance changed')
    before = read_bound(root, receipt['baseline'])
    require(before.get('passed') is True and before['contract']['project'] == receipt['project'],
            'GM Scene baseline is not a successful original preflight')
    old = receipt['oldScene']
    require(old == {'id': before['contract']['ids']['scene'], 'image': before['contract']['images']['scene'],
                    'signature': before['contract']['signatures']['scene']}, 'GM Scene prior identity is inconsistent')
    require(old['image'] == expected['services']['scene']['image'], 'GM Scene prior image differs from approved history')
    require(receipt['oldRecoverySha256'] == before['contract']['composeSha256'][files[-1]],
            'GM Scene prior recovery overlay is inconsistent')
    require(receipt['oldComposeSha256'] == before['contract']['composeSha256'][files[0]]
            and digest(Path(files[0]).read_bytes()) == receipt['newComposeSha256'],
            'GM Scene reviewed base Compose changed')
    image = _verify_chain_and_config(root, approval, receipt, config, require_image_change=True)
    actual = {row['Config']['Labels'].get('com.docker.compose.service'): row for row in rows}
    require(set(actual) == set(before['contract']['ids']), 'GM Scene service inventory changed')
    for name, row in actual.items():
        if name == 'scene':
            require(row['Id'] != old['id'] and row['Image'] == image
                    and row['Config']['Labels'].get(LABEL) == approval, 'GM Scene promoted runtime identity mismatch')
        else:
            require(row['Id'] == before['contract']['ids'][name] and row['Image'] == before['contract']['images'][name]
                    and signature(row) == before['contract']['signatures'][name],
                    'GM Scene promotion changed another service: ' + name)
    promoted = deepcopy(expected)
    promoted['services']['scene']['image'] = image
    actual_recovery = original_recovery_raw if original_recovery_raw is not None else Path(files[-1]).read_bytes()
    require(digest(actual_recovery) == receipt['newRecoverySha256'] and json.loads(actual_recovery) == promoted,
            'GM Scene recovery overlay changed outside the single approved image')
    return promoted


def verify_post_rebuild(root, values, document, rows, config, signature, authored_overlay_path):
    """核验在 x-xuanming-rebuild 栈上原地补齐 GM 端口与密钥的 Scene 单容器前移。"""
    approval, receipt = _load_approved_receipt(root, values)
    if approval is None or receipt.get('afterRebuild') is not True:
        return
    require(receipt.get('schema') == 1 and receipt.get('project') == 'xuanming-debug-current'
            and receipt.get('service') == 'scene', 'Invalid GM Scene promotion scope')
    require(authored_overlay_path is not None, 'Post-rebuild GM Scene promotion requires an active authored overlay')
    files = [str(root / 'deploy/debug/compose.game.yml'), str(root / 'deploy/debug/compose.debug.yml'),
             str(authored_overlay_path), str(root / 'build/verification/container-recovery-20261117/compose.recovery.json')]
    require(receipt.get('composeFiles') == files, 'GM Scene Compose provenance changed')
    before = read_bound(root, receipt['baseline'])
    require(before.get('passed') is True and before['contract']['project'] == receipt['project'],
            'GM Scene baseline is not a successful original preflight')
    old = receipt['oldScene']
    require(old == {'id': before['contract']['ids']['scene'], 'image': before['contract']['images']['scene'],
                    'signature': before['contract']['signatures']['scene']}, 'GM Scene prior identity is inconsistent')
    require(receipt['oldRecoverySha256'] == before['contract']['composeSha256'][files[-1]]
            and receipt['newRecoverySha256'] == receipt['oldRecoverySha256'],
            'GM Scene prior recovery overlay is inconsistent')
    require(receipt['oldComposeSha256'] == before['contract']['composeSha256'][files[0]]
            and digest(Path(files[0]).read_bytes()) == receipt['newComposeSha256'],
            'GM Scene reviewed base Compose changed')
    evidence = document.get('x-xuanming-rebuild', {})
    require(receipt['buildResult']['sha256'] in evidence.get('builds', {}),
            'GM Scene build receipt is not in rebuild recovery provenance')
    image = _verify_chain_and_config(root, approval, receipt, config, require_image_change=False)
    actual = {row['Config']['Labels'].get('com.docker.compose.service'): row for row in rows}
    require(set(actual) == set(before['contract']['ids']), 'GM Scene service inventory changed')
    same_build = evidence.get('updates', {}).get('scene', {}).get('build') == receipt['buildResult']['sha256']
    if same_build:
        require(digest(Path(files[-1]).read_bytes()) == receipt['newRecoverySha256']
                and document['services']['scene']['image'] == image,
                'GM Scene recovery overlay changed outside the approved rebuild pin')
        for name, row in actual.items():
            if name == 'scene':
                require(row['Id'] != old['id'] and row['Image'] == image
                        and row['Config']['Labels'].get(LABEL) == approval, 'GM Scene promoted runtime identity mismatch')
            else:
                require(row['Id'] == before['contract']['ids'][name] and row['Image'] == before['contract']['images'][name]
                        and signature(row) == before['contract']['signatures'][name],
                        'GM Scene promotion changed another service: ' + name)
    else:
        scene_row = actual['scene']
        require(scene_row['Id'] != old['id']
                and scene_row['Image'] in (image, document['services']['scene']['image'])
                and scene_row['Config']['Labels'].get(LABEL) == approval, 'GM Scene promoted runtime identity mismatch')
