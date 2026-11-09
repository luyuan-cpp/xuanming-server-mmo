#!/usr/bin/env python3
"""从冻结交接和正式源表生成隔离联调配置，不修改正式 BaseScene/World。"""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import shutil
import subprocess
import sys
from pathlib import Path

import openpyxl
import yaml

from sync_festival_maps import HANDOFF_SHA

REPO = Path(__file__).resolve().parents[2]
EXPECTED = {
    "01001": ("spring", "mid-autumn"), "01002": ("spring", "mid-autumn"),
    "05008": ("spring", "mid-autumn"), "10000": ("spring", "mid-autumn"),
    "19004": ("mid-autumn",), "21003": ("spring", "mid-autumn"),
    "23006": ("mid-autumn",), "28007": ("spring",),
    "35004": ("spring", "mid-autumn"),
}


def sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def load_handoff(path: Path) -> dict:
    if sha(path) != HANDOFF_SHA:
        raise ValueError("Frozen handoff SHA mismatch")
    data = json.loads(path.read_text(encoding="utf-8-sig"))
    maps = data["maps"]
    if len(maps) != 9 or {m["paddedMapId"] for m in maps} != set(EXPECTED):
        raise ValueError("Expected exactly nine delivered map IDs")
    for item in maps:
        key = item["paddedMapId"]
        if item["numericSceneId"] != int(key) or item["key"] != "map" + key:
            raise ValueError(f"Map/scene identity mismatch: {key}")
        if tuple(a["id"] for a in item["appearances"]) != EXPECTED[key]:
            raise ValueError(f"Unexpected delivered appearance: {key}")
        if item["clientOnly"] is not True or item["automaticOnlineActivationAllowed"] is not False:
            raise ValueError(f"Handoff activation policy changed: {key}")
    return data


def append_rows(path: Path, additions: list[dict]) -> None:
    book = openpyxl.load_workbook(path)
    sheet = book.active
    columns = [cell.value for cell in sheet[1]]
    ids = {row[0] for row in sheet.iter_rows(min_row=6, values_only=True) if row[0] is not None}
    for row in additions:
        if row["id"] in ids:
            raise ValueError(f"Existing scene ID conflict in {path.name}: {row['id']}")
        if set(row) != set(columns):
            raise ValueError(f"Unsupported source schema in {path.name}")
        sheet.append([row[c] for c in columns])
        ids.add(row["id"])
    book.save(path)


def export(out: Path, data_dir: Path, log: Path) -> Path:
    helper = REPO / "tools/data_table_exporter/tools/sandbox_export.py"
    spec = importlib.util.spec_from_file_location("festival_sandbox_export", helper)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    config = module.build_sandbox_config(out)
    values = yaml.safe_load(config.read_text(encoding="utf-8"))
    values["excel"]["data_dir"] = str(data_dir)
    config.write_text(yaml.safe_dump(values, allow_unicode=True, sort_keys=False), encoding="utf-8")
    with log.open("w", encoding="utf-8") as stream:
        result = subprocess.run([sys.executable, "-X", "utf8", "-B",
                                 str(REPO / "tools/data_table_exporter/run.py"), str(config)],
                                cwd=REPO, stdout=stream, stderr=subprocess.STDOUT)
    if result.returncode:
        raise RuntimeError(f"Official table exporter failed ({result.returncode}); see {log}")
    return out / "generated/tables"


def prepare(client: Path, output: Path) -> dict:
    # 沙盒工具会重建自己的 state/templates；只接受全新、仓内且被 Git 忽略的目录。
    output = output.resolve()
    if not output.is_relative_to((REPO / "build").resolve()) or output == REPO / "build":
        raise ValueError("Output must be a new directory below the repository build directory")
    if output.exists():
        raise ValueError("Output already exists; use a new directory to preserve previous evidence")
    subprocess.run(["git", "check-ignore", "--quiet", str(output / "manifest.json")], cwd=REPO, check=True)
    handoff = client / "Docs/VerificationEvidence/festival-maps-20261107/server-handoff-data.json"
    data = load_handoff(handoff)
    source_hashes = {name: sha(REPO / "data" / name) for name in ("BaseScene.xlsx", "World.xlsx")}
    output.mkdir(parents=True)
    staged = output / "source-data"
    staged.mkdir()
    for path in (REPO / "data").glob("*.xlsx"):
        shutil.copy2(path, staged / path.name)
    shutil.copytree(REPO / "data/schema", staged / "schema")
    base_rows, world_rows, mapping = [], [], []
    for item in data["maps"]:
        key, scene, point = item["paddedMapId"], item["numericSceneId"], item["spawnWorld"]
        nav_path = f"data/scene_nav_bin/festival_maps/{key}/scene.bin"
        base_rows.append(dict(id=scene, nav_bin_file=nav_path,
                              spawn_x=point["z"], spawn_y=point["x"], spawn_z=point["y"]))
        world_rows.append(dict(id=scene, scene_id=scene))
        mapping.append(dict(paddedMapId=key, sceneConfigId=scene, worldId=scene,
                            appearances=list(EXPECTED[key]), navBinFile=nav_path,
                            spawnServer=[point["z"], point["x"], point["y"]],
                            packedMaskSha256=item["navigation"]["packedSha256"],
                            walkableCells=item["navigation"]["walkableCells"],
                            components=item["navigation"]["componentCount"],
                            clientOnly=True, productionTravelEnabled=False))
    append_rows(staged / "BaseScene.xlsx", base_rows)
    append_rows(staged / "World.xlsx", world_rows)
    baseline = export(output / "baseline", REPO / "data", output / "baseline-export.log")
    generated = export(output / "candidate", staged, output / "candidate-export.log")
    before = {p.name: sha(p) for p in baseline.iterdir() if p.is_file()}
    after = {p.name: sha(p) for p in generated.iterdir() if p.is_file()}
    changed = sorted(k for k in before.keys() | after.keys() if before.get(k) != after.get(k))
    allowed = {"basescene.json", "basescene.pb", "world.json", "world.pb", "manifest.json"}
    if set(changed) != allowed:
        raise RuntimeError(f"Unexpected table changes: {changed}")
    if source_hashes != {name: sha(REPO / "data" / name) for name in source_hashes}:
        raise RuntimeError("Formal source tables changed during export")
    report = dict(schemaVersion=1, developmentOnly=True, productionTravelEnabled=False,
                  serverHead=subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=REPO, text=True).strip(),
                  clientHead=subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=client, text=True).strip(),
                  handoffSha256=sha(handoff), formalSourceSha256=source_hashes,
                  generatedTables=str(generated), changedTableFiles=changed,
                  tableSha256={k: after[k] for k in changed}, maps=mapping,
                  liveServerVerified=False, unityVerified=False)
    (output / "festival-test-tables.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return report


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--client-root", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    result = prepare(args.client_root.resolve(), args.output)
    print(json.dumps({"result": "PASS", "maps": len(result["maps"]),
                      "tables": result["generatedTables"], "developmentOnly": True}))


if __name__ == "__main__":
    main()
