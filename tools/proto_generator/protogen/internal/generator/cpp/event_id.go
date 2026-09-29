package cpp

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"protogen/internal"
	_config "protogen/internal/config"
	utils2 "protogen/internal/utils"
	"protogen/logger"

	"go.uber.org/zap"
)

type ProtoEventInfo struct {
	Id            uint64
	IdName        string
	MessageName   string
	QualifiedName string
	ProtoInclude  string
}

// 墓碑行格式与语义见 internal.ReservedEventIdPrefix(turn-based §22 D67)。

const (
	// maxNewTombstonesPerRun 是一轮生成里允许新转墓碑的事件数上限。墓碑永久且不可逆,
	// 正常删事件一次只删一两个(D67 删 Bind/UnbindBattleEvent 是 2 个);一次冒出一大批,
	// 几乎总是本轮 proto 输入不完整(事件目录配空、指错到空目录),照写会让大批活号永久作废、
	// 同名事件全部换号。超限即中止,确实要批量删时显式设 allowMassEventTombstoneEnv=1 放行。
	maxNewTombstonesPerRun = 4

	// allowMassEventTombstoneEnv 只认 "1";放行后仍逐条打 WARN,提交 event_id.txt 前要人工核对。
	allowMassEventTombstoneEnv = "PROTOGEN_ALLOW_MASS_EVENT_TOMBSTONE"

	// kafkaEventIdNamePrefix 是 Kafka 契约事件的 IdName 前缀,即 buildEventIdName 对
	// `package contracts.kafka;` 的拼法。contracts_kafka 目录没配时,event_id.txt 里
	// 带这个前缀的活事件本轮必然"消失",据此判定输入不完整。
	kafkaEventIdNamePrefix = "ContractsKafka"
)

var (
	globalProtoEventList []*ProtoEventInfo
	// event_id.txt 的内存视图,由 ReadEventIdFile 填。
	loadedEventIdFile = newEventIdFileState()
	// 以下由 InitEventId 填:本轮活事件(号 → 事件)、全部墓碑(文件原有 + 本轮新转,号 → 原 IdName)、
	// 发号后的最大号(含墓碑,决定 C++ 注册表的数组长度)。
	eventIdInfoMap     = map[uint64]*ProtoEventInfo{}
	reservedEventIdMap = map[uint64]string{}
	eventFileMaxId     uint64
)

// eventIdFileEntry 是 event_id.txt 的一行:活事件 `N=<IdName>`,或墓碑 `N=reserved:<原 IdName>`。
type eventIdFileEntry struct {
	Id       uint64
	Name     string // 活事件为 IdName;墓碑为被删事件的原 IdName
	Reserved bool
}

// parseEventIdLine 解析 event_id.txt 的一行(纯函数)。
// ok=false:不是 `N=...` 的形状,调用方告警后跳过(与改前的处置一致);
// err 非空:号不是十进制整数,调用方应中止生成。
func parseEventIdLine(line string) (entry eventIdFileEntry, ok bool, err error) {
	parts := strings.Split(line, "=")
	if len(parts) != 2 {
		return eventIdFileEntry{}, false, nil
	}

	id, err := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil {
		return eventIdFileEntry{}, false, err
	}

	name := strings.TrimSpace(parts[1])
	if originalName, isReserved := strings.CutPrefix(name, internal.ReservedEventIdPrefix); isReserved {
		return eventIdFileEntry{Id: id, Name: strings.TrimSpace(originalName), Reserved: true}, true, nil
	}
	return eventIdFileEntry{Id: id, Name: name}, true, nil
}

// eventIdFileState 是 event_id.txt 的内存视图。
type eventIdFileState struct {
	Active   map[string]uint64 // 活事件:IdName → 号
	Reserved map[uint64]string // 墓碑:号 → 被删事件的原 IdName
	MaxId    uint64            // 全部行(含墓碑)里的最大号
}

func newEventIdFileState() *eventIdFileState {
	return &eventIdFileState{
		Active:   map[string]uint64{},
		Reserved: map[uint64]string{},
	}
}

