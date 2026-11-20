"""C++ 类 / 结构体不放裸指针成员的文本预检;只读源码,不编译、不运行服务。

真门禁是 MSBuild 每个工程编译前跑的 tools/scripts/check_no_raw_pointer_member.ps1(Directory.Build.targets 的
NoRawPointerMemberCheck),规则在 cpp/plugin/no_raw_ptr_matcher.cq:`fieldDecl(hasType(pointerType()))`。类 / 结构体 /
联合体的非静态成员只要是指针类型就算,`const char *` 与函数里的局部结构体(含测试夹具)也算。

为什么另有这份预检:门禁只在 Windows 的 MSBuild 里跑,逐个翻译单元检查,碰到第一个违例就让整个工程失败,后面的
单元不再检查,这个工程的用例一条都跑不到。2026-11-13 整解停在 scene 节点的 `EventLoop* loop_`,cross_zone_test 停在
测试里的 `NamedMethod`,断线保留与换节点重投的用例因此一周没有运行记录。这份预检几秒钟跑完,Linux 上也能跑,
写代码的人交付前自己先筛一遍;CI 的 `test_cpp*` 一步也会跑它。

识别靠文本,不是编译器:
- 注释、字符串、字符字面量、预处理行先去掉,行号不变;
- 按花括号层级找类 / 结构体 / 联合体的体(函数里的局部结构体也找),只看体内以分号结尾的声明;
- 形如 `T *name;`、`T *name = …;`、`T *name{…};`、`R (*name)(…);` 的算指针成员;
- 不算:带 `static` 的(静态成员不是字段)、函数声明、指针数组、`std::vector<T*>` 这类容器、引用成员、
  `using` / `typedef` / `friend`。
认不出来的写法(宏拼出的成员、`typedef` 出来的指针类型、成员声明前有不带分号的宏)不在预检范围内,结论以门禁为准。
误报或漏报时改识别规则,并把那种写法补进下面的 RecognitionTest。

排除口径与门禁一致:third_party、generated、构建输出、cpp/libs/engine/muduo_windows;cpp/plugin 是检查器自己的
源码,不随 game.sln 编译。

命中之后怎么改:名字用 `std::string_view`;非空借用用引用成员;可空借用用 `std::optional<std::reference_wrapper<T>>`;
拥有用智能指针。`std::string + std::string_view` 要到 C++26 才有,拼接时包一层 `std::string(...)`。

运行(仓库根目录):python tools/scripts/tests/test_cpp_no_raw_pointer_member.py -v
"""

import os
from pathlib import Path
import re
import unittest


REPO = Path(__file__).resolve().parents[3]
SCAN_ROOT = "cpp"
SOURCE_SUFFIXES = (".h", ".hpp", ".hh", ".hxx", ".cpp", ".cc", ".cxx")
SKIPPED_DIRECTORIES = {"third_party", "generated", "build", "bin", "x64"}
SKIPPED_PREFIXES = ("cpp/libs/engine/muduo_windows/", "cpp/plugin/")

NOT_A_FIELD = re.compile(r"^(?:using|typedef|friend|static_assert|template|enum)\b|\boperator\b|\bstatic\b")
ACCESS_LABELS = {"public", "private", "protected"}
# 默认成员初始化式的等号(不是 ==、<=、>=、!=)
INITIALIZER = re.compile(r"(?<![=!<>])=(?!=)")
# 最内层的一对模板实参;反复删掉之后,std::function<void(T*)> 里的括号和星号就不会被当成声明自己的
TEMPLATE_ARGUMENTS = re.compile(r"<[^<>]*>")
# R (*name)(args):第一个括号里就是 *name 的才是函数指针成员;void Set(void (*cb)(int)) 是带函数指针形参的函数声明
FUNCTION_POINTER_FIELD = re.compile(r"^[^()=]*\(\s*\*\s*\w+\s*\)\s*\(")
ARRAY_FIELD = re.compile(r"\[[^\]]*\]$")
# 声明以「* 成员名」结尾才是指针成员;std::vector<T*> items 的 * 后面隔着 >,不算
POINTER_FIELD = re.compile(r"[\w>\]]\s*(?:const\s*|volatile\s*)*\*+\s*(?:const\s+|volatile\s+)*\w+\s*(?:\{\})?$")
RAW_STRING_OPEN = re.compile(r'R"([^()\s\\]{0,16})\(')


