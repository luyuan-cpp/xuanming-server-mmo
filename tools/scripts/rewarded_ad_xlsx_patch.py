#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""激励视频广告系统(docs/design/rewarded-ad-system.md)xlsx 幂等补丁脚本。

用法(仓库根目录执行):
    python tools/scripts/rewarded_ad_xlsx_patch.py new-tables       # 建 data/RewardedAdSlot.xlsx
    python tools/scripts/rewarded_ad_xlsx_patch.py tip-codes        # 追加 //rewarded_ad_error (39000 段)
    python tools/scripts/rewarded_ad_xlsx_patch.py message-limiter  # 跑完 proto-gen 后追加 3 条 RPC 限流
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
        REPO_ROOT,
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
except ImportError as exc:  # pragma: no cover
    sys.exit(f"cannot load dependencies ({exc}): run  pip install pyyaml openpyxl  first")


SLOT_SHEET = "RewardedAdSlot"
SLOT_ROWS = [
    dict(
        id=1,
        enabled=0,           # 首审默认关闭(零拒审铁律)
        min_level=10,        # 对齐首日 10 级里程碑，过滤 1 级白号
        daily_limit=2,       # 每日最多 2 次(600 绑定银两/天)
        cooldown_sec=60,     # 两次成功领取至少间隔 60 秒
        min_watch_sec=15,    # 最短观看 15 秒
        session_ttl_sec=300, # 300 秒内允许断线重连补领
        currency_type=3,     # CURRENCY_TYPE_BIND_GOLD(绑定银两)
        reward_amount=300,   # 单次 300 绑定银两
        require_ssv=0,       # V1 会话校验；V2 切 1
        platform_mask=3,     # 1=微信小游戏 | 2=抖音小游戏（App 端默认关闭）
    ),
]

TIP_GROUP = "rewarded_ad_error"
TIP_BASE = 39000
TIP_WIDTH = 1000
TIP_CODES: list[tuple[str, str, bool]] = [
    ("RewardedAdUnavailable", "观影小礼暂未开放，请稍后再试", True),
    ("RewardedAdDisabled", "当前暂未开启观影小礼", False),
    ("RewardedAdPlatformUnsupported", "当前平台暂未开启观影小礼", False),
    ("RewardedAdLevelNotEnough", "角色等级不足，暂不可领取观影小礼", False),
    ("RewardedAdDailyLimitReached", "今日观影小礼已领完，请明日再来", False),
    ("RewardedAdCooldown", "领取过于频繁，请稍后再试", False),
    ("RewardedAdSessionInvalid", "观影凭证无效，请重新观看后再领奖", False),
    ("RewardedAdSessionExpired", "观影凭证已过期，请重新观看", False),
    ("RewardedAdWatchTooShort", "未完整观看短视频，未领取奖励", False),
    ("RewardedAdCurrencyBlocked", "当前账号暂不可获取该奖励", False),
    ("RewardedAdInBattle", "战斗中暂不可领取观影小礼，请战斗结束后再试", False),
    ("RewardedAdFrozen", "角色当前状态暂不可领取观影小礼，请稍后再试", True),
    ("RewardedAdSsvRequired", "观影校验暂未完成，请稍后重试", False),
]

MESSAGE_ID_TXT = REPO_ROOT / "proto" / "message_id.txt"
MESSAGE_LIMITER_XLSX = DATA_DIR / "MessageLimiter.xlsx"
MESSAGE_LIMITER_SPECS = [
    ("SceneRewardedAdClientPlayerGetRewardedAdInfo", 20, 60, 1004),
    ("SceneRewardedAdClientPlayerBeginRewardedAdSession", 10, 60, 1004),
    ("SceneRewardedAdClientPlayerClaimRewardedAd", 10, 60, 1004),
]


