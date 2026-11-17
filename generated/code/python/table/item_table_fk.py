"""Item 的外键跨表查询助手（自动生成，请勿手改）。

DO NOT EDIT — regenerate from Excel via Data Table Exporter.

这里只做一件事：把 Item 行里存的**目标表 id** 解析成目标表的行。
解析结果和源行一样属于「当前快照」，同样的热更契约：**只存 id，不存行**。

放在独立模块而不是塞进 item_table.py，是因为它要 import 目标表的
管理器；写在一起时两张互相引用的表就会在 import 期成环。
"""

from __future__ import annotations

import item_table_pb2 as _pb
import equipaffixrule_table_pb2 as _pb_equipaffixrule
import equipartifact_table_pb2 as _pb_equipartifact
import item_table_pb2 as _pb_item

from .item_table import ItemTableManager
from .equipaffixrule_table import EquipAffixRuleTableManager
from .equipartifact_table import EquipArtifactTableManager
from .item_table import ItemTableManager

Row = _pb.ItemTable


def get_affix_rule_row(row: Row) -> _pb_equipaffixrule.EquipAffixRuleTable | None:
    """解析 Item.affix_rule -> EquipAffixRule 行；解析不到返回 ``None``。"""
    return EquipAffixRuleTableManager.instance().find_by_id(row.affix_rule)


def get_affix_rule_row_by_id(table_id: int) -> _pb_equipaffixrule.EquipAffixRuleTable | None:
    """同上，入参换成 Item 自己的 id。"""
    row = ItemTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_affix_rule_row(row)


def get_forge_material_id_row(row: Row) -> _pb_item.ItemTable | None:
    """解析 Item.forge_material_id -> Item 行；解析不到返回 ``None``。"""
    return ItemTableManager.instance().find_by_id(row.forge_material_id)


def get_forge_material_id_row_by_id(table_id: int) -> _pb_item.ItemTable | None:
    """同上，入参换成 Item 自己的 id。"""
    row = ItemTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_forge_material_id_row(row)


def get_artifact_id_row(row: Row) -> _pb_equipartifact.EquipArtifactTable | None:
    """解析 Item.artifact_id -> EquipArtifact 行；解析不到返回 ``None``。"""
    return EquipArtifactTableManager.instance().find_by_id(row.artifact_id)


def get_artifact_id_row_by_id(table_id: int) -> _pb_equipartifact.EquipArtifactTable | None:
    """同上，入参换成 Item 自己的 id。"""
    row = ItemTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_artifact_id_row(row)


# ---- 反向外键（HasMany）：按外键列的值反查源表的行 ----


def find_rows_by_affix_rule(key: int) -> tuple[Row, ...]:
    """反查：全部 affix_rule == key 的 Item 行。走的是已建好的二级索引。"""
    return ItemTableManager.instance().get_by_affix_rule(key)


def find_rows_by_forge_material_id(key: int) -> tuple[Row, ...]:
    """反查：全部 forge_material_id == key 的 Item 行。走的是已建好的二级索引。"""
    return ItemTableManager.instance().get_by_forge_material_id(key)


def find_rows_by_artifact_id(key: int) -> tuple[Row, ...]:
    """反查：全部 artifact_id == key 的 Item 行。走的是已建好的二级索引。"""
    return ItemTableManager.instance().get_by_artifact_id(key)
