#pragma once

#include <functional>

#include "engine/core/type_define/type_define.h"

#include "proto/common/component/item_base_comp.pb.h"

struct InitItemParam
{
	InitItemParam() { itemPBComp.set_size(1); itemPBComp.set_item_id(kInvalidGuid); }
	ItemComp itemPBComp;
};

// 新铸实例的初始化回调:一件不可叠加物品**第一次**成为实例时,给它填实例数据
// (今天唯一的用途是装备掷随机属性,见 docs/design/equipment-attributes.md §4.1)。
//
// 谁装:玩法层在进程启动时经 BagService::SetItemInstanceInitializer 装一次
//       (scene 是 PlayerEquipSystem::InstallItemInitializer);容器层自己不认识「属性」。
// 何时调:BagService 的三个 AddItem / AddItems 入口,每一个不可叠加实例在进 ItemStore
//       之前各调一次,条件是「已安装 && !item.has_equip()」。has_equip() 是唯一的幂等
//       判据 —— 带着装备数据来的既有实例(穿脱搬运、邮件回流)绝不重掷;count = N 的
//       批量每件各调一次,拿到的是各自独立的 ItemComp。
//       还原(Bag::InsertItemForRestore)、搬运原语(Bag::TakeInstance / PutInstance)、
//       可叠加物品、以及绕过 BagService 直接调 Bag 的路径都**不调**。
//
// 约束(调用点在 reserve 之后、这一件写进实例层 / 布局层之前;count = N 时正处在逐件
// 写入的循环中间,前几件已经落地 —— 所以比普通回调严):
//   * **不许失败**:没有返回值,也不许抛异常。此刻位已经留好(临时格甚至已经为它挤掉了
//     旧物),这里失败就是半批。掷不出东西就什么都别写,或只 mutable_equip() 置位。
//   * **不许在里面再入包**:不许对任何 Bag / BagService 做入包、扣除、整理 —— 当前这个包
//     正处在逐件写入的循环中间,重入会让刚做完的 reserve 作废。
//   * **只许写实例数据(equip 段)**:item_id / config_id / size / acquire_seq 归容器管,
//     回调改了也会被容器改回去并记 ERROR。
//   * 回调拿到的引用只在调用期间有效,不得留存。
using ItemInstanceInitializer = std::function<void(ItemComp&)>;

//todo equipment list with unique guids per piece
