"""FestivalBeastActivity 的外键跨表查询助手（自动生成，请勿手改）。

DO NOT EDIT — regenerate from Excel via Data Table Exporter.

这里只做一件事：把 FestivalBeastActivity 行里存的**目标表 id** 解析成目标表的行。
解析结果和源行一样属于「当前快照」，同样的热更契约：**只存 id，不存行**。

放在独立模块而不是塞进 festivalbeastactivity_table.py，是因为它要 import 目标表的
管理器；写在一起时两张互相引用的表就会在 import 期成环。
"""

from __future__ import annotations

import festivalbeastactivity_table_pb2 as _pb
import dungeon_table_pb2 as _pb_dungeon
import pet_table_pb2 as _pb_pet

from .festivalbeastactivity_table import FestivalBeastActivityTableManager
from .dungeon_table import DungeonTableManager
from .pet_table import PetTableManager

Row = _pb.FestivalBeastActivityTable


def get_battle_config_id_row(row: Row) -> _pb_dungeon.DungeonTable | None:
    """解析 FestivalBeastActivity.battle_config_id -> Dungeon 行；解析不到返回 ``None``。"""
    return DungeonTableManager.instance().find_by_id(row.battle_config_id)


def get_battle_config_id_row_by_id(table_id: int) -> _pb_dungeon.DungeonTable | None:
    """同上，入参换成 FestivalBeastActivity 自己的 id。"""
    row = FestivalBeastActivityTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_battle_config_id_row(row)


def get_reward_pet_table_id_row(row: Row) -> _pb_pet.PetTable | None:
    """解析 FestivalBeastActivity.reward_pet_table_id -> Pet 行；解析不到返回 ``None``。"""
    return PetTableManager.instance().find_by_id(row.reward_pet_table_id)


def get_reward_pet_table_id_row_by_id(table_id: int) -> _pb_pet.PetTable | None:
    """同上，入参换成 FestivalBeastActivity 自己的 id。"""
    row = FestivalBeastActivityTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_reward_pet_table_id_row(row)


# ---- 反向外键（HasMany）：按外键列的值反查源表的行 ----


def find_rows_by_battle_config_id(key: int) -> tuple[Row, ...]:
    """反查：全部 battle_config_id == key 的 FestivalBeastActivity 行。走的是已建好的二级索引。"""
    return FestivalBeastActivityTableManager.instance().get_by_battle_config_id(key)


def find_rows_by_reward_pet_table_id(key: int) -> tuple[Row, ...]:
    """反查：全部 reward_pet_table_id == key 的 FestivalBeastActivity 行。走的是已建好的二级索引。"""
    return FestivalBeastActivityTableManager.instance().get_by_reward_pet_table_id(key)
