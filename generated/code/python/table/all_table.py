"""全部配置表的聚合加载入口（自动生成，请勿手改）。

DO NOT EDIT — regenerate from Excel via Data Table Exporter.

用法
====
::

    from table import all_table

    all_table.load_tables("generated/tables", use_binary=True)

    from table.item_table import ItemTableManager
    row = ItemTableManager.instance().find_by_id(1001)

sys.path 说明
=============
本目录下的模块之间用相对 import，所以它整体是一个包，把**上级目录**放进
``sys.path`` 即可。生成出来的 ``*_table_pb2.py`` 是另一棵树（导表器的
``proto_python_output_dir``），默认按顶层模块名 import，所以那个目录也要在
``sys.path`` 上 —— 和 ``core/generators/binary_gen.py`` 自己做的事一模一样。
真起 Python 服务之后，把它们装成一个真包，并在 ``exporter_config.yaml`` 的
``languages.python.proto_import_path`` 填上包名，这里就会改成绝对 import。

为什么加载是「先全读、后全换」
==============================
每个管理器把「读盘建索引」（``build_snapshot``）和「换上去」（``apply_snapshot``）
拆成了两步。这里先把 27 张表全部读完，一张都没换；
中途任何一张读失败，异常直接抛出去，**一张表都不会被换掉** ——
进程继续跑在上一批完整的配置上，而不是半新半旧。
"""

from __future__ import annotations

from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Any, Protocol

from .actoractioncombatstate_table import ActorActionCombatStateTableManager
from .actoractionstate_table import ActorActionStateTableManager
from .attributeautoplan_table import AttributeAutoPlanTableManager
from .attributedimension_table import AttributeDimensionTableManager
from .attributepool_table import AttributePoolTableManager
from .attributerule_table import AttributeRuleTableManager
from .basescene_table import BaseSceneTableManager
from .buff_table import BuffTableManager
from .class_table import ClassTableManager
from .condition_table import ConditionTableManager
from .cooldown_table import CooldownTableManager
from .dungeon_table import DungeonTableManager
from .equipslot_table import EquipSlotTableManager
from .globalvariable_table import GlobalVariableTableManager
from .item_table import ItemTableManager
from .messagelimiter_table import MessageLimiterTableManager
from .mirror_table import MirrorTableManager
from .mission_table import MissionTableManager
from .monster_table import MonsterTableManager
from .pet_table import PetTableManager
from .petrule_table import PetRuleTableManager
from .reward_table import RewardTableManager
from .skill_table import SkillTableManager
from .skillpermission_table import SkillPermissionTableManager
from .test_table import TestTableManager
from .testmultikey_table import TestMultiKeyTableManager
from .world_table import WorldTableManager


class TableManager(Protocol):
    """本模块只依赖管理器的这两个方法：先全读、后全换。"""

    def build_snapshot(self, config_dir: str | Path, use_binary: bool = False) -> Any: ...

    def apply_snapshot(self, snapshot: Any) -> None: ...


#: 表名 -> 管理器单例。想遍历全部表（校验、导出、调试）就用它。
MANAGERS: dict[str, TableManager] = {
    "ActorActionCombatState": ActorActionCombatStateTableManager.instance(),
    "ActorActionState": ActorActionStateTableManager.instance(),
    "AttributeAutoPlan": AttributeAutoPlanTableManager.instance(),
    "AttributeDimension": AttributeDimensionTableManager.instance(),
    "AttributePool": AttributePoolTableManager.instance(),
    "AttributeRule": AttributeRuleTableManager.instance(),
    "BaseScene": BaseSceneTableManager.instance(),
    "Buff": BuffTableManager.instance(),
    "Class": ClassTableManager.instance(),
    "Condition": ConditionTableManager.instance(),
    "Cooldown": CooldownTableManager.instance(),
    "Dungeon": DungeonTableManager.instance(),
    "EquipSlot": EquipSlotTableManager.instance(),
    "GlobalVariable": GlobalVariableTableManager.instance(),
    "Item": ItemTableManager.instance(),
    "MessageLimiter": MessageLimiterTableManager.instance(),
    "Mirror": MirrorTableManager.instance(),
    "Mission": MissionTableManager.instance(),
    "Monster": MonsterTableManager.instance(),
    "Pet": PetTableManager.instance(),
    "PetRule": PetRuleTableManager.instance(),
    "Reward": RewardTableManager.instance(),
    "Skill": SkillTableManager.instance(),
    "SkillPermission": SkillPermissionTableManager.instance(),
    "Test": TestTableManager.instance(),
    "TestMultiKey": TestMultiKeyTableManager.instance(),
    "World": WorldTableManager.instance(),
}

_load_success_callback: Callable[[], None] | None = None


def on_tables_load_success(callback: Callable[[], None] | None) -> None:
    """注册一个「全部表加载完」之后调用的回调。传 ``None`` 取消。"""
    global _load_success_callback
    _load_success_callback = callback


def _apply(staged: list[tuple[TableManager, Any]]) -> None:
    for manager, snapshot in staged:
        manager.apply_snapshot(snapshot)
    if _load_success_callback is not None:
        _load_success_callback()


def load_tables(config_dir: str | Path, use_binary: bool = False) -> None:
    """串行加载全部 27 张表。

    :param use_binary: True 读 ``*.pb``（proto 二进制），False 读 ``*.json``。
        口径与 Go/Java 的 ``useBinary`` 一致。
    """
    staged = [
        (manager, manager.build_snapshot(config_dir, use_binary))
        for manager in MANAGERS.values()
    ]
    _apply(staged)


def load_tables_async(
    config_dir: str | Path,
    use_binary: bool = False,
    max_workers: int | None = None,
) -> None:
    """并行读盘，读完再串行换上去。

    读盘和 protobuf 解析都在 C 扩展里放开 GIL，所以线程池是有效的；
    换快照那一步很便宜，串行做即可，也顺带保证「要么全换要么全不换」。
    """
    with ThreadPoolExecutor(max_workers=max_workers) as pool:
        pending = [
            (manager, pool.submit(manager.build_snapshot, config_dir, use_binary))
            for manager in MANAGERS.values()
        ]
        # .result() 会把工作线程里的异常原样抛到这里；异常抛出时还一张表都没换。
        staged = [(manager, future.result()) for manager, future in pending]
    _apply(staged)


def reload_tables(config_dir: str | Path, use_binary: bool = False) -> None:
    """热更重载。与 :func:`load_tables` 完全同义 —— 加载本来就是「全读完再换」。

    单独留这个名字只为和 Go/Java 的 ``ReloadTables`` 对齐；调用方不必区分
    首次加载和热更，两边走的是同一条路径。
    """
    load_tables(config_dir, use_binary)
