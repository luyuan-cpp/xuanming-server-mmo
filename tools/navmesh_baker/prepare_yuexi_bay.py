#!/usr/bin/env python3
"""用客户端双节正式数据生成隔离的月汐湾场景3导航与导表产物。"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import math
from pathlib import Path
import shutil
import subprocess

import openpyxl

from prepare_festival_test_tables import export, sha

ROOT = Path(__file__).resolve().parents[2]
REVISION = "yuexi-bay-20261115"
YUEXI_NAV_PATH = "data/scene_nav_bin/current_maps/village/yuexi-bay-20261115/scene.bin"
NAV_PATH = Path(YUEXI_NAV_PATH)
NAV_FIELDS = ("worldRect", "effectiveWorldRect", "imageRect", "cellSize",
              "walkmaskColumns", "walkmaskRows", "spawnWorld")


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def require(condition, message):
    if not condition:
        raise ValueError(message)


def read_json(path):
    return json.loads(path.read_bytes())


def check_source(client, appearance):
    resources = client / "Assets/Resources"
    catalog = read_json(resources / "World/QdaoMaps20261102/catalog.json")
    entry = next(m for m in catalog["maps"] if m["sceneId"] == 3 and m["key"] == "village")
    require(entry["revision"] == REVISION, "Client scene 3 revision mismatch")
    ap = next(a for a in entry["appearances"] if a["id"] == appearance)
    folder = (resources / ap["resourceRoot"]).resolve()
    require(folder.is_relative_to(resources.resolve()), "Client asset path escapes Assets/Resources")
    source = read_json(folder / "source.json")
    require(source["schemaVersion"] == 2 and source["key"] == "village" and source["sceneId"] == 3
            and source["revision"] == REVISION and source["appearance"] == appearance
            and source["resourceRoot"] == ap["resourceRoot"]
            and source["layoutSha256"] == ap["layoutSha256"], "Client source identity mismatch")
    for name, key in (("layout.png", "layoutSha256"), ("walkmask.txt", "walkmaskSha256"),
                      ("navigation.json", "navigationSha256")):
        require(sha(folder / name) == source[key], f"Client source digest mismatch: {appearance}/{name}")
    plan = (client / source["navigationPlanPath"]).resolve()
    require(plan.is_relative_to(client) and sha(plan) == source["navigationPlanSha256"],
            "Client navigation plan digest mismatch")
    require(source["maskEncoding"] == "base64-msb-first-row-major-top-to-bottom",
            "Unsupported client mask format")
    packed = base64.b64decode((folder / "walkmask.txt").read_bytes().strip(), validate=True)
    count = source["walkmaskColumns"] * source["walkmaskRows"]
    require(digest(packed) == source["walkmaskPackedSha256"] and len(packed) == (count + 7) // 8
            and not (packed[-1] & ((1 << (-count % 8)) - 1))
            and sum(value.bit_count() for value in packed) == source["walkableCellCount"],
            "Client mask digest, size, padding or count mismatch")
    nav = read_json(folder / "navigation.json")
    require(all(nav["source"][field] == source[field] for field in NAV_FIELDS)
            and nav["navigationPlanSha256"] == source["navigationPlanSha256"]
            and nav["stats"]["maskSha256"] == source["walkmaskPackedSha256"],
            "Client navigation review does not bind the delivered geometry")
    return folder, source, nav, plan


def run(command, log):
    result = subprocess.run([str(part) for part in command], capture_output=True,
                            text=True, encoding="utf-8", timeout=180)
    log.write_text(result.stdout + result.stderr, encoding="utf-8")
    require(result.returncode == 0, f"Command failed ({result.returncode}); see {log}")
    return result.stdout


def csv(values):
    return ",".join(map(str, values))


def vec(point):
    return [point[axis] for axis in ("x", "y", "z")]


def rect(value):
    return [value[field] for field in ("x", "y", "width", "height")]


def bake(folder, source, nav, plan, output, baker, query):
    destination = output / NAV_PATH.parent
    destination.mkdir(parents=True)
    for name in ("source.json", "walkmask.txt", "navigation.json"):
        shutil.copy2(folder / name, destination / name)
    scene_bin = destination / NAV_PATH.name
    spawn = vec(source["spawnWorld"])
    stats = nav["stats"]
    columns, rows = source["walkmaskColumns"], source["walkmaskRows"]
    world = rect(source["worldRect"])
    command = [baker, "--mask-base64", destination / "walkmask.txt",
               "--mask-grid", csv([columns, rows, source["cellSize"], *world]),
               "--effective-rect", csv(rect(source["effectiveWorldRect"])),
               "--cs", "0.0625", "--min-region-area", "0",
               "--probe", csv(spawn), "--out", scene_bin]
    text = run(command, output / "bake.log")
    require(f"mask cells checked={columns * rows} mismatches=0" in text
            and f"mask connectivity components=1 reachable_cells={source['walkableCellCount']} "
            "cross_component_bridges=0" in text, "Baker did not verify exact grid and connectivity")
    legal = {name: vec(record["world"]) for name, record in stats["landmarks"].items()}
    spec = read_json(plan)["maps"]["3"]
    width, height = spec["canvasPixels"]
    blocked = {name: [world[0] + x * world[2] / width, 0, world[1] + (height - y) * world[3] / height]
               for name, (x, y) in spec["blockedLandmarkPixels"].items()}
    require(set(blocked) == set(stats["blockedLandmarks"]), "Client blockers do not match review")
    query_args = [query, "--nav", scene_bin]
    for position in [*legal.values(), *blocked.values()]:
        query_args.extend(["--point", csv(position)])
    for begin, end in stats["connectedRoutes"]:
        query_args.extend(["--route", csv(legal[begin] + legal[end])])
    points = json.loads(run(query_args, output / "runtime-query.json"))
    require([p["onMesh"] for p in points["points"]] == [True] * len(legal) + [False] * len(blocked),
            "Final MSET admits a blocked landmark or rejects a walkable one")
    require(all(r["complete"] for r in points["routes"])
            and len(points["routes"]) == len(stats["connectedRoutes"]),
            "Final MSET rejected a reviewed route")
    spawn_sample = points["points"][list(legal).index("spawn")]["nearest"]
    require(all(abs(a - b) <= .02 for a, b in zip(spawn, spawn_sample, strict=True)),
            "Final MSET changes the intended spawn")
    nav_hash = sha(scene_bin)
    mask_hash = sha(destination / "scene.bin.mask")
    (destination / "scene.bin.sha256").write_bytes(f"{nav_hash}\n{mask_hash}\n".encode("ascii"))
    return dict(spawn=[spawn[2], spawn[0], spawn[1]], rows=rows, columns=columns,
                walkable=source["walkableCellCount"], points=len(legal), blocked=len(blocked),
                completeRoutes=len(points["routes"]), msetSha256=nav_hash, fmaskSha256=mask_hash)


def prepare_tables(output, spawn):
    source_data = ROOT / "data"
    source_base = source_data / "BaseScene.xlsx"
    base_sha = sha(source_base)
    staged = output / "table-source"
    staged.mkdir()
    for path in source_data.glob("*.xlsx"):
        shutil.copy2(path, staged / path.name)
    shutil.copytree(source_data / "schema", staged / "schema")
    workbook = openpyxl.load_workbook(staged / "BaseScene.xlsx")
    sheet = workbook["BaseScene"]
    cols = {c.value: c.column for c in sheet[1]}
    rows = [i for i in range(6, sheet.max_row + 1) if sheet.cell(i, cols["id"]).value == 3]
    require(len(rows) == 1 and sheet.cell(rows[0], cols["nav_bin_file"]).value == NAV_PATH.as_posix(),
            "Unknown scene 3 source table binding")
    table_update = dict(zip(("nav_bin_file", "spawn_x", "spawn_y", "spawn_z"),
                            [NAV_PATH.as_posix(), *spawn], strict=True))
    if any(sheet.cell(rows[0], cols[key]).value != value for key, value in table_update.items()):
        for key, value in table_update.items():
            sheet.cell(rows[0], cols[key]).value = value
        workbook.save(staged / "BaseScene.xlsx")
    workbook.close()
    # 让正式导表器从现有清单版本递增，不能采用无历史沙盒从 1 起步的版本号。
    current_manifest = ROOT / "generated/tables/manifest.json"
    current_manifest_digest = sha(current_manifest)
    for name in ("baseline", "candidate"):
        staging_tables = output / name / "generated/tables"
        staging_tables.mkdir(parents=True)
        shutil.copy2(current_manifest, staging_tables / "manifest.json")
    baseline = export(output / "baseline", source_data, output / "baseline-export.log")
    candidate = export(output / "candidate", staged, output / "candidate-export.log")
    before = {p.name: sha(p) for p in baseline.iterdir() if p.is_file()}
    after = {p.name: sha(p) for p in candidate.iterdir() if p.is_file()}
    changed = {key for key in before.keys() | after.keys() if before.get(key) != after.get(key)}
    require(changed <= {"basescene.json", "basescene.pb", "manifest.json"},
            f"Exporter changed unrelated table outputs: {sorted(changed)}")
    prior, baseline_manifest, candidate_manifest = (
        read_json(path) for path in (current_manifest, baseline / "manifest.json", candidate / "manifest.json"))
    require(prior == baseline_manifest and sha(current_manifest) == current_manifest_digest,
            "Formal generated manifest is stale or changed during staging")
    require(candidate_manifest["version"] == prior["version"] + bool(changed),
            "Candidate manifest does not preserve monotonic versions")
    base_rows = read_json(baseline / "basescene.json")["data"]
    new_rows = read_json(candidate / "basescene.json")["data"]
    require([r for r in base_rows if r["id"] != 3] == [r for r in new_rows if r["id"] != 3],
            "Exporter changed other scene rows")
    row = next(r for r in new_rows if r["id"] == 3)
    require(row["nav_bin_file"] == NAV_PATH.as_posix() and all(
        math.isclose(row["spawn_" + axis], value, rel_tol=0, abs_tol=1e-12)
        for axis, value in zip("xyz", spawn, strict=True)), "Wrong emitted scene 3 binding")
    require(sha(source_base) == base_sha, "Formal source table changed during staging")
    return dict(originalBaseSceneSha256=base_sha, sourceBaseSceneSha256=sha(staged / "BaseScene.xlsx"),
                originalManifestSha256=current_manifest_digest,
                generatedDataSha256={k: after[k] for k in ("basescene.json", "basescene.pb", "manifest.json")},
                changedTableFiles=sorted(changed))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--client-root", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--baker", type=Path, required=True)
    parser.add_argument("--query", type=Path, required=True)
    args = parser.parse_args()
    output = args.output.resolve()
    require(not output.exists() and output.is_relative_to(ROOT / "build"),
            "Use a new output directory under the server build directory")
    spring = check_source(args.client_root.resolve(), "spring")
    autumn = check_source(args.client_root.resolve(), "mid-autumn")
    for field in [*NAV_FIELDS, "walkmaskSha256", "navigationSha256", "navigationPlanSha256"]:
        require(spring[1][field] == autumn[1][field], f"Festival navigation mismatch: {field}")
    output.mkdir(parents=True)
    nav = bake(*spring, output, args.baker.resolve(), args.query.resolve())
    report = dict(revision=REVISION, navPath=NAV_PATH.as_posix(), navigation=nav,
                  springSourceSha256=sha(spring[0] / "source.json"),
                  midAutumnSourceSha256=sha(autumn[0] / "source.json"),
                  tables=prepare_tables(output, nav["spawn"]))
    (output / "yuexi-bay-preparation.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(report))


if __name__ == "__main__":
    main()
