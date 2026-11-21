#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""角色改名、帮会改名与内容安全(docs/design/rename-and-content-safety.md §4–§7)的 xlsx 幂等补丁脚本。

改动范围(全部幂等,重复执行无副作用,发现现状与预期冲突则中止且绝不覆盖已有数据):
  1. role-name-rule-columns: RoleNameRule.xlsx 追加 4 列(冷却、旧名保护期、改名卡 id、数量)
  2. guild-rule-columns:     GuildRule.xlsx 追加 3 列(冷却、旧名保护期、帮会资金费用)
  3. rename-item-row:        Item.xlsx 追加 3401 改名卡
  4. tip-codes:              Tip.xlsx 向 login_error / guild_error / trade_error / pet_error 追加提示码,
                             并在表尾新开 //chat_error base=18000 width=1000

用法(仓库根目录执行):
    python tools/scripts/rename_content_safety_xlsx_patch.py all
    python tools/scripts/rename_content_safety_xlsx_patch.py all --dry-run
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
        header_cells,
        item_row_cells,
        only_sheet,
        predict_tip_ids,
        rel,
        schema_layout,
        sheet_matrix,
        tip_parse,
    )
except ImportError as exc:
    sys.exit(f"cannot load dependencies ({exc}): run  pip install pyyaml openpyxl  first")


ROLE_NAME_RULE_SHEET = "RoleNameRule"
ROLE_NAME_RULE_XLSX = DATA_DIR / "RoleNameRule.xlsx"
ROLE_NAME_NEW_COLS = [
    ("rename_cooldown_seconds", 604800),
    ("old_name_hold_seconds", 604800),
    ("rename_item_id", 3401),
    ("rename_item_count", 1),
]

GUILD_RULE_SHEET = "GuildRule"
GUILD_RULE_XLSX = DATA_DIR / "GuildRule.xlsx"
GUILD_RULE_NEW_COLS = [
    ("rename_cooldown_seconds", 604800),
    ("name_hold_seconds", 604800),
    ("rename_cost_funds", 1000),
]

