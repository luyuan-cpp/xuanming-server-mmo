"""RankedRule 的外键跨表查询助手（自动生成，请勿手改）。

DO NOT EDIT — regenerate from Excel via Data Table Exporter.

这里只做一件事：把 RankedRule 行里存的**目标表 id** 解析成目标表的行。
解析结果和源行一样属于「当前快照」，同样的热更契约：**只存 id，不存行**。

放在独立模块而不是塞进 rankedrule_table.py，是因为它要 import 目标表的
管理器；写在一起时两张互相引用的表就会在 import 期成环。
"""

from __future__ import annotations

import rankedrule_table_pb2 as _pb
import class_table_pb2 as _pb_class
import dungeon_table_pb2 as _pb_dungeon

from .rankedrule_table import RankedRuleTableManager
from .class_table import ClassTableManager
from .dungeon_table import DungeonTableManager

Row = _pb.RankedRuleTable


def get_battle_config_id_row(row: Row) -> _pb_dungeon.DungeonTable | None:
    """解析 RankedRule.battle_config_id -> Dungeon 行；解析不到返回 ``None``。"""
    return DungeonTableManager.instance().find_by_id(row.battle_config_id)


def get_battle_config_id_row_by_id(table_id: int) -> _pb_dungeon.DungeonTable | None:
    """同上，入参换成 RankedRule 自己的 id。"""
    row = RankedRuleTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_battle_config_id_row(row)


def get_reference_class_id_row(row: Row) -> _pb_class.ClassTable | None:
    """解析 RankedRule.reference_class_id -> Class 行；解析不到返回 ``None``。"""
    return ClassTableManager.instance().find_by_id(row.reference_class_id)


def get_reference_class_id_row_by_id(table_id: int) -> _pb_class.ClassTable | None:
    """同上，入参换成 RankedRule 自己的 id。"""
    row = RankedRuleTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_reference_class_id_row(row)


# ---- 反向外键（HasMany）：按外键列的值反查源表的行 ----


def find_rows_by_battle_config_id(key: int) -> tuple[Row, ...]:
    """反查：全部 battle_config_id == key 的 RankedRule 行。走的是已建好的二级索引。"""
    return RankedRuleTableManager.instance().get_by_battle_config_id(key)


def find_rows_by_reference_class_id(key: int) -> tuple[Row, ...]:
    """反查：全部 reference_class_id == key 的 RankedRule 行。走的是已建好的二级索引。"""
    return RankedRuleTableManager.instance().get_by_reference_class_id(key)
