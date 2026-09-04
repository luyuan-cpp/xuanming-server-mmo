"""GlobalVariable 配置表管理器（自动生成，请勿手改）。

DO NOT EDIT — regenerate from Excel via Data Table Exporter.

热更契约
========
:meth:`GlobalVariableTableManager.load` **整批**建好一个新的不可变快照，再一次性
换掉旧的（``build_snapshot`` 读盘建索引，``apply_snapshot`` 换指针）。返回给调用方的
行对象属于**当时那个快照**：热更之后它既不会变也不会崩，但会永远停在旧值上。
所以调用方**只存 id**，要用的时候现查，不要长期持有行引用。

读侧为什么必须「先取一次本地引用」
==================================
``self._snap = new_snap`` 这一步本身是原子的：一次指针存储，撕裂不了，
free-threading 构建下也一样。**不原子的是「读两次」**——同一个方法里两次
``self._snap`` 可能落在换快照的前后，于是拿到两个互不一致的对象：用前一个算
``len(rows)``、用后一个按下标取值，表在中间缩小就直接越界。所以下面每个访问方法
都在开头 ``snap = self._snap`` 取一次局部引用，之后只用这个局部名。
"""

from __future__ import annotations

import random
from collections.abc import Callable, Iterable, Mapping
from dataclasses import dataclass
from pathlib import Path
from types import MappingProxyType

from google.protobuf import json_format

import globalvariable_table_pb2 as _pb

#: 这张表的行类型。所有查询返回的都是它，或者它的元组。
Row = _pb.GlobalVariableTable

#: 查不到时统一返回它：省掉每次构造空元组，也保证「返回值不可变」这条不破。
_NO_ROWS: tuple[Row, ...] = ()


@dataclass(frozen=True, slots=True)
class Snapshot:
    """一次 ``build_snapshot`` 的全部产物：行 + 全部索引。建好之后再也不改。

    ``frozen=True`` 不是为了好看：快照被多个线程同时读，任何一处「就地改一下」
    都会让已经拿到它的读者看见半成品。要换数据就整个换一个新 Snapshot。
    """

    #: protobuf 容器本体。行是容器的子消息，容器被回收行也就没了，所以必须留着。
    container: _pb.GlobalVariableTableData
    #: 表内顺序的全部行。
    rows: tuple[Row, ...]
    #: 主键唯一：一个 id 一行。
    by_id: dict[int, Row]


#: 还没 load 过时的空快照。让「没加载」和「加载成 0 行」走同一条读路径，
#: 免得每个访问器都要先判一次 ``self._snap is None``。
_EMPTY_SNAPSHOT: Snapshot = Snapshot(
    container=_pb.GlobalVariableTableData(),
    rows=(),
    by_id={},
)


class GlobalVariableTableManager:
    """GlobalVariable 表的进程内单例。用 :meth:`instance` 取。"""

    __slots__ = ("_snap",)

    def __init__(self) -> None:
        self._snap: Snapshot = _EMPTY_SNAPSHOT

    @classmethod
    def instance(cls) -> "GlobalVariableTableManager":
        """进程内单例。与 Java 的 ``getInstance()`` / Go 的 ``...ManagerInstance`` 对齐。"""
        return _INSTANCE

    # ---- 加载 ----

    def build_snapshot(self, config_dir: str | Path, use_binary: bool = False) -> Snapshot:
        """读盘 + 建索引，返回一个新快照。**不碰管理器当前状态。**

        和 :meth:`apply_snapshot` 分开，是为了让 ``all_table.load_tables`` 做到
        「全部表都读成功了才换」——中间任何一张表读失败，一张都不会被换掉，
        进程继续跑在上一批完整的配置上。

        :param use_binary: True 读 ``globalvariable.pb``（proto 二进制），
            False 读 JSON。口径与 Go/Java 的 ``useBinary`` 一致。
        """
        container = _pb.GlobalVariableTableData()
        base = Path(config_dir)
        if use_binary:
            container.ParseFromString((base / "globalvariable.pb").read_bytes())
        else:
            # 文件名全小写,与 .pb 及其余五门语言同口径(json_gen.py 从前落 PascalCase,
            # 在大小写不敏感的 Windows 上碰巧能开、Linux 上必然读不到 —— 已在源头修掉)。
            path = base / "globalvariable.json"
            json_format.Parse(
                path.read_text(encoding="utf-8"), container, ignore_unknown_fields=True
            )

        rows: tuple[Row, ...] = tuple(container.data)
        by_id: dict[int, Row] = {}

        for row in rows:
            by_id[row.id] = row

        return Snapshot(
            container=container,
            rows=rows,
            by_id=by_id,
        )

    def apply_snapshot(self, snapshot: Snapshot) -> None:
        """把新快照换上去。一次属性赋值，原子，不需要锁。"""
        self._snap = snapshot

    def load(self, config_dir: str | Path, use_binary: bool = False) -> None:
        """读盘并换上新快照。等价于 ``apply_snapshot(build_snapshot(...))``。

        读失败会抛异常，且**在抛出之前一行都没换**——旧快照原封不动还在服役。
        """
        self.apply_snapshot(self.build_snapshot(config_dir, use_binary))

    @property
    def snapshot(self) -> Snapshot:
        """当前快照。要连做多次查询就先取它一次，保证这几次查的是同一份数据。"""
        return self._snap

    # ---- 主键查询 ----

    def find_all(self) -> tuple[Row, ...]:
        """表内顺序的全部行。"""
        return self._snap.rows

    def find_by_id(self, id_: int) -> Row | None:
        """按 id 查一行；没有就返回 ``None``。

        热更契约：返回的行属于当前快照，调用方**只存 id**，不要长期持有引用。
        """
        return self._snap.by_id.get(id_)

    @property
    def kv_data(self) -> Mapping[int, Row]:
        """id -> 行 的只读视图。视图跟着**取它时**那个快照走，不是拷贝。"""
        return MappingProxyType(self._snap.by_id)

    def find_by_ids(self, ids: Iterable[int]) -> tuple[Row, ...]:
        """批量按 id 取行（SQL 的 IN）。查不到的 id 直接跳过，不占位。"""
        snap = self._snap
        result: list[Row] = []
        for id_ in ids:
            row = snap.by_id.get(id_)
            if row is not None:
                result.append(row)
        return tuple(result)

    # ---- Exists ----

    def exists(self, id_: int) -> bool:
        return id_ in self._snap.by_id

    # ---- Count ----

    def count(self) -> int:
        return len(self._snap.rows)

    # ---- RandOne ----

    def rand_one(self) -> Row | None:
        """随机取一行；空表返回 ``None``。"""
        snap = self._snap
        if not snap.rows:
            return None
        return random.choice(snap.rows)

    # ---- Where / First ----

    def where(self, pred: Callable[[Row], bool]) -> tuple[Row, ...]:
        """全表扫描，返回全部满足 pred 的行。没有现成索引时才用它。"""
        snap = self._snap
        return tuple(row for row in snap.rows if pred(row))

    def first(self, pred: Callable[[Row], bool]) -> Row | None:
        """全表扫描，返回第一个满足 pred 的行；没有就 ``None``。"""
        snap = self._snap
        for row in snap.rows:
            if pred(row):
                return row
        return None


#: 模块级单例。别再 ``GlobalVariableTableManager()`` 自己新建一个 —— 那个实例是空的，
#: all_table 也不会给它加载数据。
_INSTANCE: GlobalVariableTableManager = GlobalVariableTableManager()
