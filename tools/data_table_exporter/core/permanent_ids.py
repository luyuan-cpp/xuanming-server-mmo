"""表行 id 只增不删:声明了 ``(cfg_ids_permanent)`` 的表,台账里记下的 id 不许从表里消失。

为什么(混版本持久字段保护设计 §13 第 10 条,2026-11-11 定):
玩家存档、在途的资产通道指令都按配置 id 引用表行。scene 遇到本节点表里查不到的 id 时,只有在表行永不删除的
前提下,才能判定它是更新版本或尚未热更到本节点的行,回 RETRY 等它出现;表行允许删除的话,「新行」和「已删行」
在旧节点上分不开,改判 RETRY 会让引用已删行的指令永远卡住。

规矩:

* 台账 ``state/permanent_ids/<表名小写>_ids.txt``:这张表出现过的全部 id,升序,每行一个;进 git、走 code review。
* 导表时表里少了台账里的 id:整批不产出,报出被删的 id。要停止产出某一行,去掉掉落、商店、奖励里对它的引用,
  表行保留。
* 表里新出现的 id 在生成阶段追加进台账,台账与产物同一次提交。
* 只在开发期存在、从未发布过的行要删时,在同一次提交里从台账删掉这个 id,并在提交说明里写明理由 ——
  这是要过评审的显式决定,导表器不替人做。
* 台账缺失时按当前表初始化并打 WARNING。台账进了 git,缺失或被改小都会出现在 diff 里;这与位序状态文件的
  fail-closed 不同:位序丢了会让存档位图串位(数据损坏),这份台账丢了只是少一道检查。
* 台账格式不对(非整数、重复 id)一律 fail-closed:按坏台账继续导,被删的行会逃过检查。
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional

from core.config_loader import ExporterConfig
from core.excel_reader import read_id_column
from core.schema import TableSchema

logger = logging.getLogger(__name__)

_HEADER = (
    "# {name} 表出现过的全部行 id,升序。由导表器维护,只增不删(data/AGENTS.md「行 id 只增不删」)。\n"
    "# 表里删掉这里列出的任何一行,导表即失败;从未发布过的行要删,在同一次提交里删掉这里的 id 并写明理由。\n"
)


class PermanentIdsError(RuntimeError):
    """台账文件不可用(格式不对)。必须让整次导表失败。"""


@dataclass
class PermanentIdsReport:
    errors: list[str] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return not self.errors


def ledger_path(cfg: ExporterConfig, table: TableSchema) -> Path:
    return cfg.state_dir / "permanent_ids" / f"{table.name.lower()}_ids.txt"


def load_ledger(path: Path) -> Optional[set[int]]:
    """读台账;文件不存在返回 None。格式不对抛 :class:`PermanentIdsError`。"""
    if not path.exists():
        return None
    ids: set[int] = set()
    for number, raw in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        try:
            value = int(line)
        except ValueError as exc:
            raise PermanentIdsError(f"{path}:{number}: not an integer row id: {line!r}") from exc
        if value in ids:
            raise PermanentIdsError(f"{path}:{number}: duplicate row id {value}")
        ids.add(value)
    return ids


def validate_permanent_ids(tables: list[TableSchema], cfg: ExporterConfig) -> PermanentIdsReport:
    """表里少了台账里的 id 即报错。只读,不写台账;写台账在 :func:`record_permanent_ids`。"""
    report = PermanentIdsReport()
    for table in tables:
        if not table.ids_permanent:
            continue
        path = ledger_path(cfg, table)
        try:
            published = load_ledger(path)
        except PermanentIdsError as exc:
            report.errors.append(f"[{table.name}] {exc}")
            continue
        if published is None:
            continue
        removed = sorted(published - set(read_id_column(table, cfg)))
        if not removed:
            continue
        sample = ", ".join(str(i) for i in removed[:10])
        more = "" if len(removed) <= 10 else f" and {len(removed) - 10} more"
        report.errors.append(
            f"[{table.name}] {len(removed)} recorded row ids were removed from the table: {sample}{more}. "
            f"Row ids of this table are permanent (cfg_ids_permanent): keep the row and remove its references "
            f"(drops, shops, rewards) instead. A row that was never released may be removed only by deleting "
            f"its id from {path} in the same commit, with the reason stated."
        )
    return report


def record_permanent_ids(tables: list[TableSchema], cfg: ExporterConfig) -> None:
    """把表里新出现的 id 追加进台账。只在 :func:`validate_permanent_ids` 通过之后调用。"""
    for table in tables:
        if not table.ids_permanent:
            continue
        path = ledger_path(cfg, table)
        published = load_ledger(path)
        current = set(read_id_column(table, cfg))
        if published is None:
            logger.warning("[%s] Permanent row-id ledger is missing; initializing it from the current table: %s",
                           table.name, path)
            published = set()
        elif current <= published:
            continue
        added = sorted(current - published)
        if added:
            logger.info("[%s] Recorded %d new permanent row ids in %s", table.name, len(added), path)
        path.parent.mkdir(parents=True, exist_ok=True)
        # newline="\n":仓库里这些文件是 LF,Windows 上默认的文本模式会写成 CRLF,每次导表都整文件"被修改"。
        with open(path, "w", encoding="utf-8", newline="\n") as f:
            f.write(_HEADER.format(name=table.name))
            for value in sorted(published | current):
                f.write(f"{value}\n")
