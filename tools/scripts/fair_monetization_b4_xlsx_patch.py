"""Batch B4: Patch data/RewardedAdSlot.xlsx with reward_kind (1..4) and slots 2..4 (ALL default enabled=0)."""
from pathlib import Path
import openpyxl

REPO = Path(__file__).resolve().parents[2]
XLSX = REPO / "data" / "RewardedAdSlot.xlsx"

ROWS = [
    # id, enabled, min_level, daily_limit(weekly), cooldown_sec, min_watch_sec, session_ttl_sec, currency_type, reward_amount, require_ssv, platform_mask, reward_kind
    (1, 0, 10, 14, 60, 15, 300, 3, 300, 0, 3, 1),  # 观影绑定银两小礼（300绑银，14次/周，默认关闭）
    (2, 0, 20, 7, 60, 15, 300, 3, 0, 0, 3, 2),     # 幻境奇策重随（7次/周且单局限1次，0资产产出，默认关闭）
    (3, 0, 10, 7, 60, 15, 300, 3, 0, 0, 3, 3),     # 衣柜续时1小时试穿（7次/周，绝不授永久权，默认关闭）
    (4, 0, 10, 5, 60, 15, 300, 3, 0, 0, 3, 4),     # 神秘商人个人换货（5次/周且每轮限1次，默认关闭）
]


def main() -> None:
    wb = openpyxl.load_workbook(XLSX)
    ws = wb["RewardedAdSlot"]
    headers = [ws.cell(1, c).value for c in range(1, ws.max_column + 1)]
    if "reward_kind" not in headers:
        col = len(headers) + 1
        ws.cell(1, col, "reward_kind")
        ws.cell(5, col, "激励奖励类型(1=绑定银两,2=幻境奇策重随,3=衣柜1h试穿,4=神秘商人个人换货)")
    # Remove existing data rows (row 6+) and write canonical idempotent rows 1..4
    if ws.max_row >= 6:
        ws.delete_rows(6, ws.max_row - 5)
    for row_vals in ROWS:
        ws.append(list(row_vals))
    wb.save(XLSX)
    print(f"Patched {XLSX}: {len(ROWS)} slots (all enabled=0)")


if __name__ == "__main__":
    main()
