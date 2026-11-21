"""静态守卫(docs/design/config-table-hot-reload.md §8.4):
cpp/nodes/battle 与 cpp/libs/services/battle 里除数据供给器、配表指纹和启动钩子外,
禁止直接调用 *TableManager::Instance(),保证战斗房间始终从各自绑定的不可变 TableSet 读表。
"""

from pathlib import Path
import re
import unittest

REPO = Path(__file__).resolve().parents[3]
SCANNED_DIRS = ("cpp/nodes/battle", "cpp/libs/services/battle")
SOURCE_SUFFIXES = {".h", ".hpp", ".cpp", ".cc"}

# 允许直接触碰 *TableManager::Instance() 的唯一三处边界。
ALLOWED = {
    "cpp/libs/services/battle/data/table_battle_data_provider.cpp",
    "cpp/libs/services/battle/data/battle_table_fingerprint.cpp",
    "cpp/nodes/battle/main.cpp",
}

COMMENT_OR_LITERAL = re.compile(
    r"//[^\n]*|/\*.*?\*/|\"(?:\\.|[^\"\\\n])*\"|'(?:\\.|[^'\\\n])*'", re.S
)
SINGLETON_CALL = re.compile(r"\b[A-Za-z0-9_]+TableManager\s*::\s*Instance\s*\(")


class BattleNoDirectTableSingletonTest(unittest.TestCase):
    def test_battle_reads_tables_only_through_provider(self):
        scanned = 0
        violations = []
        for directory in SCANNED_DIRS:
            for path in sorted((REPO / directory).rglob("*")):
                if path.suffix not in SOURCE_SUFFIXES or not path.is_file():
                    continue
                rel = path.relative_to(REPO).as_posix()
                if "/tests/" in rel:
                    continue
                scanned += 1
                if rel in ALLOWED:
                    continue
                text = path.read_text(encoding="utf-8", errors="replace")
                stripped = COMMENT_OR_LITERAL.sub(lambda m: "\n" * m.group(0).count("\n"), text)
                for lineno, line in enumerate(stripped.splitlines(), 1):
                    for match in SINGLETON_CALL.finditer(line):
                        violations.append(f"  {rel}:{lineno}  {match.group(0)}")
        self.assertGreater(scanned, 15, "scanned too few battle source files")
        self.assertFalse(
            violations,
            "Direct *TableManager::Instance() call outside BattleDataProvider breaks per-match immutable TableSet binding:\n"
            + "\n".join(violations),
        )


if __name__ == "__main__":
    unittest.main()
