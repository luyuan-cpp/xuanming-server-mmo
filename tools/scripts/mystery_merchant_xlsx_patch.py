#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""神秘商人(docs/design/mystery-merchant.md §3)要对 xlsx 做的全部改动,写成可重复执行的脚本。

为什么是脚本而不是手改(理由与 equip_xlsx_patch.py 相同,这里只留结论):
  - data/tip/Tip.xlsx 同时有多个会话在写,它是二进制、不能 3-way 合并;唯一安全的写法是
    从磁盘重新载入 → 只往**空格子**里写自己那几行 → save → 读回来逐格核对。
  - 导表器要求 schema 与 xlsx **成对**:data/schema/ 下已有 3 张新表的 schema,缺 xlsx 时任何人跑
    `dev.bat gen` 都会整批失败。所以 new-tables 必须先于下一次导表执行,并与 schema 同批提交。
    新建的 xlsx 只在一台机器上生成并提交(openpyxl 每次写出的字节都不同)。

两个子命令都**幂等**:已经是目标状态就只报告、不改文件;发现与预期不符的现状就中止,绝不覆盖。

用法(仓库根目录执行;需要 Python 3 + openpyxl + pyyaml,与导表器同一套依赖):

    python tools/scripts/mystery_merchant_xlsx_patch.py new-tables   # ① 建 MysteryMerchantRule / Goods / Spot
    python tools/scripts/mystery_merchant_xlsx_patch.py tip-codes    # ② Tip.xlsx 新开 //mystery_merchant_error 段

加 --dry-run 只打印将要做什么,不写文件。

通用的 xlsx 读写与核对函数直接复用 equip_xlsx_patch.py 里的那一套(同目录,作为模块导入):
它们与具体玩法无关,第二份拷贝只会各自漂移。那个脚本改了这些函数的签名时,本脚本会在导入或
调用处当场报错,不会静默写坏表。

数值出处:docs/design/mystery-merchant.md §3(全部是占位建议)。改数值 = 改本文件顶部的常量表再重新
生成;表一旦被策划手调过,new-tables 会拒绝覆盖,此后直接改 xlsx。
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
# 数据常量表(设计文档 §3)。改数值只改这一段。
# ════════════════════════════════════════════════════════════════════════════

# §3.1 单行规则。
RULE_ROW = dict(
    id=1,
    interval_seconds=3600,     # 每个整点出现
    stay_seconds=1800,         # 停留 30 分钟(用户 2026-10-09 拍板)
    shelf_size=6,              # 每轮上架 6 件
    weight_discount60=50,      # 6 折 50%
    weight_discount50=35,      # 5 折 35%
    weight_discount40=15,      # 4 折 15%
    interact_range=8,          # 购买距离(服务器坐标单位;玩家身宽约 2)
    desc="神秘商人全局规则(单行)",
)

# §3.2 商品池。货币一律银两(currency_type = 0);价格是原价,量级参照宝宝改名 200、加点方案 1000。
#   展示名与帮会商店(GuildShop)卖同一物品时的叫法保持一致;物品 10 / 11 是战斗回血 / 回蓝药
#   (Item 表 battle_heal_hp = 300 / battle_heal_mp = 120),物品表没有名字列的值,这里按用途起名。
GOODS_ROWS = [
    # id  展示名            item_id  每份数量  货币  原价    权重   限购
    (1,   "回血丹",          10,      10,       0,    400,    100,   5),
    (2,   "回蓝丹",          11,      10,       0,    400,    100,   5),
    (3,   "培元丹",          15,      5,        0,    300,    100,   5),
    (4,   "回灵散",          16,      5,        0,    300,    100,   5),
    (5,   "精炼石",          17,      1,        0,    800,    80,    3),
    (6,   "修行秘录残页",    18,      1,        0,    1500,   60,    3),
    (7,   "灵兽口粮",        20,      10,       0,    1000,   80,    3),
    (8,   "玄铁护符",        12,      1,        0,    6000,   30,    1),
    (9,   "藏经阁手札",      13,      1,        0,    12000,  20,    1),
    (10,  "",                1102,    1,        0,    5000,   40,    1),   # 青锋剑(20 级武器),名字取物品表
    (11,  "",                1302,    1,        0,    4000,   40,    1),   # 青纱袍(20 级衣服)
    (12,  "",                1103,    1,        0,    20000,  15,    1),   # 寒铁剑(40 级武器)
]
GOODS_COLUMNS = ("id", "name", "item_id", "item_count", "currency_type", "price", "weight", "limit_count")

