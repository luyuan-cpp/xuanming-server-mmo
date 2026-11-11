"""LevelComp 存储等级直读白名单(规则见 player_level_rules.h 文件头);只读源码,不编译、不运行服务。

玩法一律读 playerlevel::CappedLevel(存储等级)。不经 CappedLevel 直读 LevelComp.level() 只允许出现在四处:
加载器(超上限只告警)、存盘(原样写出)、GmSetLevel(写前取旧值)、管理端 dump(原样展示)。
这四处的直读在同一行写标记注释 `// 直读存储等级:<场合>`,场合与所在文件必须对得上(ALLOWED_MARKERS)。

识别靠正则,不是编译器:
- 读点 = `<LevelComp>(...)` 调用链或 `level_component()` 后面紧跟 `.level()` / `->level()`,以及绑定到 LevelComp 的
  变量(声明、函数形参、`auto& x = *y` 这类别名)上的 `.level()` / `->level()`;
- 读点落在同一条语句里一个还没闭合的 `CappedLevel(` 之内(可以跨行),就算经过了换算;
- 注释、字符串、原始字符串、字符字面量先等长替换成空白,偏移与行号不变。
认不出来的写法(结构化绑定、经函数返回的引用、宏)不在守卫范围内,靠评审;发现漏网的写法时把它补进识别规则和
下面的自测用例。

运行(仓库根目录):python tools/scripts/tests/test_cpp_level_comp_reads.py -v
"""

from dataclasses import dataclass, field
from pathlib import Path
import re
import unittest


REPO = Path(__file__).resolve().parents[3]
SCAN_ROOTS = ("cpp/libs", "cpp/nodes")
SOURCE_SUFFIXES = {".h", ".hpp", ".cpp", ".cc"}
SKIPPED_PARTS = {"build", "Debug", "Release", "x64", "CMakeFiles", "generated"}

MARKER = "直读存储等级:"
MARKER_LABEL = re.compile(r"直读存储等级:\s*(加载器|存盘|GmSetLevel|管理端 dump)")
# 允许直读的四处:文件 → 允许的场合。存盘与管理端 dump 目前整份拷贝 / 整份打印,没有 .level() 调用,所以没有标记。
ALLOWED_MARKERS = {
    "cpp/libs/services/scene/player/system/player_database_loader.cpp": {"加载器", "存盘"},
    "cpp/libs/services/scene/player/system/player_attribute.cpp": {"GmSetLevel"},
    "cpp/nodes/scene/handler/rpc/scene_admin_handler.cpp": {"管理端 dump"},
}

IDENTIFIER = r"[A-Za-z_]\w*"
# 至多三层嵌套的一对括号:调用实参
CALL_ARGS = r"\((?:[^()]|\((?:[^()]|\([^()]*\))*\))*\)"
LEVEL_COMP_TYPE = r"(?:const\s+)?LevelComp\b\s*(?:const\b\s*)?[*&]{0,2}"
LEVEL_CALL = r"\s*(?:\.|->)\s*level\s*\(\s*\)"
# 不经变量的读法:registry.get<LevelComp>(player).level()、static_cast<const LevelComp*>(p)->level()、
# message.level_component().level()
DIRECT_CHAIN = re.compile(
    r"(?:<\s*" + LEVEL_COMP_TYPE + r"\s*>\s*" + CALL_ARGS + r"|\blevel_component\s*\(\s*\))" + LEVEL_CALL
)
# 初始化式里取 LevelComp 的声明:const auto* lc = registry.try_get<LevelComp>(player);(含 if 的初始化语句)
DECLARED = re.compile(
    r"\b(?:const\s+)?(?:auto|LevelComp)\b\s*(?:const\b\s*)?[*&]{0,2}\s*(?P<name>" + IDENTIFIER + r")\s*=\s*"
    r"[^;{}]*?(?:<\s*" + LEVEL_COMP_TYPE + r"\s*>|\blevel_component\s*\(\s*\))"
)
# 写明类型的形参与局部变量:const LevelComp& lc、LevelComp* lc、LevelComp lc;
TYPED = re.compile(r"\b" + LEVEL_COMP_TYPE + r"\s*(?P<name>" + IDENTIFIER + r")\s*(?=[,;)={])")
# 别名:auto& lc = *ptr;、const auto* lc = other;
ALIAS = re.compile(
    r"\b(?:const\s+)?(?:auto|LevelComp)\b\s*(?:const\b\s*)?[*&]{0,2}\s*(?P<name>" + IDENTIFIER + r")\s*=\s*"
    r"[*&]?\s*(?P<source>" + IDENTIFIER + r")\s*;"
)
CAPPED_LEVEL_OPEN = re.compile(r"\bCappedLevel\s*\(")
RAW_STRING_OPEN = re.compile(r'R"([^()\\\s]{0,16})\(')


