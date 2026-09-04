#!/usr/bin/env python3
"""从现有 xlsx 表头 + 现有产物 proto,播种 ``data/schema/<Sheet>_table.proto``。

这是「schema 从表格的副产品变成仓库里的源文件」这一步的 bootstrap。

三条硬纪律:

1. **字段号从现有产物 ``generated/code/proto/<sheet>_table.proto`` 里逐条抄,不重算。**
   抄不到就中止 —— 这个工具的全部契约就是「固化现状」,抄不到号就没有理由继续跑。
   产物比源表旧的时候,真实 server 列会在产物里查不到号;那种情况必须报错,
   绝不能把它当成「这一列被 owner 丢弃了」而发一个 9000+ 的号。
2. **写盘前先自检往返**:render 出来的文本立刻用 ``schema_proto`` 读回来,
   与旧路的 TableSchema 对拍;对不上就不落盘。写出一份自己都读不回来的 schema 是最坏的结果。
3. **默认不覆盖**。它是一次性播种器,第二次无声覆盖会抹掉人工维护的 ``reserved`` 与注释。
   要重播必须显式 ``--force``。

用法::

    py tools/data_table_exporter/tools/bootstrap_schema.py            # 播种缺失的
    py tools/data_table_exporter/tools/bootstrap_schema.py --only World --force
    py tools/data_table_exporter/tools/bootstrap_schema.py --out /tmp/x --dry-run
"""

from __future__ import annotations

import argparse
import logging
import re
import shutil
import sys
from pathlib import Path

_HERE = Path(__file__).resolve().parent
_ROOT = _HERE.parent                      # tools/data_table_exporter
if str(_ROOT) not in sys.path:
    sys.path.insert(0, str(_ROOT))

import openpyxl                                                    # noqa: E402

from core.config_loader import ExporterConfig, load_config          # noqa: E402
from core.excel_reader import read_table                            # noqa: E402
from core.file_utils import list_xlsx                               # noqa: E402
from core.schema import ColumnDef, TableSchema                      # noqa: E402
from core.schema_parity import diff                                 # noqa: E402
from core.schema_proto import (                                     # noqa: E402
    EXCLUDED_FIELD_NUMBER_BASE, LIST, MAP, SCALAR, SET, STRUCT_LIST,
    SchemaProtoError, build_table_schema, quote,
)

logger = logging.getLogger("bootstrap_schema")

_RE_MESSAGE = re.compile(r"^message\s+(\w+)\s*\{")
_RE_FIELD = re.compile(r"^\s*(.+?)\s+(\w+)\s*=\s*(\d+)\s*;")


class BootstrapError(RuntimeError):
    """播种前提不成立。一律中止,不写半份文件。"""


# ---------------------------------------------------------------------------
# 读现有产物 proto:字段号 + 子消息定义
# ---------------------------------------------------------------------------

def read_generated_proto(path: Path, sheet: str):
    """-> (主消息字段号 ``{名: 号}``, 子消息 ``{名: [(类型, 字段名, 号)]}``)"""
    if not path.exists():
        raise BootstrapError(
            "找不到产物 %s。本工具只从产物抄字段号,抄不到就不能继续 —— "
            "先跑一次 run.py 生成产物再来播种。" % path)
    main_name = "%sTable" % sheet
    numbers: dict[str, int] = {}
    subs: dict[str, list[tuple[str, str, int]]] = {}
    cur = None
    for line in path.read_text(encoding="utf-8").splitlines():
        m = _RE_MESSAGE.match(line)
        if m:
            cur = m.group(1)
            continue
        if line.startswith("}"):
            cur = None
            continue
        m = _RE_FIELD.match(line)
        if not m or cur is None:
            continue
        typ, name, num = m.group(1).strip(), m.group(2), int(m.group(3))
        if cur == main_name:
            numbers[name] = num
        elif cur != main_name + "Data":
            subs.setdefault(cur, []).append((typ, name, num))
    if not numbers:
        raise BootstrapError("产物 %s 里没有 message %s,无法抄字段号" % (path.name, main_name))
    return numbers, subs