// add 登记一行。同一个号出现两次(包括一个号既是活号又是墓碑),或同一个活事件名出现两次,
// 都说明文件被手改坏了:照旧生成会让两个事件共用一个号,或让某个号在下一轮被当成空洞回收。
// 返回错误,由调用方中止生成(fail-closed)。
func (s *eventIdFileState) add(entry eventIdFileEntry) error {
	if originalName, dup := s.Reserved[entry.Id]; dup {
		return fmt.Errorf("event id %d listed twice: already reserved for %q", entry.Id, originalName)
	}
	for name, id := range s.Active {
		if id == entry.Id {
			return fmt.Errorf("event id %d listed twice: already used by %q", entry.Id, name)
		}
	}

	if entry.Reserved {
		s.Reserved[entry.Id] = entry.Name
	} else {
		if prevId, dup := s.Active[entry.Name]; dup {
			return fmt.Errorf("event %q listed twice: ids %d and %d", entry.Name, prevId, entry.Id)
		}
		s.Active[entry.Name] = entry.Id
	}

	if s.MaxId < entry.Id {
		s.MaxId = entry.Id
	}
	return nil
}

func buildEventIdName(packageName, messageName string) string {
	parts := make([]string, 0)
	for _, part := range strings.Split(packageName, ".") {
		if part == "" {
			continue
		}
		parts = append(parts, strings.ToUpper(part[:1])+part[1:])
	}
	parts = append(parts, messageName)
	return strings.Join(parts, "")
}

func registerProtoEvents(protoRelativeDir string, files []os.DirEntry) {
	for _, file := range files {
		protoFilePath := filepath.Join(_config.Global.Paths.ProtoDir, protoRelativeDir, file.Name())
		messages, err := parseProtoMessages(protoFilePath)
		if err != nil {
			logger.Global.Fatal("Failed to parse event proto file",
				zap.String("proto_file", protoFilePath),
				zap.Error(err),
			)
		}

		packageName, err := parseProtoPackage(protoFilePath)
		if err != nil {
			logger.Global.Fatal("Failed to parse event proto package",
				zap.String("proto_file", protoFilePath),
				zap.Error(err),
			)
		}

		protoInclude := filepath.ToSlash(filepath.Join(
			_config.Global.DirectoryNames.ProtoDirName,
			protoRelativeDir,
			strings.Replace(file.Name(), _config.Global.FileExtensions.Proto, _config.Global.FileExtensions.PbH, 1),
		))

		for _, messageName := range messages {
			idName := buildEventIdName(packageName, messageName)
			for _, existing := range globalProtoEventList {
				if existing.IdName == idName {
					logger.Global.Fatal("Duplicate event ID name",
						zap.String("event_name", idName),
						zap.String("qualified_name", qualifyProtoType(packageName, messageName)),
					)
				}
			}

			globalProtoEventList = append(globalProtoEventList, &ProtoEventInfo{
				Id:            math.MaxUint64,
				IdName:        idName,
				MessageName:   messageName,
				QualifiedName: qualifyProtoType(packageName, messageName),
				ProtoInclude:  protoInclude,
			})
		}
	}
}

func ReadAllProtoEvents(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()

		logicEventFiles, err := readProtoFiles(_config.Global.PathLists.ProtoDirs.LogicEvent)
		if err != nil {
			logger.Global.Fatal("Failed to read logic event proto directory",
				zap.String("dir", _config.Global.PathLists.ProtoDirs.LogicEvent),
				zap.Error(err),
			)
		}
		registerProtoEvents(_config.Global.PathLists.ProtoDirs.LogicEvent, filterProtoFiles(logicEventFiles))

		// contracts_kafka 没配时这里跳过;若 event_id.txt 里还有 Kafka 活事件,
		// InitEventId 的 checkEventIdInputComplete 会中止,不会把它们转成墓碑。
		if _config.Global.PathLists.ProtoDirs.ContractsKafka != "" {
			kafkaEventFiles, err := readProtoFiles(_config.Global.PathLists.ProtoDirs.ContractsKafka)
			if err != nil {
				// 以前是告警后跳过。有了墓碑之后跳过就是破坏性的:本轮 proto 里看不到任何 Kafka 事件,
				// InitEventId 会把 event_id.txt 里全部 Kafka 事件号永久转成墓碑、再给它们发新号。
				// 读不到目录 = 输入不完整,直接中止(fail-closed)。
				logger.Global.Fatal("Failed to read Kafka event proto directory",
					zap.String("dir", _config.Global.PathLists.ProtoDirs.ContractsKafka),
					zap.Error(err),
				)
			}
			registerProtoEvents(_config.Global.PathLists.ProtoDirs.ContractsKafka, filterProtoFilesBySuffix(kafkaEventFiles, "_event.proto"))
		}

		sort.Slice(globalProtoEventList, func(i, j int) bool {
			return globalProtoEventList[i].IdName < globalProtoEventList[j].IdName
		})
	}()
}

