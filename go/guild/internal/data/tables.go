// Package data 的库名与表清单:mmorpg_guild 独占库(port-decisions D-14,§8 已修订)。
// 表结构唯一事实源是 proto/guild/guild_db.proto;建表 / 加列只经 go/schemamigrate,本包不写 DDL。
package data

import (
	pb "proto/guild"

	"google.golang.org/protobuf/proto"
)

// DatabaseName 是帮会独占的逻辑库名,本地与 K8s 同名。config.Validate 断言 DSN 库名等于它,
// schemamigrate 连接后再断言 SELECT DATABASE() 等于它;tools/merge_zone 的 defaultGuildSchema 镜像本常量。
const DatabaseName = "mmorpg_guild"

// Tables 返回本库全部表,顺序即 docs/design/guild-phase2/01-storage.md §2.1 的加锁位置。
// 新增表 = guild_db.proto 加 message 并在这里追加(schemamigrate 会补建后加的表),
// 同时追加 guild_repo_test.go 的 guildTestDropTables。
func Tables() []proto.Message {
	return []proto.Message{
		&pb.GuildRecord{},
		&pb.GuildPlayerStateRecord{},
		&pb.GuildMemberRecord{},
		&pb.GuildApplicationRecord{},
	}
}