def strip_comments_and_literals(text):
    """注释与预处理行去掉,字符串与字符字面量换成空引号;换行原样保留,所以行号不变。"""
    out = []
    i, n = 0, len(text)
    while i < n:
        c = text[i]
        following = text[i + 1] if i + 1 < n else ""
        if c == "/" and following == "/":
            while i < n and text[i] != "\n":
                i += 1
        elif c == "/" and following == "*":
            end = text.find("*/", i + 2)
            end = n if end < 0 else end + 2
            out.append("\n" * text.count("\n", i, end))
            i = end
        elif c == "R" and following == '"' and RAW_STRING_OPEN.match(text, i):
            closing = ")" + RAW_STRING_OPEN.match(text, i).group(1) + '"'
            end = text.find(closing, i)
            end = n if end < 0 else end + len(closing)
            out.append('""' + "\n" * text.count("\n", i, end))
            i = end
        elif c == '"' or (c == "'" and not (i > 0 and text[i - 1].isalnum())):
            # 单引号前面是字母或数字时是数字分隔符(1'000),不是字符字面量
            end = i + 1
            while end < n and text[end] != c and text[end] != "\n":
                end += 2 if text[end] == "\\" else 1
            out.append(c + c)
            i = end + 1
        elif c == "#" and text[text.rfind("\n", 0, i) + 1:i].strip() == "":
            # 预处理行整行丢掉(含反斜杠续行):宏定义里的花括号不参与层级
            while i < n:
                end = text.find("\n", i)
                if end < 0:
                    i = n
                elif text[end - 1] == "\\" or text[end - 2:end] == "\\\r":
                    out.append("\n")
                    i = end + 1
                    continue
                else:
                    i = end
                break
        else:
            out.append(c)
            i += 1
    return "".join(out)


def _block_kind(header, inside_class):
    """左花括号之前、上一个分号或花括号之后的那段文字,决定这一对花括号是什么。"""
    header = header.strip()
    if re.search(r"\benum\b", header):
        return "enum"
    if re.search(r"\bnamespace\b", header) or re.search(r'\bextern\s*""', header):
        return "namespace"
    if "(" in header:
        return "code"  # 函数体、lambda、构造函数初始化列表里的 a{1}
    if re.search(r"\b(?:class|struct|union)\b", header) and "=" not in header:
        return "class"
    if inside_class and header and header[-1] not in "=,)":
        return "initializer"  # 成员的花括号初始化:T* p{nullptr};
    return "code"


def _is_pointer_field(statement):
    statement = " ".join(statement.split())
    if not statement or NOT_A_FIELD.search(statement):
        return False
    if FUNCTION_POINTER_FIELD.match(statement):
        return True
    declaration = INITIALIZER.split(statement, maxsplit=1)[0].strip()
    while TEMPLATE_ARGUMENTS.search(declaration):
        declaration = TEMPLATE_ARGUMENTS.sub("", declaration)
    if "(" in declaration or ARRAY_FIELD.search(declaration):
        return False  # 函数声明;指针数组的类型是数组
    if POINTER_FIELD.search(declaration):
        return True
    # int *a, *b; 这种一条声明多个成员的写法:末尾那个认不出来,整条报出来
    return "*" in declaration and "," in declaration


def raw_pointer_members(text):
    """返回 [(行号, 声明原文)]:类 / 结构体 / 联合体体内看起来是指针成员的声明。"""
    code = strip_comments_and_literals(text)
    found = []
    blocks = []  # 每层未闭合的花括号: (类别, 进入前攒下的声明片段, 那段声明的起始行)
    statement, statement_line, line = "", 1, 1  # statement 为空串 = 当前这条声明还没有非空白字符

    def inside_class():
        return bool(blocks) and blocks[-1][0] == "class"

    for index, c in enumerate(code):
        if c == "\n":
            line += 1
        if c == "{":
            blocks.append((_block_kind(statement, inside_class()), statement, statement_line))
            statement = ""
        elif c == "}":
            kind, outer_statement, outer_line = blocks.pop() if blocks else ("code", "", line)
            statement = ""
            if kind == "initializer":
                statement, statement_line = outer_statement + "{}", outer_line
            elif kind == "code" and inside_class():
                # 构造函数初始化列表 Foo() : a{1}, b{2} {}:花括号后面紧跟 , 或 { 时还是同一个函数定义
                rest = code[index + 1:index + 200].lstrip()
                if rest[:1] in (",", "{"):
                    statement, statement_line = outer_statement + "{}", outer_line
        elif c == ";":
            if inside_class() and _is_pointer_field(statement):
                found.append((statement_line, " ".join(statement.split())))
            statement = ""
        elif c == ":" and statement.strip() in ACCESS_LABELS:
            statement = ""  # public: / private: / protected: 不属于后面那条声明
        elif statement or not c.isspace():
            if not statement:
                statement_line = line
            statement += c
    return found


def cpp_sources():
    """(仓库相对路径, 源码) 逐个给出 cpp/ 下要查的源文件,排除口径见文件头。"""
    for folder, directories, names in os.walk(REPO / SCAN_ROOT):
        relative_folder = Path(folder).relative_to(REPO).as_posix() + "/"
        directories[:] = sorted(
            name for name in directories
            if name not in SKIPPED_DIRECTORIES and not name.startswith(".")
            and not (relative_folder + name + "/").startswith(SKIPPED_PREFIXES)
        )
        for name in sorted(names):
            if name.endswith(SOURCE_SUFFIXES):
                path = Path(folder) / name
                yield relative_folder + name, path.read_text(encoding="utf-8-sig", errors="replace")


