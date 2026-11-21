"""产物批次清单(manifest.json)测试。

清单存在的意义是让 Go / C++ 两端能发现自己加载的表"半新半旧",
所以三件事必须成立:每张表都在册、指纹能对上、版本号只在内容变了时才 +1。
"""

from __future__ import annotations

import hashlib
import json
from pathlib import Path

from conftest import Field, TableSpec, build_tables
from core.generators.json_gen import generate_json
from core.generators.config_gen import generate_config_classes
from core.manifest import MANIFEST_FILENAME, MANIFEST_SCHEMA_VERSION, generate_manifest
from core.schema import compute_schema_digest


def _specs() -> list[TableSpec]:
    return [
        TableSpec(
            name="Reward",
            fields=[Field(name="id", options="key"), Field(name="item_id")],
            rows=[[1, 101], [2, 102]],
        ),
        TableSpec(
            name="Mission",
            fields=[Field(name="id", options="key"), Field(name="reward_id")],
            rows=[[1, 1], [2, 2], [3, 1]],
        ),
    ]


def _export(cfg, specs: list[TableSpec]):
    tables = build_tables(cfg, specs)
    generate_json(cfg, tables)
    generate_manifest(cfg, tables)
    return tables


def _read_manifest(cfg) -> dict:
    with open(cfg.json_dir / MANIFEST_FILENAME, "r", encoding="utf-8") as f:
        return json.load(f)


def test_manifest_covers_every_table(cfg):
    _export(cfg, _specs())

    manifest = _read_manifest(cfg)

    assert manifest["schema_version"] == MANIFEST_SCHEMA_VERSION
    assert manifest["table_count"] == 2
    assert [t["name"] for t in manifest["tables"]] == ["Mission", "Reward"]


def test_manifest_records_row_count_and_sha256(cfg):
    _export(cfg, _specs())

    manifest = _read_manifest(cfg)
    mission = next(t for t in manifest["tables"] if t["name"] == "Mission")

    assert mission["rows"] == 3
    assert mission["source"]["file"] == "Mission.xlsx"
    assert len(mission["source"]["sha256"]) == 64

    art = next(a for a in mission["artifacts"] if a["kind"] == "json")
    data = (cfg.json_dir / "mission.json").read_bytes()
    assert art["sha256"] == hashlib.sha256(data).hexdigest()
    assert art["size"] == len(data)


def test_artifact_paths_are_relative_to_manifest(cfg):
    _export(cfg, _specs())

    manifest = _read_manifest(cfg)
    for table in manifest["tables"]:
        for art in table["artifacts"]:
            assert not Path(art["file"]).is_absolute()
            assert (cfg.json_dir / art["file"]).is_file()


def test_version_stable_when_content_unchanged(cfg):
    tables = _export(cfg, _specs())
    first = _read_manifest(cfg)

    generate_json(cfg, tables)
    generate_manifest(cfg, tables)
    second = _read_manifest(cfg)

    assert second["version"] == first["version"] == 1
    assert second["content_digest"] == first["content_digest"]
    assert second["generated_at"] == first["generated_at"], "内容没变就不该重写清单"


def test_version_bumps_when_content_changes(cfg):
    _export(cfg, _specs())
    assert _read_manifest(cfg)["version"] == 1

    changed = _specs()
    changed[1].rows.append([4, 2])
    _export(cfg, changed)

    manifest = _read_manifest(cfg)
    assert manifest["version"] == 2
    mission = next(t for t in manifest["tables"] if t["name"] == "Mission")
    assert mission["rows"] == 4


def test_version_is_monotonic_across_many_changes(cfg):
    versions = []
    for extra in range(3):
        specs = _specs()
        specs[1].rows.extend([[10 + i, 1] for i in range(extra + 1)])
        _export(cfg, specs)
        versions.append(_read_manifest(cfg)["version"])

    assert versions == sorted(versions)
    assert versions == [1, 2, 3]


def test_corrupt_previous_manifest_does_not_abort(cfg):
    """清单读坏了不该阻断导表——它是元数据,不是位序那种权威状态。"""
    tables = build_tables(cfg, _specs())
    generate_json(cfg, tables)
    (cfg.json_dir / MANIFEST_FILENAME).write_text("not json", encoding="utf-8")

    generate_manifest(cfg, tables)

    assert _read_manifest(cfg)["version"] == 1


