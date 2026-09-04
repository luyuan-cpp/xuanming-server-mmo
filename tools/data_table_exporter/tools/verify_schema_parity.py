#!/usr/bin/env python3
"""对拍:同一张表,旧路(5 行表头)与新路(权威 schema proto)产出的 TableSchema 必须等价。

这是迁移期的切换闸门。判据定义在 ``core/schema_parity.py``。

**它是一次性闸门,不是长期护栏。** 等 xlsx 第 2~5 行被清掉,旧路就没有输入了,
这个脚本也就失效;那之后接替它的是 ``schema_proto`` 自身的 fail-closed 校验
(字段号复算、option 白名单、容量核对)与 ``tools/sandbox_export.py`` 的产物对拍。

「什么都没查」一律按失败处理:没有任何表被检查、或有表读不出来,都退非零。

用法::

    py tools/data_table_exporter/tools/verify_schema_parity.py
    py tools/data_table_exporter/tools/verify_schema_parity.py --only Skill -v
"""

from __future__ import annotations

import argparse
import logging
import subprocess
import sys
from pathlib import Path

_HERE = Path(__file__).resolve().parent
_ROOT = _HERE.parent
if str(_ROOT) not in sys.path:
    sys.path.insert(0, str(_ROOT))

from core.config_loader import load_config                         # noqa: E402
from core.excel_reader import read_table                           # noqa: E402
from core.file_utils import list_xlsx                              # noqa: E402
from core.schema_parity import diff                                # noqa: E402
from core.schema_proto import build_table_schema, schema_proto_path  # noqa: E402

logger = logging.getLogger("verify_parity")

_REPO = _ROOT.parent.parent
_REPO_PROTOC = _REPO / "third_party" / "grpc" / "install_vs2026_dbg" / "bin" / "protoc.exe"
_PROTOBUF_SRC = _REPO / "third_party" / "grpc" / "third_party" / "protobuf" / "src"


def lint_with_protoc(schema_dir: Path, protoc: Path) -> bool:
    """用 protoc 语法校验权威 schema。

    导表器自己用的是手写文本解析器,它默默容忍的东西没人查;所以必须有一道真 protoc
    把关,否则 ``data/schema/*.proto`` 会慢慢漂成「proto 味的 DSL」。
    """
    protos = sorted(p.name for p in schema_dir.glob("*.proto"))
    if not protos:
        logger.error("protoc lint:%s 下一个 .proto 都没有", schema_dir)
        return False
    cmd = [str(protoc), "--proto_path=.", "--proto_path=%s" % _PROTOBUF_SRC,
           "--descriptor_set_out=%s" % (schema_dir / ".lint.desc")] + protos
    proc = subprocess.run(cmd, cwd=str(schema_dir), capture_output=True, text=True)
    (schema_dir / ".lint.desc").unlink(missing_ok=True)
    if proc.returncode != 0:
        logger.error("protoc lint 失败:\n%s", (proc.stderr or proc.stdout).strip())
        return False
    logger.info("protoc lint:%d 份 schema 语法通过", len(protos))
    return True


def _already_migrated(xlsx: Path, cfg, schema_dir: Path) -> bool:
    """这张表已经迁完了吗?

    判据是两条同时成立:有权威 schema,且 xlsx 的类型行整行为空。
    满足时旧路没有输入,对拍这个动作本身就不适用了 —— 不能报成失败。
    """
    import openpyxl
    proto_dir_has = any(schema_dir.glob("*.proto")) if schema_dir.exists() else False
    if not proto_dir_has:
        return False
    wb = openpyxl.load_workbook(xlsx)
    try:
        ws = wb[wb.sheetnames[0]]
        if not (schema_dir / ("%s_table.proto" % ws.title.lower())).exists():
            return False
        type_row = cfg.metadata_rows.get("field_type", 2)
        return all(ws.cell(row=type_row, column=c).value in (None, "")
                   for c in range(1, (ws.max_column or 0) + 1))
    finally:
        wb.close()


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="新旧两条 schema 路径的等价性对拍")
    ap.add_argument("config", nargs="?", default=None)
    ap.add_argument("--only", action="append", default=None)
    ap.add_argument("--schema-dir", default=None)
    ap.add_argument("--require-all", action="store_true",
                    help="要求每一张表都已经有权威 schema,缺一张即失败")
    ap.add_argument("--protoc", default=None, help="protoc 路径,缺省用仓内自带的那个")
    ap.add_argument("--no-lint", action="store_true", help="跳过 protoc 语法校验")
    ap.add_argument("-v", "--verbose", action="store_true")
    args = ap.parse_args(argv)

    logging.basicConfig(level=logging.DEBUG if args.verbose else logging.INFO,
                        format="%(levelname)s %(message)s")
    cfg = load_config(args.config)
    schema_dir = Path(args.schema_dir) if args.schema_dir else (cfg.data_dir / "schema")

    lint_ok = True
    if not args.no_lint:
        protoc = Path(args.protoc) if args.protoc else _REPO_PROTOC
        if protoc.exists():
            lint_ok = lint_with_protoc(schema_dir, protoc)
        else:
            logger.warning("找不到 protoc(%s),跳过语法校验", protoc)

    seen: set[str] = set()
    checked = passed = 0
    missing: list[str] = []
    migrated: list[str] = []
    failures: dict[str, list[str]] = {}

    for xlsx in list_xlsx(cfg.data_dir):
        if _already_migrated(xlsx, cfg, schema_dir):
            migrated.append(xlsx.stem)
            continue
        old = read_table(xlsx, cfg)
        if old is None:
            failures[xlsx.name] = ["旧路 read_table 返回 None"]
            continue
        if args.only and old.name not in args.only:
            continue
        seen.add(old.name)
        proto = schema_proto_path(schema_dir, old.name)
        if not proto.exists():
            missing.append(old.name)
            continue
        checked += 1
        try:
            new = build_table_schema(proto, xlsx, check_positional=True)
        except Exception as exc:
            failures[old.name] = ["新路构造失败:%s" % exc]
            continue
        d = diff(old, new)
        if d:
            failures[old.name] = d
        else:
            passed += 1
            logger.info("OK   %-24s %d 个物理列", old.name, len(old.columns))

    for name in sorted(set(args.only or []) - seen):
        failures[name] = ["--only 指定的表不存在"]

    for name, msgs in failures.items():
        logger.error("FAIL %s", name)
        for m in msgs[:40]:
            logger.error("       %s", m)
        if len(msgs) > 40:
            logger.error("       ...(还有 %d 条)", len(msgs) - 40)

    if missing:
        level = logger.error if args.require_all else logger.info
        level("尚无权威 schema:%s", ", ".join(sorted(missing)))

    if migrated:
        logger.info("已迁移(旧表头已清空,对拍不适用):%d 张 —— %s",
                    len(migrated), ", ".join(sorted(migrated)))

    logger.info("对拍完成:%d 张一致 / %d 张有差异 / %d 张尚无权威 schema",
                passed, len(failures), len(missing))

    if checked == 0 and not migrated:
        logger.error("一张表都没检查到 —— 按失败处理(检查 --only 与 --schema-dir)")
        return 1
    if checked == 0 and migrated and not args.only:
        logger.info("全部表都已迁移,这道闸已经完成使命;"
                    "后续护栏是 schema_proto 自身的 fail-closed 校验 + sandbox_export 产物对拍")
    if failures or not lint_ok:
        return 1
    if missing and args.require_all:
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
