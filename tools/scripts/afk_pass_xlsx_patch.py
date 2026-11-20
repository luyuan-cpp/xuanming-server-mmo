#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""挂机月卡(docs/design/afk-monthly-card.md §4.4、§4.5)要对 xlsx 做的全部改动,写成可重复执行的脚本。

写法与理由同 mystery_merchant_xlsx_patch.py(这里只留结论):
  - data/tip/Tip.xlsx 多个会话同时在写,它是二进制、不能三方合并:从磁盘重新载入 → 只往空格子里写自己那几行 →
    save → 读回来逐格核对。
  - 导表器要求 schema 与 xlsx 成对:data/schema/afkpassrule_table.proto 已经在了,缺 xlsx 时任何人跑 `dev.bat gen`
    都会整批失败,所以 new-tables 必须先于下一次导表执行,并与 schema 同批提交。新建的 xlsx 只在一台机器上生成并提交
    (openpyxl 每次写出的字节都不同)。

两个子命令都幂等:已经是目标状态就只报告、不改文件;发现与预期不符的现状就中止,绝不覆盖。

用法(仓库根目录执行;需要 Python 3 + openpyxl + pyyaml,与导表器同一套依赖):

    python tools/scripts/afk_pass_xlsx_patch.py new-tables   # ① 建 AfkPassRule.xlsx(单行规则)
    python tools/scripts/afk_pass_xlsx_patch.py tip-codes    # ② Tip.xlsx 新开 //afk_pass_error 段

加 --dry-run 只打印将要做什么,不写文件。通用的 xlsx 读写与核对函数复用 equip_xlsx_patch.py(同目录,作为模块导入)。

数值出处:设计文档 §4.4。改数值 = 改本文件顶部的常量再重新生成;表一旦被策划手调过,new-tables 拒绝覆盖,此后直接改 xlsx。
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
try:
    import openpyxl
    from equip_xlsx_patch import (
        COMMENT_ROW,
        DATA_BEGIN_ROW,
        DATA_DIR,
        HEADER_ROW,
        TIP_STATE,
        TIP_XLSX,
        Patch,
        apply_patches_and_verify,
        bind,
        fail,
        first_difference,
        only_sheet,
        rel,
        schema_layout,
        sheet_matrix,
        tip_parse,
    )
except ImportError as exc:  # pragma: no cover - 环境问题,给一句能照着做的话
    sys.exit(f"cannot load dependencies ({exc}): run  pip install pyyaml openpyxl  first, and make sure equip_xlsx_patch.py is in the same directory")


# ════════════════════════════════════════════════════════════════════════════
# 数据常量(设计文档 §4.4、§4.5)。改数值只改这一段。
# ════════════════════════════════════════════════════════════════════════════

SHEET = "AfkPassRule"
RULE_ROW = dict(
    id=1,
    period_days=30,            # 开通一次 30 天
    price_currency_type=1,     # 灵石(CURRENCY_TYPE_DIAMOND)
    price=300,                 # 正好是 30 元充值档的灵石数(payment-recharge.md §4)
    max_remaining_days=180,    # 叠加上限:剩余最多 180 天
    remind_before_hours=72,    # 剩余 3 天内提醒
    desc="挂机月卡全局规则(单行)",
)

# 新域 afk_pass_error;fault = True 的是服务端内部故障(导表后进 tip.Faults)。
#   码名不带 k 前缀:导表器生成枚举时自己加(AfkPassPriceChanged -> kAfkPassPriceChanged)。
#   C++ 按枚举名引用,顺序就是发号顺序(32000 起),不要重排。
TIP_GROUP = "afk_pass_error"
TIP_BASE = 32000
TIP_WIDTH = 1000
TIP_CODES = [
    # 码名                          文案                                          fault
    ("AfkPassPriceChanged",       "挂机月卡价格已变化,请刷新后再试",              False),
    ("AfkPassCurrencyNotEnough",  "灵石不足,无法开通挂机月卡",                    False),
    ("AfkPassStackLimit",         "挂机月卡剩余时长已达上限,暂不能续费",          False),
    ("AfkPassFrozen",             "角色当前状态不能开通挂机月卡,请稍后再试",      False),
    ("AfkPassUnavailable",        "挂机月卡暂时无法开通",                         True),
]


# ════════════════════════════════════════════════════════════════════════════
# 常量自检:把本文件里的笔误拦在写任何文件之前(取值范围与 scene 的启动校验 afk_pass_rules.h 一致)
# ════════════════════════════════════════════════════════════════════════════