def test_json_overwrites_stale_rows_when_table_becomes_empty(cfg):
    """合法空表必须产出空数组，不能静默沿用上一批非空 JSON。"""
    tables = build_tables(cfg, [TableSpec(
        name="Permission",
        fields=[Field(name="id"), Field(name="result", options="tip_ref")],
        rows=[[1, 1000]],
    )])
    generate_json(cfg, tables)

    empty_tables = build_tables(cfg, [TableSpec(
        name="Permission",
        fields=[Field(name="id"), Field(name="result", options="tip_ref")],
        rows=[],
    )])
    generate_json(cfg, empty_tables)

    payload = json.loads((cfg.json_dir / "permission.json").read_text(encoding="utf-8"))
    assert payload == {"data": []}


def test_manifest_v2_includes_schema_digest_and_table_flags(cfg):
    tables = build_tables(cfg, _specs())
    tables[0].hot_reload = True
    tables[1].ids_permanent = True
    generate_json(cfg, tables)
    generate_manifest(cfg, tables)

    manifest = _read_manifest(cfg)
    assert manifest["schema_version"] == 2
    digest = manifest["schema_digest"]
    assert len(digest) == 16
    assert digest == compute_schema_digest(tables)

    by_name = {t["name"]: t for t in manifest["tables"]}
    assert by_name["Reward"]["hot_reload"] is True
    assert by_name["Reward"]["ids_permanent"] is False
    assert by_name["Mission"]["hot_reload"] is False
    assert by_name["Mission"]["ids_permanent"] is True


def test_schema_digest_ignores_non_layout_metadata_and_client_columns(cfg):
    base_tables = build_tables(cfg, _specs())
    base_digest = compute_schema_digest(base_tables)

    # 改 hot_reload、ids_permanent、列注释、索引选项、纯客户端列都不应改变 schema_digest。
    mutated = build_tables(cfg, [
        TableSpec(
            name="Reward",
            fields=[
                Field(name="id", options="key", comment="changed comment"),
                Field(name="item_id", options="idx"),
                Field(name="client_tip", data_type="string", owner="client"),
            ],
            rows=[[1, 101, "x"]],
        ),
        TableSpec(
            name="Mission",
            fields=[Field(name="id", options="key"), Field(name="reward_id")],
            rows=[[1, 1]],
        ),
    ])
    mutated[0].hot_reload = True
    mutated[0].ids_permanent = True
    assert compute_schema_digest(mutated) == base_digest

    # 改服务端字段类型必须改变 schema_digest。
    changed_type = build_tables(cfg, [
        TableSpec(
            name="Reward",
            fields=[Field(name="id", options="key"), Field(name="item_id", data_type="uint64")],
            rows=[[1, 101]],
        ),
        TableSpec(
            name="Mission",
            fields=[Field(name="id", options="key"), Field(name="reward_id")],
            rows=[[1, 1]],
        ),
    ])
    assert compute_schema_digest(changed_type) != base_digest


def test_generated_loaders_embed_schema_digest_and_hot_reload_list(cfg):
    tables = build_tables(cfg, _specs())
    tables[0].hot_reload = True
    cfg.cpp.enabled = True
    cfg.go.enabled = True
    cfg.java.enabled = True
    generate_config_classes(cfg, tables)

    digest = compute_schema_digest(tables)
    cpp_h = (cfg.cpp.code_dir / "table_loader.h").read_text(encoding="utf-8")
    cpp_c = (cfg.cpp.code_dir / "table_loader.cpp").read_text(encoding="utf-8")
    go_loader = (cfg.go.code_dir / "table_loader.go").read_text(encoding="utf-8")
    java_loader = (cfg.java.code_dir / "TableLoader.java").read_text(encoding="utf-8")

    assert digest in cpp_h
    assert "struct TableSet" in cpp_h
    assert "ScopedTableSetOverride" in cpp_h
    assert '"Reward"' in cpp_c and "IsTableHotReloadable" in cpp_c
    assert f'SchemaDigest = "{digest}"' in go_loader
    assert "func Prepare(" in go_loader and "func Commit(" in go_loader
    assert "func ReloadTables(" not in go_loader
    assert f'SCHEMA_DIGEST = "{digest}"' in java_loader
    assert '"Reward"' in java_loader
