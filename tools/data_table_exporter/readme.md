# Data Table Exporter

Excel → multi-language code generator for game configuration tables.

## Quick Start

```bash
pip install -r requirements.txt
python run.py                        # default config
python run.py path/to/config.yaml    # custom config
python run.py --bitindex-bootstrap   # 仅限「表从未发布过」时初始化位序,见下文红线

# 测试(pytest)
pip install -r requirements-dev.txt
python -m pytest                     # 在本目录下跑
```

## What It Does

Reads `.xlsx` data tables under `data/` and generates:

| Output            | Languages       | Description                              |
|-------------------|-----------------|------------------------------------------|
| JSON data         | —               | Runtime-loadable data files              |
| `manifest.json`   | —               | 产物批次清单(版本号 + 每个文件 sha256)  |
| `.proto` messages | —               | Protobuf message definitions             |
| Compiled protobuf | C++, Go         | `.pb.h/.pb.cc` and `.pb.go`              |
| Config managers   | C++, Go, Java   | Typed table-manager classes              |
| Named constants   | C++, Go, Java   | `constexpr` / `const` / `static final`   |
| Table-ID enums    | C++, Go, Java   | Per-row ID enumerations                  |
| Bit-index maps    | C++, Go         | Stable ID → bit-position mappings        |
| Operator enums    | Proto           | From `Operator.xlsx`                     |
| Tip error enums   | Proto           | From `Tip.xlsx`                          |

## Configuration

All settings live in `exporter_config.yaml`:

- **excel**: source data directory, special Excel paths, metadata row numbers
- **output**: generated file root directories
- **protoc**: protoc binary path and include directories
- **languages**: per-language enable flags, output directories, deploy targets
- **constant_tables**: which tables generate table-ID enums

## Schema 与源表

**schema 的事实源是 `data/schema/<Sheet>_table.proto`,不是 xlsx 表头。**
2026-09-03 迁移完成:类型 / owner / 选项 / 注释全部从 Excel 的第 2~5 行搬进了权威 schema,
xlsx 只留列名与数据。

```proto
// data/schema/mission_table.proto
message MissionTable {
  option (cfg_sheet)       = "Mission";
  option (cfg_primary_key) = "id";

  uint32 id = 1 [(cfg_bit_index) = true];
  // condition表的id
  repeated uint32 condition_id = 2 [(cfg_slots) = 6, (cfg_gfk) = "Condition"];
  uint32 reward_id = 7 [(cfg_fk) = "Reward"];
}
```

对应的 xlsx:

| 行 | 内容 |
|-----|------|
| 1 | 字段名。**机器按这一行的名字绑列**,冻结,改名等于改绑定 |
| 2-4 | 空 |
| 5 | 中文说明。由 `strip_header_rows.py --decorate` 从 schema 的 `//` 注释**投影**而来,不是事实源;在表里改它不生效 |
| 6+ | 数据 |

三条设计要点:

- **按列名绑定,不按列位。** 策划在任意位置插列/删列/重排,其它字段不受影响,字段号一个不动。
- **容量显式。** `(cfg_slots)` 说明一个数组/映射/子消息列表占几个槽;从前这条信息只存在于
  「右边留了几个空格子」里。
- **字段号由人写死。** 产物模板只查 `TableSchema.field_numbers`,不再自己发号;
  从前它按列序自增,于是「在中间插一列」会让后面所有字段的号整体 +1,而 `.pb` 是入库的二进制。
  删字段用 `reserved`,复用退休号会被拒。

option 词表在 `data/schema/cfg_options.proto`,**导表器运行时解析它**得到合法 option 名与取值 ——
词表与解析器不存在漏同步。

### 主键可重复 / 热更契约

主键列写 `(cfg_multi)` = 这张表的 `id` 允许重复：生成 `FindAllById`、不生成 `FindById`、
不允许 `(cfg_bit_index)`。没声明却出现重复 `id` 一律导表失败
（`core/primary_key.py`，与外键校验同一位置 fail-closed）。

生成的管理器返回的行**属于当前快照**：C++ 的旧快照在 `Load()` 时当场析构，
存指针即野指针；Go/Java 不崩但会永远陈旧。契约是**只存 id**，生成代码里写着同一句话。

### 尚未迁移的表怎么办