RENAME_CARD_ITEM = dict(
    id=3401,
    max_stack_size=99,
    equip_kind=0,
    battle_usable=0,
    battle_heal_hp=0,
    battle_heal_mp=0,
    name="改名卡",
    description="使用后可修改一次角色名（冷却 7 天，旧名保留保护 7 天）。",
    icon_key=None,
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

# 既有组末尾追加的新提示码(顺序固定,不可重排;与已有引用及设计稿一致)
APPEND_TIP_GROUPS = [
    ("login_error", 2000, 1000, [
        ("RoleNameCheckUnavailable", "角色名安全检测暂时不可用，请稍后重试", True),
        ("RoleRenameCooldown", "角色改名冷却中，请稍后再试", False),
        ("RoleRenameNoCard", "背包中没有可用的改名卡", False),
        ("RoleRenameInProgress", "已有角色改名正在进行中，请稍后再试", False),
        ("RoleRenameFrozen", "当前角色状态暂时不能改名，请稍后再试", False),
        ("RoleRenameSameName", "新角色名与当前名字相同", False),
        ("RoleRenameExpired", "本次改名超时已失效，请重新发起", False),
    ]),
    ("guild_error", 14000, 1000, [
        ("GuildNameSensitive", "帮会名称包含不允许使用的词语，请修改", False),
        ("GuildAnnouncementSensitive", "帮会公告包含不允许使用的词语，请修改", False),
        ("GuildTextCheckUnavailable", "帮会内容安全检测暂时不可用，请稍后重试", True),
        ("GuildRenameCooldown", "帮会改名冷却中，请稍后再试", False),
        ("GuildRenameSameName", "新帮会名称与当前名称相同", False),
    ]),
    ("trade_error", 20000, 1000, [
        ("TradeTitleSensitive", "寄售标题包含不允许使用的词语，请修改", False),
        ("TradeTextCheckUnavailable", "寄售内容安全检测暂时不可用，请稍后重试", True),
    ]),
    ("pet_error", 26000, 1000, [
        ("PetNameSensitive", "宝宝名称包含不允许使用的词语，请修改", False),
    ]),
]

# 新建 //chat_error 组(使用 Tip.xlsx row 300 预留给 chat_error 的 18000 段)
NEW_CHAT_GROUP = ("chat_error", 18000, 1000, [
    ("ChatContentRejected", "发言包含不允许发送的内容，请修改后重试", False),
    ("ChatContentCheckUnavailable", "聊天内容安全检测暂时不可用，请稍后重试", True),
    ("ChatLevelTooLow", "角色等级不足，暂不能在世界频道发言", False),
    ("ChatDuplicateSpam", "请勿在短时间内重复刷屏发言", False),
    ("ChatPrivateTargetLimit", "新角色短期内私聊人数已达上限，请稍后再试", False),
    ("ChatContactRejected", "请勿在聊天中发布外部联系方式或推广信息", False),
    ("ChatMessageNotFound", "该聊天消息不存在或已失效", False),
])


def append_scalar_columns_to_singleton_rule(
    sheet: str,
    xlsx_path: Path,
    new_cols: list[tuple[str, object]],
    dry_run: bool,
) -> None:
    layout = schema_layout(sheet)
    field_by_name = {f.name: f for f in layout}
    for name, _val in new_cols:
        if name not in field_by_name:
            fail(f"{sheet} schema is missing field {name!r}")
        if field_by_name[name].width != 1:
            fail(f"{sheet}.{name} must be scalar")

    wb = openpyxl.load_workbook(xlsx_path)
    ws = only_sheet(wb, xlsx_path, sheet)
    before = sheet_matrix(ws)
    header = header_cells(ws)

    present = [name for name, _ in new_cols if name in header]
    if len(present) == len(new_cols):
        bind(sheet, xlsx_path)
        print(f"[ALREADY CURRENT] {rel(xlsx_path)} already has columns {present}; schema binding passed.")
        return
    if present:
        fail(f"{rel(xlsx_path)} already has partial columns {present} -- inspect manually before patching")

    start_col = ws.max_column + 1
    patches: list[Patch] = []
    for offset, (name, value) in enumerate(new_cols):
        col = start_col + offset
        comment = field_by_name[name].comment or None
        patches.append((HEADER_ROW, col, name))
        if comment is not None:
            patches.append((COMMENT_ROW, col, comment))
        patches.append((DATA_BEGIN_ROW, col, value))

    print(f"{rel(xlsx_path)}: appending {len(new_cols)} columns starting at col {start_col}: {[n for n, _ in new_cols]}")
    if dry_run:
        print("[dry-run] No files written.")
        return
    apply_patches_and_verify(xlsx_path, sheet, wb, ws, before, patches, bind_schema=True)
    print(f"[DONE] {rel(xlsx_path)} updated and schema binding passed.")


def cmd_role_name_rule(dry_run: bool) -> None:
    append_scalar_columns_to_singleton_rule(
        ROLE_NAME_RULE_SHEET, ROLE_NAME_RULE_XLSX, ROLE_NAME_NEW_COLS, dry_run
    )


def cmd_guild_rule(dry_run: bool) -> None:
    append_scalar_columns_to_singleton_rule(
        GUILD_RULE_SHEET, GUILD_RULE_XLSX, GUILD_RULE_NEW_COLS, dry_run
    )


def cmd_rename_item_row(dry_run: bool) -> None:
    layout = schema_layout(ITEM_SHEET)
    row = RENAME_CARD_ITEM
    if set(row.keys()) != {f.name for f in layout}:
        fail(
            f"RENAME_CARD_ITEM keys differ from Item schema fields: "
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
            print(f"[ALREADY CURRENT] {rel(ITEM_XLSX)} already has rename card id={row['id']} on row {row_idx}.")
            return
        fail(f"{rel(ITEM_XLSX)} row {row_idx} already has id={row['id']} with different contents -- refusing to overwrite")

    target_row = last_data_row + 1
    patches: list[Patch] = [(target_row, col, val) for col, val in sorted(wanted.items())]
    print(f"{rel(ITEM_XLSX)}: appending rename card id={row['id']} ({row['name']}) at row {target_row}")
    if dry_run:
        print("[dry-run] No files written.")
        return
    apply_patches_and_verify(ITEM_XLSX, ITEM_SHEET, wb, ws, before, patches, bind_schema=True)
    print(f"[DONE] {rel(ITEM_XLSX)} appended id={row['id']}; schema binding passed.")


def append_to_existing_tip_group(
    group_name: str,
    base: int,
    width: int,
    codes: list[tuple[str, str, bool]],
    dry_run: bool,
) -> None:
    wb = openpyxl.load_workbook(TIP_XLSX)
    ws = only_sheet(wb, TIP_XLSX)
    before = sheet_matrix(ws)
    header = [str(v).strip().lower() if v is not None else "" for v in before[HEADER_ROW - 1]]
    if header[:2] != ["name", "commen"] or header.count("fault") != 1:
        fail(f"{TIP_XLSX.name} unexpected header: {before[HEADER_ROW - 1]}")
    fault_col = header.index("fault") + 1

    groups, notes = tip_parse(before, fault_col)
    mine = next((g for g in groups if g["name"] == group_name), None)
    if mine is None:
        fail(f"{TIP_XLSX.name} missing group //{group_name}")
    if (mine["base"], mine["width"]) != (base, width):
        fail(f"{TIP_XLSX.name} //{group_name} base/width mismatch: got {(mine['base'], mine['width'])}, want {(base, width)}")

    wanted = [(name, text, 1 if fault else None) for name, text, fault in codes]
    existing_in_mine = {name: (name, text, fault) for name, text, fault in mine["codes"]}
    missing = [item for item in wanted if item[0] not in existing_in_mine]
    for item in wanted:
        if item[0] in existing_in_mine and existing_in_mine[item[0]] != item:
            fail(f"{TIP_XLSX.name} //{group_name} already has {item[0]} with different value: {existing_in_mine[item[0]]} vs {item}")
    if not missing:
        print(f"[ALREADY CURRENT] //{group_name} already has all {len(wanted)} target codes.")
        return

    owner = {code: g["name"] for g in groups for code, _t, _f in g["codes"]}
    clash = [f"{name} (in {owner[name]})" for name, _t, _f in missing if name in owner]
    if clash:
        fail(f"tip code collision across groups: {clash}")

    state_ids = {}
    if TIP_STATE.exists():
        state_ids = json.loads(TIP_STATE.read_text(encoding="utf-8")).get("groups", {}).get(group_name, {}).get("ids", {})
    names_after = [n for n, _t, _f in mine["codes"]] + [n for n, _t, _f in missing]
    predicted = predict_tip_ids(names_after, base, state_ids)[-len(missing):]
    if any(not base <= cid < base + width for cid in predicted):
        fail(f"//{group_name} out of range [{base}, {base + width}) for {missing}")

    last_code_row = mine["code_rows"][-1]
    insert_at = last_code_row + 1
    trailing = [r for r in range(insert_at, len(before) + 1) if any(v is not None for v in before[r - 1])]
    patches: list[Patch] = []
    print(f"{TIP_XLSX.name} //{group_name}: inserting {len(missing)} code(s) at row {insert_at}:")
    for offset, (name, text, fault) in enumerate(missing):
        row_idx = insert_at + offset
        patches += [(row_idx, 1, name), (row_idx, 2, text)]
        if fault is not None:
            patches.append((row_idx, fault_col, fault))
        print(f"  row {row_idx}: {name} | {text}" + (" | fault=1" if fault else "") + f" (id ~ {predicted[offset]})")
    if dry_run:
        print("[dry-run] No files written.")
        return

    shifted = before
    if trailing:
        ws.insert_rows(insert_at, amount=len(missing))
        blank = [None] * max(len(r) for r in before)
        shifted = before[:insert_at - 1] + [list(blank) for _ in missing] + before[insert_at - 1:]
    apply_patches_and_verify(TIP_XLSX, None, wb, ws, shifted, patches, bind_schema=False)

    check_ws = only_sheet(openpyxl.load_workbook(TIP_XLSX), TIP_XLSX)
    groups_after, notes_after = tip_parse(sheet_matrix(check_ws), fault_col)
    expected_groups = [
        (g["name"], g["base"], g["width"], g["codes"] + missing if g is mine else g["codes"])
        for g in groups
    ]
    if [(g["name"], g["base"], g["width"], g["codes"]) for g in groups_after] != expected_groups \
            or [text for _r, text in notes_after] != [text for _r, text in notes]:
        fail(f"post-save verification failed for //{group_name}")
    print(f"[DONE] //{group_name} +{len(missing)} codes verified.")


def create_chat_tip_group(dry_run: bool) -> None:
    group_name, base, width, codes = NEW_CHAT_GROUP
    wb = openpyxl.load_workbook(TIP_XLSX)
    ws = only_sheet(wb, TIP_XLSX)
    before = sheet_matrix(ws)
    header = [str(v).strip().lower() if v is not None else "" for v in before[HEADER_ROW - 1]]
    fault_col = header.index("fault") + 1

    groups, _notes = tip_parse(before, fault_col)
    wanted = [(name, text, 1 if fault else None) for name, text, fault in codes]
    mine = next((g for g in groups if g["name"] == group_name), None)
    if mine is not None:
        if (mine["base"], mine["width"]) == (base, width) and mine["codes"][:len(wanted)] == wanted:
            print(f"[ALREADY CURRENT] //{group_name} already exists with expected codes.")
            return
        fail(f"//{group_name} already exists with different declaration/codes")

    owner = {code: g["name"] for g in groups for code, _t, _f in g["codes"]}
    clash = [f"{name} (in {owner[name]})" for name, _t, _f in wanted if name in owner]
    if clash:
        fail(f"tip code collision: {clash}")
    lo, hi = base, base + width
    overlap = [f"{g['name']} [{g['base']},{g['base'] + g['width']})" for g in groups
               if g["base"] < hi and lo < g["base"] + g["width"]]
    if overlap:
        fail(f"range [{lo},{hi}) overlaps existing group: {overlap}")

    last_row = max((r for r, row in enumerate(before, start=1) if any(v is not None for v in row)), default=0)
    header_row = last_row + 2
    header_text = f"//{group_name} base={base} width={width}"
    patches: list[Patch] = [(header_row, 1, header_text)]
    print(f"{TIP_XLSX.name}: creating {header_text} at row {header_row}:")
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
    groups_after, _notes_after = tip_parse(sheet_matrix(check_ws), fault_col)
    if [(g["name"], g["codes"]) for g in groups_after[:-1]] != [(g["name"], g["codes"]) for g in groups] \
            or groups_after[-1]["name"] != group_name or groups_after[-1]["codes"] != wanted:
        fail(f"post-save verification failed for new group //{group_name}")
    print(f"[DONE] created //{group_name} with {len(wanted)} codes verified.")


def cmd_tip_codes(dry_run: bool) -> None:
    for group_name, base, width, codes in APPEND_TIP_GROUPS:
        append_to_existing_tip_group(group_name, base, width, codes, dry_run)
    create_chat_tip_group(dry_run)


def cmd_all(dry_run: bool) -> None:
    cmd_rename_item_row(dry_run)
    cmd_role_name_rule(dry_run)
    cmd_guild_rule(dry_run)
    cmd_tip_codes(dry_run)


COMMANDS = {
    "role-name-rule-columns": cmd_role_name_rule,
    "guild-rule-columns": cmd_guild_rule,
    "rename-item-row": cmd_rename_item_row,
    "tip-codes": cmd_tip_codes,
    "all": cmd_all,
}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("command", choices=tuple(COMMANDS))
    parser.add_argument("--dry-run", action="store_true", help="只打印计划,不写文件")
    args = parser.parse_args()
    COMMANDS[args.command](args.dry_run)


if __name__ == "__main__":
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="replace")
    main()