func ReadEventIdFile(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()

		file, err := os.Open(_config.Global.Paths.EventIdFile)
		if err != nil {
			if os.IsNotExist(err) {
				return
			}
			logger.Global.Fatal("Failed to open event ID file",
				zap.String("file_path", _config.Global.Paths.EventIdFile),
				zap.Error(err),
			)
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := scanner.Text()
			entry, ok, parseErr := parseEventIdLine(line)
			if parseErr != nil {
				logger.Global.Fatal("Failed to parse event ID",
					zap.String("line", line),
					zap.Error(parseErr),
				)
			}
			if !ok {
				logger.Global.Warn("Invalid line in event ID file, skipping",
					zap.String("line", line),
				)
				continue
			}

			if addErr := loadedEventIdFile.add(entry); addErr != nil {
				logger.Global.Fatal("Corrupted event ID file",
					zap.String("file_path", _config.Global.Paths.EventIdFile),
					zap.String("line", line),
					zap.Error(addErr),
				)
			}
		}

		if err = scanner.Err(); err != nil {
			logger.Global.Fatal("Failed to scan event ID file",
				zap.String("file_path", _config.Global.Paths.EventIdFile),
				zap.Error(err),
			)
		}
	}()
}

// eventIdAssignment 是 assignEventIds 的结果。
type eventIdAssignment struct {
	Infos         map[uint64]*ProtoEventInfo // 本轮活事件:号 → 事件
	Reserved      map[uint64]string          // 全部墓碑(文件原有 + 本轮新转):号 → 原 IdName
	MaxId         uint64                     // 发号后的最大号(含墓碑)
	NewlyReserved []uint64                   // 本轮新转墓碑的号,升序,供调用方打日志
}

// assignEventIds 按 event_id.txt 给本轮 proto 里的事件定号。不读写包级状态、不打日志;
// 唯一的副作用是写 events 里每个事件的 Id(与改前的 InitEventId 一致)。
//
//  1. 文件里已登记的事件沿用原号;
//  2. 文件里的活号在本轮 proto 里找不到对应事件(消息被删,或改名 —— 改名等同删旧加新)
//     → 转墓碑,号永久保留;
//  3. 新事件先按 events 的顺序填 0..MaxId 中既非活号也非墓碑的空洞,填完再从 MaxId+1 往后发。
//     墓碑号无论新旧一律跳过。与墓碑同名的事件重新出现时同样按新事件发新号,墓碑不动。
func assignEventIds(events []*ProtoEventInfo, file *eventIdFileState) eventIdAssignment {
	result := eventIdAssignment{
		Infos:    make(map[uint64]*ProtoEventInfo, len(events)),
		Reserved: make(map[uint64]string, len(file.Reserved)),
		MaxId:    file.MaxId,
	}
	for id, originalName := range file.Reserved {
		result.Reserved[id] = originalName
	}

	liveNames := make(map[string]struct{}, len(events))
	for _, event := range events {
		liveNames[event.IdName] = struct{}{}
		if id, known := file.Active[event.IdName]; known {
			event.Id = id
			result.Infos[id] = event
		} else {
			event.Id = math.MaxUint64
		}
	}

	for name, id := range file.Active {
		if _, live := liveNames[name]; live {
			continue
		}
		result.Reserved[id] = name
		result.NewlyReserved = append(result.NewlyReserved, id)
	}
	sort.Slice(result.NewlyReserved, func(i, j int) bool { return result.NewlyReserved[i] < result.NewlyReserved[j] })

	holes := make([]uint64, 0)
	for id := uint64(0); id <= file.MaxId; id++ {
		if _, used := result.Infos[id]; used {
			continue
		}
		if _, reserved := result.Reserved[id]; reserved {
			continue
		}
		holes = append(holes, id)
	}

	nextHole := 0
	for _, event := range events {
		if event.Id != math.MaxUint64 {
			continue
		}
		if nextHole < len(holes) {
			event.Id = holes[nextHole]
			nextHole++
		} else {
			result.MaxId++
			event.Id = result.MaxId
		}
		result.Infos[event.Id] = event
	}
	return result
}

