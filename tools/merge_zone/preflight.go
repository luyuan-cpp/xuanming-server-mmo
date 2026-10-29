package main

// 合服 / 撤销的前置门禁(preflight)。全部是**拒绝执行**,不是警告。
//
// 门禁存在的理由都一样:合服的四个写入面之间没有事务,一旦开写就没有便宜的
// 回头路。所有「这次能不能开始」的判断必须在第一次写之前问完。
//
// 检查清单(顺序即执行顺序)。「那个 zone」合服时是源区,撤销时是目标区(见 preflightParams.zone):
//   P1 源 / 目标 zone 库存在              —— 目标 zone 从没部署过就不是合服目标(MySQL 侧,调用方先做)
//   P2 那个 zone 的场景节点全下线          —— scene_nodes:zone:{zone}:load 必须空
//   P3 go/db 对那个 zone topic 的积压 = 0   —— 还有没落库的存盘任务就动 = 动的是一份旧的。
//                                            只对「唯一权威组」算积压:候选组(按 zone 推导的 db_rpc_consumer_group_z<zone>
//                                            加 legacy 共享组,或 -kafka-group 显式指定的一个)里恰好一个在本 topic 上有已提交位点;
//                                            两个都有 / 都没有 / CLI 读不出,一律拒绝(见 resolveAuthoritativeLag);
//                                            LAG 合计为 0 之后再用 kafka-get-offsets 证明每个非空分区都在表里且 LAG=0
//                                            (Empty 组的 describe 不列从未提交过的分区,见 verifyAuthoritativeCoverage)
//   P4 kafka:retry / kafka:dead 队列为空    —— 同上,而且这两个是**已经失败**的任务;含按原始分区拆开的
//                                            ready 列表 kafka:retry:queue:{topic}:p*(经 SCAN 发现,见 dbTaskQueueKeys)
//   P5 本次涉及的玩家没有任何在持的锁      —— lock:player:* / player:{id}:__lock
//   P6 本次涉及的玩家没有在线会话          —— player:session:*
//   P7 本次涉及的玩家不在任何活队伍里      —— 全球SQL team_membership.team_id必须为0或无原件
//
// 合服与撤销共用这一套(2026-09-28,player-storage-placement.md §12 A7):撤销要把玩家改回源区、删目标库
// 的行,目标区还有节点在跑、目标 topic 还有没落库的存盘、玩家还在线,都等于把合服之后产生的写直接丢掉。
//
// P3 的可注入性:Kafka 在开发机上根本不跑,而这条检查又不能省(它保护的是
// 「玩家最后一次存盘有没有落库」)。所以 lag 的来源是一个接口:
//   - kafkaCLILagSource   对每个候选组跑一次 kafka-consumer-groups(.sh|.bat) --describe,
//                         认定唯一权威组后解析它的 LAG 列,再用同目录的 kafka-get-offsets 核对非空分区全覆盖 —— 生产用法
//   - attestedLagSource   仅保留旧参数的明确拒绝出口，人工声明不能给出成功排空
//   - fakeLagSource       测试注入
// 缺正式NORMAL冻结原件或CLI复核 = 拒绝，不能把「查不到」当0。
//
// P3 为什么是「唯一权威组」(2026-10-15,docs/design/db-consumer-group-per-zone.md §7):
// go/db 的存盘消费组正从全 zone 共用的 db_rpc_consumer_group 逐 zone 交接到 db_rpc_consumer_group_z<zone>。
// 交接收尾(kafka_offset_reset.ps1 -FinalizeHandoff)之前,两个组在本 topic 上可能都有位点,其中一个是冻结的旧位点。
// 「逐组 / 逐分区取 LAG 最小值」是 fail-open:冻结在高位的那一组 LAG 可以是 0,而真正在消费的组还有积压。
// 所以只认在本 topic 上有已提交位点的那一个组;判不出唯一的一个就拒绝,让运维先收尾或用 -kafka-group 显式指定。

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// ── Kafka 消费积压 ────────────────────────────────────────────

// kafkaLagSource 报告候选 consumer group 里「唯一权威组」在某 topic 上的总积压。
type kafkaLagSource interface {
	// TotalLag 在 groups(候选组,见 kafkaGroupCandidates)里认定唯一权威组 —— 恰好一个组在该 topic 上
	// 有数值已提交位点 —— 只返回它在该 topic 上所有分区的 LAG 之和,以及它的组名(进日志与拒绝文案)。
	// 返回 (0, group, nil) 才算通过;任何错误都让 preflight 失败。
	//
	// 不取各组 / 各分区 LAG 的最小值:交接收尾之前,冻结在高位的旧组 LAG 可以是 0,
	// 而真正在消费的组还有积压 —— 取最小值等于拿旧组的 0 替新组担保,是 fail-open。
	TotalLag(ctx context.Context, topic string, groups []string) (lag int64, group string, err error)
	// Describe 一行说明,进日志与审计记录 —— 运维必须能看出这次的 0 是
	// 「真查的」还是「人肉声明的」。
	Describe() string
}

// kafkaCLILagSource 调用 Kafka 自带的 kafka-consumer-groups CLI(以及同目录的 kafka-get-offsets)。
// 为什么不直接连 broker:merge_zone 是独立 go.mod,拉 sarama 只为了跑一次
// describe-group 不划算,而 CLI 在任何装了 Kafka 的运维机上都有。
type kafkaCLILagSource struct {
	cmdPath   string // kafka-consumer-groups.sh / .bat 的绝对路径;kafka-get-offsets 按同目录同扩展名推导(kafkaGetOffsetsCmdFor)
	bootstrap string
}

func (s kafkaCLILagSource) Describe() string {
	return fmt.Sprintf("kafka-consumer-groups + kafka-get-offsets CLI (%s, bootstrap=%s)", s.cmdPath, s.bootstrap)
}

