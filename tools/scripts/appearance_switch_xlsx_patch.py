#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""角色形象切换（10 元角色形象卡）与一小时免费试用（bit 存储）的 xlsx 幂等补丁脚本。

改动范围(全部幂等,重复执行无副作用,发现现状与预期冲突则中止且绝不覆盖已有数据):
  1. new-tables:       新建 CharacterAppearance.xlsx(11 个现役形象,id 1..11)与 AppearanceRule.xlsx(单行规则)
  2. card-item-row:    Item.xlsx 追加 3402 角色形象卡
  3. tip-codes:        Tip.xlsx 文末新开 //appearance_error base=33000 width=1000(7 个提示码)

用法(仓库根目录执行):
    python tools/scripts/appearance_switch_xlsx_patch.py all
    python tools/scripts/appearance_switch_xlsx_patch.py all --dry-run
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
        ITEM_SHEET,
        ITEM_XLSX,
        TIP_STATE,
        TIP_XLSX,
        Patch,
        apply_patches_and_verify,
        bind,
        fail,
        first_difference,
        header_cells,
        item_row_cells,
        only_sheet,
        rel,
        schema_layout,
        sheet_matrix,
        tip_parse,
    )
except ImportError as exc:
    sys.exit(f"cannot load dependencies ({exc}): run  pip install pyyaml openpyxl  first")


APPEARANCE_SHEET = "CharacterAppearance"
APPEARANCE_ROWS = [
    dict(id=1, appearance_id="08_alchemy_prodigy_boy", name="炼丹童子", enabled=1, sort_order=1, desc="丹炉紫气绕身，灵药随行"),
    dict(id=2, appearance_id="00_reference_topright_boy", name="金发带Q道童", enabled=1, sort_order=2, desc="金冠束发，葫芦傍身的青玉小道童"),
    dict(id=3, appearance_id="01_ice_sword_girl", name="冰剑少女", enabled=1, sort_order=3, desc="寒霜凝刃，清冷出尘"),
    dict(id=4, appearance_id="02_fire_talisman_boy", name="火符少年", enabled=1, sort_order=4, desc="朱砂赤符，烈焰破邪"),
    dict(id=5, appearance_id="03_lotus_healer_girl", name="莲花医者", enabled=1, sort_order=5, desc="青莲济世，妙手回春"),
    dict(id=6, appearance_id="04_mountain_guardian_boy", name="山岳守卫", enabled=1, sort_order=6, desc="玄甲镇岳，稳如磐石"),
    dict(id=7, appearance_id="05_celestial_musician_girl", name="天音少女", enabled=1, sort_order=7, desc="瑶琴仙乐，清音涤尘"),
    dict(id=8, appearance_id="09_bamboo_archer_girl", name="竹弓少女", enabled=1, sort_order=8, desc="青竹穿云，百步穿杨"),
    dict(id=9, appearance_id="10_crimson_spear_girl", name="赤枪少女", enabled=1, sort_order=9, desc="红缨长枪，飒爽凌云"),
    dict(id=10, appearance_id="15_water_dragon_scholar_boy", name="水龙书生", enabled=1, sort_order=10, desc="沧澜化龙，文武兼修"),
    dict(id=11, appearance_id="17_ghost_script_calligrapher_boy", name="灵篆书生", enabled=1, sort_order=11, desc="墨笔天篆，镇妖驱魔"),
]

RULE_SHEET = "AppearanceRule"
RULE_ROWS = [
    dict(
        id=1,
        card_item_id=3402,
        card_item_count=1,
        trial_duration_seconds=3600,
        mall_sku_id=3402,
        mall_price_fen=1000,
        desc="角色形象切换与一小时免费试用全局规则(单行)",
    ),
]

APPEARANCE_CARD_ITEM = dict(
    id=3402,
    max_stack_size=99,
    equip_kind=0,
    battle_usable=0,
    battle_heal_hp=0,
    battle_heal_mp=0,
    name="角色形象卡",
    description="使用后可永久切换一次角色形象，切换后将替换当前形象（商城售价 10 元）。",
    icon_key="UI/Icons/item_appearance_card",
    equip_level=0,
    equip_class=0,
    affix_pool=0,
    affix_rule=0,
    base_attr=[],
    equip_profile=0,
    forge_material_id=0,
    forge_material_count=0,
    artifact_id=0,
)

TIP_GROUP = "appearance_error"
TIP_BASE = 33000
TIP_WIDTH = 1000
TIP_CODES = [
    ("AppearanceInvalid", "所选角色形象不可用", False),
    ("AppearanceAlreadyCurrent", "当前已是该角色形象，无需重复切换", False),
    ("AppearanceNoCard", "背包中没有可用的角色形象卡，请前往商城购买", False),
    ("AppearanceTrialAlreadyUsed", "该角色形象已使用过免费试用机会", False),
    ("AppearanceNoActiveTrial", "当前没有进行中的形象试用", False),
    ("AppearanceFrozen", "当前角色状态暂时不能切换形象，请稍后再试", False),
    ("AppearanceUnavailable", "角色形象服务暂时不可用", True),
]