// checkEventIdInputComplete 判定本轮 proto 输入是否完整到可以落墓碑(纯函数),
// 在 assignEventIds 之后、WriteEventIdFile 之前调用。墓碑写进 event_id.txt 就永久生效,
// 所以输入可疑时宁可中止也不猜(fail-closed):
//  1. contracts_kafka 目录没配,而 event_id.txt 里还有 Kafka 活事件 → 本轮根本没读 Kafka proto,
//     不受 allowMassTombstone 放行;
//  2. 新转墓碑超过 maxNewTombstonesPerRun 且未显式放行 → 多半是事件目录配空或指错到空目录。
//
// 返回 nil 表示可以继续;错误信息列出涉及的 `号=名`,供排障。
func checkEventIdInputComplete(file *eventIdFileState, result eventIdAssignment, kafkaDirConfigured, allowMassTombstone bool) error {
	if !kafkaDirConfigured {
		kafkaNames := make(map[uint64]string)
		kafkaIds := make([]uint64, 0)
		for name, id := range file.Active {
			if strings.HasPrefix(name, kafkaEventIdNamePrefix) {
				kafkaNames[id] = name
				kafkaIds = append(kafkaIds, id)
			}
		}
		if len(kafkaIds) > 0 {
			sort.Slice(kafkaIds, func(i, j int) bool { return kafkaIds[i] < kafkaIds[j] })
			return fmt.Errorf("proto_dirs.contracts_kafka is not configured, but the event ID file still lists %d live Kafka events (%s); "+
				"they would all be tombstoned", len(kafkaIds), formatEventIds(kafkaIds, kafkaNames))
		}
	}

	if len(result.NewlyReserved) > maxNewTombstonesPerRun && !allowMassTombstone {
		return fmt.Errorf("%d events would be tombstoned in one run, limit is %d (%s); "+
			"check proto_dirs.logic_event / contracts_kafka, or set %s=1 if they were really removed",
			len(result.NewlyReserved), maxNewTombstonesPerRun, formatEventIds(result.NewlyReserved, result.Reserved), allowMassEventTombstoneEnv)
	}
	return nil
}

// formatEventIds 把号按入参顺序拼成 `号=名, 号=名`。
func formatEventIds(ids []uint64, names map[uint64]string) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%d=%s", id, names[id]))
	}
	return strings.Join(parts, ", ")
}

func InitEventId() {
	result := assignEventIds(globalProtoEventList, loadedEventIdFile)

	kafkaDirConfigured := _config.Global.PathLists.ProtoDirs.ContractsKafka != ""
	allowMassTombstone := os.Getenv(allowMassEventTombstoneEnv) == "1"
	if err := checkEventIdInputComplete(loadedEventIdFile, result, kafkaDirConfigured, allowMassTombstone); err != nil {
		// 此时 event_id.txt 还没写,中止即不落任何墓碑。
		logger.Global.Fatal("Event proto input looks incomplete; refusing to tombstone event IDs",
			zap.String("event_id_file", _config.Global.Paths.EventIdFile),
			zap.Error(err),
		)
	}

	eventIdInfoMap = result.Infos
	reservedEventIdMap = result.Reserved
	eventFileMaxId = result.MaxId

	// WARN 而不是 INFO:删事件时这是预期输出,要在提交 event_id.txt 之前被看见;
	// 批量的情况已由 checkEventIdInputComplete 拦下或经显式放行。
	for _, id := range result.NewlyReserved {
		logger.Global.Warn("Event no longer in proto; its ID is now reserved and will never be reused",
			zap.Uint64("event_id", id),
			zap.String("event_name", result.Reserved[id]),
		)
	}
}