def self_check() -> None:
    rule = RULE_ROW
    if rule["id"] != 1:
        fail("RULE_ROW.id must be 1 (scene reads the first row only)")
    if not 1 <= rule["period_days"] <= 366:
        fail("RULE_ROW.period_days must be within 1..366")
    if rule["price_currency_type"] not in (0, 1, 2):
        fail("RULE_ROW.price_currency_type must be 0 / 1 / 2")
    if not 1 <= rule["price"] <= 1_000_000_000:
        fail("RULE_ROW.price must be within 1..1000000000")
    if not rule["period_days"] <= rule["max_remaining_days"] <= 3660:
        fail("RULE_ROW.max_remaining_days must be within period_days..3660")
    if not 0 <= rule["remind_before_hours"] <= 720:
        fail("RULE_ROW.remind_before_hours must be within 0..720")

    names = [name for name, _text, _fault in TIP_CODES]
    if len(set(names)) != len(names):
        fail("TIP_CODES has duplicate code names")
    if len(TIP_CODES) > TIP_WIDTH:
        fail("TIP_CODES exceeds the range width")


# ════════════════════════════════════════════════════════════════════════════
# ① new-tables
# ════════════════════════════════════════════════════════════════════════════

def cmd_new_tables(dry_run: bool) -> None:
    xlsx_path = DATA_DIR / f"{SHEET}.xlsx"
    layout = schema_layout(SHEET)
    if any(f.width != 1 for f in layout):
        fail(f"the {SHEET} schema has non-scalar columns, but new-tables only writes all-scalar tables; update the script first")
    names = [f.name for f in layout]
    if list(RULE_ROW.keys()) != names:
        fail(f"{SHEET}: data row keys {list(RULE_ROW.keys())} differ from the schema fields {names} -- the schema columns changed; update this script first")

    wanted = [[None] * len(names) for _ in range(DATA_BEGIN_ROW)]
    wanted[HEADER_ROW - 1] = list(names)
    wanted[COMMENT_ROW - 1] = [f.comment or None for f in layout]
    # 空字符串按「留空」写(openpyxl 读回来是 None;导表器把空格子当该类型的零值)。
    wanted[DATA_BEGIN_ROW - 1] = [None if RULE_ROW[name] == "" else RULE_ROW[name] for name in names]

    if xlsx_path.exists():
        ws = only_sheet(openpyxl.load_workbook(xlsx_path), xlsx_path)
        if ws.title == SHEET and first_difference(sheet_matrix(ws), wanted) is None:
            print(f"[ALREADY CURRENT] {rel(xlsx_path)} exists with identical contents; file unchanged.")
            return
        fail(f"{rel(xlsx_path)} already exists with different contents (designers may have tuned the values) -- this script does not overwrite it.")

    print(f"Will create {rel(xlsx_path)}: sheet={SHEET!r}, {len(names)} columns x 1 data row")
    print(f"  columns: {names}")
    print(f"  row: {[RULE_ROW[n] for n in names]}")
    if dry_run:
        print("[dry-run] No files written.")
        return
    wb = openpyxl.Workbook()
    ws = wb.active
    # 首个 sheet 的名字必须等于 cfg_sheet:导表器按它匹配 schema。
    ws.title = SHEET
    for r, values in enumerate(wanted, start=1):
        for c, value in enumerate(values, start=1):
            if value is not None:
                ws.cell(row=r, column=c, value=value)
    wb.save(xlsx_path)

    check_ws = only_sheet(openpyxl.load_workbook(xlsx_path), xlsx_path, SHEET)
    diff = first_difference(sheet_matrix(check_ws), wanted)
    if diff:
        fail(f"after saving, {xlsx_path.name} read back differs from what was written ({diff}) -- delete the file and retry")
    bind(SHEET, xlsx_path)
    print(f"[DONE] {rel(xlsx_path)}")


# ════════════════════════════════════════════════════════════════════════════
# ② tip-codes
# ════════════════════════════════════════════════════════════════════════════

