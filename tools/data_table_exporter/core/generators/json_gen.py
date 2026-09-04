"""JSON data generator.

Reads each data table and writes a ``{SheetName}.json`` file
containing the data rows.
"""

from __future__ import annotations

import json
import logging
from concurrent.futures import ThreadPoolExecutor

from core.config_loader import ExporterConfig
from core.excel_reader import read_data_rows
from core.file_utils import write_file, ensure_dirs
from core.schema import TableSchema

logger = logging.getLogger(__name__)


def generate_json(cfg: ExporterConfig, tables: list[TableSchema]) -> None:
    """Generate JSON files for all tables."""
    ensure_dirs(cfg.json_dir)
    if not tables:
        logger.warning("No tables to generate JSON for")
        return

    with ThreadPoolExecutor() as pool:
        # Executor.map 是惰性的；不消费结果就会吞掉工作线程异常，随后拿旧产物
        # 继续编译/部署。逐项迭代才能把任一表的失败传播给 orchestrator。
        for _ in pool.map(lambda t: _export_one(t, cfg), tables):
            pass


def _export_one(table: TableSchema, cfg: ExporterConfig) -> None:
    try:
        rows = read_data_rows(table, cfg)
        # 小写与 binary_gen 的 f"{table.name.lower()}.pb" 以及 cpp/go/java 三端模板里的
        # "{{ sheetname | lower }}.json" 对齐。从前这里是 PascalCase:Windows 大小写不敏感
        # 所以一直没炸,**Linux 上 JSON 模式必然读不到文件**(线上跑 binary 才没暴露)。
        out = cfg.json_dir / f"{table.name.lower()}.json"
        content = json.dumps(
            {"data": rows},
            sort_keys=True,
            indent=1,
            separators=(",", ": "),
            ensure_ascii=False,
        )
        write_file(out, content)
        logger.info("Generated %s", out.name)
    except Exception as exc:
        raise RuntimeError(
            f"JSON generation failed for {table.name}: {exc}"
        ) from exc
