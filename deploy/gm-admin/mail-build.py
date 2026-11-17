"""从当前源码构建独立 mail 镜像；只写本任务证据目录，不启动或重建原栈。"""
from pathlib import Path
import importlib.util
import json
import os
import subprocess
import sys
import argparse
import re

ROOT = Path(__file__).resolve().parents[2]
OUT = ROOT / 'build/verification/gm-admin-20261117/mail'
BASE = 'ghcr.io/luyuancpp/mmorpg-mail@sha256:f926d34c6fe6c1cc2137f99117558c748cb734c18af8253bbe917fb7b8a17fc6'
LOCAL_BASE = 'local/gm-mail-runtime:pin-f926d34c6fe6c1cc'

def main():
    spec = importlib.util.spec_from_file_location('gm_mail_build_tools', ROOT / 'tools/scripts/build_docker_go_debug.py')
    build = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(build)
    parser = argparse.ArgumentParser()
    parser.add_argument('--attempt', type=int, default=1)
    parser.add_argument('--resume-packaging', action='store_true')
    args = parser.parse_args()
    if args.attempt < 1 or args.attempt > 99:
        raise RuntimeError('invalid_attempt')
    OUT.mkdir(parents=True, exist_ok=True)
    directory = OUT / ('image' if args.attempt == 1 else 'image-' + str(args.attempt))
    resume = args.resume_packaging
    directory.mkdir(exist_ok=resume)
    environment = build.build_environment(network=False)
    if build.capture(['go', 'env', 'GOVERSION'], env=environment).strip() != build.GO_VERSION:
        raise RuntimeError('go_version_mismatch')
    before = build.snapshot('mail')
    commit = build.capture(['git', 'rev-parse', 'HEAD']).strip()
    stamp = build.utc()
    version = 'gm-mail-20261117'
    binary = directory / 'service'
    if resume:
        if before != json.loads((directory / 'source-before.json').read_text()) or before != json.loads((directory / 'source-after.json').read_text()):
            raise RuntimeError('mail_source_changed_before_packaging')
        previous = dict(line.split('=', 1) for line in (directory / 'BUILD_INFO').read_text().splitlines())
        if previous['commit'] != commit + '-dirty':
            raise RuntimeError('mail_revision_changed_before_packaging')
        stamp = previous['built_at']
    else:
        build.write_json(directory / 'source-before.json', before)
        build.run_logged(['go', 'build', '-p=1', '-mod=readonly', '-buildvcs=false', '-trimpath',
            '-ldflags=-X shared/buildinfo.Version=' + version + ' -X shared/buildinfo.Commit=' + commit + '-dirty -X shared/buildinfo.BuildTime=' + stamp,
            '-o', str(binary), 'mail.go'], cwd=ROOT / 'go/mail', env=environment, log=directory / 'go-build.log', timeout=1200)
        after = build.snapshot('mail')
        build.write_json(directory / 'source-after.json', after)
        if before != after:
            raise RuntimeError('mail_source_changed_during_build')
    build.check_elf(binary)
    if resume:
        artifacts, resources = {}, {}
        for destination, source in build.runtime_resource_plan('mail').items():
            source_name = source.relative_to(ROOT).as_posix()
            actual = build.sha256(directory / 'runtime_assets' / destination.lstrip('/'))
            if actual != before[source_name]:
                raise RuntimeError('mail_resource_changed_before_packaging')
            artifacts[destination], resources[destination] = actual, source_name
    else:
        artifacts, resources = build.freeze_runtime_resources('mail', directory / 'runtime_assets', before)
    (directory / 'BUILD_INFO').write_text('version=' + version + '\ncommit=' + commit + '-dirty\nbuilt_at=' + stamp + '\n', encoding='utf-8')
    image = 'local/mmorpg-mail:gm-admin-20261117-' + build.sha256(binary)[:12]
    # 已有本地镜像摘要必须吻合，不拉取或用别的服务替换业务代码。
    if build.capture(['docker', 'image', 'inspect', BASE, '--format', '{{.Id}}']).strip() != BASE.split('@')[1]:
        raise RuntimeError('runtime_image_identity_mismatch')
    build.capture(['docker', 'tag', BASE, LOCAL_BASE])
    if build.capture(['docker', 'image', 'inspect', LOCAL_BASE, '--format', '{{.Id}}']).strip() != BASE.split('@')[1]:
        raise RuntimeError('runtime_local_tag_identity_mismatch')
    build.run_logged(['docker', 'build', '--pull=false', '--network=none', '--progress=plain',
        '-f', str(ROOT / 'deploy/gm-admin/mail-Dockerfile'), '-t', image, str(directory)],
        cwd=ROOT, env=os.environ.copy(), log=directory / ('docker-package.log' if resume else 'docker-build.log'), timeout=180)
    image_id = build.capture(['docker', 'image', 'inspect', image, '--format', '{{.Id}}']).strip()
    build.write_json(OUT / 'image.json', {'image': image, 'imageId': image_id, 'base': BASE,
        'binarySha256': build.sha256(binary), 'sourceCommit': commit, 'sourceDirty': True,
        'resources': resources, 'artifacts': artifacts, 'builtAt': stamp})
    print(json.dumps({'status': 'built', 'image': image, 'imageId': image_id}))

if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        # 子命令细节已留在本任务日志；不回显部署环境或异常携带的原始配置。
        reason = str(error)
        print(json.dumps({'status': 'failed', 'kind': type(error).__name__,
                          'reason': reason if re.fullmatch('[a-z_]+', reason) else 'see_build_log'}), file=sys.stderr)
        sys.exit(1)