SLOT_COMMENTS = {
    "id": "广告位业务编号(首批固定 1 = 观影绑定银两小礼)",
    "enabled": "总开关:0=关闭(首审默认态),1=开启",
    "min_level": "解锁最低角色等级(默认10)",
    "daily_limit": "每日最大领取次数(UTC+8 05:00切日,默认2)",
    "cooldown_sec": "两次成功领取最短冷却秒数(默认60)",
    "min_watch_sec": "签发会话到领奖的最短观看秒数(默认15)",
    "session_ttl_sec": "会话有效TTL秒数(默认300,容忍断线重连)",
    "currency_type": "发放币种(固定3=CURRENCY_TYPE_BIND_GOLD绑定银两)",
    "reward_amount": "单次发放绑定银两数量(默认300)",
    "require_ssv": "是否强制校验平台SSV回调(0=V1轻量会话,1=V2强制SSV)",
    "platform_mask": "允许宿主掩码(1=微信小游戏 2=抖音小游戏 4=iOS 8=Android,默认3)",
}


def _write_table(sheet: str, rows: list[dict], dry_run: bool) -> None:
    xlsx_path = DATA_DIR / f"{sheet}.xlsx"
    names = list(SLOT_COMMENTS.keys())
    for row in rows:
        if list(row.keys()) != names:
            fail(f"{sheet}: row keys {list(row.keys())} differ from expected fields {names}")

    wanted = [[None] * len(names) for _ in range(DATA_BEGIN_ROW - 1 + len(rows))]
    wanted[HEADER_ROW - 1] = list(names)
    wanted[COMMENT_ROW - 1] = [SLOT_COMMENTS[n] for n in names]
    for i, row in enumerate(rows):
        wanted[DATA_BEGIN_ROW - 1 + i] = [None if row[n] == "" else row[n] for n in names]

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
    for r_idx, values in enumerate(wanted, start=1):
        for c_idx, value in enumerate(values, start=1):
            if value is not None:
                ws.cell(row=r_idx, column=c_idx, value=value)
    wb.save(xlsx_path)

    check_ws = only_sheet(openpyxl.load_workbook(xlsx_path), xlsx_path, sheet)
    diff = first_difference(sheet_matrix(check_ws), wanted)
    if diff:
        fail(f"after saving, {xlsx_path.name} read back differs ({diff})")
    bind(sheet, xlsx_path)
    print(f"[DONE] {rel(xlsx_path)}")


def cmd_new_tables(dry_run: bool) -> None:
    _write_table(SLOT_SHEET, SLOT_ROWS, dry_run)


def cmd_tip_codes(dry_run: bool) -> None:
    wb = openpyxl.load_workbook(TIP_XLSX)
    ws = only_sheet(wb, TIP_XLSX)
    before = sheet_matrix(ws)
    header = [str(v).strip().lower() if v is not None else "" for v in before[HEADER_ROW - 1]]
    if header[:2] != ["name", "commen"] or header.count("fault") != 1:
        fail(f"{TIP_XLSX.name} row 1 unexpected: {before[HEADER_ROW - 1]}")
    fault_col = header.index("fault") + 1

    groups, notes = tip_parse(before, fault_col)
    header_text = f"//{TIP_GROUP} base={TIP_BASE} width={TIP_WIDTH}"
    wanted = [(name, text, 1 if fault else None) for name, text, fault in TIP_CODES]
    new_names = [name for name, _t, _f in TIP_CODES]

    mine = next((g for g in groups if g["name"] == TIP_GROUP), None)
    if mine is not None:
        if (mine["base"], mine["width"]) == (TIP_BASE, TIP_WIDTH) and mine["codes"][:len(wanted)] == wanted:
            print(f"[ALREADY CURRENT] {header_text} already has these {len(wanted)} codes.")
            return
        fail(f"{TIP_XLSX.name} already has group //{TIP_GROUP} with different contents.")

    owner = {code: g["name"] for g in groups for code, _t, _f in g["codes"]}
    clash = [f"{n} (in {owner[n]})" for n in new_names if n in owner]
    if clash:
        fail(f"duplicate tip code names: {clash}")
    lo, hi = TIP_BASE, TIP_BASE + TIP_WIDTH
    overlap = [f"{g['name']} [{g['base']},{g['base'] + g['width']})" for g in groups
               if g["base"] < hi and lo < g["base"] + g["width"]]
    if overlap:
        fail(f"tip range [{lo},{hi}) overlaps: {overlap}")
    reserved = [f"row {row} {text!r}" for row, text in notes if str(TIP_BASE) in text]
    if reserved:
        fail(f"tip range {TIP_BASE} reserved by note rows: {reserved}")
    if TIP_STATE.exists():
        state_groups = json.loads(TIP_STATE.read_text(encoding="utf-8")).get("groups", {})
        tomb = [f"{n} [{g['base']},{g['base'] + g['width']})" for n, g in state_groups.items()
                if n != TIP_GROUP and int(g["base"]) < hi and lo < int(g["base"]) + int(g["width"])]
        if tomb:
            fail(f"tip range [{lo},{hi}) taken in state: {tomb}")

    last_row = max((r for r, row in enumerate(before, start=1) if any(v is not None for v in row)), default=0)
    header_row = last_row + 2
    patches: list[Patch] = [(header_row, 1, header_text)]
    for offset, (name, text, fault) in enumerate(wanted, start=1):
        row_idx = header_row + offset
        patches += [(row_idx, 1, name), (row_idx, 2, text)]
        if fault is not None:
            patches.append((row_idx, fault_col, fault))
    if dry_run:
        print(f"[dry-run] Would append {header_text} with {len(wanted)} codes at row {header_row}.")
        return
    apply_patches_and_verify(TIP_XLSX, None, wb, ws, before, patches, bind_schema=False)
    print(f"[DONE] Appended {header_text} ({len(wanted)} codes) to {rel(TIP_XLSX)}")