`core/table_source.py` 按 sheet 名派发:有权威 schema 走 schema-first,没有就退回读
xlsx 第 2~5 行的旧格式(`core/excel_reader.py`,格式见该文件 docstring)。
两条路产出同一个 `TableSchema`,所以生成器与模板不受影响。

### 相关工具

| 命令 | 用途 |
|------|------|
| `tools/bootstrap_schema.py` | 从现有 xlsx + 现有产物播种权威 schema(字段号只抄不算,写盘前 round-trip 自检) |
| `tools/verify_schema_parity.py` | 迁移期闸门:新旧两路 TableSchema 等价 + protoc 语法 lint |
| `tools/strip_header_rows.py` | 迁移最后一步:清掉已搬走的表头行,把中文说明投影回第 5 行 |
| `tools/sandbox_export.py` | **长期验收判据**:在沙盒里完整导一次表,与仓内产物逐字节比对(对仓库只读) |

### Foreign Key Syntax

- `fk:TableName` → references `TableName.id`
- `fk:TableName.column` → references `TableName.column`
- `gfk:TableName` → this column group references `TableName.id`

### 外键校验(会中止导表)

`core/foreign_key.py` 分两层校验,结果分 errors / warnings 两档:

| 档位     | 情况                                          | 后果                       |
|----------|-----------------------------------------------|----------------------------|
| error    | 目标表不存在 / 目标列不存在                    | `sys.exit(1)`,**产出不落盘** |
| error    | 引用列里的取值在目标表对应列中找不到           | `sys.exit(1)`,**产出不落盘** |
| warning  | 目标表没有数据行(无从校验取值)               | 记一条日志,继续           |

- `0`、`-1`、空单元格视为"无引用",不参与校验(与 `excel_reader` 的空值哨兵一致)。
- 数字与文本形态互认(同一个 ID 在两张表里一边存数字一边存文本不会误报)。
- 校验跑在**所有生成之前**:失配时不会用坏数据覆盖上一批好产物。

## 位序(bit_index)红线

带 `bit_index` option 的列会生成 ID → 位序映射,状态持久化在
`state/mapping/table_index_mapping/<table>_mapping.json`。
**这个位序就是玩家存档位图的下标**(`std::bitset<kMissionMaxBitIndex>` /
`RewardBitset`),一经发布就写进了所有存量玩家的档案,因此:

1. **位序只增不改、永不复用**。表里删掉某个 ID,它在 mapping json 里的条目
   **保留原样当占位**;新 ID 一律追加到当前最大位之后。
   绝不能把退休 ID 腾出的位分给新 ID —— 那会让所有存量玩家这一位的含义静默改变。
   即使有人手工把退休条目从 json 里删掉留出空洞,导出器也不会去填它。
2. **状态文件是唯一权威,fail-closed**:
   - 文件损坏 / 格式非法 / 出现重复位 → 抛 `BitIndexStateError` 中止导表,文件不动;
   - 文件缺失 → 中止,提示从 git 恢复;
   - 只有显式 `python run.py --bitindex-bootstrap` 才允许从空初始化,
     **且只在确认该表从未发布过**(没有任何存量玩家位图依赖它)时才可以用。
3. 位图容量取 `max(表里 bit_index 列最大值, 已分配的最大位 + 1)`,只增不减 ——
   最大的 ID 被删掉后它的位仍占着,只看表数据会算出偏小的 bitset 而越界。

`state/mapping/**` 必须进版本库,并且**不要手改**。

## 产物批次清单(`generated/tables/manifest.json`)

Go 与 C++ 双端各自扫 `generated/tables/` 加载同一批表,原先没有任何机制能发现
两端半新半旧。清单给每一批产物一个可比对的身份;
**加载端的校验不在本工具范围内**,这里只负责产出。

```jsonc
{
  "schema_version": 1,                  // 清单格式版本,字段增删时 +1
  "version": 7,                         // 批次版本,单调递增,只在内容变化时 +1
  "content_digest": "…64 hex…",         // 内容指纹,见下
  "generated_at": "2026-08-10T08:55:09Z",
  "source_rev": {
    "commit": "0ddfcad4…",              // 源表所在仓库的 HEAD;取不到写 "unknown"
    "data_dirty": false                 // data/ 是否有未提交改动;取不到按 true(不可信)
  },
  "table_count": 20,
  "tables": [                           // 按表名升序,保证产出稳定
    {
      "name": "Mission",
      "rows": 17,                       // 产物 json 里的数据行数(不是源表行数)
      "source": {                       // 源表指纹
        "file": "Mission.xlsx",
        "size": 12345,
        "sha256": "…"
      },
      "artifacts": [                    // 该表的全部产物;表被 owner 过滤空了则为 []
        { "kind": "json",   "file": "Mission.json", "size": 1234, "sha256": "…" },
        { "kind": "binary", "file": "mission.pb",   "size": 567,  "sha256": "…" }
      ]
    }
  ]
}
```

