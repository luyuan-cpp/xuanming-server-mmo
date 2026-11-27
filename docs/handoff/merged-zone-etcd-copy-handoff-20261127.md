# 合服映射的 etcd 副本:换电脑接手交接(2026-11-27)

> 写给新电脑上的 Claude Code 与 Gemini。负责人在新电脑上说「继续做完 handoff」时,先读换电脑总入口单 `docs/handoff/new-computer-start-here-handoff-20261127.md`;本单是它 §3 的第 4 行,轮到时从本单 §2 开始做。
> 本单只管一条线:**目录第 0 片失联时 data_service 照样启动**(合服映射 `zone_merged_into` 的 etcd 副本)。方案以[设计稿](../design/merged-zone-etcd-copy.md)为准,本单只写现状、待办和接手步骤。

## 0. 现状

- **代码与文档两批都写完了,都在 main**(负责人 11-27 的快照提交 `bcea63be6` 00:54、`6b4ae7852` 01:17)。两批分别是:
  - 第一批:data_service 读侧。第 0 片连不上时按 etcd 副本启动;每个新版本先 CAS 发布进副本,再拿来回答。
  - 第二批:
    - 合服工具 `tools/merge_zone` 真写提交之后发布副本,审计比对副本;
    - 判定规则抽成共享包 `go/shared/mergedzonecopy`;
    - 副本键改按第 0 片库名。
- **全部未编译、未运行**,没有任何运行记录。
- **`tools/merge_zone` 现在编不过**,要先跑 `go mod tidy`:
  - 它的 go.mod 加了 etcd 客户端,但缺 etcd 依赖链的 `// indirect` 行;
  - go.sum 已先并入 `go/shared/go.sum` 的全部行。这些是同一模块、同一版本、逐字相同的校验和,合并时核过没有冲突,所以是个超集。
- 不涉及客户端、proto、表结构、C++、Java(Java 版没有 data_service 与合服工具,不加 PARITY 行)。
- 前置的那条线「data_service 启动期故障隔离」(批 1、批 2、第二轮)的代码也都在 main,同样没跑过;它的命令要先跑(§3)。

## 1. 换机前(本机,负责人做)

1. **提交并推送本机工作区**。新电脑只能从 GitHub 拿到东西,没推送的改动随旧电脑回收一起丢:
   - 本线的代码与文档已随快照 `3d715db97`(11-27 02:05)全部入库并推送;本单之后的改动(这一条和开头的入口说明)要再提交一次;
   - 换电脑总入口单 `docs/handoff/new-computer-start-here-handoff-20261127.md` 是别的会话在写的,11-27 02:10 核对时还没入库;
   - 旧入口 `docs/handoff/README.md` 已入库,但在工作区被删了,总入口单换成了上面那个名字;
   - 新旧入口这两处要一起提交,否则新电脑上没有总入口;
   - 工作区里还有别的会话没提交的改动(11-27 02:10 核对时 41 处),回收前不提交推送就会丢,要不要提交由负责人定。
2. **Claude Code 的本机记忆不在仓库里**,不会跟到新电脑。本线要记住的定案已经抄进本单 §5;别的线的记忆要不要搬,由负责人定。

## 2. 新电脑开工(Claude Code)

1. 拉取 main,先核对换机前推送齐了。三条都成立才往下做,缺一条就停下问负责人:
   - `git log --oneline -1 -- docs/handoff/merged-zone-etcd-copy-handoff-20261127.md` 有输出(本单已入库);
   - `git log -1 --format=%h -- go/shared/mergedzonecopy/copy.go` 有输出(共享包已入库);
   - `git grep -c "合服映射的 etcd 副本第二批" -- docs/PROGRESS.md` 是 1(第二批条目已入库)。
2. 按 `AGENTS.md` 会话门禁读:`AGENTS.md`、设计稿全文、PROGRESS 2026-11-27 的两条(按标题 grep「合服映射的 etcd 副本」,读条目,不要从头读整份 PROGRESS)、本单。
3. 查 Gemini 跑过没有:PROGRESS 第二批条目之后有没有 Gemini 写的本线验证结果。
   - 没有 → 把 §3 整节交给 Gemini。Claude 不跑编译和测试(`AGENTS.md` §10.1)。
   - 有、而且有红的 → 按首错原文改代码(§10.3 最高档模型;负责人明说用当前模型时就用当前模型),改完更新 PROGRESS 与设计稿,再交 Gemini 重跑。
   - 全绿 → 做 §4。