def cmd_tip_codes(dry_run: bool) -> None:
    wb = openpyxl.load_workbook(TIP_XLSX)
    ws = only_sheet(wb, TIP_XLSX)
    before = sheet_matrix(ws)
    header = [str(v).strip().lower() if v is not None else "" for v in before[HEADER_ROW - 1]]
    if header[:2] != ["name", "commen"] or header.count("fault") != 1:
        fail(f"{TIP_XLSX.name} row 1 should be name / commen / ... / fault (exactly one fault column), got {before[HEADER_ROW - 1]}")
    fault_col = header.index("fault") + 1

    groups, notes = tip_parse(before, fault_col)
    header_text = f"//{TIP_GROUP} base={TIP_BASE} width={TIP_WIDTH}"
    wanted = [(name, text, 1 if fault else None) for name, text, fault in TIP_CODES]
    new_names = [name for name, _t, _f in TIP_CODES]

    mine = next((g for g in groups if g["name"] == TIP_GROUP), None)
    if mine is not None:
        # 已是目标状态 = 段声明一致,且这批码是本组开头的连续一段。不要求恰好在组尾:以后还会往组尾追加。
        if (mine["base"], mine["width"]) == (TIP_BASE, TIP_WIDTH) and mine["codes"][:len(wanted)] == wanted:
            print(f"[ALREADY CURRENT] {header_text} (row {mine['row']}) already has these {len(wanted)} codes; file unchanged.")
            return
        fail(f"{TIP_XLSX.name} row {mine['row']} already has group //{TIP_GROUP}, but its range declaration or first {len(wanted)} codes differ from this script -- find out who added it first.")

    # 以下四项都是「先核对再动手」:码名全表唯一、段没被占、没被说明行预留、发号 state 里也不是墓碑。
    owner = {code: g["name"] for g in groups for code, _t, _f in g["codes"]}
    clash = [f"{n} (in {owner[n]})" for n in new_names if n in owner]
    if clash:
        fail(f"these code names are already in the table: {clash} -- code names are globally unique; find out first.")
    lo, hi = TIP_BASE, TIP_BASE + TIP_WIDTH
    overlap = [f"{g['name']} [{g['base']},{g['base'] + g['width']})" for g in groups
               if g["base"] < hi and lo < g["base"] + g["width"]]
    if overlap:
        fail(f"range [{lo},{hi}) is already taken: {overlap} -- switch to the next free thousand-wide range (change TIP_BASE) "
             f"and update docs/design/afk-monthly-card.md §4.5.")
    reserved = [f"row {row} {text!r}" for row, text in notes if str(TIP_BASE) in text]
    if reserved:
        fail(f"range {TIP_BASE} is reserved by note rows: {reserved} -- switch to the next free thousand-wide range and update design doc §4.5.")
    if TIP_STATE.exists():
        state_groups = json.loads(TIP_STATE.read_text(encoding="utf-8")).get("groups", {})
        tomb = [f"{n} [{g['base']},{g['base'] + g['width']})" for n, g in state_groups.items()
                if n != TIP_GROUP and int(g["base"]) < hi and lo < int(g["base"]) + int(g["width"])]
        if tomb:
            fail(f"in the ID state ({rel(TIP_STATE)}), range [{lo},{hi}) already belongs to {tomb} -- pick another range; do not edit the state by hand.")

    last_row = max((r for r, row in enumerate(before, start=1) if any(v is not None for v in row)), default=0)
    header_row = last_row + 2          # 与既有组一样,组头前留一个空行
    patches: list[Patch] = [(header_row, 1, header_text)]
    print(f"{TIP_XLSX.name}: {len(groups)} groups, {len(owner)} codes, last row {last_row}; "
          f"range [{lo},{hi}) is free and all {len(new_names)} code names are unique in the table. Will append:")
    print(f"  row {header_row}: {header_text}")
    for offset, (name, text, fault) in enumerate(wanted, start=1):
        row_idx = header_row + offset
        patches += [(row_idx, 1, name), (row_idx, 2, text)]
        if fault is not None:
            patches.append((row_idx, fault_col, fault))
        print(f"  row {row_idx}: {name} | {text}" + (" | fault=1" if fault else ""))
    if dry_run:
        print("[dry-run] No files written.")
        return
    apply_patches_and_verify(TIP_XLSX, None, wb, ws, before, patches, bind_schema=False)

    check_ws = only_sheet(openpyxl.load_workbook(TIP_XLSX), TIP_XLSX)
    groups_after, _notes = tip_parse(sheet_matrix(check_ws), fault_col)
    if [(g["name"], g["codes"]) for g in groups_after[:-1]] != [(g["name"], g["codes"]) for g in groups] \
            or groups_after[-1]["name"] != TIP_GROUP or groups_after[-1]["codes"] != wanted:
        fail(f"after saving, the codes per group are not as expected -- restore it now with git checkout -- {rel(TIP_XLSX)}")
    print(f"[DONE] groups {len(groups)} -> {len(groups_after)}; the existing {len(owner)} codes are identical cell by cell. "
          f"After exporting, check: the ids of these {len(wanted)} codes should be {lo}..{lo + len(wanted) - 1}.")


COMMANDS = {
    "new-tables": cmd_new_tables,
    "tip-codes": cmd_tip_codes,
}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("command", choices=tuple(COMMANDS))
    parser.add_argument("--dry-run", action="store_true", help="只打印将要做什么,不写文件")
    args = parser.parse_args()
    self_check()
    COMMANDS[args.command](args.dry_run)


if __name__ == "__main__":
    # Windows 默认控制台编码打不出部分字符时会在 print 上抛 UnicodeEncodeError,而那时文件可能已经写完 ——
    # 统一成 utf-8,免得"改成功了却以非 0 退出"误导人。
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="replace")
    main()
