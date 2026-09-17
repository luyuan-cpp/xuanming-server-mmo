#include "data/battle_table_fingerprint.h"

#include <cstdint>
#include <string>

#include "google/protobuf/io/coded_stream.h"
#include "google/protobuf/io/zero_copy_stream_impl_lite.h"
#include "google/protobuf/message.h"

#include "core/utils/encode/sha256.h"

#include "table/code/skill_table.h"
#include "table/code/buff_table.h"
#include "table/code/cooldown_table.h"
#include "table/code/skillpermission_table.h"
#include "table/code/dungeon_table.h"
#include "table/code/monster_table.h"

#include "table/proto/skill_table.pb.h"
#include "table/proto/buff_table.pb.h"
#include "table/proto/cooldown_table.pb.h"
#include "table/proto/skillpermission_table.pb.h"
#include "table/proto/dungeon_table.pb.h"
#include "table/proto/monster_table.pb.h"

namespace turnbattle
{

	namespace
	{
		// 追加一段:表名 + 段长 + 确定性序列化字节。
		// 表名/段长前缀让各段边界固定,避免相邻两表内容互相"借位"产生相同拼接。
		void AppendTableSection(std::string& out, const char* tableName,
								const google::protobuf::Message& table)
		{
			std::string bytes;
			{
				google::protobuf::io::StringOutputStream sink(&bytes);
				google::protobuf::io::CodedOutputStream coded(&sink);
				// 关键:map 字段按 key 排序输出,否则同一份表两次加载的 map 迭代序可能不同
				coded.SetSerializationDeterministic(true);
				// 表对象来自已解析成功的快照,序列化不会失败;万一失败也不能静默产出半截指纹
				if (!table.SerializeToCodedStream(&coded))
				{
					bytes.clear();
				}
			}
			out.append(tableName);
			out.push_back('\0');
			const uint64_t size = static_cast<uint64_t>(bytes.size());
			for (int shift = 56; shift >= 0; shift -= 8)
			{
				out.push_back(static_cast<char>((size >> shift) & 0xFF));
			}
			out.append(bytes);
		}

		std::string& CachedFingerprint()
		{
			static std::string cached;
			return cached;
		}
	} // namespace

	std::string BattleTableFingerprint::ComputeFrom(const SkillTableData& skill,
													 const BuffTableData& buff,
													 const CooldownTableData& cooldown,
													 const SkillPermissionTableData& skillPermission,
													 const DungeonTableData& dungeon,
													 const MonsterTableData& monster)
	{
		// 表序固定:改这里的顺序 = 改指纹契约,scene/battle 两端必须同版本发布
		std::string buffer;
		AppendTableSection(buffer, "skill", skill);
		AppendTableSection(buffer, "buff", buff);
		AppendTableSection(buffer, "cooldown", cooldown);
		AppendTableSection(buffer, "skillpermission", skillPermission);
		AppendTableSection(buffer, "dungeon", dungeon);
		AppendTableSection(buffer, "monster", monster);

		return Sha256::HashToHex(buffer).substr(0, kHexLength);
	}

	std::string BattleTableFingerprint::Compute()
	{
		return ComputeFrom(SkillTableManager::Instance().FindAll(),
						   BuffTableManager::Instance().FindAll(),
						   CooldownTableManager::Instance().FindAll(),
						   SkillPermissionTableManager::Instance().FindAll(),
						   DungeonTableManager::Instance().FindAll(),
						   MonsterTableManager::Instance().FindAll());
	}

	const std::string& BattleTableFingerprint::Current()
	{
		auto& cached = CachedFingerprint();
		if (cached.empty())
		{
			cached = Compute();
		}
		return cached;
	}

	const std::string& BattleTableFingerprint::Refresh()
	{
		auto& cached = CachedFingerprint();
		cached = Compute();
		return cached;
	}

} // namespace turnbattle
