#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""技能书架上限(数据覆写审计 r2 第 4 轮 r2-craft#2,docs/design/skill-book-wanted-order-trade-escrow.md §10)的 Tip.xlsx 补丁。

在 //skill_error 段末尾追加两条码(排在本段最后,已有码号不变),写法同 skill_build_loadout_7slots_xlsx_patch.py:
从磁盘重新载入 → 码名全表唯一才写 → 在下一个段头之前插行 → 保存 → 读回核对。幂等:两条都在就只报告、不改文件。

用法(仓库根目录):python tools/scripts/craft_bookshelf_cap_xlsx_patch.py [--dry-run]
之后照常导表(dev.bat gen),C++ 引用的 kSkillBookshelfFull / kSkillWantedPosterBookshelfFull 才有定义。
"""

from __future__ import annotations

import argparse
from pathlib import Path

import openpyxl

TIP_XLSX = Path(__file__).resolve().parents[2] / "data" / "tip" / "Tip.xlsx"
GROUP = "//skill_error"
TIPS: list[tuple[str, str]] = [
    ("SkillBookshelfFull", "书架已满（最多 100 本），请先研习、拆页或封卷后再试"),
    ("SkillWantedPosterBookshelfFull", "发榜人的书架已满，暂时无法交付这张悬赏单"),
]


def names_by_row(ws) -> dict[str, int]:
    out: dict[str, int] = {}
    for row in range(1, ws.max_row + 1):
        value = ws.cell(row, 1).value
        if isinstance(value, str) and value.strip() and not value.strip().startswith("//"):
            if value.strip() in out:
                raise SystemExit(f"Tip.xlsx: code name {value.strip()} appears twice (rows {out[value.strip()]} and {row}); fix the table first")
            out[value.strip()] = row
    return out


def group_end(ws) -> int:
    inside = False
    for row in range(1, ws.max_row + 1):
        value = ws.cell(row, 1).value
        if isinstance(value, str) and value.strip().startswith("//"):
            if value.strip().startswith(GROUP + " "):
                inside = True
            elif inside:
                return row
    raise SystemExit(f"Tip.xlsx: cannot find the end of the {GROUP} group")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    wb = openpyxl.load_workbook(TIP_XLSX)
    ws = wb.active
    existing = names_by_row(ws)
    present = [name for name, _ in TIPS if name in existing]
    if len(present) == len(TIPS):
        print(f"[ALREADY CURRENT] Tip.xlsx: {', '.join(present)} present; file unchanged")
        return
    if present:
        raise SystemExit(f"Tip.xlsx: only some of the codes are present ({present}); find out who added them before patching")

    insert_at = group_end(ws)
    print(f"Tip.xlsx: inserting {len(TIPS)} codes at the end of {GROUP} (before row {insert_at})")
    if args.dry_run:
        print("[dry-run] no files written")
        return
    ws.insert_rows(insert_at, len(TIPS))
    for offset, (name, text) in enumerate(TIPS):
        ws.cell(insert_at + offset, 1, name)
        ws.cell(insert_at + offset, 2, text)
    wb.save(TIP_XLSX)

    check = openpyxl.load_workbook(TIP_XLSX).active
    after = names_by_row(check)
    for offset, (name, text) in enumerate(TIPS):
        row = insert_at + offset
        if after.get(name) != row or check.cell(row, 2).value != text or check.cell(row, 3).value is not None:
            raise SystemExit(f"Tip.xlsx: read-back mismatch for {name} at row {row}")
    if group_end(check) != insert_at + len(TIPS):
        raise SystemExit("Tip.xlsx: read-back mismatch, the next group header did not move down by the inserted rows")
    if len(after) != len(existing) + len(TIPS):
        raise SystemExit("Tip.xlsx: read-back mismatch, code count changed unexpectedly")
    print(f"[DONE] Tip.xlsx: inserted {', '.join(name for name, _ in TIPS)} at rows {insert_at}-{insert_at + len(TIPS) - 1}")


if __name__ == "__main__":
    main()