# §3.3 候选站位。全部在天墉城(scene_config_id = 1,出生点服务器坐标 (180, 200, 0))。
#   坐标是服务器坐标;括号里是对应的 Unity (x, z)。每个点都用客户端的可走掩码
#   (mmorpg-client TianyongPaintedCity.WalkMaskBase64,150×150、每格 2 单位)核对过:
#   与出生点连通,且以它为中心的 7×7 格(14×14 单位)全部可走。
SPOT_ROWS = [
    # id  地图  x       y       z     说明
    (1,   1,    176.0,  206.0,  0.0,  "中央广场·出生点旁(Unity 206,176)"),
    (2,   1,    161.0,  159.0,  0.0,  "中央广场西侧(Unity 159,161)"),
    (3,   1,    161.0,  243.0,  0.0,  "中央广场东侧(Unity 243,161)"),
    (4,   1,    255.0,  201.0,  0.0,  "北广场(Unity 201,255)"),
    (5,   1,    101.0,  201.0,  0.0,  "南大街(Unity 201,101)"),
    (6,   1,    113.0,  297.0,  0.0,  "东南小广场(Unity 297,113)"),
]
SPOT_COLUMNS = ("id", "scene_config_id", "x", "y", "z", "desc")

# §3.4 提示码。新域 mystery_merchant_error;fault = True 的是服务端内部故障(导表后进 tip.Faults)。
#   表里的码名**不带** k 前缀:导表器生成枚举时自己加(MysteryMerchantNotHere -> kMysteryMerchantNotHere)。
TIP_GROUP = "mystery_merchant_error"
TIP_BASE = 30000
TIP_WIDTH = 1000
TIP_CODES = [
    # 码名                               文案                                          fault
    ("MysteryMerchantNotPresent",       "神秘商人现在不在,请留意下次出现的公告",       False),
    ("MysteryMerchantNotHere",          "神秘商人不在这条线上",                       False),
    ("MysteryMerchantTooFar",           "离神秘商人太远了,走近一些再买",              False),
    ("MysteryMerchantShelfChanged",     "货架已经更新,请重新查看",                    False),
    ("MysteryMerchantLimitReached",     "这件商品本轮已达限购数量",                   False),
    ("MysteryMerchantCurrencyNotEnough", "货币不足,无法购买",                         False),
    ("MysteryMerchantBagFull",          "背包已满,无法购买",                          False),
    ("MysteryMerchantInBattle",         "战斗中不能购买",                             False),
    ("MysteryMerchantFrozen",           "角色正在跨区传送,暂时不能购买",              False),
    ("MysteryMerchantInvalidCount",     "购买数量无效",                               False),
    ("MysteryMerchantItemBlocked",      "这件商品暂时无法购买",                       False),
    ("MysteryMerchantInternalError",    "神秘商人暂时无法交易,请稍后再试",            True),
]


# ════════════════════════════════════════════════════════════════════════════
# 常量表自检:把本文件里的笔误拦在写任何文件之前
# ════════════════════════════════════════════════════════════════════════════

def self_check() -> None:
    rule = RULE_ROW
    if not 60 <= rule["interval_seconds"] <= 86400:
        fail("RULE_ROW.interval_seconds must be within 60..86400")
    if not 30 <= rule["stay_seconds"] <= rule["interval_seconds"]:
        fail("RULE_ROW.stay_seconds must be within 30..interval_seconds")
    if not 1 <= rule["shelf_size"] <= 32:
        fail("RULE_ROW.shelf_size must be within 1..32")
    if rule["weight_discount60"] + rule["weight_discount50"] + rule["weight_discount40"] <= 0:
        fail("RULE_ROW: the three discount weights must sum to > 0")
    if not 1 <= rule["interact_range"] <= 100:
        fail("RULE_ROW.interact_range must be within 1..100")

    goods_ids = [row[0] for row in GOODS_ROWS]
    if len(set(goods_ids)) != len(goods_ids):
        fail("GOODS_ROWS has duplicate ids")
    for goods_id, _name, item_id, item_count, currency_type, price, _weight, limit_count in GOODS_ROWS:
        if item_id <= 0 or item_count <= 0:
            fail(f"GOODS_ROWS id={goods_id}: item_id / item_count must be positive")
        if currency_type not in (0, 1, 2):
            fail(f"GOODS_ROWS id={goods_id}: currency_type must be 0 / 1 / 2")
        if not 1 <= price <= 1_000_000_000:
            fail(f"GOODS_ROWS id={goods_id}: price must be within 1..1000000000")
        if not 1 <= limit_count <= 99:
            fail(f"GOODS_ROWS id={goods_id}: limit_count must be within 1..99")
    if sum(row[6] for row in GOODS_ROWS) <= 0:
        fail("GOODS_ROWS: all shelf weights are 0, so the shelf would always be empty")

    spot_ids = [row[0] for row in SPOT_ROWS]
    if len(set(spot_ids)) != len(spot_ids):
        fail("SPOT_ROWS has duplicate ids")
    if any(row[1] <= 0 for row in SPOT_ROWS):
        fail("SPOT_ROWS: scene_config_id must be positive")

    names = [name for name, _text, _fault in TIP_CODES]
    if len(set(names)) != len(names):
        fail("TIP_CODES has duplicate code names")
    if len(TIP_CODES) > TIP_WIDTH:
        fail("TIP_CODES exceeds the range width")


