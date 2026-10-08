#pragma once

#include <cstdint>
#include <map>
#include <string>

// Agones SDK sidecar `GET /gameserver` 响应的只读视图。
//
// 只取本仓库用得到的四样东西:status.state、status.address、status.ports(name -> port)、
// 元数据 labels。解析走 google::protobuf::Struct(JSON 的通用表示),不照抄 Agones
// 的 GameServer proto 写一份并行结构体 —— Agones 的字段随版本增减,我们只认这几个键。
//
// 兼容两种键名写法:Agones sidecar 的 REST 网关按 proto 原名输出(object_meta),
// 标准 protojson 按驼峰输出(objectMeta);两种都接受,object_meta 优先。
// 值为 null 的字段视同缺省(EmitUnpopulated 会把未设置的子消息输出成 null)。
//
// 实测 JSON 形状以集群外入口验收阶段 B 的 curl 结果为准(k8s-client-entry.md)。

namespace agones
{

	struct GameServerView
	{
		// status.state,如 "Ready" / "Allocated" / "Shutdown";缺省为空。
		std::string state;
		// status.address:Agones 控制器按 ExternalDNS > ExternalIP > InternalDNS > InternalIP
		// 的优先级为本 GameServer 选好的节点地址;缺省为空。
		std::string address;
		// status.ports[].name -> port(Dynamic 端口即 Agones 分配的 hostPort)。
		std::map<std::string, std::uint32_t> ports;
		// object_meta.labels。排空标签(mmorpg.io/drain)从这里读。
		std::map<std::string, std::string> labels;
	};

	// 解析 GET /gameserver 的响应体。
	//   - 成功返回 true 并整体覆盖 view;字段缺失不算错(state / address 为空、ports / labels 为空)。
	//     "地址为空 / 没有某个端口能不能用"由调用方按自己的语义判定。
	//   - JSON 格式错误、顶层不是对象、已知字段类型不符(如 labels 不是对象、port 不是 0..65535
	//     的整数)返回 false,error 写原因,view 保持调用前的内容不变。
	// 纯函数,无 I/O,可在任意线程调用。
	bool ParseGameServer(const std::string& json, GameServerView& view, std::string& error);

	// 只解析元数据段的 labels,供排空轮询(D83)使用。与 ParseGameServer 的差别有两条,都是为了
	// 让排空判定只取决于标签本身:
	//   - 元数据段(object_meta / objectMeta)缺失或为 null 返回 false:这是响应形状漂移,不是
	//     "标签被移除"。当成空 labels 会把正在排空的实例静默放回接单元(fail-open)。
	//     元数据段存在但没有 labels 键是合法的"没有标签",返回 true 且 labels 为空。
	//   - 不看 status:status 里与排空无关的字段类型不符(如某个 ports 项缺 port)不拦标签判定。
	// JSON 格式错误、顶层不是对象、元数据段不是对象、labels 不是字符串到字符串的对象,同样返回
	// false 并写 error。失败时 labels 保持调用前的内容不变。纯函数,无 I/O,可在任意线程调用。
	bool ParseGameServerLabels(const std::string& json, std::map<std::string, std::string>& labels, std::string& error);

} // namespace agones
