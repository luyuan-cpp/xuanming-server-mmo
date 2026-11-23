"""RankedPreset 的外键跨表查询助手（自动生成，请勿手改）。

DO NOT EDIT — regenerate from Excel via Data Table Exporter.

这里只做一件事：把 RankedPreset 行里存的**目标表 id** 解析成目标表的行。
解析结果和源行一样属于「当前快照」，同样的热更契约：**只存 id，不存行**。

放在独立模块而不是塞进 rankedpreset_table.py，是因为它要 import 目标表的
管理器；写在一起时两张互相引用的表就会在 import 期成环。
"""

from __future__ import annotations

import rankedpreset_table_pb2 as _pb
import pet_table_pb2 as _pb_pet
import petentry_table_pb2 as _pb_petentry
import skill_table_pb2 as _pb_skill

from .rankedpreset_table import RankedPresetTableManager
from .pet_table import PetTableManager
from .petentry_table import PetEntryTableManager
from .skill_table import SkillTableManager

Row = _pb.RankedPresetTable


def get_pet_table_id_row(row: Row) -> _pb_pet.PetTable | None:
    """解析 RankedPreset.pet_table_id -> Pet 行；解析不到返回 ``None``。"""
    return PetTableManager.instance().find_by_id(row.pet_table_id)


def get_pet_table_id_row_by_id(table_id: int) -> _pb_pet.PetTable | None:
    """同上，入参换成 RankedPreset 自己的 id。"""
    row = RankedPresetTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_pet_table_id_row(row)


def get_skill_ids_rows(row: Row) -> tuple[_pb_skill.SkillTable, ...]:
    """解析 RankedPreset.skill_ids[] -> Skill 行。解析不到的 id 直接跳过。"""
    manager = SkillTableManager.instance()
    result: list[_pb_skill.SkillTable] = []
    for fk_id in row.skill_ids:
        target_row = manager.find_by_id(fk_id)
        if target_row is not None:
            result.append(target_row)
    return tuple(result)


def get_skill_ids_rows_by_id(table_id: int) -> tuple[_pb_skill.SkillTable, ...]:
    """同上，入参换成 RankedPreset 自己的 id。源行不存在时返回空元组。"""
    row = RankedPresetTableManager.instance().find_by_id(table_id)
    if row is None:
        return ()
    return get_skill_ids_rows(row)


def get_pet_entry_ids_rows(row: Row) -> tuple[_pb_petentry.PetEntryTable, ...]:
    """解析 RankedPreset.pet_entry_ids[] -> PetEntry 行。解析不到的 id 直接跳过。"""
    manager = PetEntryTableManager.instance()
    result: list[_pb_petentry.PetEntryTable] = []
    for fk_id in row.pet_entry_ids:
        target_row = manager.find_by_id(fk_id)
        if target_row is not None:
            result.append(target_row)
    return tuple(result)


def get_pet_entry_ids_rows_by_id(table_id: int) -> tuple[_pb_petentry.PetEntryTable, ...]:
    """同上，入参换成 RankedPreset 自己的 id。源行不存在时返回空元组。"""
    row = RankedPresetTableManager.instance().find_by_id(table_id)
    if row is None:
        return ()
    return get_pet_entry_ids_rows(row)


# ---- 反向外键（HasMany）：按外键列的值反查源表的行 ----


def find_rows_by_pet_table_id(key: int) -> tuple[Row, ...]:
    """反查：全部 pet_table_id == key 的 RankedPreset 行。走的是已建好的二级索引。"""
    return RankedPresetTableManager.instance().get_by_pet_table_id(key)