// TotalLag 对每个候选组各跑一次 describe,把输出与退出状态原样交给 resolveAuthoritativeLag 判定。
// 退出码非 0 在这里不直接失败:有的 Kafka 版本对「组不存在」以非 0 退出,而不存在的候选组是合法情形
// (例如从没切过组的 zone 没有 _z 组),由 resolveAuthoritativeLag 按白名单文案区分。
//
// 权威组 LAG 合计为 0 之后,再取本 topic 的日志边界(kafka-get-offsets --time -2 / -1)交给 verifyAuthoritativeCoverage:
// 合服 T-0 时源区 db 已停、权威组是 Empty,Kafka 的 describe 只列出有已提交位点的分区,从未提交过、却已有消息的分区
// (新 zone / 新世代 topic 刚收到第一批存盘、db 卡在某分区首条消息上 CrashLoop 到 zone-down)根本不出现在表里,
// 其余分区 LAG 都是 0 时合计为 0 —— 只看合计会放行一份旧行,该分区上最后的存盘留在源 topic、再没有人消费(R7 复核 [3])。
func (s kafkaCLILagSource) TotalLag(ctx context.Context, topic string, groups []string) (int64, string, error) {
	results := make([]groupDescribeResult, 0, len(groups))
	for _, group := range groups {
		cmd := exec.CommandContext(ctx, s.cmdPath,
			"--bootstrap-server", s.bootstrap, "--describe", "--group", group)
		out, runErr := cmd.CombinedOutput()
		results = append(results, groupDescribeResult{group: group, output: string(out), runErr: runErr})
	}
	lag, group, err := resolveAuthoritativeLag(topic, results)
	if err != nil || lag != 0 {
		return lag, group, err
	}
	earliest, err := s.logBoundaries(ctx, topic, "-2")
	if err != nil {
		return 0, group, fmt.Errorf("authoritative consumer group %s: %w", group, err)
	}
	latest, err := s.logBoundaries(ctx, topic, "-1")
	if err != nil {
		return 0, group, fmt.Errorf("authoritative consumer group %s: %w", group, err)
	}
	if err := verifyAuthoritativeCoverage(topic, describeOutputOf(results, group), earliest, latest); err != nil {
		return 0, group, fmt.Errorf("authoritative consumer group %s: %w", group, err)
	}
	return 0, group, nil
}

// logBoundaries 跑一次 kafka-get-offsets --time <-2|-1>,返回本 topic 每个分区的 earliest / latest 位点。
// 任何失败(工具推导不出、退出码非 0、Error 行、一个分区都没有)都报错(fail-closed)。
func (s kafkaCLILagSource) logBoundaries(ctx context.Context, topic, at string) (map[int]int64, error) {
	cmdPath, err := kafkaGetOffsetsCmdFor(s.cmdPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, cmdPath, "--bootstrap-server", s.bootstrap, "--topic", topic, "--time", at)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return nil, fmt.Errorf("kafka-get-offsets --topic %s --time %s: %w (output: %s)", topic, at, runErr, truncateForLog(string(out), 500))
	}
	return parseGetOffsets(string(out), topic)
}

// describeOutputOf 取某个候选组的 describe 原文(resolveAuthoritativeLag 认定的权威组一定在 results 里)。
func describeOutputOf(results []groupDescribeResult, group string) string {
	for _, r := range results {
		if r.group == group {
			return r.output
		}
	}
	return ""
}

// kafkaGetOffsetsCmdFor 从 kafka-consumer-groups 的路径推导同目录、同扩展名的 kafka-get-offsets
// (/opt/kafka/bin/kafka-consumer-groups.sh → /opt/kafka/bin/kafka-get-offsets.sh;bin\windows\…\.bat 同理;
// 裸命令名照样替换,交给 exec 的 PATH 查找)。文件名里没有 kafka-consumer-groups 就报错,不猜。
func kafkaGetOffsetsCmdFor(consumerGroupsCmd string) (string, error) {
	const from, to = "kafka-consumer-groups", "kafka-get-offsets"
	dir, base := filepath.Split(consumerGroupsCmd)
	if !strings.Contains(base, from) {
		return "", fmt.Errorf("cannot derive kafka-get-offsets from -kafka-consumer-groups-cmd %q: the file name does not contain %q "+
			"(P3 needs both tools from the same Kafka bin directory)", consumerGroupsCmd, from)
	}
	return dir + strings.Replace(base, from, to, 1), nil
}

// parseGetOffsets 解析 kafka-get-offsets 的输出:每行 "topic:partition:offset",只收 topic 名完全相同的行,返回 分区 → 位点。
// 与 tools/scripts/lib/kafka_consumer_groups.ps1 的 ConvertFrom-KafkaGetOffsets 同口径(那边行首 Error: 由
// Assert-KafkaToolOutputClean 先判,这里一并判):任一行首 "Error:" 即报错;一个分区都没有即报错。
// topic 名里不会有冒号(Kafka 合法字符只有 [a-zA-Z0-9._-]),按最后两个冒号切分。
func parseGetOffsets(out, topic string) (map[int]int64, error) {
	offsets := map[int]int64{}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "Error:") {
			return nil, fmt.Errorf("kafka-get-offsets reported an error: %s", truncateForLog(line, 300))
		}
		last := strings.LastIndex(line, ":")
		if last <= 0 {
			continue
		}
		mid := strings.LastIndex(line[:last], ":")
		if mid <= 0 || line[:mid] != topic {
			continue
		}
		partition, errP := strconv.Atoi(line[mid+1 : last])
		offset, errO := strconv.ParseInt(line[last+1:], 10, 64)
		if errP != nil || errO != nil || partition < 0 {
			continue
		}
		offsets[partition] = offset
	}
	if len(offsets) == 0 {
		return nil, fmt.Errorf("kafka-get-offsets output has no partition of topic %s (expected lines like '%s:<partition>:<offset>')", topic, topic)
	}
	return offsets, nil
}

// describeRow 是 describe 表格里本 topic 一个分区的 CURRENT-OFFSET / LAG("-" 时对应的 ok 为 false)。
type describeRow struct {
	currentOK bool
	current   int64
	lagOK     bool
	lag       int64
}

