"""TournamentReward 的外键跨表查询助手（自动生成，请勿手改）。

DO NOT EDIT — regenerate from Excel via Data Table Exporter.

这里只做一件事：把 TournamentReward 行里存的**目标表 id** 解析成目标表的行。
解析结果和源行一样属于「当前快照」，同样的热更契约：**只存 id，不存行**。

放在独立模块而不是塞进 tournamentreward_table.py，是因为它要 import 目标表的
管理器；写在一起时两张互相引用的表就会在 import 期成环。
"""

from __future__ import annotations

import tournamentreward_table_pb2 as _pb
import item_table_pb2 as _pb_item

from .tournamentreward_table import TournamentRewardTableManager
from .item_table import ItemTableManager

Row = _pb.TournamentRewardTable


def get_item_ids_rows(row: Row) -> tuple[_pb_item.ItemTable, ...]:
    """解析 TournamentReward.item_ids[] -> Item 行。解析不到的 id 直接跳过。"""
    manager = ItemTableManager.instance()
    result: list[_pb_item.ItemTable] = []
    for fk_id in row.item_ids:
        target_row = manager.find_by_id(fk_id)
        if target_row is not None:
            result.append(target_row)
    return tuple(result)


def get_item_ids_rows_by_id(table_id: int) -> tuple[_pb_item.ItemTable, ...]:
    """同上，入参换成 TournamentReward 自己的 id。源行不存在时返回空元组。"""
    row = TournamentRewardTableManager.instance().find_by_id(table_id)
    if row is None:
        return ()
    return get_item_ids_rows(row)


# ---- 反向外键（HasMany）：按外键列的值反查源表的行 ----