def _blank(segment):
    # 等长换成空格、保留换行,偏移与行号不变
    return re.sub(r"[^\n]", " ", segment)


def _is_word_char(text, index):
    return 0 <= index < len(text) and (text[index].isalnum() or text[index] == "_")


def _quoted_end(text, start, quote):
    index = start + 1
    while index < len(text) and text[index] not in (quote, "\n"):
        index += 2 if text[index] == "\\" else 1
    return min(index + 1, len(text))


def strip_comments_and_literals(text):
    pieces = []
    index, length = 0, len(text)
    while index < length:
        end = None
        if text.startswith("//", index):
            end = text.find("\n", index)
            end = length if end < 0 else end
        elif text.startswith("/*", index):
            end = text.find("*/", index + 2)
            end = length if end < 0 else end + 2
        elif text.startswith('R"', index) and not _is_word_char(text, index - 1):
            opened = RAW_STRING_OPEN.match(text, index)
            if opened:
                close = ")" + opened.group(1) + '"'
                end = text.find(close, opened.end())
                end = length if end < 0 else end + len(close)
        elif text[index] == '"':
            end = _quoted_end(text, index, '"')
        elif text[index] == "'" and not _is_word_char(text, index - 1):
            # 前面紧挨着字母或数字的是数字分隔符(1'000'000),不是字符字面量
            end = _quoted_end(text, index, "'")
        if end is None:
            pieces.append(text[index])
            index += 1
        else:
            pieces.append(_blank(text[index:end]))
            index = end
    return "".join(pieces)


def _line_of(text, offset):
    return text.count("\n", 0, offset) + 1


def _inside_capped_level(code, offset):
    # 同一条语句里最近的一个 CappedLevel( 在读点处还没闭合,读点就在它的实参里
    statement_start = max(code.rfind(";", 0, offset), code.rfind("{", 0, offset), code.rfind("}", 0, offset)) + 1
    opened = None
    for match in CAPPED_LEVEL_OPEN.finditer(code, statement_start, offset):
        opened = match
    if opened is None:
        return False
    between = code[opened.end() - 1:offset]
    return between.count("(") > between.count(")")


def _level_comp_bindings(code):
    names = {match.group("name") for match in DECLARED.finditer(code)}
    names |= {match.group("name") for match in TYPED.finditer(code)}
    aliases = [(match.group("name"), match.group("source")) for match in ALIAS.finditer(code)]
    changed = True
    while changed:
        changed = False
        for name, source in aliases:
            if source in names and name not in names:
                names.add(name)
                changed = True
    return names


@dataclass
class ScanResult:
    wrapped_lines: list = field(default_factory=list)
    violations: list = field(default_factory=list)  # (行号, 原因)