# ---------------------------------------------------------------------------
# 从旧 TableSchema 反推每个逻辑字段
# ---------------------------------------------------------------------------

def _spans(xlsx: Path, sheet: str) -> list[tuple[int, int, str]]:
    # 与 core.schema_proto._header_spans 同因:read_only 模式下 max_column 不可靠。
    wb = openpyxl.load_workbook(xlsx)
    try:
        ws = wb[sheet]
        max_col = ws.max_column or 0
        starts = []
        for i in range(max_col):
            v = ws.cell(row=1, column=i + 1).value
            text = str(v).strip() if v is not None else ""
            if text:
                starts.append((i, text))
        return [(s, starts[k + 1][0] if k + 1 < len(starts) else max_col, name)
                for k, (s, name) in enumerate(starts)]
    finally:
        wb.close()


def _shape_of(col: ColumnDef) -> str:
    if col.struct == "":
        return SCALAR
    if col.struct == "repeated":
        return LIST
    if col.struct == "set":
        return SET
    if col.struct in ("map_key", "map_value"):
        return MAP
    if col.struct.startswith("message:"):
        return STRUCT_LIST
    raise BootstrapError("未知 struct: %r" % col.struct)


def _comment_lines(text: str) -> list[str]:
    """xlsx 第 5 行原文 -> ``//`` 注释行。

    用 ``splitlines()`` 而不是 ``split("\\n")``:两侧必须用同一套切分规则,
    否则 U+2028 / U+2085 这类行分隔符会在写侧留在行内、读侧被切开,整段注释静默丢掉。
    也不做 rstrip、不做缩进归一 —— ``schema_proto`` 按「去掉 ``//`` 再去掉至多一个空格」
    还原,两边合起来才是逐字符往返。Buff 那段 C++ enum 的缩进就靠这个。
    """
    if not text:
        return []
    return ["// " + ln if ln else "//" for ln in text.splitlines()]