字段约定:

- `artifacts[].file` 是**相对清单自身目录**的 posix 路径,清单和产物一起搬走也不会失效。
- `content_digest` 只覆盖内容相关字段(表名、行数、源表 sha256、各产物 kind/file/sha256),
  **不含** `version` / `generated_at` / `source_rev`。
- 内容没变 → digest 不变 → **版本号不动、清单文件原样不重写**,重跑导表不会制造 diff;
  内容一变 → `version = 旧值 + 1`。所以"版本号大 = 更新"这个判断是可靠的。
- 旧清单读不出来时只告警并从 `version: 1` 重来 —— 它是元数据,不像位序那样是权威状态,
  最坏只是让加载端多报一次不一致。

加载端建议的用法(待各端自行实现):启动时读 `version` + `content_digest` 并上报;
逐文件核对 `sha256`;两端 `content_digest` 不一致即判定为"半新半旧",拒绝进入对局。

## Project Structure

```
data_table_exporter/
├── exporter_config.yaml      # Central configuration
├── run.py                    # Entry point
├── requirements.txt          # Python dependencies
├── requirements-dev.txt      # 测试依赖(pytest)
├── conftest.py               # pytest 根配置(把包根塞进 sys.path)
├── smoke_test.py             # 对仓库真实配表的冒烟断言
├── tests/                    # 单元 / 集成测试(用临时造的 xlsx)
├── core/
│   ├── config_loader.py      # YAML config loading & path resolution
│   ├── schema.py             # TableSchema / ColumnDef data models
│   ├── excel_reader.py       # Excel → schema + data extraction
│   ├── foreign_key.py        # FK validation across tables(结构 + 取值)
│   ├── manifest.py           # → generated/tables/manifest.json 批次清单
│   ├── type_mapping.py       # C++ / Go / Java type conversions
│   ├── file_utils.py         # File I/O + MD5/SHA-256 + MD5-based copy
│   ├── orchestrator.py       # Pipeline coordinator
│   └── generators/
│       ├── json_gen.py       # → JSON data files
│       ├── proto_gen.py      # → .proto + protoc compilation
│       ├── config_gen.py     # → config manager classes
│       ├── constants_gen.py  # → named constants
│       ├── table_id_gen.py   # → table-ID enums
│       ├── bit_index_gen.py  # → bit-index mappings
│       └── enum_gen.py       # → operator / tip enums
├── templates/                # Jinja2 templates (all .j2)
│   ├── cpp_*.j2              # C++ templates
│   ├── go_*.j2               # Go templates
│   ├── java_*.j2             # Java templates (NEW)
│   ├── proto_table.proto.j2  # Proto message template
│   ├── operator_enum.proto.j2
│   └── tip_enum.proto.j2
├── state/                    # Persistent ID mappings
└── mapping/                  # Reference mapping data
```

## Adding a New Language

1. Add a section under `languages:` in `exporter_config.yaml`
2. Add type mappings in `core/type_mapping.py`
3. Create templates under `templates/{lang}_*.j2`
4. Add generation logic in the relevant generator (e.g., `config_gen.py`)
5. Wire it into `orchestrator.py` if needed

## Pipeline Steps

1. **Read schemas** — parse all `.xlsx` metadata into `TableSchema` objects
2. **Validate FKs** — 结构 + 取值双层校验;**errors 非空即 `sys.exit(1)`,产出不落盘**
3. **Generate JSON** — data rows → `.json` files
4. **Generate protos** — schema → `.proto` message definitions
5. **Generate enums** — Operator/Tip → proto enum files
6. **Compile protos** — `protoc` → C++ / Go compiled outputs
7. **Generate config** — schema → config manager classes (C++/Go/Java)
8. **Generate IDs** — table-ID enums, constants, bit-index maps
9. **Write manifest** — `generated/tables/manifest.json` 批次清单(必须在表数据全部写完之后)
10. **Deploy** — MD5-checked copy to final destinations