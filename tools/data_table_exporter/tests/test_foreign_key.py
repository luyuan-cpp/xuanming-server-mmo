"""外键校验回归测试。

重点是**数据层**:旧实现只比对目标表/列是否存在,配表填了个不存在的 ID 照样静默导出,
错误一路漏到运行时(C++/Go 的 FindById 返回空)。
"""

from __future__ import annotations

from conftest import Field, TableSpec, build_tables
from core.foreign_key import validate_foreign_keys


def _reward(ids: list[int]) -> TableSpec:
    return TableSpec(
        name="Reward",
        fields=[Field(name="id", options="key"), Field(name="item_id")],
        rows=[[i, 100 + i] for i in ids],
    )


def _mission(reward_refs: list[int], fk: str = "fk:Reward") -> TableSpec:
    return TableSpec(
        name="Mission",
        fields=[Field(name="id", options="key"), Field(name="reward_id", options=fk)],
        rows=[[i + 1, ref] for i, ref in enumerate(reward_refs)],
    )


# ---------------------------------------------------------------------------
# 数据层
# ---------------------------------------------------------------------------

def test_valid_references_pass(cfg):
    tables = build_tables(cfg, [_reward([1, 2, 3]), _mission([1, 2, 3])])

    report = validate_foreign_keys(tables, cfg)

    assert report.ok
    assert report.errors == []


def test_dangling_value_is_error(cfg):
    """引用了目标表里不存在的 ID → errors,导表必须中止。

    这是本次修复的核心回归点:旧实现只看表/列存不存在,这里会一条不报。
    """
    tables = build_tables(cfg, [_reward([1, 2, 3]), _mission([1, 99])])

    report = validate_foreign_keys(tables, cfg)

    assert not report.ok
    assert len(report.errors) == 1
    assert "99" in report.errors[0]
    assert "Mission.reward_id" in report.errors[0]


def test_zero_means_no_reference(cfg):
    """0 是"不引用"的约定值,不能报错。"""
    tables = build_tables(cfg, [_reward([1, 2]), _mission([0, 1, 0])])

    report = validate_foreign_keys(tables, cfg)

    assert report.ok


def test_empty_cell_means_no_reference(cfg):
    tables = build_tables(cfg, [_reward([1, 2]), _mission([None, 2])])

    report = validate_foreign_keys(tables, cfg)

    assert report.ok


def test_all_dangling_values_reported_once_per_column(cfg):
    """同一列多个坏值合并成一条错误,并把坏值都列出来。"""
    tables = build_tables(cfg, [_reward([1]), _mission([7, 8, 8, 9])])

    report = validate_foreign_keys(tables, cfg)

    assert len(report.errors) == 1
    for bad in ("7", "8", "9"):
        assert bad in report.errors[0]


def test_repeated_group_foreign_key_values_checked(cfg):
    """gfk 打在 repeated 列上时,整组取值都要查。"""
    condition = TableSpec(
        name="Condition",
        fields=[Field(name="id", options="key")],
        rows=[[1], [2], [3]],
    )
    mission = TableSpec(
        name="Mission",
        fields=[
            Field(name="id", options="key"),
            Field(name="condition_id", type="repeated uint32", options="gfk:Condition", span=3),
        ],
        rows=[[1, 1, 2, 0], [2, 3, 77, 0]],
    )
    tables = build_tables(cfg, [condition, mission])

    report = validate_foreign_keys(tables, cfg)

    assert not report.ok
    assert "77" in report.errors[0]
    assert "Mission.condition_id" in report.errors[0]


def test_reference_to_non_id_column(cfg):
    """fk:Table.column 走目标表的指定列,不是 id 列。"""
    target = TableSpec(
        name="Reward",
        fields=[Field(name="id", options="key"), Field(name="code")],
        rows=[[1, 500], [2, 501]],
    )
    src = TableSpec(
        name="Mission",
        fields=[Field(name="id", options="key"), Field(name="reward_code", options="fk:Reward.code")],
        rows=[[1, 500], [2, 999]],
    )
    tables = build_tables(cfg, [target, src])

    report = validate_foreign_keys(tables, cfg)

    assert not report.ok
    assert "999" in report.errors[0]
    assert "Reward.code" in report.errors[0]


# ---------------------------------------------------------------------------
# 结构层
# ---------------------------------------------------------------------------

def test_missing_target_table_is_error(cfg):
    tables = build_tables(cfg, [_mission([1], fk="fk:NoSuchTable")])

    report = validate_foreign_keys(tables, cfg)

    assert not report.ok
    assert "NoSuchTable" in report.errors[0]


def test_missing_target_column_is_error(cfg):
    tables = build_tables(cfg, [_reward([1]), _mission([1], fk="fk:Reward.no_such_col")])

    report = validate_foreign_keys(tables, cfg)

    assert not report.ok
    assert "no_such_col" in report.errors[0]


def test_empty_target_table_only_warns(cfg):
    """目标表没数据 = 无法校验,不是校验失败,只告警放行。"""
    tables = build_tables(cfg, [_reward([]), _mission([1])])

    report = validate_foreign_keys(tables, cfg)

    assert report.ok
    assert len(report.warnings) == 1
    assert "跳过数据层校验" in report.warnings[0]
