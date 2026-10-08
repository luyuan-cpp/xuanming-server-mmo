# data/ —— 配置表

策划配置的源头。一张表由两个文件共同定义:

| 文件 | 管什么 | 谁来改 |
|------|--------|--------|
| `<Sheet>.xlsx` | 列名与数据 | 策划 |
| `schema/<sheet>_table.proto` | 结构:类型、主键、索引、外键、注释 | 程序,走代码评审 |

结构的事实源是 `schema/` 里的 proto,不是 xlsx 表头。两者按列名绑定,对不上时导表器当场报错,
不产出任何文件。

## 目录

```
data/
├── <Sheet>.xlsx           源表,一张表一个文件
├── schema/                每张表的权威结构定义
│   ├── cfg_options.proto  可用的 option 词表(主键、索引、外键等)
│   └── <sheet>_table.proto
├── tip/Tip.xlsx           错误码(tip):全仓一条数轴,由导表器分段发号
├── operator/Operator.xlsx 运算符枚举
└── scene_nav_bin/         场景导航网格,由 tools/navmesh_baker 烘焙
```

## 导表

```powershell
dev.bat export    # 只导表
dev.bat gen       # 导表 + 重新生成 protobuf 代码
```

导表器在 [`tools/data_table_exporter/`](../tools/data_table_exporter/)。产物去向:

| 产物 | 位置 |
|------|------|
| 表数据(JSON、`.pb`) | `generated/tables/` |
| C++ 查表代码 | `cpp/generated/table/` |
| Go 查表代码 | `go/shared/generated/` |
| Java 查表代码 | `java/config_node/` |
| 客户端查表代码 | 同级仓库 `../mmorpg-client/` |

## 常见操作

- **加一列**:xlsx 里加列并写英文列名 → `schema/<sheet>_table.proto` 加一个字段,编号取当前最大值加一 → `dev.bat gen`
- **加一个错误码**:`tip/Tip.xlsx` 加一行 → `dev.bat gen` → 在服务里引用生成的枚举,不手写数字
- **删一列**:字段编号上线后不复用,在 schema 里写 `reserved N;`

完整流程、禁忌和全表索引(哪张表有主键、索引、外键)见 [AGENTS.md](AGENTS.md)。