// describeRowsForTopic 按表头列名(TOPIC / PARTITION / CURRENT-OFFSET / LAG)取本 topic 每个分区的一行。
// 没有表头、分区号不是非负整数、同一分区两行、单元格既不是整数也不是 "-",都报错(不猜)。
func describeRowsForTopic(out, topic string) (map[int]describeRow, error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var cols map[string]int
	rows := map[int]describeRow{}
	required := []string{"TOPIC", "PARTITION", "CURRENT-OFFSET", "LAG"}
	maxCol := 0
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if cols == nil {
			candidate := map[string]int{}
			for i, f := range fields {
				candidate[strings.ToUpper(f)] = i
			}
			complete := true
			for _, name := range required {
				idx, ok := candidate[name]
				if !ok {
					complete = false
					break
				}
				if idx > maxCol {
					maxCol = idx
				}
			}
			if complete {
				cols = candidate
			} else {
				maxCol = 0
			}
			continue
		}
		if len(fields) <= maxCol || fields[cols["TOPIC"]] != topic {
			continue
		}
		partition, err := strconv.Atoi(fields[cols["PARTITION"]])
		if err != nil || partition < 0 {
			return nil, fmt.Errorf("topic %s row has PARTITION %q (not a non-negative integer)", topic, fields[cols["PARTITION"]])
		}
		if _, dup := rows[partition]; dup {
			return nil, fmt.Errorf("topic %s partition %d appears twice in the describe output", topic, partition)
		}
		var row describeRow
		var cellErr error
		row.current, row.currentOK, cellErr = parseOffsetCell(fields[cols["CURRENT-OFFSET"]])
		if cellErr != nil {
			return nil, fmt.Errorf("topic %s partition %d CURRENT-OFFSET: %w", topic, partition, cellErr)
		}
		row.lag, row.lagOK, cellErr = parseOffsetCell(fields[cols["LAG"]])
		if cellErr != nil {
			return nil, fmt.Errorf("topic %s partition %d LAG: %w", topic, partition, cellErr)
		}
		rows[partition] = row
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if cols == nil {
		return nil, errors.New("could not find TOPIC/PARTITION/CURRENT-OFFSET/LAG columns in kafka-consumer-groups output")
	}
	return rows, nil
}

// parseOffsetCell:"-" → (0, false, nil);整数 → (v, true, nil);其它 → 报错。
func parseOffsetCell(cell string) (int64, bool, error) {
	if cell == "-" {
		return 0, false, nil
	}
	v, err := strconv.ParseInt(cell, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%q is neither an integer nor '-'", cell)
	}
	return v, true, nil
}

// verifyAuthoritativeCoverage 证明唯一权威组在本 topic 的每个非空分区上都已提交、且 LAG = 0(纯函数,merge_unit_test.go 覆盖)。
//
// 只有 earliest == latest 的空分区(从未写入或已全部过期)可以没有位点 —— 与 tools/scripts/lib/kafka_consumer_groups.ps1
// 的 Test-HandoffFinalizeReady 同一条规则,改一边必须同步另一边。逐条:
//   - earliest 与 latest 的分区集合必须相同;
//   - describe 里本 topic 出现的每个分区都必须在 kafka-get-offsets 里(get-offsets 少了分区 = 读不全);
//   - get-offsets 的每个非空分区:describe 必须有它的行,CURRENT-OFFSET 为数值且 LAG = 0
//     (逐分区判 0,不靠合计:截断后 LAG 可能为负,与别的分区的正积压相抵)。
//
// 问题一次列全、按文本排序(map 遍历无序,错误文本要确定)。
func verifyAuthoritativeCoverage(topic, describeOut string, earliest, latest map[int]int64) error {
	rows, err := describeRowsForTopic(describeOut, topic)
	if err != nil {
		return err
	}
	var problems []string
	for p := range earliest {
		if _, ok := latest[p]; !ok {
			problems = append(problems, fmt.Sprintf("partition %d: kafka-get-offsets gave an earliest offset but no latest offset", p))
		}
	}
	for p := range latest {
		if _, ok := earliest[p]; !ok {
			problems = append(problems, fmt.Sprintf("partition %d: kafka-get-offsets gave a latest offset but no earliest offset", p))
		}
	}
	for p := range rows {
		if _, ok := latest[p]; !ok {
			problems = append(problems, fmt.Sprintf("partition %d: in the describe output but missing from kafka-get-offsets", p))
		}
	}
	for p, high := range latest {
		low, ok := earliest[p]
		if !ok || low == high {
			continue
		}
		row, ok := rows[p]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("partition %d: non-empty (earliest=%d latest=%d) but the group has no committed offset on it "+
				"(describe of an Empty group omits never-committed partitions)", p, low, high))
		case !row.currentOK:
			problems = append(problems, fmt.Sprintf("partition %d: non-empty (earliest=%d latest=%d) but CURRENT-OFFSET is '-'", p, low, high))
		case !row.lagOK || row.lag != 0:
			problems = append(problems, fmt.Sprintf("partition %d: LAG is not 0 (CURRENT-OFFSET=%d, latest=%d)", p, row.current, high))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("cannot prove topic %s is drained (%d problem(s)): %s", topic, len(problems), strings.Join(problems, "; "))
}

// groupDescribeResult 是一个候选组一次 kafka-consumer-groups --describe 的原始结果(stdout+stderr 合并)。
type groupDescribeResult struct {
	group  string
	output string
	runErr error
}

// consumerGroupMissingPatterns 是「消费组不存在」的文案白名单。
// 与 tools/scripts/lib/kafka_consumer_groups.ps1 的 Get-KafkaGroupMissingPatterns 是同一份,改一边必须同步另一边:
//   - Kafka 3.x:"Error: Consumer group 'g' does not exist." / "The consumer group 'g' does not exist."
//   - 新 group coordinator 可能给出的 GroupIdNotFoundException / GROUP_ID_NOT_FOUND
//
// 后两条是按版本行为推定的;钉版 apache/kafka:4.3.1 上的原文由 Codex 实测(设计文档 §11 的 V3 (b)(h)(i)),
// 拿到后两边同步补正则,并把原文追加为 merge_unit_test.go 的 fixture。原文一条都不命中时这里已是 fail-closed
// (组不存在会落到「无表头 → 报错」),只是会把合法的「legacy 组不存在」误报成失败。
var consumerGroupMissingPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)consumer group '[^']*' does not exist`),
	regexp.MustCompile(`GroupIdNotFoundException`),
	regexp.MustCompile(`GROUP_ID_NOT_FOUND`),
}

