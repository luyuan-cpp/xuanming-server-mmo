"""JianghuRule 的外键跨表查询助手（自动生成，请勿手改）。

DO NOT EDIT — regenerate from Excel via Data Table Exporter.

这里只做一件事：把 JianghuRule 行里存的**目标表 id** 解析成目标表的行。
解析结果和源行一样属于「当前快照」，同样的热更契约：**只存 id，不存行**。

放在独立模块而不是塞进 jianghurule_table.py，是因为它要 import 目标表的
管理器；写在一起时两张互相引用的表就会在 import 期成环。
"""

from __future__ import annotations

import jianghurule_table_pb2 as _pb
import item_table_pb2 as _pb_item

from .jianghurule_table import JianghuRuleTableManager
from .item_table import ItemTableManager

Row = _pb.JianghuRuleTable


def get_gold_writ_item_id_row(row: Row) -> _pb_item.ItemTable | None:
    """解析 JianghuRule.gold_writ_item_id -> Item 行；解析不到返回 ``None``。"""
    return ItemTableManager.instance().find_by_id(row.gold_writ_item_id)


def get_gold_writ_item_id_row_by_id(table_id: int) -> _pb_item.ItemTable | None:
    """同上，入参换成 JianghuRule 自己的 id。"""
    row = JianghuRuleTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_gold_writ_item_id_row(row)


def get_clear_item_id_row(row: Row) -> _pb_item.ItemTable | None:
    """解析 JianghuRule.clear_item_id -> Item 行；解析不到返回 ``None``。"""
    return ItemTableManager.instance().find_by_id(row.clear_item_id)


def get_clear_item_id_row_by_id(table_id: int) -> _pb_item.ItemTable | None:
    """同上，入参换成 JianghuRule 自己的 id。"""
    row = JianghuRuleTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_clear_item_id_row(row)


# ---- 反向外键（HasMany）：按外键列的值反查源表的行 ----


def find_rows_by_gold_writ_item_id(key: int) -> tuple[Row, ...]:
    """反查：全部 gold_writ_item_id == key 的 JianghuRule 行。走的是已建好的二级索引。"""
    return JianghuRuleTableManager.instance().get_by_gold_writ_item_id(key)


def find_rows_by_clear_item_id(key: int) -> tuple[Row, ...]:
    """反查：全部 clear_item_id == key 的 JianghuRule 行。走的是已建好的二级索引。"""
    return JianghuRuleTableManager.instance().get_by_clear_item_id(key)
