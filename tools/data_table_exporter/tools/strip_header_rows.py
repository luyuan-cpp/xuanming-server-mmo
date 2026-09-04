#!/usr/bin/env python3
"""迁移最后一步:清掉 xlsx 里已经搬进权威 schema 的表头行(类型 / owner / options / 注释)。

跑完之后每张表只剩::

    第 1 行  字段名        <- 机器绑定用,冻结
    第 2 行  中文说明      <- 由权威 schema 的注释投影而来(--decorate,默认开)
    第 3~5 行 空
    第 6 行起 数据         <- 一个格子都没动

**这是整条路线里唯一一步会改策划的表。** 三条约束:

1. 只处理**已经有权威 schema**的表。没有 schema 的表原样不动 —— 清了它就没人能读了。
2. 清之前先用权威 schema 构造一次 ``TableSchema``,构造不出来就跳过这张表。
3. 原文件先备份到 ``--backup-dir``(缺省 scratchpad),再原地改。xlsx 是 git-tracked 的,
   ``git checkout -- data/<T>.xlsx`` 也能还原。

**关于第 2 行的中文说明**:它是**投影,不是事实源**。真正的注释写在
``data/schema/<sheet>_table.proto`` 的 ``//`` 里,重跑本工具会按 proto 覆盖第 2 行。
在表里改中文不会生效、还会在下次重跑时丢掉 —— 要改就改 proto。
不想要这个投影就加 ``--no-decorate``,那样第 2 行也留空。

用法::

    py tools/data_table_exporter/tools/strip_header_rows.py --dry-run
    py tools/data_table_exporter/tools/strip_header_rows.py
    py tools/data_table_exporter/tools/strip_header_rows.py --only World
"""

from __future__ import annotations

import argparse
import logging
import shutil
import sys
from pathlib import Path

_HERE = Path(__file__).resolve().parent
_ROOT = _HERE.parent
if str(_ROOT) not in sys.path:
    sys.path.insert(0, str(_ROOT))

import openpyxl                                                    # noqa: E402

from core.config_loader import load_config                         # noqa: E402
from core.file_utils import list_xlsx                              # noqa: E402
from core.schema_proto import SchemaProtoError, build_table_schema  # noqa: E402
from core.table_source import index_schema_protos                  # noqa: E402

logger = logging.getLogger("strip_header")

_DEFAULT_BACKUP = Path(
    r"C:\Users\ADMINI~1\AppData\Local\Temp\claude"
    r"\F--work\e96e173d-13e5-431f-9eaf-fb0319d75006\scratchpad\xlsx_backup")


def _lock_file(xlsx: Path) -> Path:
    return xlsx.with_name("~$" + xlsx.name)


def strip_one(xlsx: Path, sheet: str, schema, cfg, decorate: bool) -> tuple[int, int]:
    """清掉元数据行,返回 (清掉的格子数, 写回的中文说明数)。"""
    wb = openpyxl.load_workbook(xlsx)
    ws = wb[sheet]
    max_col = ws.max_column or 0

    name_row = cfg.metadata_rows.get("field_name", 1)
    meta_rows = sorted({r for k, r in cfg.metadata_rows.items() if r != name_row})
    meta_rows = [r for r in meta_rows if r < cfg.data_begin_row]

    cleared = 0
    for row in meta_rows:
        for col in range(1, max_col + 1):
            cell = ws.cell(row=row, column=col)
            if cell.value is not None:
                cell.value = None
                cleared += 1

    written = 0
    if decorate and meta_rows:
        # 中文说明写回**注释行**(第 5 行),不是紧挨表头的第 2 行。
        # 第 2 行是旧解析器的类型行:往那儿写中文会让「这张表迁完了没有」不可判定,
        # 也会让旧路把中文当成类型名解析成功(Python 的 \w 认中文)。
        # 让它待在原来的位置,策划的肌肉记忆也不用改。
        target_row = cfg.metadata_rows.get("comment", meta_rows[-1])
        # 注释挂在逻辑字段的首列上,与 ColumnDef 一致。
        first_of_span: dict[int, str] = {}
        for c in schema.columns:
            if c.comment and c.excel_index not in first_of_span:
                first_of_span[c.excel_index] = c.comment.splitlines()[0].strip()
        for idx, text in first_of_span.items():
            if text:
                ws.cell(row=target_row, column=idx + 1).value = text
                written += 1

    wb.save(xlsx)
    wb.close()
    return cleared, written


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="清掉已迁移表的 xlsx 元数据行")
    ap.add_argument("config", nargs="?", default=None)
    ap.add_argument("--only", action="append", default=None)
    ap.add_argument("--backup-dir", default=None)
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--no-decorate", action="store_true",
                    help="不把权威 schema 的注释投影回第 2 行,该行也留空")
    args = ap.parse_args(argv)

    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
    cfg = load_config(args.config)
    index = index_schema_protos(cfg.schema_dir)
    if not index:
        logger.error("%s 下没有任何权威 schema,没有可清的表", cfg.schema_dir)
        return 1

    backup_dir = Path(args.backup_dir) if args.backup_dir else _DEFAULT_BACKUP
    done = skipped = fail = 0

    for xlsx in list_xlsx(cfg.data_dir):
        wb = openpyxl.load_workbook(xlsx, read_only=True)
        sheet = wb.sheetnames[0] if wb.sheetnames else ""
        wb.close()
        if args.only and sheet not in args.only:
            continue
        proto = index.get(sheet)
        if proto is None:
            logger.info("跳过 %s:还没有权威 schema", xlsx.name)
            skipped += 1
            continue
        if _lock_file(xlsx).exists():
            logger.error("跳过 %s:正被 Excel 打开(%s),关掉再跑",
                         xlsx.name, _lock_file(xlsx).name)
            fail += 1
            continue
        try:
            schema = build_table_schema(proto, xlsx)
        except SchemaProtoError as exc:
            logger.error("跳过 %s:权威 schema 现在就读不通 —— %s", xlsx.name, exc)
            fail += 1
            continue

        if args.dry_run:
            logger.info("[dry-run] 将清 %s 的第 %s 行",
                        xlsx.name,
                        "/".join(str(r) for r in sorted(
                            {v for k, v in cfg.metadata_rows.items()
                             if v != cfg.metadata_rows.get("field_name", 1)
                             and v < cfg.data_begin_row})))
            done += 1
            continue

        backup_dir.mkdir(parents=True, exist_ok=True)
        shutil.copy2(xlsx, backup_dir / xlsx.name)
        cleared, written = strip_one(xlsx, sheet, schema, cfg, not args.no_decorate)
        logger.info("%-26s 清掉 %3d 个元数据格,写回 %2d 条中文说明",
                    xlsx.name, cleared, written)
        done += 1

    logger.info("完成:%d 张处理,%d 张跳过(无 schema),%d 张失败", done, skipped, fail)
    if not args.dry_run and done:
        logger.info("原文件备份在 %s;也可以 git checkout -- data/ 还原", backup_dir)
    return 1 if fail else 0


if __name__ == "__main__":
    raise SystemExit(main())