class RecognitionTest(unittest.TestCase):
    def lines(self, source):
        return [line for line, _ in raw_pointer_members(source)]

    def test_pointer_members_are_reported_with_their_lines(self):
        source = "struct NamedMethod\n{\n\tconst Descriptor *service;\n\tconst char *method;\n};\n"
        self.assertEqual([3, 4], self.lines(source))
        self.assertEqual("const char *method", raw_pointer_members(source)[1][1])

    def test_initialized_and_qualified_pointer_members_are_reported(self):
        source = (
            "class Transport {\nprivate:\n"
            "  muduo::net::EventLoop* loop_ = nullptr;\n"
            "  Foo *braced{nullptr};\n"
            "  mutable const Bar* const cached_ = Lookup();\n"
            "  [[maybe_unused]] struct Legacy* legacy_;\n"
            "  Outer<Inner*>* nested_;\n"
            "};\n"
        )
        self.assertEqual([3, 4, 5, 6, 7], self.lines(source))

    def test_function_pointer_member_is_reported(self):
        self.assertEqual([1], self.lines("struct Hooks { void (*onClose)(int code); };"))
        self.assertEqual([], self.lines("struct Hooks { void Set(void (*callback)(int)); };"))

    def test_local_struct_inside_a_function_is_checked(self):
        source = "TEST(Suite, Case)\n{\n\tstruct Violation\n\t{\n\t\tconst char *name;\n\t\tint code;\n\t};\n\tint *local = nullptr;\n}\n"
        self.assertEqual([5], self.lines(source))

    def test_declarations_that_are_not_pointer_fields_are_ignored(self):
        source = (
            "class Registry {\n"
            "  static Registry* instance_;\n"
            "  inline static const char* kName = \"registry\";\n"
            "  Entry* Find(int id) const;\n"
            "  const char* name() const { return \"x\"; }\n"
            "  Entry* slots_[16];\n"
            "  std::vector<Entry*> entries_;\n"
            "  std::function<void(Entry*)> visitor_;\n"
            "  Entry& first_;\n"
            "  std::string_view label_;\n"
            "  std::optional<std::reference_wrapper<Entry>> current_;\n"
            "  using EntryPtr = Entry*;\n"
            "  typedef Entry* LegacyPtr;\n"
            "  friend Entry* Borrow(Registry&);\n"
            "  int Registry::* member_pointer_;\n"
            "};\n"
            "namespace detail { Entry* global = nullptr; }\n"
            "Entry* Make(Entry* seed) { Entry* copy = seed; return copy; }\n"
        )
        self.assertEqual([], self.lines(source))

    def test_member_after_inline_bodies_and_initializer_lists_is_still_seen(self):
        source = (
            "struct Session {\n"
            "  Session() : id_{1}, name_{\"n\"} {}\n"
            "  void Reset() { Peer* local = nullptr; (void)local; }\n"
            "  Peer* peer_;\n"
            "  int id_;\n"
            "  std::string name_;\n"
            "};\n"
        )
        self.assertEqual([4], self.lines(source))

    def test_nested_types_and_anonymous_unions_are_checked(self):
        source = (
            "struct Outer {\n"
            "  enum class Kind { kA, kB };\n"
            "  struct Inner { char* text; };\n"
            "  union { int number; void* opaque; };\n"
            "  Inner* inner;\n"
            "};\n"
        )
        self.assertEqual([3, 4, 5], self.lines(source))

    def test_several_pointers_in_one_declaration_are_reported(self):
        self.assertEqual([1], self.lines("struct Pair { Node *left, *right; };"))

    def test_comments_literals_and_preprocessor_lines_are_ignored(self):
        source = (
            "#define DECLARE_HOLDER(name) struct name { void* raw; }\n"
            "#define LONG_MACRO(name) \\\n  struct name { void* raw; }\n"
            "struct Text {\n"
            "  // Foo* commented;\n"
            "  /* Bar* blocked;\n     Baz* still_blocked; */\n"
            "  std::string a = \"int* inside; {\";\n"
            "  std::string b = R\"sql(SELECT * FROM t; {)sql\";\n"
            "  char brace = '{';\n"
            "  int big = 1'000'000;\n"
            "  Real* found;\n"
            "};\n"
        )
        self.assertEqual([12], self.lines(source))


class RepositoryRawPointerMemberTest(unittest.TestCase):
    def test_no_class_or_struct_declares_a_raw_pointer_member(self):
        scanned = 0
        problems = []
        for relative_path, text in cpp_sources():
            scanned += 1
            problems += [f"{relative_path}:{line}: {statement}" for line, statement in raw_pointer_members(text)]
        self.assertGreater(scanned, 500, "must scan the whole cpp tree, not a single directory")
        self.assertEqual(
            [], problems,
            "\n" + "\n".join(problems) + "\nRaw pointer members fail the MSBuild no-raw-pointer-member gate "
            "(cpp/plugin/no_raw_ptr_matcher.cq). Use std::string_view for names, a reference for a non-null borrow, "
            "std::optional<std::reference_wrapper<T>> for a nullable one. If this is a false positive, fix the "
            "recognizer in this file and add the pattern to RecognitionTest.",
        )


if __name__ == "__main__":
    unittest.main()