// renderEventIdFile 按号升序输出活事件与墓碑(纯函数);墓碑写成 `N=reserved:<原 IdName>`,
// 读回再写出逐字节不变,下一轮生成仍能认出它。
func renderEventIdFile(infos map[uint64]*ProtoEventInfo, reserved map[uint64]string) string {
	ids := make([]uint64, 0, len(infos)+len(reserved))
	for id := range infos {
		ids = append(ids, id)
	}
	for id := range reserved {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var builder strings.Builder
	for _, id := range ids {
		if event, live := infos[id]; live {
			builder.WriteString(fmt.Sprintf("%d=%s\n", id, event.IdName))
			continue
		}
		builder.WriteString(fmt.Sprintf("%d=%s%s\n", id, internal.ReservedEventIdPrefix, reserved[id]))
	}
	return builder.String()
}

func WriteEventIdFile() {
	utils2.WriteFileIfChanged(_config.Global.Paths.EventIdFile, []byte(renderEventIdFile(eventIdInfoMap, reservedEventIdMap)))
}

// EventIdLen 是 C++ 注册表(kMaxEventCount / IsValidEventId)的长度:最大号 + 1,墓碑号也算在内。
// 墓碑号不生成分派 case,旧节点发来的墓碑号消息落到 DispatchProtoEvent 的 default 分支被丢弃。
func EventIdLen() uint64 {
	if len(eventIdInfoMap) == 0 {
		return 0
	}
	return eventFileMaxId + 1
}

// eventIdHeaderFileName derives a unique header file name from a ProtoInclude path.
// e.g. "proto/common/event/scene_event.pb.h" -> "common_event_scene_event_event_id.h"
// e.g. "proto/contracts/kafka/player_event.pb.h" -> "contracts_kafka_player_event_event_id.h"
func eventIdHeaderFileName(protoInclude string) string {
	// Strip leading "proto/" prefix
	name := strings.TrimPrefix(protoInclude, _config.Global.DirectoryNames.ProtoDirName)
	// Strip ".pb.h" suffix
	name = strings.TrimSuffix(name, ".pb.h")
	// Replace path separators with underscores
	name = strings.ReplaceAll(name, "/", "_")
	return name + "_event_id.h"
}

// writeEventIdHeaderFiles writes per-proto-file event ID header files,
// mirroring the pattern used by per-service message ID headers.
func writeEventIdHeaderFiles() {
	// Group events by their proto include path
	groups := make(map[string][]*ProtoEventInfo)
	var groupOrder []string
	for _, event := range globalProtoEventList {
		if _, seen := groups[event.ProtoInclude]; !seen {
			groupOrder = append(groupOrder, event.ProtoInclude)
		}
		groups[event.ProtoInclude] = append(groups[event.ProtoInclude], event)
	}

	for _, protoInclude := range groupOrder {
		events := groups[protoInclude]
		headerFileName := eventIdHeaderFileName(protoInclude)

		var builder strings.Builder
		builder.WriteString("#pragma once\n#include <cstdint>\n\n")
		for _, event := range events {
			builder.WriteString(fmt.Sprintf("constexpr uint32_t %s%s = %d;\n",
				event.IdName, _config.Global.Naming.EventId, event.Id))
		}

		outputPath := _config.Global.Paths.ServiceInfoDir + headerFileName
		utils2.WriteFileIfChanged(outputPath, []byte(builder.String()))
	}

	logger.Global.Info("Event ID header files generated",
		zap.Int("file_count", len(groups)),
		zap.Int("event_count", len(globalProtoEventList)),
	)
}

// EventIdHeaderIncludes returns the #include lines for all per-proto-file event ID headers.
func EventIdHeaderIncludes() []string {
	seen := make(map[string]struct{})
	var includes []string
	for _, event := range globalProtoEventList {
		if _, ok := seen[event.ProtoInclude]; ok {
			continue
		}
		seen[event.ProtoInclude] = struct{}{}
		includes = append(includes, fmt.Sprintf("#include \"%s\"", eventIdHeaderFileName(event.ProtoInclude)))
	}
	return includes
}