// consumerGroupNoActiveMembersPattern:Empty 组(没有成员)一个已提交位点都没有时,CLI 只打这一行、不打表头。
// 与 lib/kafka_consumer_groups.ps1 的 Test-KafkaGroupNoActiveMembersText 同一份。
var consumerGroupNoActiveMembersPattern = regexp.MustCompile(`(?i)consumer group '[^']*' has no active members`)

// consumerGroupMissing 报告 describe 输出是否表明「消费组不存在」(命中任一白名单正则)。
func consumerGroupMissing(out string) bool {
	for _, re := range consumerGroupMissingPatterns {
		if re.MatchString(out) {
			return true
		}
	}
	return false
}

// describeTableScan 是一段 describe 输出的表格扫描结果(不看 Error 行、不判成败)。
type describeTableScan struct {
	headerFound bool // 有一行同时含 TOPIC 与 CURRENT-OFFSET
	present     bool // 本 topic 至少一行数值 CURRENT-OFFSET
}

// scanDescribeTable 按表头列名(TOPIC / CURRENT-OFFSET,不按下标)扫本 topic 的行;
// "-" 表示该分区没有已提交位点;既不是整数也不是 "-" 就报错(不猜)。
func scanDescribeTable(out, topic string) (describeTableScan, error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	topicCol, offsetCol := -1, -1
	var res describeTableScan
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if !res.headerFound {
			t, o := -1, -1
			for i, f := range fields {
				switch strings.ToUpper(f) {
				case "TOPIC":
					t = i
				case "CURRENT-OFFSET":
					o = i
				}
			}
			if t >= 0 && o >= 0 {
				topicCol, offsetCol, res.headerFound = t, o, true
			}
			continue
		}
		if len(fields) <= topicCol || len(fields) <= offsetCol || fields[topicCol] != topic {
			continue
		}
		cell := fields[offsetCol]
		if cell == "-" {
			continue
		}
		if _, err := strconv.ParseInt(cell, 10, 64); err != nil {
			return describeTableScan{}, fmt.Errorf("topic %s row has CURRENT-OFFSET %q (neither an integer nor '-')", topic, cell)
		}
		res.present = true
	}
	if err := sc.Err(); err != nil {
		return describeTableScan{}, err
	}
	return res, nil
}

// firstErrorLine 返回第一条行首(去掉前导空白后)为 "Error:" 的行。
func firstErrorLine(out string) (string, bool) {
	for _, raw := range strings.Split(out, "\n") {
		if line := strings.TrimSpace(raw); strings.HasPrefix(line, "Error:") {
			return line, true
		}
	}
	return "", false
}

// groupMissingInDescribe 报告 describe 输出能否认定「组不存在」:命中白名单(consumerGroupMissing)且**没有表头**才算。
//
// 白名单在整段输出上匹配:管理客户端在新旧 describe API 之间回退时,可能先往 stderr 打一行带 GroupIdNotFoundException 的 WARN、
// 之后照常打印表格(R7 复核 [8])。命中白名单又有表头时:
//   - 本 topic 有数值 CURRENT-OFFSET → 矛盾输出,报错(fail-closed),不猜哪一边为真 —— 当成 missing 会拿另一组的 LAG 放行 P3;
//   - 没有 → 不认定 missing,交给常规判定(Error 行、退出码照旧 fail-closed;两种读法都是「没有位点」)。
//
// 与 tools/scripts/lib/kafka_consumer_groups.ps1 Resolve-KafkaGroupTopicOffsets 的规则 1 同口径,改一边必须同步另一边。
func groupMissingInDescribe(out, topic string) (bool, error) {
	if !consumerGroupMissing(out) {
		return false, nil
	}
	scan, err := scanDescribeTable(out, topic)
	if err != nil {
		return false, err
	}
	if !scan.headerFound {
		return true, nil
	}
	if scan.present {
		return false, fmt.Errorf("contradictory kafka-consumer-groups output: it matches a missing-group message but also lists committed offsets on topic %s; "+
			"refusing to guess which is true (update consumerGroupMissingPatterns and lib Get-KafkaGroupMissingPatterns with this Kafka version's wording): %s",
			topic, truncateForLog(out, 300))
	}
	return false, nil
}

// committedOffsetPresent 报告该组在 topic 上是否至少有一行数值 CURRENT-OFFSET。
//
// 判定(与 lib/kafka_consumer_groups.ps1 的 Resolve-KafkaGroupTopicOffsets 同口径):
//   - 命中「组不存在」白名单且没有表头 → (false, nil);命中白名单、有表头且本 topic 有数值位点 → 报错(groupMissingInDescribe);
//   - 有行首 "Error:" 的行 → 报错(读不到不等于没有);
//   - 找到表头(同一行含 TOPIC 与 CURRENT-OFFSET,按列名定位,不按下标)→ 只看 TOPIC==topic 的行:
//     有一行是整数即 true;"-" 表示该分区没有已提交位点;既不是整数也不是 "-" 就报错(不猜);
//   - 没有表头但有 "has no active members" → (false, nil)(Empty 组一个位点都没有时 CLI 不打表头);
//   - 其余(无表头、无关键字)一律报错。
func committedOffsetPresent(out, topic string) (bool, error) {
	missing, err := groupMissingInDescribe(out, topic)
	if err != nil {
		return false, err
	}
	if missing {
		return false, nil
	}
	if line, ok := firstErrorLine(out); ok {
		return false, fmt.Errorf("kafka-consumer-groups reported an error: %s", truncateForLog(line, 300))
	}
	scan, err := scanDescribeTable(out, topic)
	if err != nil {
		return false, err
	}
	if scan.headerFound {
		return scan.present, nil
	}
	if consumerGroupNoActiveMembersPattern.MatchString(out) {
		return false, nil
	}
	return false, fmt.Errorf("could not parse kafka-consumer-groups output (no TOPIC/CURRENT-OFFSET header, not a missing group, "+
		"not an empty group): %s", truncateForLog(out, 300))
}