4. 本线的代码改动只由 Claude 写,交接给 Gemini 的只有生成、编译、测试、冒烟;Gemini 跑出要改代码的地方,停下交回 Claude。

## 3. 交给 Gemini

**命令与通过标准只有一个出处**:PROGRESS 2026-11-27「合服映射的 etcd 副本第二批」(它取代第一批条目的命令,第二批覆盖第一批)。这里只补新电脑上要多做的准备和顺序。

1. **准备 Go 环境**(新电脑才需要):
   - 装 Go 1.27.2 或更新(三个模块的 go.mod 都写 `go 1.27.2`,命令里设了 `GOTOOLCHAIN=local`,不会自动下工具链)。
   - PROGRESS 里的命令设了 `GOPROXY=off`,那是按旧电脑已有的模块缓存写的;新电脑缓存是空的。**第一次跑时不要设 `GOPROXY=off`**:联网把依赖拉下来(默认代理或本地可用的镜像),缓存齐了之后再照 PROGRESS 设回去。
   - 本线依次在 `go/shared`、`go/data_service`、`tools/merge_zone` 三个目录下跑。`tools/merge_zone` 的第一步就是 `go mod tidy`。
2. **准备一次性的真库与 etcd**(自己起,跑完删,不借别的线的容器;版本跟 `deploy/docker-compose*.yml` 一致):
   ```powershell
   docker run -d --name mzc-it-etcd  -p 127.0.0.1:2379:2379  quay.io/coreos/etcd:v3.6.16 etcd --listen-client-urls http://0.0.0.0:2379 --advertise-client-urls http://127.0.0.1:2379
   docker run -d --name mzc-it-tidb  -p 127.0.0.1:14000:4000 pingcap/tidb:v8.5.5
   docker run -d --name mzc-it-mysql -p 127.0.0.1:13306:3306 -e MYSQL_ROOT_PASSWORD=<一次性口令> mysql:8.0.46
   ```
   - `MERGEDZONECOPY_IT_ETCD=127.0.0.1:2379`。
   - `MERGE_ZONE_IT_DSN`:TiDB 档 `root:@tcp(127.0.0.1:14000)/`;MySQL 档 `root:<一次性口令>@tcp(127.0.0.1:13306)/`,同时把 `MMORPG_SQL_BACKEND` 换成 `mysql`。DSN 一律数字 loopback、不带库名、不带参数,用例自己建库删库(`tools/merge_zone/merge_integration_test.go` 文件头)。
   - `ACCOUNT_DIRECTORY_TEST_DSN` 指 TiDB 实例,夹具要求看 `go/data_service/internal/store/account_directory_tidb_test.go` 与 `shared/sqltest` 的 `OptionsFromEnv`。
   - 跑完 `docker rm -f mzc-it-etcd mzc-it-tidb mzc-it-mysql`。
3. **顺序**:
   1. 前置线:PROGRESS 2026-11-27「data_service 启动期故障隔离」的批 1、批 2、第二轮三条,按这个顺序跑(它们和本线都改了 `go/data_service/internal/svc/servicecontext.go`)。
   2. 本线:PROGRESS「合服映射的 etcd 副本第二批」整段命令,逐条对照它的「通过标准」。
   3. 可选的反向确认:同一条目里的「可选的反向确认」,各改一处、跑完改回。
4. **入库**:
   - `go mod tidy` 改出来的 `tools/merge_zone/go.mod`、`go.sum` 要入库。正常情况下 go.mod 只多出 `// indirect` 行、go.sum 只删行;要是新增了行,照实记下是哪几行。
   - 结果写一条新的 PROGRESS 条目(只追加)。
   - 提交要负责人同意,而且只带这几个文件(`AGENTS.md` §9)。
5. **停下来交回 Claude 的情形**:
   - 编译错误、用例红;
   - tidy 改了 go.mod、go.sum 以外的文件;
   - 需要改任何代码或断言。
   保留第一条错误的原文,不改断言,不连续重试。

## 4. Gemini 全绿之后还剩的

