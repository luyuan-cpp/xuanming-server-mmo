#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""生死状资格预检判「上一局普通战书银两未结清」(数据覆写审计 r2 第 4 轮剩余单 §3.3 第 4 条)的 Tip.xlsx 补丁。

在 //jianghu_error 段末尾追加一条码(已有码号不变),写法同 craft_bookshelf_cap_xlsx_patch.py:
从磁盘重新载入 → 码名全表唯一才写 → 插在段尾(本段是表里最后一段时就追加在表尾)→ 保存 → 读回核对。
幂等:码已在就只报告、不改文件。

用法(仓库根目录):python tools/scripts/jianghu_fee_pending_tip_xlsx_patch.py [--dry-run]
之后照常导表(dev.bat gen),match 引用的 kJianghuDuelSilverFeePending 才有定义。
"""

from __future__ import annotations

import argparse
from pathlib import Path

import openpyxl

TIP_XLSX = Path(__file__).resolve().parents[2] / "data" / "tip" / "Tip.xlsx"
GROUP = "//jianghu_error"
TIPS: list[tuple[str, str]] = [
    ("JianghuDuelSilverFeePending", "上一场普通战书的银两还在退还或结算中，请稍候再下战书"),
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
    """返回本段之后第一行的行号:下一个段头,或本段是最后一段时最后一个非空行的下一行。"""
    inside = False
    last_filled = 0
    for row in range(1, ws.max_row + 1):
        value = ws.cell(row, 1).value
        if isinstance(value, str) and value.strip().startswith("//"):
            if value.strip().startswith(GROUP + " "):
                inside = True
            elif inside:
                return row
        if inside and value is not None and str(value).strip():
            last_filled = row
    if not inside:
        raise SystemExit(f"Tip.xlsx: cannot find the {GROUP} group")
    return last_filled + 1


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
    print(f"Tip.xlsx: inserting {len(TIPS)} codes at the end of {GROUP} (row {insert_at})")
    if args.dry_run:
        print("[dry-run] no files written")
        return
    if insert_at <= ws.max_row:
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
    print(f"[OK] Tip.xlsx: {', '.join(name for name, _ in TIPS)} appended to {GROUP}")


if __name__ == "__main__":
    main()