// resolveAuthoritativeLag 从各候选组的 describe 结果里认定唯一权威组,并只对它算 LAG(纯函数,merge_unit_test.go 覆盖)。
//
//   - 命中「组不存在」白名单且没有表头的组视为不存在(不论退出码);白名单与本 topic 的数值位点同时出现即报错(groupMissingInDescribe);
//   - 其余组退出码非 0、或输出读不出(committedOffsetPresent 报错)→ 直接报错(fail-closed);
//   - 恰好一个组在本 topic 上有数值位点 → 用 parseConsumerGroupLag 对它算 LAG(沿用对 "-" 的 fail-closed);
//   - 多于一个 → 报错:上一次交接没收尾(冻结的旧位点与新位点并存),哪个是真的不由这里猜;
//   - 一个都没有 → 报错:没有任何组能证明本 topic 已排空。
//
// LAG 合计只覆盖 describe 列出来的分区;非空分区全覆盖由调用方(kafkaCLILagSource.TotalLag)再用 verifyAuthoritativeCoverage 证明。
func resolveAuthoritativeLag(topic string, results []groupDescribeResult) (int64, string, error) {
	if len(results) == 0 {
		return 0, "", errors.New("no candidate consumer group to check")
	}
	var holders []groupDescribeResult
	checked := make([]string, 0, len(results))
	for _, r := range results {
		missing, err := groupMissingInDescribe(r.output, topic)
		if err != nil {
			return 0, "", fmt.Errorf("describe consumer group %s: %w", r.group, err)
		}
		if missing {
			checked = append(checked, r.group+"=missing")
			continue
		}
		if r.runErr != nil {
			return 0, "", fmt.Errorf("describe consumer group %s: %w (output: %s)", r.group, r.runErr, truncateForLog(r.output, 500))
		}
		present, err := committedOffsetPresent(r.output, topic)
		if err != nil {
			return 0, "", fmt.Errorf("describe consumer group %s: %w", r.group, err)
		}
		if !present {
			checked = append(checked, r.group+"=no-offsets")
			continue
		}
		checked = append(checked, r.group+"=offsets")
		holders = append(holders, r)
	}
	switch len(holders) {
	case 1:
		h := holders[0]
		lag, err := parseConsumerGroupLag(h.output, topic)
		if err != nil {
			return 0, h.group, fmt.Errorf("authoritative consumer group %s: %w", h.group, err)
		}
		return lag, h.group, nil
	case 0:
		return 0, "", fmt.Errorf("no candidate group holds committed offsets on topic %s (%s) — cannot prove the backlog is drained; "+
			"pass -kafka-group with the go/db consumer group that actually consumes this topic", topic, strings.Join(checked, ", "))
	default:
		names := make([]string, 0, len(holders))
		for _, h := range holders {
			names = append(names, h.group)
		}
		return 0, "", fmt.Errorf("more than one candidate group holds committed offsets on topic %s (%s): a consumer group handoff "+
			"was not finalized, so one of them is a frozen stale position and the lag of either cannot be trusted. Finalize it first "+
			"(tools/scripts/kafka_offset_reset.ps1 -FinalizeHandoff -Topic %s -Group <current group> -RetireGroup <stale group> "+
			"-Partitions <N>, dry-run then -Apply). If this machine mixed the single-zone default start (bare group name) with "+
			"-Zone N starts (_zN), pass -kafka-group (dev_tools: -MergeKafkaGroup) naming the group that is actually running",
			topic, strings.Join(names, ", "), topic)
	}
}

// dbConsumerGroupForZone 是按 zone 推导的 go/db 消费组名:db_rpc_consumer_group_z<zone>。
// 与 tools/scripts/k8s_deploy.ps1(K8s 切组后的 ConfigMap)和 tools/scripts/go_services.ps1(本机 -Zone N 启动)同名。
func dbConsumerGroupForZone(zone uint32) string {
	return defaultKafkaGroup + "_z" + strconv.FormatUint(uint64(zone), 10)
}

// kafkaGroupCandidates 给出 P3 的候选组:explicit(-kafka-group)非空时只看它;
// 否则是 zone 组,再加上非空且与 zone 组不同的 legacy(-kafka-legacy-group,缺省 db_rpc_consumer_group,传空则只看 zone 组)。
func kafkaGroupCandidates(explicit, legacy string, zone uint32) []string {
	if explicit != "" {
		return []string{explicit}
	}
	zoneGroup := dbConsumerGroupForZone(zone)
	groups := []string{zoneGroup}
	if legacy != "" && legacy != zoneGroup {
		groups = append(groups, legacy)
	}
	return groups
}

// parseConsumerGroupLag 解析 kafka-consumer-groups --describe 的表格输出。
//
// 表头形如(列顺序在 2.x~3.x 稳定):
//
//	GROUP TOPIC PARTITION CURRENT-OFFSET LOG-END-OFFSET LAG CONSUMER-ID HOST CLIENT-ID
//
// 按**表头名**定位 TOPIC / LAG 两列,不按固定下标 —— 版本间会插列。
// LAG 为 "-"(消费者不在线且没有已提交位点)按错误处理:那说明这个 group
// 从没消费过这个 topic,「积压 0」的结论不成立。
func parseConsumerGroupLag(out, topic string) (int64, error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	topicCol, lagCol := -1, -1
	var total int64
	matched := 0
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if topicCol < 0 {
			for i, f := range fields {
				switch strings.ToUpper(f) {
				case "TOPIC":
					topicCol = i
				case "LAG":
					lagCol = i
				}
			}
			if topicCol >= 0 && lagCol >= 0 {
				continue // 这一行是表头
			}
			topicCol, lagCol = -1, -1
			continue
		}
		if len(fields) <= topicCol || len(fields) <= lagCol {
			continue
		}
		if fields[topicCol] != topic {
			continue
		}
		matched++
		lag, err := strconv.ParseInt(fields[lagCol], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("topic %s partition row has non-numeric LAG %q — group has no committed offset; cannot prove the backlog is drained",
				topic, fields[lagCol])
		}
		total += lag
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	if topicCol < 0 || lagCol < 0 {
		return 0, fmt.Errorf("could not find TOPIC/LAG columns in kafka-consumer-groups output")
	}
	if matched == 0 {
		return 0, fmt.Errorf("consumer group has no partition assigned to topic %s — cannot prove the backlog is drained", topic)
	}
	return total, nil
}