1. **第二批的独立复核**(建议做,要等额度):
   - 收尾时全部模型的周额度已用 85%,过了负责人定的 80% 线,所以没派子代理,只做了自查(设计稿 §12.2)。
   - 额度重置(12-03)之后,派一个只读子代理,**只给代码和需求,不给设计稿的论证**。
   - 范围:`go/shared/mergedzonecopy/`、`tools/merge_zone/{main,merge,route_store}.go`、`go/data_service/internal/svc/merged_zone_copy.go`。
   - 重点看:
     - 工具提交后发布与 data_service 自己发布并发时的 CAS;
     - 提交之后各种失败的退出码;
     - 审计是不是只读;
     - 两边副本键的推导是否一致;
     - 第 0 片回档、删键之后的行为。
   - 查出的问题按「现在修」处理,结论写回设计稿 §12.2。
2. **多实例演练**(上线前必须跑一次):合服手册 `docs/ops/merge-zone-runbook.md` §11「第 0 片失联与 etcd 副本」。要两个 SQL 实例、目录 K>1,单实例调试栈做不了;交 Gemini 在 kind 或测试集群上跑。
3. **不是待办**(定案,别当成漏项重做):
   - etcd 开鉴权、把映射整张搬进 etcd(设计稿 §3.2、§10:等全控制面开 etcd 鉴权时再评估);
   - etcd 回档之后让在跑的副本回写(§10,已否掉,取舍写在那里);
   - Java 版(不涉及)。

## 5. 定案(新电脑没有旧电脑的 Claude 记忆,以这里为准)

- 第 0 片是合服映射唯一真源。
  - 版本 = `zone_merge_history` 的行数(每次变更在同一个事务里恰好追加一行,只增不改);
  - 世代 = 第 0 片 `route_meta.generation`;
  - 映射、版本、世代一条语句读出(`mergedzonecopy.ReadSQL`)。
- 副本键 `/mmorpg/merged_zone_copy/<第 0 片库名>`:
  - data_service 用 `dirshard.DirectoryDBName(SnapshotMySQL.DBName, 0, K)` 算,合服工具取 DSN 里的库名;
  - K=1 时是基名,K>1 时是 `<基名>_00`。
- 读的顺序永远是先读副本、后读第 0 片;只在提交之后发布;发布按 ModRevision CAS,撞上就重读,最多 3 轮。
- data_service 启动时,只有 `authoritysql.ErrUnreachable`、`store.ErrNoWriterGrant`(第 0 片连不上)才从副本起;连得上却读不对(1142、1146、语句错)照旧拒启,滚动发布停在第一个副本。
- 一律拒绝:
  - 第 0 片落后于副本、同版本内容不同、世代不同,不自动覆盖;
  - 进程记住自己回答过的整份映射,版本变小、同号换内容、换了库,都要重启才接受。
- 合服工具:
  - 真写必须带 `-etcd-hosts`,没带就在连库之前拒绝(退出码 1);
  - 提交之后的任何失败报退出码 2,文案写明映射已提交;重跑同一条命令补发布(幂等重跑、没东西可删也发布);
  - 审计带 etcd 地址就比对副本,只读;对不上按拒绝退出。
- 删副本键 = 声明以第 0 片为准:
  - 第 0 片 PITR、清表、库重建之后,要**先停掉全部 data_service 再删**;
  - etcd 从快照恢复之后删键,不用停;
  - PITR 之后先删键、再跑审计。
- 新的读取方要读这份映射,用 `go/shared/mergedzonecopy`,不要另写一套 SQL 或判定。

## 6. 索引

- 设计稿:`docs/design/merged-zone-etcd-copy.md`(§4 设计、§5 一致性论证、§6 漏洞分析、§8 文件清单、§9 单测、§10 残余、§11 可复用做法、§12 复核)。
- PROGRESS 2026-11-27:「合服映射的 etcd 副本」(第一批)、「合服映射的 etcd 副本第二批」(第二批,命令以它为准)。
- 手册:
  - 合服 `docs/ops/merge-zone-runbook.md`(§3 前置条件与 etcd 地址、§5 故障处置、§11 验证);
  - 开服 `docs/ops/k8s-open-server-runbook.md` §1、§9;
  - PITR `docs/ops/mysql-backup-pitr-runbook.md`;
  - etcd `docs/ops/etcd-maintenance-runbook.md` §7a。
- 前置线:`docs/design/data-service-startup-isolation.md`(§4、§10 已同步本线)。
