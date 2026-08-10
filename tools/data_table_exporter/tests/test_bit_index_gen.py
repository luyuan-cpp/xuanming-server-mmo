"""位序生成器回归测试。

守两条红线(说明见 core/generators/bit_index_gen.py 模块 docstring):
1. 位序只增不改、**永不复用**——删 ID 再加 ID 不能让新 ID 捡走旧位。
2. 位序状态文件 **fail-closed**——缺失 / 损坏一律中止,不许静默重建。
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from conftest import Field, TableSpec, build_tables
from core.generators.bit_index_gen import BitIndexStateError, generate_bit_indexes


def _mission_spec(ids: list[int]) -> TableSpec:
    """一张带 bit_index 标记的最小表(bit_index 打在 id 列上,与真实 Mission 表一致)。"""
    return TableSpec(
        name="Mission",
        fields=[
            Field(name="id", options="key bit_index"),
            Field(name="reward_id"),
        ],
        rows=[[i, 1] for i in ids],
    )


def _mapping_path(cfg) -> Path:
    return cfg.state_dir / "mapping" / "table_index_mapping" / "mission_mapping.json"


def _write_mapping(cfg, mapping: dict[int, int]) -> Path:
    path = _mapping_path(cfg)
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        json.dump({str(k): v for k, v in mapping.items()}, f, indent=4)
    return path


def _read_mapping(cfg) -> dict[int, int]:
    with open(_mapping_path(cfg), "r", encoding="utf-8") as f:
        return {int(k): v for k, v in json.load(f).items()}


# ---------------------------------------------------------------------------
# 红线 1:位序永不复用
# ---------------------------------------------------------------------------

def test_existing_assignments_are_preserved(cfg):
    """已分配的位一个都不能动——状态文件是权威。"""
    _write_mapping(cfg, {1: 0, 2: 1, 3: 2})
    tables = build_tables(cfg, [_mission_spec([1, 2, 3])])

    generate_bit_indexes(cfg, tables)

    assert _read_mapping(cfg) == {1: 0, 2: 1, 3: 2}


def test_removed_id_keeps_its_bit_as_placeholder(cfg):
    """表里删掉 ID 2 之后,它在状态文件里的条目必须原样保留当占位。"""
    _write_mapping(cfg, {1: 0, 2: 1, 3: 2})
    tables = build_tables(cfg, [_mission_spec([1, 3])])

    generate_bit_indexes(cfg, tables)

    assert _read_mapping(cfg) == {1: 0, 2: 1, 3: 2}


def test_new_id_never_takes_a_freed_bit(cfg):
    """状态文件里出现空位(有人把退休 ID 的条目删了)时,新 ID 也绝不能去填。

    这是本次修复的核心回归点。旧实现按 docstring 写的
    "reusing gaps from removed IDs" —— ``unused.pop(0)`` 会把空出来的位 1 分给 ID 4,
    于是所有"已完成任务 2"的存量玩家会凭空变成"已完成任务 4"。
    """
    _write_mapping(cfg, {1: 0, 3: 2})  # 位 1 是 ID 2 退休后被人工清理留下的空洞
    tables = build_tables(cfg, [_mission_spec([1, 3, 4])])

    generate_bit_indexes(cfg, tables)

    mapping = _read_mapping(cfg)
    assert mapping[4] == 3, "新 ID 必须追加到最大位之后,不能捡走空位 1"
    assert mapping == {1: 0, 3: 2, 4: 3}


def test_multiple_gaps_are_never_reclaimed(cfg):
    """多个空洞 + 多个新 ID:全部往后排,空位一个都不回收。"""
    _write_mapping(cfg, {1: 0, 5: 4})  # 位 1/2/3 都是空洞
    tables = build_tables(cfg, [_mission_spec([1, 5, 6, 7])])

    generate_bit_indexes(cfg, tables)

    mapping = _read_mapping(cfg)
    assert mapping[6] == 5
    assert mapping[7] == 6
    assert sorted(mapping.values()) == [0, 4, 5, 6]


def test_bitset_capacity_covers_retired_top_id(cfg):
    """最大 ID 被删后,位图容量仍按已分配位数算,不能缩水。

    ``_find_max_bit`` 只看当前表数据,ID 5 删掉后它算出 4,而位 4 仍被退休的 ID 5 占着,
    ``std::bitset<4>`` 就会放不下位 4。
    """
    _write_mapping(cfg, {1: 0, 2: 1, 3: 2, 4: 3, 5: 4})
    tables = build_tables(cfg, [_mission_spec([1, 2, 3, 4])])

    generate_bit_indexes(cfg, tables)

    header = (cfg.cpp.bit_index_dir / "mission_table_id_bit_index.h").read_text(encoding="utf-8")
    assert "kMissionMaxBitIndex = 5" in header


# ---------------------------------------------------------------------------
# 红线 2:状态文件 fail-closed
# ---------------------------------------------------------------------------

def test_missing_state_file_aborts(cfg):
    """状态文件不存在 → 中止,并提示从 git 恢复。"""
    tables = build_tables(cfg, [_mission_spec([1, 2, 3])])

    with pytest.raises(BitIndexStateError) as exc:
        generate_bit_indexes(cfg, tables)
    assert "git" in str(exc.value)


def test_missing_state_file_with_bootstrap_initializes(cfg):
    """只有显式 --bitindex-bootstrap 才允许从空开始。"""
    cfg.bit_index_bootstrap = True
    tables = build_tables(cfg, [_mission_spec([10, 20, 30])])

    generate_bit_indexes(cfg, tables)

    assert _read_mapping(cfg) == {10: 0, 20: 1, 30: 2}


def test_corrupt_state_file_aborts(cfg):
    """状态文件解析失败 → 中止(旧实现是 except: pass 后整表重排)。"""
    path = _mapping_path(cfg)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("{ this is not json", encoding="utf-8")
    tables = build_tables(cfg, [_mission_spec([1, 2, 3])])

    with pytest.raises(BitIndexStateError):
        generate_bit_indexes(cfg, tables)


def test_corrupt_state_file_not_rewritten(cfg):
    """中止时状态文件必须原样留着,方便人工/该 git 恢复。"""
    path = _mapping_path(cfg)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("{ this is not json", encoding="utf-8")
    tables = build_tables(cfg, [_mission_spec([1, 2, 3])])

    with pytest.raises(BitIndexStateError):
        generate_bit_indexes(cfg, tables)
    assert path.read_text(encoding="utf-8") == "{ this is not json"


def test_non_integer_entry_aborts(cfg):
    _write_mapping(cfg, {1: 0})
    _mapping_path(cfg).write_text('{"1": "abc"}', encoding="utf-8")
    tables = build_tables(cfg, [_mission_spec([1])])

    with pytest.raises(BitIndexStateError):
        generate_bit_indexes(cfg, tables)


def test_duplicated_bit_in_state_aborts(cfg):
    """两个 ID 占同一位 = 历史上被复用过,必须人工处理而不是继续导。"""
    _write_mapping(cfg, {1: 0, 2: 0})
    tables = build_tables(cfg, [_mission_spec([1, 2])])

    with pytest.raises(BitIndexStateError):
        generate_bit_indexes(cfg, tables)


# ---------------------------------------------------------------------------
# 产出稳定性
# ---------------------------------------------------------------------------

def test_state_file_written_with_lf(cfg):
    """状态文件用 LF 落盘:仓库里是 LF,Windows 默认换行会让每次导表都制造整文件 diff。"""
    _write_mapping(cfg, {1: 0})
    tables = build_tables(cfg, [_mission_spec([1, 2])])

    generate_bit_indexes(cfg, tables)

    assert b"\r\n" not in _mapping_path(cfg).read_bytes()


def test_generated_sources_follow_bit_order(cfg):
    """C++ / Go 产物按位序升序输出,和状态文件同一个口径。"""
    _write_mapping(cfg, {3: 2, 1: 0, 2: 1})  # 故意乱序,产物仍须按位序升序
    tables = build_tables(cfg, [_mission_spec([1, 3, 4])])

    generate_bit_indexes(cfg, tables)

    header = (cfg.cpp.bit_index_dir / "mission_table_id_bit_index.h").read_text(encoding="utf-8")
    assert header.index("{ 1, 0 }") < header.index("{ 2, 1 }") < header.index("{ 4, 3 }")

    go_src = (cfg.go.bit_index_dir / "mission" / "mission_table_id_bit_index.go").read_text(
        encoding="utf-8"
    )
    assert "package mission" in go_src
    assert "ID_4 = 3" in go_src
    assert "MaxBitIndex = 4" in go_src