// attestedLagSource 是运维的显式声明:「我已经用别的手段确认排空了」。
// 它不查任何东西,但会在日志里留下一条刺眼的记录 —— 这条声明进事故复盘。
type attestedLagSource struct{}

func (attestedLagSource) Describe() string {
	return "已拒绝的人工排空声明"
}

func (attestedLagSource) TotalLag(context.Context, string, []string) (int64, string, error) {
	return 0, "", errNormalFreezeProof
}

// fakeLagSource 只给测试用。group 是假装认定的权威组名(可省略)。
type fakeLagSource struct {
	lag   int64
	group string
	err   error
}

func (f fakeLagSource) Describe() string { return "fake" }
func (f fakeLagSource) TotalLag(context.Context, string, []string) (int64, string, error) {
	return f.lag, f.group, f.err
}

// dbTaskTopic 镜像 go/db/internal/config.DbTaskTopicForGeneration。
// 改一处必须同步另一处(merge_zone 是独立 module,不引主工程包)。
func dbTaskTopic(zone, generation uint32) string {
	base := fmt.Sprintf("db_task_zone_%d", zone)
	if generation <= 1 {
		return base
	}
	return fmt.Sprintf("%s_g%d", base, generation)
}

// dbRetryQueueKey / dbDeadQueueKey 镜像 go/db/internal/kafka/key_ordered_consumer.go
// 里 newConsumer 拼的三把 key(retryQueueKey / retryProcessingKey / retryDeadQueueKey)。
// 它们在 go/db 的 RedisClient 上,go/db/etc/db.yaml 里是 DB 0。
func dbRetryQueueKey(topic string) string      { return "kafka:retry:queue:" + topic }
func dbRetryProcessingKey(topic string) string { return "kafka:retry:processing:" + topic }
func dbDeadQueueKey(topic string) string       { return "kafka:dead:queue:" + topic }

// 2026-10 起 go/db 可以多实例,processing 列表按实例拆开(go/db/internal/kafka/retry_ownership.go):
//   - dbRetryProcessingKey(topic) 是旧版共享列表,新版只回收不写入(滚动升级期间仍可能非空);
//   - 每个实例认领到 kafka:retry:processing:{topic}:{instanceID};
//   - 实例登记在 ZSET kafka:retry:instances:{topic} 里,列表搬空之前不会被摘掉。
//
// 所以"processing 是否为空"必须把登记表里每个实例的列表都算上 —— 只看旧键,门禁会恒为 0、静默放行。
func dbRetryInstancesKey(topic string) string { return "kafka:retry:instances:" + topic }
func dbRetryInstanceProcessingKey(topic, instanceID string) string {
	return dbRetryProcessingKey(topic) + ":" + instanceID
}

// 2026-10-15 起(S1 ④ 重试只交给分区属主,docs/design/db-retry-owner-routing.md)ready 再拆一层,
// 镜像 go/db/internal/kafka 的 retryPartitionReadyKey(topic, p):
//   - dbRetryQueueKey(topic) 变成共享「未分拣收件箱」(回收、停机归还、旧版实例、人工重投都进这里);
//   - RetryRouting=owner 时按载荷里的原始分区分拣到 kafka:retry:queue:{topic}:p{N},N∈[0, PartitionCnt)。
//
// p* 只按 SCAN MATCH 发现,不按 p0..p{N-1} 枚举:
//   - 同一个 Redis 上一次完整的 SCAN 迭代保证返回迭代期间一直存在的键,足以证明「没有非空的 p 列表」;
//   - merge_zone 不知道 PartitionCnt,再写一份常数就违反「契约值单一来源」,而且越界 / 换代残留的 p 键枚举不到;
//   - SCAN 任一页出错整体报错(fail-closed),不能把「没扫完」当成「没有」。
func dbRetryPartitionQueuePrefix(topic string) string { return dbRetryQueueKey(topic) + ":p" }

// 与 go/db/internal/saveoutbox 的64桶契约镜像。索引不分topic世代，旧世代遗留也必须挡住合服。
type saveOutboxReader interface {
	ZCard(context.Context, string) *redis.IntCmd
}

func assertSaveOutboxDrained(ctx context.Context, reader saveOutboxReader, zone uint32) error {
	for bucket := uint64(0); bucket < 64; bucket++ {
		key := fmt.Sprintf("player:save_pending:%d:%d", zone, bucket)
		count, err := reader.ZCard(ctx, key).Result()
		if err != nil {
			return fmt.Errorf("读取存盘补投索引 %s 失败: %w", key, err)
		}
		if count != 0 {
			return fmt.Errorf("%s 仍有 %d 个存盘意图；须由两表SQL ACK完成回收后再合服/撤销，禁止删除未提交意图", key, count)
		}
	}
	return nil
}

// dbRetryPartitionQueuePattern 是 SCAN MATCH 用的 glob:topic 先做 glob 转义,再接 "*"。
// db_task_zone_1 的模式是 kafka:retry:queue:db_task_zone_1:p*,不会匹配 db_task_zone_11 / db_task_zone_1_g2 的键
// (前缀里紧跟 topic 的是 ":p")。
func dbRetryPartitionQueuePattern(topic string) string {
	return escapeRedisGlob(dbRetryPartitionQueuePrefix(topic)) + "*"
}