# ════════════════════════════════════════════════════════════════════════════
# ① new-tables
# ════════════════════════════════════════════════════════════════════════════

def new_tables() -> dict[str, list[dict]]:
    return {
        "MysteryMerchantRule": [dict(RULE_ROW)],
        "MysteryMerchantGoods": [dict(zip(GOODS_COLUMNS, row)) for row in GOODS_ROWS],
        "MysteryMerchantSpot": [dict(zip(SPOT_COLUMNS, row)) for row in SPOT_ROWS],
    }


def cmd_new_tables(dry_run: bool) -> None:
    for sheet, rows in new_tables().items():
        xlsx_path = DATA_DIR / f"{sheet}.xlsx"
        layout = schema_layout(sheet)
        if any(f.width != 1 for f in layout):
            fail(f"the {sheet} schema has non-scalar columns, but new-tables only writes all-scalar tables; update the script first")
        names = [f.name for f in layout]
        for row in rows:
            if list(row.keys()) != names:
                fail(f"{sheet}: data row keys {list(row.keys())} differ from the schema fields {names} -- the schema columns changed; update this script first")

        wanted = [[None] * len(names) for _ in range(DATA_BEGIN_ROW - 1 + len(rows))]
        wanted[HEADER_ROW - 1] = list(names)
        wanted[COMMENT_ROW - 1] = [f.comment or None for f in layout]
        for offset, row in enumerate(rows):
            # 空字符串按「留空」写(openpyxl 读回来是 None;导表器把空格子当该类型的零值)。
            wanted[DATA_BEGIN_ROW - 1 + offset] = [None if row[name] == "" else row[name] for name in names]

        if xlsx_path.exists():
            ws = only_sheet(openpyxl.load_workbook(xlsx_path), xlsx_path)
            if ws.title == sheet and first_difference(sheet_matrix(ws), wanted) is None:
                print(f"[ALREADY CURRENT] {rel(xlsx_path)} exists with identical contents ({len(rows)} rows); file unchanged.")
                continue
            fail(f"{rel(xlsx_path)} already exists with different contents (designers may have tuned the values) -- this script does not overwrite it.")

        print(f"Will create {rel(xlsx_path)}: sheet={sheet!r}, {len(names)} columns x {len(rows)} data rows")
        print(f"  columns: {names}")
        print(f"  first row: {[rows[0][n] for n in names]}  last row: {[rows[-1][n] for n in names]}")
        if dry_run:
            continue
        wb = openpyxl.Workbook()
        ws = wb.active
        # 首个 sheet 的名字必须等于 cfg_sheet:导表器按它匹配 schema。
        ws.title = sheet
        for r, values in enumerate(wanted, start=1):
            for c, value in enumerate(values, start=1):
                if value is not None:
                    ws.cell(row=r, column=c, value=value)
        wb.save(xlsx_path)

        check_ws = only_sheet(openpyxl.load_workbook(xlsx_path), xlsx_path, sheet)
        diff = first_difference(sheet_matrix(check_ws), wanted)
        if diff:
            fail(f"after saving, {xlsx_path.name} read back differs from what was written ({diff}) -- delete the file and retry")
        bind(sheet, xlsx_path)
        print(f"[DONE] {rel(xlsx_path)}")
    if dry_run:
        print("[dry-run] No files written.")


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
             f"and update docs/design/mystery-merchant.md §3.4.")
    reserved = [f"row {row} {text!r}" for row, text in notes if str(TIP_BASE) in text]
    if reserved:
        fail(f"range {TIP_BASE} is reserved by note rows: {reserved} -- switch to the next free thousand-wide range and update design doc §3.4.")
    if TIP_STATE.exists():
        state_groups = json.loads(TIP_STATE.read_text(encoding="utf-8")).get("groups", {})
        tomb = [f"{n} [{g['base']},{g['base'] + g['width']})" for n, g in state_groups.items()
                if int(g["base"]) < hi and lo < int(g["base"]) + int(g["width"])]
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
          f"After exporting, check: the ids of these {len(wanted)} codes should be {lo}..{lo + len(wanted) - 1}; "
          f"register the new domain's proto/tip/{TIP_GROUP}_tip.pb.{{h,cc}} in CMakeLists.txt and table.vcxproj under cpp/generated/table.")


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