def self_check() -> None:
    seen_ids = set()
    seen_app = set()
    for row in APPEARANCE_ROWS:
        if row["id"] <= 0 or row["id"] in seen_ids:
            fail(f"invalid or duplicate CharacterAppearance id: {row['id']}")
        if not row["appearance_id"] or row["appearance_id"] in seen_app:
            fail(f"invalid or duplicate appearance_id: {row['appearance_id']}")
        seen_ids.add(row["id"])
        seen_app.add(row["appearance_id"])
    rule = RULE_ROWS[0]
    if rule["id"] != 1 or rule["card_item_id"] != APPEARANCE_CARD_ITEM["id"]:
        fail("AppearanceRule card_item_id must match APPEARANCE_CARD_ITEM.id")
    if not 1 <= rule["card_item_count"] <= 99:
        fail("AppearanceRule card_item_count out of range")
    if not 60 <= rule["trial_duration_seconds"] <= 86400:
        fail("AppearanceRule trial_duration_seconds out of range")
    if len({name for name, _, _ in TIP_CODES}) != len(TIP_CODES):
        fail("TIP_CODES has duplicate code names")


def write_scalar_table(sheet: str, rows: list[dict], dry_run: bool) -> None:
    xlsx_path = DATA_DIR / f"{sheet}.xlsx"
    layout = schema_layout(sheet)
    if any(f.width != 1 for f in layout):
        fail(f"the {sheet} schema has non-scalar columns")
    names = [f.name for f in layout]
    for idx, row in enumerate(rows):
        if list(row.keys()) != names:
            fail(f"{sheet} row {idx} keys {list(row.keys())} != schema fields {names}")

    wanted = [[None] * len(names) for _ in range(DATA_BEGIN_ROW - 1 + len(rows))]
    wanted[HEADER_ROW - 1] = list(names)
    wanted[COMMENT_ROW - 1] = [f.comment or None for f in layout]
    for offset, row in enumerate(rows):
        wanted[DATA_BEGIN_ROW - 1 + offset] = [None if row[n] == "" else row[n] for n in names]

    if xlsx_path.exists():
        ws = only_sheet(openpyxl.load_workbook(xlsx_path), xlsx_path)
        if ws.title == sheet and first_difference(sheet_matrix(ws), wanted) is None:
            print(f"[ALREADY CURRENT] {rel(xlsx_path)} exists with identical contents; file unchanged.")
            return
        fail(f"{rel(xlsx_path)} already exists with different contents -- refusing to overwrite.")

    print(f"Will create {rel(xlsx_path)}: sheet={sheet!r}, {len(names)} columns x {len(rows)} rows")
    if dry_run:
        return
    wb = openpyxl.Workbook()
    ws = wb.active
    ws.title = sheet
    for r, values in enumerate(wanted, start=1):
        for c, value in enumerate(values, start=1):
            if value is not None:
                ws.cell(row=r, column=c, value=value)
    wb.save(xlsx_path)
    check_ws = only_sheet(openpyxl.load_workbook(xlsx_path), xlsx_path, sheet)
    diff = first_difference(sheet_matrix(check_ws), wanted)
    if diff:
        fail(f"after saving, {xlsx_path.name} read back differs ({diff})")
    bind(sheet, xlsx_path)
    print(f"[DONE] {rel(xlsx_path)}")


def cmd_new_tables(dry_run: bool) -> None:
    write_scalar_table(APPEARANCE_SHEET, APPEARANCE_ROWS, dry_run)
    write_scalar_table(RULE_SHEET, RULE_ROWS, dry_run)


def cmd_card_item_row(dry_run: bool) -> None:
    layout = schema_layout(ITEM_SHEET)
    row = APPEARANCE_CARD_ITEM
    if set(row.keys()) != {f.name for f in layout}:
        fail(
            f"APPEARANCE_CARD_ITEM keys differ from Item schema fields: "
            f"extra={sorted(set(row) - {f.name for f in layout})}, "
            f"missing={sorted({f.name for f in layout} - set(row))}"
        )

    wb = openpyxl.load_workbook(ITEM_XLSX)
    ws = only_sheet(wb, ITEM_XLSX, ITEM_SHEET)
    before = sheet_matrix(ws)
    header = header_cells(ws)
    bind(ITEM_SHEET, ITEM_XLSX)

    id_col = header["id"]
    existing: dict[int, int] = {}
    last_data_row = DATA_BEGIN_ROW - 1
    for row_idx in range(DATA_BEGIN_ROW, ws.max_row + 1):
        val = ws.cell(row=row_idx, column=id_col).value
        if val is None or val == "":
            continue
        existing[int(val)] = row_idx
        last_data_row = row_idx

    wanted = item_row_cells(row, layout, header)
    if row["id"] in existing:
        row_idx = existing[row["id"]]
        actual = {c: v for c, v in enumerate(before[row_idx - 1], start=1) if v is not None}
        if actual == wanted:
            print(f"[ALREADY CURRENT] {rel(ITEM_XLSX)} already has appearance card id={row['id']} on row {row_idx}.")
            return
        fail(f"{rel(ITEM_XLSX)} row {row_idx} already has id={row['id']} with different contents -- refusing to overwrite")

    target_row = last_data_row + 1
    patches: list[Patch] = [(target_row, col, val) for col, val in sorted(wanted.items())]
    print(f"{rel(ITEM_XLSX)}: appending appearance card id={row['id']} ({row['name']}) at row {target_row}")
    if dry_run:
        return
    apply_patches_and_verify(ITEM_XLSX, ITEM_SHEET, wb, ws, before, patches, bind_schema=True)
    print(f"[DONE] {rel(ITEM_XLSX)} appended id={row['id']}; schema binding passed.")