def _parse_message_ids() -> dict[str, int]:
    if not MESSAGE_ID_TXT.exists():
        fail(f"missing {rel(MESSAGE_ID_TXT)}; run proto-gen first")
    result: dict[str, int] = {}
    for line in MESSAGE_ID_TXT.read_text(encoding="utf-8").splitlines():
        s = line.strip()
        if not s or s.startswith("#") or "=" not in s:
            continue
        left, right = s.split("=", 1)
        left, right = left.strip(), right.strip()
        if left.isdigit() and right:
            result[right] = int(left)
        elif right.isdigit() and left:
            result[left] = int(right)
    return result


def cmd_message_limiter(dry_run: bool) -> None:
    msg_ids = _parse_message_ids()
    targets: list[tuple[int, int, int, int]] = []
    for name, max_req, window, tip in MESSAGE_LIMITER_SPECS:
        if name not in msg_ids:
            fail(f"{name} not found in {rel(MESSAGE_ID_TXT)}; run dev.bat gen / proto-gen first")
        targets.append((msg_ids[name], max_req, window, tip))

    wb = openpyxl.load_workbook(MESSAGE_LIMITER_XLSX)
    ws = only_sheet(wb, MESSAGE_LIMITER_XLSX)
    before = sheet_matrix(ws)
    header = [str(v).strip() if v is not None else "" for v in before[HEADER_ROW - 1]]
    if header[:4] != ["id", "max_requests", "time_window", "tip_message"]:
        fail(f"{MESSAGE_LIMITER_XLSX.name} header unexpected: {header}")

    existing: dict[int, tuple[int, int, int]] = {}
    last_row = DATA_BEGIN_ROW - 1
    for r_idx, row in enumerate(before[DATA_BEGIN_ROW - 1:], start=DATA_BEGIN_ROW):
        if any(v is not None for v in row):
            last_row = r_idx
            if row[0] is not None:
                existing[int(row[0])] = (int(row[1]), int(row[2]), int(row[3]))

    patches: list[Patch] = []
    next_row = last_row + 1
    for mid, max_req, window, tip in targets:
        if mid in existing:
            if existing[mid] == (max_req, window, tip):
                print(f"[ALREADY CURRENT] MessageLimiter id={mid} already matches.")
                continue
            fail(f"MessageLimiter id={mid} already exists with different values {existing[mid]}")
        patches += [
            (next_row, 1, mid),
            (next_row, 2, max_req),
            (next_row, 3, window),
            (next_row, 4, tip),
        ]
        next_row += 1

    if not patches or dry_run:
        return
    apply_patches_and_verify(MESSAGE_LIMITER_XLSX, "MessageLimiter", wb, ws, before, patches, bind_schema=True)
    print(f"[DONE] Updated {rel(MESSAGE_LIMITER_XLSX)}")


COMMANDS = {
    "new-tables": cmd_new_tables,
    "tip-codes": cmd_tip_codes,
    "message-limiter": cmd_message_limiter,
}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("command", choices=tuple(COMMANDS))
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    COMMANDS[args.command](args.dry_run)


if __name__ == "__main__":
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="replace")
    main()