def scan_source(relative_path, text):
    """找出一份源码里的 LevelComp 等级读点,检查每个直读是否在白名单内。relative_path 用仓库内的正斜杠路径。"""
    code = strip_comments_and_literals(text)
    reads = {match.end() - 1: match.start() for match in DIRECT_CHAIN.finditer(code)}
    names = _level_comp_bindings(code)
    if names:
        by_name = re.compile(
            r"\b(?:" + "|".join(sorted(map(re.escape, names))) + r")" + LEVEL_CALL
        )
        for match in by_name.finditer(code):
            reads.setdefault(match.end() - 1, match.start())

    lines = text.split("\n")
    markers = {}
    for number, line in enumerate(lines, start=1):
        if MARKER in line:
            label = MARKER_LABEL.search(line)
            markers[number] = label.group(1) if label else None

    allowed = ALLOWED_MARKERS.get(relative_path, set())
    result = ScanResult()
    raw_lines = set()
    for _, start in sorted(reads.items()):
        # 标记写在读点开头所在的行;多行调用链即 `<LevelComp>` 那一行
        line = _line_of(code, start)
        if _inside_capped_level(code, start):
            result.wrapped_lines.append(line)
            continue
        if line in raw_lines:
            continue  # 同一行的多个直读只判一次
        raw_lines.add(line)
        # 诊断文案按 AGENTS.md §2 用英文;标记本身是代码注释,保持中文
        if line not in markers:
            result.violations.append(
                (line, "raw LevelComp.level() read bypasses playerlevel::CappedLevel and is not whitelisted "
                       "(missing a same-line `// 直读存储等级:<place>` marker)")
            )
        elif markers[line] is None:
            result.violations.append(
                (line, "raw-read marker must name one of the places: 加载器 / 存盘 / GmSetLevel / 管理端 dump")
            )
        elif markers[line] not in allowed:
            result.violations.append(
                (line, f"place '{markers[line]}' is not allowed in this file; see ALLOWED_MARKERS for the four places")
            )

    for line in sorted(markers):
        if line not in raw_lines:
            result.violations.append(
                (line, "stale raw-read marker: this line no longer reads LevelComp.level() directly; remove the marker")
            )
    return result


def level_comp_sources():
    for root in SCAN_ROOTS:
        for path in sorted((REPO / root).rglob("*")):
            if not path.is_file() or path.suffix not in SOURCE_SUFFIXES:
                continue
            if any(part in SKIPPED_PARTS or part.startswith(".build") for part in path.relative_to(REPO).parts):
                continue
            # 个别第三方源码不是 UTF-8;替换解码只为不因编码报与本规则无关的错,含 LevelComp 的都是自有源码
            text = path.read_text(encoding="utf-8-sig", errors="replace")
            if "LevelComp" in text or "level_component" in text:
                yield path.relative_to(REPO).as_posix(), text


