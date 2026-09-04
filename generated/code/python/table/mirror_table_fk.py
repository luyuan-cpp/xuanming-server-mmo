"""Mirror 的外键跨表查询助手（自动生成，请勿手改）。

DO NOT EDIT — regenerate from Excel via Data Table Exporter.

这里只做一件事：把 Mirror 行里存的**目标表 id** 解析成目标表的行。
解析结果和源行一样属于「当前快照」，同样的热更契约：**只存 id，不存行**。

放在独立模块而不是塞进 mirror_table.py，是因为它要 import 目标表的
管理器；写在一起时两张互相引用的表就会在 import 期成环。
"""

from __future__ import annotations

import mirror_table_pb2 as _pb
import basescene_table_pb2 as _pb_basescene
import world_table_pb2 as _pb_world

from .mirror_table import MirrorTableManager
from .basescene_table import BaseSceneTableManager
from .world_table import WorldTableManager

Row = _pb.MirrorTable


def get_scene_id_row(row: Row) -> _pb_basescene.BaseSceneTable | None:
    """解析 Mirror.scene_id -> BaseScene 行；解析不到返回 ``None``。"""
    return BaseSceneTableManager.instance().find_by_id(row.scene_id)


def get_scene_id_row_by_id(table_id: int) -> _pb_basescene.BaseSceneTable | None:
    """同上，入参换成 Mirror 自己的 id。"""
    row = MirrorTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_scene_id_row(row)


def get_main_scene_id_row(row: Row) -> _pb_world.WorldTable | None:
    """解析 Mirror.main_scene_id -> World 行；解析不到返回 ``None``。"""
    return WorldTableManager.instance().find_by_id(row.main_scene_id)


def get_main_scene_id_row_by_id(table_id: int) -> _pb_world.WorldTable | None:
    """同上，入参换成 Mirror 自己的 id。"""
    row = MirrorTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_main_scene_id_row(row)


# ---- 反向外键（HasMany）：按外键列的值反查源表的行 ----


def find_rows_by_scene_id(key: int) -> tuple[Row, ...]:
    """反查：全部 scene_id == key 的 Mirror 行。走的是已建好的二级索引。"""
    return MirrorTableManager.instance().get_by_scene_id(key)


def find_rows_by_main_scene_id(key: int) -> tuple[Row, ...]:
    """反查：全部 main_scene_id == key 的 Mirror 行。走的是已建好的二级索引。"""
    return MirrorTableManager.instance().get_by_main_scene_id(key)
