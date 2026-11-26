#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""7 套非永久「战法·加点」流派方案槽位系统（docs/planning/skill-build-loadout-7slots-v1.md）配表增量补丁脚本。

幂等执行：
1. data/tip/Tip.xlsx：在 //skill_error 末尾追加 7 条流派方案命名、按套租用与加点联动错误码；
2. data/MessageLimiter.xlsx：当传入 --rent-message-id <id> 时，为 RentSkillLoadoutSlots 追加单行限流（5次/1秒，tip=1000）。
"""

from __future__ import annotations

import argparse
from pathlib import Path
import openpyxl

REPO_ROOT = Path(__file__).resolve().parents[2]
DATA_DIR = REPO_ROOT / "data"

SKILL_LOADOUT_TIPS: list[tuple[str, str]] = [
    ("SkillLoadoutNameInvalid", "流派方案名称不合法或包含敏感词"),
    ("SkillLoadoutRentPriceChanged", "方案槽位租用价格已变化，请刷新后再试"),
    ("SkillLoadoutRentDiamondNotEnough", "灵石不足，无法开通或续期方案槽位"),
    ("SkillLoadoutRentStackLimit", "方案槽位剩余租期已达上限，暂不能继续续期"),
    ("SkillLoadoutRentSlotsInvalid", "所选方案槽位数量超出可租上限或不可在租期内缩减"),
    ("SkillLoadoutAttributeInvalid", "所选四维属性加点超出可用点数或配置不合法"),
    ("SkillLoadoutAttributeResetRequired", "本次加点低于已分配值，需确认洗点后方可保存"),
]


def patch_tip_xlsx() -> None:
    path = DATA_DIR / "tip" / "Tip.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active

    existing: set[str] = set()
    for row_idx in range(1, ws.max_row + 1):
        val = ws.cell(row_idx, 1).value
        if isinstance(val, str) and val.strip():
            existing.add(val.strip())

    missing = [(k, msg) for k, msg in SKILL_LOADOUT_TIPS if k not in existing]
    if not missing:
        print("[ALREADY CURRENT] Tip.xlsx: all 7 skill loadout tips present")
        return

    insert_at = None
    in_skill_section = False
    for row_idx in range(1, ws.max_row + 1):
        val = ws.cell(row_idx, 1).value
        if isinstance(val, str):
            text = val.strip()
            if text.startswith("//skill_error"):
                in_skill_section = True
            elif in_skill_section and text.startswith("//"):
                insert_at = row_idx
                break
    if insert_at is None:
        raise RuntimeError("Tip.xlsx: failed to locate end of //skill_error section")

    ws.insert_rows(insert_at, len(missing))
    for offset, (key, msg) in enumerate(missing):
        ws.cell(insert_at + offset, 1, key)
        ws.cell(insert_at + offset, 2, msg)
    wb.save(path)
    print(f"[DONE] Tip.xlsx: inserted {len(missing)} skill loadout tips at row {insert_at}")


def patch_message_limiter(message_id: int) -> None:
    if message_id <= 0:
        return
    path = DATA_DIR / "MessageLimiter.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    for row_idx in range(6, ws.max_row + 1):
        val = ws.cell(row_idx, 1).value
        if val is not None and str(val).strip() != "" and int(val) == message_id:
            print(f"[ALREADY CURRENT] MessageLimiter.xlsx: id={message_id} at row {row_idx}")
            return
    new_row = ws.max_row + 1
    ws.cell(new_row, 1, message_id)
    ws.cell(new_row, 2, 5)
    ws.cell(new_row, 3, 1)
    ws.cell(new_row, 4, 1000)
    wb.save(path)
    print(f"[DONE] MessageLimiter.xlsx: appended id={message_id} at row {new_row}")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--rent-message-id", type=int, default=0)
    args = parser.parse_args()
    patch_tip_xlsx()
    patch_message_limiter(args.rent_message_id)


if __name__ == "__main__":
    main()