def cmd_tip_codes(dry_run: bool) -> None:
    wb = openpyxl.load_workbook(TIP_XLSX)
    ws = only_sheet(wb, TIP_XLSX)
    before = sheet_matrix(ws)
    header = [str(v).strip().lower() if v is not None else "" for v in before[HEADER_ROW - 1]]
    if header[:2] != ["name", "commen"] or header.count("fault") != 1:
        fail(f"{TIP_XLSX.name} row 1 header mismatch: {before[HEADER_ROW - 1]}")
    fault_col = header.index("fault") + 1

    groups, notes = tip_parse(before, fault_col)
    header_text = f"//{TIP_GROUP} base={TIP_BASE} width={TIP_WIDTH}"
    wanted = [(name, text, 1 if fault else None) for name, text, fault in TIP_CODES]
    new_names = [name for name, _t, _f in TIP_CODES]

    mine = next((g for g in groups if g["name"] == TIP_GROUP), None)
    if mine is not None:
        if (mine["base"], mine["width"]) == (TIP_BASE, TIP_WIDTH) and mine["codes"][:len(wanted)] == wanted:
            print(f"[ALREADY CURRENT] {header_text} (row {mine['row']}) already has these {len(wanted)} codes; file unchanged.")
            return
        fail(f"{TIP_XLSX.name} row {mine['row']} already has group //{TIP_GROUP} with different contents")

    owner = {code: g["name"] for g in groups for code, _t, _f in g["codes"]}
    clash = [f"{n} (in {owner[n]})" for n in new_names if n in owner]
    if clash:
        fail(f"duplicate tip code names: {clash}")
    lo, hi = TIP_BASE, TIP_BASE + TIP_WIDTH
    overlap = [f"{g['name']} [{g['base']},{g['base'] + g['width']})" for g in groups if g["base"] < hi and lo < g["base"] + g["width"]]
    if overlap:
        fail(f"range [{lo},{hi}) is already taken: {overlap}")
    reserved = [f"row {row} {text!r}" for row, text in notes if str(TIP_BASE) in text]
    if reserved:
        fail(f"range {TIP_BASE} is reserved by note rows: {reserved}")
    if TIP_STATE.exists():
        state_groups = json.loads(TIP_STATE.read_text(encoding="utf-8")).get("groups", {})
        tomb = [f"{n} [{g['base']},{g['base'] + g['width']})" for n, g in state_groups.items()
                if n != TIP_GROUP and int(g["base"]) < hi and lo < int(g["base"]) + int(g["width"])]
        if tomb:
            fail(f"range [{lo},{hi}) already in {rel(TIP_STATE)}: {tomb}")

    last_row = max((r for r, row in enumerate(before, start=1) if any(v is not None for v in row)), default=0)
    header_row = last_row + 2
    patches: list[Patch] = [(header_row, 1, header_text)]
    for offset, (name, text, fault) in enumerate(wanted, start=1):
        row_idx = header_row + offset
        patches += [(row_idx, 1, name), (row_idx, 2, text)]
        if fault is not None:
            patches.append((row_idx, fault_col, fault))
    print(f"Will append {header_text} ({len(wanted)} codes) to {rel(TIP_XLSX)} at row {header_row}")
    if dry_run:
        return
    apply_patches_and_verify(TIP_XLSX, None, wb, ws, before, patches, bind_schema=False)
    print(f"[DONE] {rel(TIP_XLSX)} {header_text} ({lo}..{lo + len(wanted) - 1})")


def cmd_all(dry_run: bool) -> None:
    cmd_new_tables(dry_run)
    cmd_card_item_row(dry_run)
    cmd_tip_codes(dry_run)


COMMANDS = {
    "new-tables": cmd_new_tables,
    "card-item-row": cmd_card_item_row,
    "tip-codes": cmd_tip_codes,
    "all": cmd_all,
}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("command", choices=tuple(COMMANDS))
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    self_check()
    COMMANDS[args.command](args.dry_run)


if __name__ == "__main__":
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="replace")
    main()
