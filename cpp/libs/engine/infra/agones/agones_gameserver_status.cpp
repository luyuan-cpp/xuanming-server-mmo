#include "agones/agones_gameserver_status.h"

#include <cmath>
#include <map>
#include <string>
#include <utility>

#include "google/protobuf/struct.pb.h"
#include "google/protobuf/util/json_util.h"

namespace agones
{

	namespace
	{
		using google::protobuf::Struct;
		using google::protobuf::Value;

		constexpr std::uint32_t kMaxPort = 65535;

		// 取对象里的字段;不存在或为 null 都返回 nullptr(null 视同缺省)。
		const Value* FindField(const Struct& object, const std::string& name)
		{
			const auto it = object.fields().find(name);
			if (it == object.fields().end() || it->second.kind_case() == Value::kNullValue)
			{
				return nullptr;
			}
			return &it->second;
		}

		// 同一字段的两种键名(proto 原名 / 驼峰),前者优先。
		const Value* FindFieldEither(const Struct& object, const std::string& protoName, const std::string& jsonName)
		{
			if (const Value* value = FindField(object, protoName))
			{
				return value;
			}
			return FindField(object, jsonName);
		}

		// value 为空(字段缺省)时输出空串并视为成功。
		bool ReadOptionalString(const Value* value, const std::string& path, std::string& out, std::string& error)
		{
			if (value == nullptr)
			{
				out.clear();
				return true;
			}
			if (value->kind_case() != Value::kStringValue)
			{
				error = path + " is not a string";
				return false;
			}
			out = value->string_value();
			return true;
		}

		// port 在 Agones proto 里是 int32,protojson 输出为 JSON 数字;
		// 数字串一并接受(grpc-gateway 对 64 位整数输出字符串,防将来字段变宽)。
		bool ReadPortNumber(const Value& value, std::uint32_t& out)
		{
			if (value.kind_case() == Value::kNumberValue)
			{
				const double number = value.number_value();
				if (!(number >= 0.0 && number <= static_cast<double>(kMaxPort)) || number != std::floor(number))
				{
					return false;
				}
				out = static_cast<std::uint32_t>(number);
				return true;
			}
			if (value.kind_case() == Value::kStringValue)
			{
				const std::string& text = value.string_value();
				if (text.empty() || text.size() > 5 || text.find_first_not_of("0123456789") != std::string::npos)
				{
					return false;
				}
				const unsigned long number = std::stoul(text);
				if (number > kMaxPort)
				{
					return false;
				}
				out = static_cast<std::uint32_t>(number);
				return true;
			}
			return false;
		}

		// JSON 文本 -> Struct。顶层不是对象(如数组)时 JsonStringToMessage 同样失败。
		bool ParseRoot(const std::string& json, Struct& root, std::string& error)
		{
			const auto parsed = google::protobuf::util::JsonStringToMessage(json, &root);
			if (!parsed.ok())
			{
				error = "gameserver json parse failed: " + std::string(parsed.message());
				return false;
			}
			return true;
		}

		const Value* FindMetadata(const Struct& root)
		{
			return FindFieldEither(root, "object_meta", "objectMeta");
		}

		bool ParseLabels(const Value& meta, std::map<std::string, std::string>& out, std::string& error)
		{
			if (meta.kind_case() != Value::kStructValue)
			{
				error = "object_meta is not an object";
				return false;
			}
			const Value* labels = FindField(meta.struct_value(), "labels");
			if (labels == nullptr)
			{
				return true;
			}
			if (labels->kind_case() != Value::kStructValue)
			{
				error = "object_meta.labels is not an object";
				return false;
			}
			for (const auto& entry : labels->struct_value().fields())
			{
				if (entry.second.kind_case() != Value::kStringValue)
				{
					error = "object_meta.labels[" + entry.first + "] is not a string";
					return false;
				}
				out[entry.first] = entry.second.string_value();
			}
			return true;
		}

		bool ParsePorts(const Value& ports, GameServerView& view, std::string& error)
		{
			if (ports.kind_case() != Value::kListValue)
			{
				error = "status.ports is not a list";
				return false;
			}
			for (const Value& entry : ports.list_value().values())
			{
				if (entry.kind_case() != Value::kStructValue)
				{
					error = "status.ports[] entry is not an object";
					return false;
				}
				std::string name;
				if (!ReadOptionalString(FindField(entry.struct_value(), "name"), "status.ports[].name", name, error))
				{
					return false;
				}
				const Value* port = FindField(entry.struct_value(), "port");
				std::uint32_t number = 0;
				if (port == nullptr || !ReadPortNumber(*port, number))
				{
					error = "status.ports[" + name + "].port is missing or not an integer in 0..65535";
					return false;
				}
				view.ports[name] = number;
			}
			return true;
		}

		bool ParseStatus(const Value& status, GameServerView& view, std::string& error)
		{
			if (status.kind_case() != Value::kStructValue)
			{
				error = "status is not an object";
				return false;
			}
			const Struct& object = status.struct_value();
			if (!ReadOptionalString(FindField(object, "state"), "status.state", view.state, error))
			{
				return false;
			}
			if (!ReadOptionalString(FindField(object, "address"), "status.address", view.address, error))
			{
				return false;
			}
			if (const Value* ports = FindField(object, "ports"))
			{
				return ParsePorts(*ports, view, error);
			}
			return true;
		}
	} // namespace

	bool ParseGameServer(const std::string& json, GameServerView& view, std::string& error)
	{
		Struct root;
		if (!ParseRoot(json, root, error))
		{
			return false;
		}

		GameServerView result;
		if (const Value* meta = FindMetadata(root))
		{
			if (!ParseLabels(*meta, result.labels, error))
			{
				return false;
			}
		}
		if (const Value* status = FindField(root, "status"))
		{
			if (!ParseStatus(*status, result, error))
			{
				return false;
			}
		}

		view = std::move(result);
		return true;
	}

	bool ParseGameServerLabels(const std::string& json, std::map<std::string, std::string>& labels, std::string& error)
	{
		Struct root;
		if (!ParseRoot(json, root, error))
		{
			return false;
		}

		const Value* meta = FindMetadata(root);
		if (meta == nullptr)
		{
			error = "gameserver json has no object_meta/objectMeta";
			return false;
		}

		std::map<std::string, std::string> result;
		if (!ParseLabels(*meta, result, error))
		{
			return false;
		}

		labels = std::move(result);
		return true;
	}

} // namespace agones