class LevelReadDetectorTest(unittest.TestCase):
    """识别规则本身的用例:每条都是合成片段,不读仓库文件。"""

    LOADER = "cpp/libs/services/scene/player/system/player_database_loader.cpp"
    OTHER = "cpp/libs/services/scene/player/system/example.cpp"

    def violations(self, source, path=OTHER):
        return scan_source(path, source).violations

    def test_chain_through_capped_level_is_allowed(self):
        result = scan_source(self.OTHER, "x = playerlevel::CappedLevel(registry.get<LevelComp>(p).level());")
        self.assertEqual([], result.violations)
        self.assertEqual([1], result.wrapped_lines)

    def test_raw_chain_is_reported(self):
        self.assertEqual(1, len(self.violations("x = registry.get<LevelComp>(p).level();")))
        self.assertEqual(1, len(self.violations("x = static_cast<const LevelComp*>(v)->level();")))

    def test_raw_read_through_a_bound_variable_is_reported(self):
        source = "const auto* lc = registry.try_get<LevelComp>(p);\nreturn lc->level();"
        self.assertEqual([2], [line for line, _ in self.violations(source)])

    def test_if_initializer_binding_is_tracked(self):
        source = "if (const auto* lc = registry.try_get<LevelComp>(p); lc != nullptr) {\n    use(lc->level());\n}"
        self.assertEqual([2], [line for line, _ in self.violations(source)])

    def test_alias_of_a_bound_variable_is_tracked(self):
        source = "auto* ptr = registry.try_get<LevelComp>(p);\nauto& lc = *ptr;\nreturn lc.level();"
        self.assertEqual([3], [line for line, _ in self.violations(source)])

    def test_typed_parameter_is_tracked(self):
        self.assertEqual(1, len(self.violations("uint32_t F(const LevelComp& lc) { return lc.level(); }")))

    def test_capped_level_may_wrap_across_lines(self):
        source = "const auto* lc = registry.try_get<LevelComp>(p);\nreturn playerlevel::CappedLevel(\n    lc->level());"
        result = scan_source(self.OTHER, source)
        self.assertEqual([], result.violations)
        self.assertEqual([3], result.wrapped_lines)

    def test_a_second_read_after_the_capped_call_closes_is_reported(self):
        source = (
            "const auto* a = registry.try_get<LevelComp>(p);\nconst auto* b = registry.try_get<LevelComp>(q);\n"
            "return CappedLevel(a->level()) + b->level();"
        )
        result = scan_source(self.OTHER, source)
        self.assertEqual(1, len(result.violations))
        self.assertEqual([3], result.wrapped_lines)

    def test_persisted_message_level_is_a_stored_level_read(self):
        self.assertEqual(1, len(self.violations("return message.level_component().level();")))

    def test_comments_and_literals_are_ignored(self):
        source = (
            "const auto* lc = registry.try_get<LevelComp>(p);\n"
            "// lc->level()\n"
            "/* lc->level() */\n"
            'const char* text = "lc->level()";\n'
            'const char* script = R"lua(lc->level() "quoted")lua";\n'
            "constexpr uint64_t kBig = 1'000'000;\n"
            "const char quote = '\"';\n"
            "return lc->level();"
        )
        self.assertEqual([8], [line for line, _ in self.violations(source)])

    def test_level_of_other_types_is_not_a_level_comp_read(self):
        source = "const auto* pet = FindPet(comp, id);\nreturn pet->level() + capRow->level();"
        self.assertEqual([], self.violations(source))

    def test_marker_allows_a_raw_read_only_in_its_own_file_and_place(self):
        source = "const auto& lc = registry.emplace<LevelComp>(p);\nconst uint32_t stored = lc.level();  // 直读存储等级:加载器"
        self.assertEqual([], self.violations(source, self.LOADER))
        self.assertEqual(1, len(self.violations(source, self.OTHER)), "a marker cannot whitelist a raw read in another file")
        wrong_place = source.replace("加载器", "GmSetLevel")
        self.assertEqual(1, len(self.violations(wrong_place, self.LOADER)), "the marker's place must match its file")
        unknown_place = source.replace("加载器", "随便")
        self.assertEqual(1, len(self.violations(unknown_place, self.LOADER)))

    def test_stale_marker_is_reported(self):
        source = "const uint32_t stored = 0;  // 直读存储等级:加载器"
        self.assertEqual(1, len(self.violations(source, self.LOADER)))


class RepositoryLevelReadContractTest(unittest.TestCase):
    def test_allowed_places_still_exist(self):
        for relative_path in ALLOWED_MARKERS:
            with self.subTest(path=relative_path):
                self.assertTrue((REPO / relative_path).is_file(), "whitelisted file moved or deleted; update ALLOWED_MARKERS")

    def test_stored_level_is_read_raw_only_in_the_four_allowed_places(self):
        sources = list(level_comp_sources())
        self.assertGreater(len(sources), 5, "must scan all of cpp/libs and cpp/nodes, not a single directory")
        wrapped = 0
        problems = []
        for relative_path, text in sources:
            result = scan_source(relative_path, text)
            wrapped += len(result.wrapped_lines)
            problems += [f"{relative_path}:{line}: {reason}" for line, reason in result.violations]
        self.assertGreaterEqual(wrapped, 5, "detector broken: fewer than 5 reads wrapped in CappedLevel were recognized")
        self.assertEqual([], problems, "\n" + "\n".join(problems) + "\nSee the header comment of player_level_rules.h")


if __name__ == "__main__":
    unittest.main()
