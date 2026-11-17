"""显式启动无玩家资产副作用的真实 Scene 验收；缺少任一就绪证明就不发送请求。"""
from __future__ import annotations

import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
from datetime import datetime, timezone

import run_local

ROOT = Path(__file__).resolve().parents[2]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--execute', action='store_true')
    args = parser.parse_args()
    if not args.execute:
        print('Prepared only. Use --execute after verified Scene and RPC bridge readiness; no RPC was sent.')
        return 0
    game = run_local.verified_game_runtime()
    if game.get('GM_SCENE_TARGETS_JSON') != '{"1":"127.0.0.1:2355"}' or len(game.get('GM_SCENE_ADMIN_SECRET', '')) < 32:
        raise RuntimeError('Verified Scene runtime is required')
    path = ROOT / 'deploy/gm-admin/rpc-bridge.py'
    spec = importlib.util.spec_from_file_location('gm_probe_rpc_bridge', path)
    bridge = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = bridge
    spec.loader.exec_module(bridge)
    endpoints = bridge.verify_ready()
    if endpoints != {'GM_DATA_TARGET': '127.0.0.1:2356', 'GM_LOCATOR_TARGET': '127.0.0.1:2357'}:
        raise RuntimeError('Verified RPC bridge endpoints are required')
    # 错误码来自导表产物；不在探针复制一个长期漂移的手写 tip 数字。
    tip_source = ROOT / 'generated/code/proto/tip/common_error_tip.proto'
    matches = re.findall(r'\bkFeatureUnavailable\s*=\s*(\d+)\s*;', tip_source.read_text('utf-8-sig'))
    if len(matches) != 1:
        raise RuntimeError('Authoritative feature-unavailable tip is missing')
    stamp = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S%fZ')
    out = ROOT / 'build/verification/gm-admin-20261117/scene/probes' / stamp
    out.mkdir(parents=True, exist_ok=False)
    env = os.environ.copy()
    env.update(game)
    env.update(endpoints)
    env.update({'GM_SCENE_PROBE_READY_VERIFIED': 'true', 'GM_SCENE_PROBE_TARGET': '127.0.0.1:2355',
                'GM_SCENE_PROBE_EXPECTED_TIP': matches[0], 'GM_SCENE_PROBE_EVIDENCE': str(out / 'result.json')})
    maven = shutil.which('mvn') or shutil.which('mvn.cmd')
    if not maven:
        raise RuntimeError('Existing Maven installation is required')
    command = [maven, '-q', '-Dtest=SceneRuntimeProbeIT', '-Dgm.scene.liveProbe=true', 'test']
    if os.name == 'nt' and Path(maven).suffix.lower() in ('.cmd', '.bat'):
        command = ['cmd.exe', '/d', '/c', 'call', *command]
    # 只执行一次；失败或未知结果不自动重发 Scene 请求。
    result = subprocess.run(command, cwd=ROOT / 'java/gm_admin', env=env, capture_output=True,
                            text=True, encoding='utf-8', errors='replace', timeout=180)
    log = result.stdout + result.stderr
    for key in ('GM_SCENE_ADMIN_SECRET', 'GM_MAIL_ADMIN_SECRET'):
        if game.get(key):
            log = log.replace(game[key], '[REDACTED]')
    (out / 'maven.log').write_text(log, encoding='utf-8')
    if result.returncode != 0 or not (out / 'result.json').is_file():
        print('Scene negative probe failed; inspect the local evidence directory: ' + str(out))
        return 1
    evidence = json.loads((out / 'result.json').read_text('utf-8'))
    if evidence.get('passed') is not True or evidence.get('executed') is not False:
        raise RuntimeError('Scene negative probe evidence is incomplete')
    print('Scene authentication and absent-player rejection verified; no player mutation was performed. Evidence: ' + str(out))
    return 0


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Exception:
        print('Scene negative probe stopped; readiness or execution failed. Sensitive details suppressed.')
        raise SystemExit(1)