def render(schema: TableSchema, gen_proto: Path) -> str:
    sheet = schema.name
    numbers, subs = read_generated_proto(gen_proto, sheet)
    by_index = {c.excel_index: c for c in schema.columns}
    spans = _spans(schema.source_path, sheet)

    out: list[str] = [
        'syntax = "proto3";',
        "",
        "// %s 表的权威 schema。源表 data/%s。" % (sheet, schema.source_path.name),
        "//",
        "// 本文件不参与 protoc 编译,也不进任何语言的构建;导表器直接文本解析它。",
        "// 字段号抄自 generated/code/proto/%s,**只增不改、删字段用 reserved**;"
        % gen_proto.name,
        "// 导表时会按产物发号规则复算并逐条核对,对不上直接中止。",
        "",
        "package mmorpg.cfgtable.v1;",
        "",
        'import "cfg_options.proto";',
        "",
    ]

    used_subs = sorted({c.struct.split(":", 1)[1] for c in schema.columns
                        if c.struct.startswith("message:")})
    for field_name in used_subs:
        msg_name = "%s%s" % (sheet, field_name)
        flds = subs.get(msg_name)
        if not flds:
            raise BootstrapError("产物 %s 里找不到子消息 %s" % (gen_proto.name, msg_name))
        out.append("message %s {" % msg_name)
        prefix = field_name + "_"
        for typ, name, num in sorted(flds, key=lambda t: t[2]):
            extra = "" if name.startswith(prefix) else " [(cfg_col) = %s]" % quote(name)
            out.append("  %s %s = %d%s;" % (typ, name, num, extra))
        out.append("}")
        out.append("")

    out.append("message %sTable {" % sheet)
    out.append("  option (cfg_sheet)       = %s;" % quote(sheet))
    out.append("  option (cfg_source_file) = %s;" % quote(schema.source_path.name))
    out.append('  option (cfg_primary_key) = "id";')
    out.append("")

    excluded_seq = 0
    for start, end, header in spans:
        col = by_index.get(start)
        if col is None:
            raise BootstrapError("[%s] 第 %d 列表头 %r 没有对应 ColumnDef" % (sheet, start + 1, header))
        span = end - start
        shape = _shape_of(col)
        opts: list[str] = []

        if shape == SCALAR:
            type_expr, slots = col.data_type, 1
        elif shape == LIST:
            type_expr, slots = "repeated %s" % col.data_type, span
        elif shape == SET:
            type_expr, slots = "map<%s, bool>" % col.data_type, span
            opts.append("(cfg_shape) = CFG_SHAPE_SET")
        elif shape == MAP:
            mf = schema.maps.get(col.name.rsplit("_", 1)[0])
            if mf is None:
                raise BootstrapError("[%s] 字段 %s 是 map,但 schema.maps 里找不到" % (sheet, header))
            type_expr, slots = "map<%s, %s>" % (mf.key_type, mf.value_type), span // 2
        else:                                       # STRUCT_LIST
            field_name = col.struct.split(":", 1)[1]
            n_sub = len({c.name for c in schema.columns
                         if c.struct == "message:%s" % field_name})
            type_expr, slots = "repeated %s%s" % (sheet, field_name), span // max(n_sub, 1)

        number = numbers.get(header)
        if number is None:
            # 「产物里查不到号」有两种成因,后果完全不同,必须分开。
            if col.is_server:
                raise BootstrapError(
                    "[%s] 字段 %s(owner=%s)会进产物,却在 %s 里找不到字段号 —— "
                    "产物比源表旧。先跑一次 run.py 再来播种。"
                    % (sheet, header, col.owner or "空", gen_proto.name))
            excluded_seq += 1
            number = EXCLUDED_FIELD_NUMBER_BASE + excluded_seq
            opts.append("(cfg_owner) = %s" % quote(col.owner or ""))
        else:
            if not col.is_server:
                raise BootstrapError(
                    "[%s] 字段 %s 的 owner=%r 不进产物,产物里却有它的字段号 %d"
                    % (sheet, header, col.owner, number))
            if col.owner and col.owner != "common":
                opts.append("(cfg_owner) = %s" % quote(col.owner))

        # 非标量一律写出容量,哪怕只有 1 个槽:容量是本次改造要显式化的东西,
        # 「省略即 1」会让它退回成隐式约定。SCALAR 恒为 1,不写。
        if shape != SCALAR:
            opts.insert(0, "(cfg_slots) = %d" % slots)

        for tok in col.options:
            low = tok.lower()
            if tok == "key":
                opts.append("(cfg_key) = true")
            elif tok == "multi":
                opts.append("(cfg_multi) = true")
            elif tok == "idx":
                opts.append("(cfg_index) = true")
            elif tok == "bit_index":
                opts.append("(cfg_bit_index) = true")
            elif tok == "tip_ref":
                opts.append("(cfg_tip_ref) = true")
            elif low.startswith("fk:"):
                opts.append("(cfg_fk) = %s" % quote(tok[3:]))
            elif low.startswith("gfk:"):
                opts.append("(cfg_gfk) = %s" % quote(tok[4:]))
            elif tok.startswith("expr:"):
                opts.append("(cfg_expr_type) = %s" % quote(tok[5:]))
            elif tok.startswith("expr_params:"):
                for p in tok[12:].split(","):
                    if p.strip():
                        opts.append("(cfg_expr_param) = %s" % quote(p.strip()))
            elif tok.startswith("composite:"):
                opts.append("(cfg_composite) = %s" % quote(tok[10:]))
            else:
                raise BootstrapError("[%s] 字段 %s 有未知 options token %r,"
                                     "先把它加进 cfg_options.proto 再来" % (sheet, header, tok))

        out.extend("  " + ln for ln in _comment_lines(col.comment))
        out.append("  %s %s = %d%s;"
                   % (type_expr, header, number, (" [%s]" % ", ".join(opts)) if opts else ""))

    out.append("}")
    out.append("")
    return "\n".join(out)


