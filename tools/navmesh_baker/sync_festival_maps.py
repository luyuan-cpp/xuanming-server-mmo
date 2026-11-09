#!/usr/bin/env python3
"""校验冻结交接并烘焙九图开发导航；不修改 BaseScene/World，不开放正式入口。"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]
HANDOFF = 'Docs/VerificationEvidence/festival-maps-20261107/server-handoff-data.json'
HANDOFF_SHA = '8c8678bf19ad568feda870834e23ea8fa7ffc2d51fb7ee90139c87e970bf0b51'


def sha(data):
    return hashlib.sha256(data).hexdigest()


def sync(client, baker, output, evidence=None):
    raw = (client / HANDOFF).read_bytes()
    if sha(raw) != HANDOFF_SHA:
        raise ValueError('Frozen handoff SHA mismatch')
    handoff = json.loads(raw)
    if handoff['counts'] != {'mapIds': 9, 'appearances': 15, 'runtimeTiles': 90, 'uniqueNavigationMasks': 9}:
        raise ValueError('Handoff count mismatch')
    records = []
    if evidence is not None:
        evidence.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='festival-nav-') as tmp:
        staging = Path(tmp)
        for item in handoff['maps']:
            map_id, nav = item['paddedMapId'], item['navigation']
            if item['clientOnly'] is not True or item['automaticOnlineActivationAllowed'] is not False:
                raise ValueError('Activation gate changed')
            folder = staging / map_id
            folder.mkdir()
            canonical = next(a for a in item['appearances'] if a['id'] == nav['canonicalAppearance'])
            for appearance in item['appearances']:
                for name, info in appearance['files'].items():
                    data = (client / info['relativePath']).read_bytes()
                    if len(data) != info['bytes'] or sha(data) != info['sha256']:
                        raise ValueError(f'{map_id}/{appearance["id"]}/{name}: resource SHA mismatch')
                if appearance['files']['walkmask.txt']['sha256'] != nav['encodedFileSha256']:
                    raise ValueError('Delivered appearances have different masks')
            for name in ('source.json', 'walkmask.txt', 'navigation.json'):
                (folder / name).write_bytes((client / canonical['files'][name]['relativePath']).read_bytes())
            packed = base64.b64decode((folder / 'walkmask.txt').read_bytes().strip(), validate=True)
            if sha(packed) != nav['packedSha256'] or sum(b.bit_count() for b in packed) != nav['walkableCells']:
                raise ValueError('Packed mask mismatch')
            world, effective, spawn = item['worldRect'], item['effectiveWorldRect'], item['spawnWorld']
            csv = lambda values: ','.join(str(v) for v in values)
            command = [str(baker), '--mask-base64', str(folder / 'walkmask.txt'),
                       '--mask-grid', csv([nav['columns'], nav['rows'], nav['cellSize'],
                           world['x'], world['y'], world['width'], world['height']]),
                       '--effective-rect', csv(effective[k] for k in ('x', 'y', 'width', 'height')),
                       '--probe', csv(spawn[k] for k in ('x', 'y', 'z')),
                       '--out', str(folder / 'scene.bin')]
            result = subprocess.run(command, capture_output=True, text=True, encoding='utf-8', timeout=180)
            (folder / 'bake.log').write_text(result.stdout + result.stderr, encoding='utf-8')
            if evidence is not None:
                (evidence / f'{map_id}-bake.log').write_text(result.stdout + result.stderr, encoding='utf-8')
            expected = f'mask cells checked={nav["totalCells"]} mismatches=0'
            connectivity = f'mask connectivity components={nav["componentCount"]} reachable_cells={nav["walkableCells"]} cross_component_bridges=0'
            if result.returncode or expected not in result.stdout or connectivity not in result.stdout:
                raise RuntimeError(f'{map_id}: bake failed\n{result.stdout}\n{result.stderr}')
            prefix = f'data/scene_nav_bin/festival_maps/{map_id}'
            record = {'paddedMapId': map_id, 'sceneId': item['numericSceneId'],
                      'clientOnly': True, 'productionTravelEnabled': False,
                      'appearances': [a['id'] for a in item['appearances']],
                      'navPath': prefix + '/scene.bin', 'maskPath': prefix + '/scene.bin.mask',
                      'sourceSha256': sha((folder / 'source.json').read_bytes()),
                      'navSha256': sha((folder / 'scene.bin').read_bytes()),
                      'maskSha256': sha((folder / 'scene.bin.mask').read_bytes()),
                      'packedSha256': nav['packedSha256'], 'walkableCells': nav['walkableCells'],
                      'componentSizes': nav['componentSizes'], 'checkedCells': nav['totalCells'],
                      'spawnServer': {'x': spawn['z'], 'y': spawn['x'], 'z': spawn['y']},
                      'restrictions': item['restrictions']}
            records.append(record)
            (folder / 'scene.bin.sha256').write_text(record['navSha256'] + '\n' + record['maskSha256'] + '\n', encoding='ascii', newline='\n')
            print(f'{map_id}: {nav["walkableCells"]} walkable cells, {expected}')
        # 全批成功以后才写目标，失败保留原批次。
        output.mkdir(parents=True, exist_ok=True)
        for record in records:
            target = output / 'festival_maps' / record['paddedMapId']
            target.mkdir(parents=True, exist_ok=True)
            for path in (staging / record['paddedMapId']).iterdir():
                if path.suffix != '.log':
                    shutil.copyfile(path, target / path.name)
        manifest = {'schemaVersion': 1, 'handoffSha256': HANDOFF_SHA,
                    'status': 'DEVELOPMENT_NAVIGATION_ONLY', 'maps': records}
        (output / 'festival_maps_20261109.json').write_text(
            json.dumps(manifest, ensure_ascii=False, indent=2) + '\n', encoding='utf-8', newline='\n')
    return records


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--client-root', type=Path, default=ROOT.parent / 'mmorpg-client')
    parser.add_argument('--baker', type=Path, default=ROOT / 'tools/navmesh_baker/build/Release/navmesh_baker.exe')
    parser.add_argument('--output', type=Path, default=ROOT / 'data/scene_nav_bin')
    parser.add_argument('--evidence-dir', type=Path, default=ROOT / 'build/verification/festival-server-20261109/bake')
    args = parser.parse_args()
    sync(args.client_root.resolve(), args.baker.resolve(), args.output.resolve(), args.evidence_dir.resolve())