// escapeRedisGlob 转义 Redis glob 的元字符 * ? [ ] \,让字面量原样参与 MATCH。
// 现网 topic 名不含这些字符;转义是为了让「topic 名里出现元字符」时不至于扫到别的 zone 的键或漏扫。
func escapeRedisGlob(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '*', '?', '[', ']', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// dbRetryPartitionScanCount 是 SCAN 每页的 COUNT 提示(只影响往返次数,不影响结果完整性)。
const dbRetryPartitionScanCount = 1000

// queueKeyReader 是 dbTaskQueueKeys 需要的最小 Redis 能力(*redis.Client 天然满足,
// 调用点 runPreflight / auditKafkaQueues 不用改);单测用假实现注入 ZRANGE / SCAN 的回复。
type queueKeyReader interface {
	ZRange(ctx context.Context, key string, start, stop int64) *redis.StringSliceCmd
	Scan(ctx context.Context, cursor uint64, match string, count int64) *redis.ScanCmd
}

// dbTaskQueueKeysFor 列出"必须为空"的全部 db_task 队列键:ready(共享收件箱)、旧版共享 processing、死信,
// 再加登记表里每个实例各自的 processing,最后是按原始分区拆开的 ready 列表(partitionedKeys 排序去重后追加)。
// 纯函数,键名镜像由 merge_unit_test.go 钉住。
func dbTaskQueueKeysFor(topic string, instanceIDs, partitionedKeys []string) []string {
	keys := []string{dbRetryQueueKey(topic), dbRetryProcessingKey(topic), dbDeadQueueKey(topic)}
	for _, id := range instanceIDs {
		keys = append(keys, dbRetryInstanceProcessingKey(topic, id))
	}
	parts := append([]string(nil), partitionedKeys...) // 不改调用方的切片
	sort.Strings(parts)
	for i, k := range parts {
		if i > 0 && k == parts[i-1] {
			continue
		}
		keys = append(keys, k)
	}
	return keys
}

// dbTaskQueueKeys 读登记表、SCAN 按分区的 ready 列表,返回 dbTaskQueueKeysFor 的结果。
// 任一读不到即报错(fail-closed):不知道有哪些实例 / 哪些 p 列表,就不能宣称它们是空的。
func dbTaskQueueKeys(ctx context.Context, rdb queueKeyReader, topic string) ([]string, error) {
	instanceIDs, err := rdb.ZRange(ctx, dbRetryInstancesKey(topic), 0, -1).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("zrange %s: %w", dbRetryInstancesKey(topic), err)
	}
	partitioned, err := scanAllKeys(ctx, rdb, dbRetryPartitionQueuePattern(topic))
	if err != nil {
		return nil, err
	}
	return dbTaskQueueKeysFor(topic, instanceIDs, partitioned), nil
}

// scanAllKeys 用 SCAN MATCH 迭代到 cursor 回到 0,汇总全部页(页间可能重复,去重交给 dbTaskQueueKeysFor)。
// 任一页出错整体返回错误,不返回半截结果。
func scanAllKeys(ctx context.Context, rdb queueKeyReader, match string) ([]string, error) {
	var keys []string
	var cursor uint64
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("scan %s: %w", match, err)
		}
		page, next, err := rdb.Scan(ctx, cursor, match, dbRetryPartitionScanCount).Result()
		if err != nil {
			return nil, fmt.Errorf("scan %s (cursor %d): %w", match, cursor, err)
		}
		keys = append(keys, page...)
		if next == 0 {
			return keys, nil
		}
		cursor = next
	}
}

// ── preflight 主体 ────────────────────────────────────────────

// preflightDeps 是 preflight 需要的全部句柄。任何一个为 nil 的检查会被跳过
// **并报错** —— 「没句柄所以没查」不能等于「查过了没问题」。
type preflightDeps struct {
	// sceneRdb: scene_manager 的 Redis(scene_nodes:zone:{z}:load)。
	sceneRdb redis.UniversalClient
	// sceneNodes: etcd 场景节点注册表(scene_registry.go)。P2 的正面证据,nil 即拒绝。
	sceneNodes sceneRegistry
	// mappingRdb: data_service mapping Redis(lock:player:*)。
	mappingRdb *redis.Client
	// sharedRdb: 共享 DB 0(player:session:* / kafka:retry|dead:queue:*)。
	sharedRdb *redis.Client
	// lag: Kafka 积压来源,见上。
	lag           kafkaLagSource
	teamAuthority teamAuthorityReader
}

// preflightParams 是本次运行的参数面。
type preflightParams struct {
	// readOnly只表达预览。实际写仍需独立TOOL和NORMAL冻结原件。
	readOnly bool
	// zone 是写之前必须已经停干净的那个 zone:场景节点全下线、它的 db_task topic 无积压、重试 / 死信队列为空。
	// 合服是源区:源区玩家最后的存盘在源 topic 上,没落库就拷 = 拷走一份旧的。
	// 撤销是目标区:合服后这批玩家的存盘走目标 topic,没落库就改回路由、删目标行 = 把这些存盘丢掉。
	zone uint32
	// scope 只进日志与拒绝文案("source" / "target"),让运维一眼看出拦住的是哪一侧。
	scope string
	// kafkaGroups 是 P3 的候选消费组(kafkaGroupCandidates):-kafka-group 显式时只有它,
	// 否则是 db_rpc_consumer_group_z<zone> 加 legacy 组。P3 只对其中唯一的权威组算积压。
	kafkaGroups     []string
	topicGeneration uint32
	// playerIDs 是本次要动的玩家(合服 = 收集到的源区玩家;撤销 = 清单),P5~P7 逐个查。
	playerIDs []uint64
}