def _self_check(text: str, old: TableSchema, sheet: str, scratch: Path,
                vocab_src: Path) -> None:
    """把刚 render 出来的文本读回来,与旧路对拍。对不上就不许落盘。"""
    scratch.mkdir(parents=True, exist_ok=True)
    (scratch / "cfg_options.proto").write_bytes(vocab_src.read_bytes())
    probe = scratch / ("%s_table.proto" % sheet.lower())
    probe.write_text(text, encoding="utf-8", newline="\n")
    try:
        new = build_table_schema(probe, old.source_path, check_positional=True)
    except SchemaProtoError as exc:
        raise BootstrapError("[%s] 自检失败:生成的 schema 读不回来 —— %s" % (sheet, exc)) from None
    d = diff(old, new)
    if d:
        raise BootstrapError("[%s] 自检失败:读回来的 TableSchema 与旧路不等价\n  - %s"
                             % (sheet, "\n  - ".join(d[:20])))


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="播种权威 schema proto(只新增文件)")
    ap.add_argument("config", nargs="?", default=None)
    ap.add_argument("--only", action="append", default=None, help="只处理指定 sheet,可重复")
    ap.add_argument("--out", default=None, help="输出目录,缺省 <data_dir>/schema")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--force", action="store_true", help="允许覆盖已存在的权威 schema")
    args = ap.parse_args(argv)

    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
    cfg: ExporterConfig = load_config(args.config)
    default_dir = cfg.data_dir / "schema"
    out_dir = Path(args.out) if args.out else default_dir
    vocab_src = default_dir / "cfg_options.proto"
    if not vocab_src.exists():
        logger.error("找不到 option 词表 %s", vocab_src)
        return 1

    scratch = out_dir / ".selfcheck"
    seen: set[str] = set()
    ok = skipped = fail = 0
    for xlsx in list_xlsx(cfg.data_dir):
        old = read_table(xlsx, cfg)
        if old is None:
            logger.error("%s 读不出 schema", xlsx.name)
            fail += 1
            continue
        if args.only and old.name not in args.only:
            continue
        seen.add(old.name)
        target = out_dir / ("%s_table.proto" % old.name.lower())
        if target.exists() and not args.force and not args.dry_run:
            logger.info("跳过 %s:已存在(要重播请加 --force)", target.name)
            skipped += 1
            continue
        try:
            text = render(old, cfg.proto_dir / ("%s_table.proto" % old.name.lower()))
            _self_check(text, old, old.name, scratch, vocab_src)
        except (BootstrapError, SchemaProtoError) as exc:
            logger.error("[%s] %s", old.name, exc)
            fail += 1
            continue
        if args.dry_run:
            logger.info("[dry-run] 将写 %s(%d 行,自检通过)", target, text.count("\n"))
        else:
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(text, encoding="utf-8", newline="\n")
            # 每份 schema 都 import 词表,输出目录必须自洽。
            dst_vocab = out_dir / "cfg_options.proto"
            if not dst_vocab.exists() or not dst_vocab.samefile(vocab_src):
                dst_vocab.write_bytes(vocab_src.read_bytes())
            logger.info("写出 %s", target)
        ok += 1

    if scratch.exists():
        shutil.rmtree(scratch, ignore_errors=True)

    missing = sorted(set(args.only or []) - seen)
    if missing:
        logger.error("--only 指定的表不存在:%s", ", ".join(missing))
        fail += 1

    logger.info("完成:%d 张写出,%d 张跳过,%d 张失败", ok, skipped, fail)
    return 1 if fail else 0


if __name__ == "__main__":
    raise SystemExit(main())
