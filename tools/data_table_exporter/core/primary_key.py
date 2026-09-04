"""主键校验:重复 id 必须显式声明,不许静默发生。

从前重复 id 是**静默丢索引**:生成的管理器把 id 建成单值 map,
Go 侧 ``snap.kvData[row.Id] = row`` 后写覆盖先写、C++ 侧 ``unordered_map::emplace``
先写胜出,两端还不一致;``FindById`` 只拿得到其中一行,``Exists``/``Count`` 少算,
而 ``FindAll`` / JSON / ``.pb`` 里那些行都还在 —— 数据没丢,丢的是索引,且零报错。

现在的规矩:

* 主键列写了 ``(cfg_multi)`` -> 这张表**允许**重复 id,生成 ``FindAllById``;
* 没写 -> 重复 id 是错误,**整批不产出**。

校验跑在任何生成之前,和外键校验同一位置 —— 否则一批坏数据会先覆盖掉上一批好产物。
"""

from __future__ import annotations

import logging
from collections import Counter
from dataclasses import dataclass, field

from core.config_loader import ExporterConfig
from core.excel_reader import read_id_column
from core.schema import TableSchema

logger = logging.getLogger(__name__)


@dataclass
class PrimaryKeyReport:
    errors: list[str] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return not self.errors


def validate_primary_keys(tables: list[TableSchema], cfg: ExporterConfig) -> PrimaryKeyReport:
    report = PrimaryKeyReport()
    for table in tables:
        try:
            ids = read_id_column(table, cfg)
        except Exception as exc:                     # 读不出 id 列本身就是问题
            report.errors.append("[%s] 读不出 id 列:%s" % (table.name, exc))
            continue

        dupes = {i: n for i, n in Counter(ids).items() if n > 1}
        if not dupes:
            continue

        if table.multi_primary_key:
            logger.info("[%s] 主键可重复((cfg_multi)):%d 个 id 有多行,共 %d 行",
                        table.name, len(dupes), len(ids))
            continue

        sample = ", ".join("%s×%d" % (i, n) for i, n in list(sorted(dupes.items()))[:5])
        report.errors.append(
            "[%s] 主键 id 重复:%d 个 id 出现多次(%s%s)。"
            "重复 id 会让 FindById 只拿到其中一行、Count 少算,而且不会有任何报错。"
            "确实需要重复就在权威 schema 的主键列上写 (cfg_multi) = true;"
            "否则请修表。"
            % (table.name, len(dupes), sample, " …" if len(dupes) > 5 else ""))
    return report