// runPreflight 依次跑 P2~P7(P1 在 MySQL 侧,由调用方先做)。任何一条不过就返回错误;
// 调用方此时已立围栏、还没写任何东西,走 mergeFence.refuseAndRelease(释放围栏再非零退出)。
func runPreflight(ctx context.Context, d preflightDeps, p preflightParams) (err error) {
	finish, err := enterMaintenanceWork(ctx)
	if err != nil {
		return err
	}
	returned := false
	defer func() {
		if !returned || err != nil {
			holdMaintenanceTool(ctx)
		}
		finish()
	}()
	if !p.readOnly {
		if normalFreezeFromContext(ctx) == nil {
			return errNormalFreezeProof
		}
		if err := checkMaintenanceTool(ctx); err != nil {
			return err
		}
	}
	// P2 那个 zone 的场景节点全下线。
	if d.sceneRdb == nil {
		return fmt.Errorf("preflight: no scene_manager Redis handle — cannot prove the %s zone %d is down", p.scope, p.zone)
	}
	if err := assertZoneDown(ctx, d.sceneRdb, d.sceneNodes, p.zone); err != nil {
		return fmt.Errorf("preflight P2 (%s zone %d): %w", p.scope, p.zone, err)
	}
	log.Printf("preflight P2 OK: %s zone %d has no registered scene node in etcd and an empty load set", p.scope, p.zone)

	// P3 Kafka 消费积压。
	if d.lag == nil {
		return errors.New("preflight P3: 缺少Kafka只读复核入口；人工声明不能替代已绑定的永久非SQL闭合原件")
	}
	if len(p.kafkaGroups) == 0 {
		return errors.New("preflight P3: no candidate consumer group (internal error: kafkaGroups is empty)")
	}
	topic := dbTaskTopic(p.zone, p.topicGeneration)
	lag, group, err := d.lag.TotalLag(ctx, topic, p.kafkaGroups)
	if err != nil {
		return fmt.Errorf("preflight P3 (%s, candidates %v): %w", d.lag.Describe(), p.kafkaGroups, err)
	}
	if lag != 0 {
		return fmt.Errorf("preflight P3: authoritative consumer group %q still has lag %d on topic %s — "+
			"the last saves on the %s zone's topic are not in zone_%d_db yet", group, lag, topic, p.scope, p.zone)
	}
	log.Printf("preflight P3 OK: authoritative group %s lag=0 on %s [%s]", group, topic, d.lag.Describe())

	// P4 retry / dead / processing 队列必须空。
	if d.sharedRdb == nil {
		return errors.New("preflight P4: no shared (DB 0) Redis handle — cannot inspect kafka retry/dead queues")
	}
	// 父快照可能已提交而Kafka中尚无消息，LAG=0和retry为空都不能证明这一窗口已排空。
	// scene已停写；仍由DB补投工作器完成双ACK回收，不在维护工具里直接清索引。
	if err := assertSaveOutboxDrained(ctx, d.sharedRdb, p.zone); err != nil {
		return fmt.Errorf("preflight P4 (save outbox): %w", err)
	}
	queueKeys, err := dbTaskQueueKeys(ctx, d.sharedRdb, topic)
	if err != nil {
		return fmt.Errorf("preflight P4: %w", err)
	}
	for _, k := range queueKeys {
		n, err := d.sharedRdb.LLen(ctx, k).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return fmt.Errorf("preflight P4: llen %s: %w", k, err)
		}
		if n > 0 {
			return fmt.Errorf("preflight P4: %s holds %d unprocessed db_task payload(s) — "+
				"those writes never reached zone_%d_db. Drain or triage them before touching these players", k, n, p.zone)
		}
	}
	log.Printf("preflight P4 OK: kafka retry/processing/dead queues for %s are empty (%d keys incl. per-instance processing lists and partitioned ready lists)", topic, len(queueKeys))

	// P5 玩家锁。lock:player:{id} 在 mapping Redis(data_service router.go),
	// player:{id}:__lock 在 data Redis —— 后者的地址由 -source-data-redis 决定,
	// 不一定配了,所以这里只硬查 mapping 侧,data 侧在 audit 里覆盖。
	if d.mappingRdb == nil {
		return errors.New("preflight P5: no mapping Redis handle")
	}
	locked, err := countExistingKeys(ctx, d.mappingRdb, p.playerIDs, "lock:player:")
	if err != nil {
		return fmt.Errorf("preflight P5: %w", err)
	}
	if locked > 0 {
		return fmt.Errorf("preflight P5: %d of the players in scope still hold lock:player:* in the mapping Redis — "+
			"someone is mid-write on them", locked)
	}
	log.Printf("preflight P5 OK: no lock:player:* held by the %d players in scope", len(p.playerIDs))

	// P6 在线会话。player:session:{id} 由 player_locator 维护,DB 0。
	sessions, err := countExistingKeys(ctx, d.sharedRdb, p.playerIDs, "player:session:")
	if err != nil {
		return fmt.Errorf("preflight P6: %w", err)
	}
	if sessions > 0 {
		return fmt.Errorf("preflight P6: %d of the players in scope still have player:session:* — zone-down is incomplete", sessions)
	}
	log.Printf("preflight P6 OK: no player:session:* for the %d players in scope", len(p.playerIDs))

	// P7全局SQL成员原件。合区已持全部64片围栏，Team提交前缀持对应片，不能在预检后插入新成员。
	if d.teamAuthority == nil {
		return errors.New("preflight P7: 缺少全球Team SQL权威读者")
	}
	inTeam, err := d.teamAuthority.PlayersInTeams(ctx, p.playerIDs)
	if err != nil {
		return fmt.Errorf("preflight P7: %w", err)
	}
	if inTeam > 0 {
		return fmt.Errorf("preflight P7: %d名玩家仍有SQL队伍成员关系，必须先安全离队或解散；Redis缺失不能解除此门禁", inTeam)
	}
	log.Printf("preflight P7 OK: none of the %d players in scope is in a live team", len(p.playerIDs))
	returned = true
	return nil
}

// countExistingKeys 数 prefix+id 里有多少存在。按 pipeline 分批,不用 SCAN:
// 我们要查的是**确定的一组 id**,SCAN 只会更慢且可能漏。
func countExistingKeys(ctx context.Context, rdb *redis.Client, ids []uint64, prefix string) (int, error) {
	if rdb == nil {
		return 0, errors.New("nil redis handle")
	}
	found := 0
	for _, batch := range chunkUint64(ids, 500) {
		pipe := rdb.Pipeline()
		cmds := make([]*redis.IntCmd, 0, len(batch))
		for _, id := range batch {
			cmds = append(cmds, pipe.Exists(ctx, prefix+strconv.FormatUint(id, 10)))
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return found, fmt.Errorf("exists %s*: %w", prefix, err)
		}
		for _, c := range cmds {
			if err := c.Err(); err != nil {
				holdMaintenanceTool(ctx)
				return found, fmt.Errorf("exists %s*: %w", prefix, err)
			}
			if c.Val() > 0 {
				found++
			}
		}
	}
	return found, nil
}

func truncateForLog(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
