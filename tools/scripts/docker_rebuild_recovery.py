"""重编恢复栈时只前移已验证镜像；原初始化、GM 与数据卷证据继续逐项复核。"""
from copy import deepcopy
import json
from pathlib import Path
import re

EXTENSION = 'x-xuanming-rebuild'


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def bound(startup, path):
    path = Path(path)
    require(path.is_absolute() and path.resolve() == path and path.is_relative_to(startup.ROOT / 'build'),
            'Rebuild evidence is outside the build directory')
    return {'path': str(path), 'sha256': startup.sha(path.read_bytes())}


def read_bound(startup, record):
    require(bound(startup, record['path']) == record, 'Rebuild evidence changed')
    return json.loads(Path(record['path']).read_bytes())


def public_origin(startup, rows, raw):
    # 不保存 Config.Env、命令、健康检查或任何可能含密码的原始 inspect 内容。
    labels = {'com.docker.compose.service', 'com.docker.compose.project',
              'com.docker.compose.project.config_files', 'com.docker.compose.oneoff',
              'com.docker.compose.container-number', 'com.xuanming.gm.scene-promotion'}
    return {'overlay': raw.decode('utf-8'), 'rows': [
        {'Id': row['Id'], 'Image': row['Image'], '_signature': startup.signature(row),
         'Config': {'Labels': {key: value for key, value in row['Config']['Labels'].items() if key in labels}}}
        for row in rows]}


def verify(startup, rows, authored_input, document):
    evidence = document.get(EXTENSION)
    require(isinstance(evidence, dict) and evidence.get('schema') == 1,
            'Invalid rebuild recovery evidence')
    origin = evidence['origin']
    require(EXTENSION not in json.loads(origin['overlay']), 'Nested rebuild recovery origin')
    # 重新运行原恢复合同，包括固定源摘要和 GM 原容器签名；不能仅相信 passed 字段。
    _, recovery = startup.recovery_contract(origin['rows'], authored_input,
        original_raw=origin['overlay'].encode(),
        original_signature=lambda row: row['_signature'])
    require(recovery is not None, 'Rebuild origin has no approved recovery provenance')
    expected = json.loads(origin['overlay'])
    changed = {}
    import rebuild_all_docker_debug as rebuild_all
    import rebuild_docker_debug as rebuild
    for key, record in evidence['builds'].items():
        language = record['language']
        require(key == record['result']['sha256'], 'Recovery build key differs')
        require(language in rebuild_all.SERVICES, 'Unknown recovery build language')
        result = read_bound(startup, record['result'])
        names = set(rebuild_all.SERVICES[language])
        require(result.get('success') is True and set(result.get('images', {})) == names
                and set(result.get('manifestPaths', {})) == names
                and set(record['manifests']) == names, 'Incomplete recovery build evidence')
        for name in names:
            manifest_record = record['manifests'][name]
            require(manifest_record['path'] == result['manifestPaths'][name], 'Recovery manifest path differs')
            manifest = read_bound(startup, manifest_record)
            require(manifest.get('schema') == 1 and manifest.get('language') == language
                    and manifest.get('files') and isinstance(manifest.get('artifacts'), dict)
                    and (language == 'cpp' or manifest['artifacts']), 'Invalid recovery source manifest')
            identity = result['images'][name]['imageId']
            require(re.fullmatch(r'sha256:[0-9a-f]{64}', identity or ''), 'Invalid recovery build image identity')
    for name, update in evidence['updates'].items():
        record = evidence['builds'][update['build']]
        source = update['service']
        require(source == name and name in startup.APPS
                or source == 'sql-boot' and name in {writer + '-sql-boot' for writer in startup.WRITERS},
                'Invalid recovery image update target')
        result = read_bound(startup, record['result'])
        changed[name] = result['images'][source]['imageId']
    require(changed and set(changed) <= set(expected['services']) - set(startup.INFRA),
            'Recovery rebuild changes infrastructure or initialization')
    for name, identity in changed.items():
        expected['services'][name]['image'] = identity
    expected[EXTENSION] = evidence
    require(document == expected, 'Rebuild recovery overlay changes more than approved images')
    original = {row['Config']['Labels']['com.docker.compose.service']: row for row in origin['rows']}
    for row in rows:
        name = row['Config']['Labels']['com.docker.compose.service']
        if name in startup.INFRA:
            require(row['Id'] == original[name]['Id'] and row['Image'] == original[name]['Image']
                    and startup.signature(row) == original[name]['_signature'],
                    'Rebuild recovery infrastructure identity changed')
    combined = json.loads(authored_input) if authored_input else {'services': {}}
    for name, service in expected['services'].items():
        combined['services'].setdefault(name, {}).update(service)
    combined['volumes'] = expected['volumes']
    recovery['overlaySha256'] = startup.sha(startup.recovery_path().read_bytes())
    recovery['rebuildEvidence'] = {language: record['result'] for language, record in evidence['builds'].items()}
    return json.dumps(combined), recovery


def prepare(startup, baseline, builds, root):
    previous = json.loads(baseline['recoveryOverlay']['raw'])
    evidence = deepcopy(previous.get(EXTENSION))
    if evidence is None:
        evidence = {'schema': 1, 'origin': public_origin(startup,
            list(baseline['services'].values()) + baseline['historical'], baseline['recoveryOverlay']['raw']), 'builds': {}, 'updates': {}}
    import rebuild_all_docker_debug as rebuild_all
    for language, built in builds.items():
        directory = Path(built['reused_from']) / language if 'reused_from' in built else root / language
        result = rebuild_all.validate_build(language, directory)
        require(result['images'] == built['images'] and result['manifestPaths'] == built['manifestPaths'],
                'Recovery build result changed before publication')
        binding = bound(startup, directory / 'result.json')
        evidence['builds'][binding['sha256']] = {'language': language, 'result': binding,
            'manifests': {name: bound(startup, path) for name, path in built['manifestPaths'].items()}}
        for name in built['images']:
            if name in startup.APPS:
                evidence['updates'][name] = {'build': binding['sha256'], 'service': name}
    # 注册镜像只随本次选中的写者前移，不触及未重编语言的旧注册器。
    selected = {name for built in builds.values() for name in built['images']}
    sql_build = next((key for key, record in reversed(list(evidence['builds'].items()))
                      if record['language'] == 'go'), None)
    if sql_build is not None:
        for writer in startup.WRITERS & selected:
            evidence['updates'][writer + '-sql-boot'] = {'build': sql_build, 'service': 'sql-boot'}
    document = json.loads(evidence['origin']['overlay'])
    for name, update in evidence['updates'].items():
        result = read_bound(startup, evidence['builds'][update['build']]['result'])
        document['services'][name]['image'] = result['images'][update['service']]['imageId']
    document[EXTENSION] = evidence
    verify(startup, list(baseline['services'].values()) + baseline['historical'],
           baseline['authoredOverlay']['input'], document)
    return json.dumps(document, indent=2).encode() + b'\n'
