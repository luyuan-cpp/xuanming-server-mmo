#include "team_follow_formation.h"

#include <algorithm>
#include <cmath>
#include <iterator>
#include <limits>

namespace
{
	// 名次顺序:入队序号小的在前,同号按玩家编号(与组队界面、旧客户端跟随的槽位规则一致)。
	bool RanksBefore(uint32_t leftSeq, uint64_t leftId, uint32_t rightSeq, uint64_t rightId)
	{
		return leftSeq != rightSeq ? leftSeq < rightSeq : leftId < rightId;
	}
} // namespace

double FollowHorizontalDistance(const FollowPoint& a, const FollowPoint& b)
{
	const double dx = a.x - b.x;
	const double dy = a.y - b.y;
	return std::sqrt(dx * dx + dy * dy);
}

namespace
{
	// 以下模板只对 TeamFollowTrail::Node 实例化(按推导取得类型,不经私有名字)。
	template <typename Node>
	Node LerpNode(const Node& a, const Node& b, double t)
	{
		Node out;
		out.point.x = a.point.x + (b.point.x - a.point.x) * t;
		out.point.y = a.point.y + (b.point.y - a.point.y) * t;
		out.point.z = a.point.z + (b.point.z - a.point.z) * t;
		out.odo = a.odo + (b.odo - a.odo) * t;
		return out;
	}

	// 线段 a->b 上里程为 odo 的点(夹在线段内)。
	template <typename Node>
	Node NodeAtOdo(const Node& a, const Node& b, double odo)
	{
		const double span = b.odo - a.odo;
		const double t = span > 0.0 ? std::clamp((odo - a.odo) / span, 0.0, 1.0) : 0.0;
		return LerpNode(a, b, t);
	}

	// point 在线段 a->b 上的水平投影(夹在线段内),高度与里程按比例插值。
	template <typename Node>
	Node ProjectOntoSegment(const Node& a, const Node& b, const FollowPoint& point)
	{
		const double dx = b.point.x - a.point.x;
		const double dy = b.point.y - a.point.y;
		const double lengthSquared = dx * dx + dy * dy;
		double t = 0.0;
		if (lengthSquared > 0.0)
		{
			t = ((point.x - a.point.x) * dx + (point.y - a.point.y) * dy) / lengthSquared;
			t = std::clamp(t, 0.0, 1.0);
		}
		return LerpNode(a, b, t);
	}
} // namespace

// ---------------------------------------------------------------------------
// TeamFollowTrail
// ---------------------------------------------------------------------------

void TeamFollowTrail::Reset(const FollowPoint& head)
{
	nodes.clear();
	nodes.push_back(Node{head, 0.0});
}

void TeamFollowTrail::Append(const FollowPoint& point)
{
	const Node& head = nodes.back();
	nodes.push_back(Node{point, head.odo + FollowHorizontalDistance(head.point, point)});
}

TeamFollowTrail::FeedResult TeamFollowTrail::Feed(const FollowPoint& leader, const double protectOdo,
	const WalkableCheck& walkable)
{
	if (nodes.empty())
	{
		Reset(leader);
		return FeedResult::kAppended;
	}
	// 拷贝:下面可能截断 nodes_。
	const Node head = nodes.back();
	const double stepFromHead = FollowHorizontalDistance(head.point, leader);
	if (stepFromHead < kTrailMinStep)
	{
		return FeedResult::kIgnored;
	}

	// 回收候选:只看头部 kTrailRewindSearchLength 之内、且不早于 protectOdo(最前面那名队员)的轨迹。
	// 在每一段上取新位置的水平投影;离得够近、且直接连过去比沿轨迹绕过去近得够多,就是一段可剪的回路。
	// 取收益最大的那一段:往回走时是正在折返的那一段,绕小圈回到原处时是圈的起点。
	const double floorOdo = std::max(protectOdo, head.odo - kTrailRewindSearchLength);
	std::optional<Node> best;
	std::size_t bestSegmentEnd = 0;
	double bestGain = kTrailRewindMinGain;
	for (std::size_t end = nodes.size() - 1; end >= 1; --end)
	{
		const Node& a = nodes[end - 1];
		const Node& b = nodes[end];
		if (b.odo <= floorOdo)
		{
			break;
		}
		Node candidate = ProjectOntoSegment(a, b, leader);
		if (candidate.odo < floorOdo)
		{
			candidate = NodeAtOdo(a, b, floorOdo);
		}
		const double shortcut = FollowHorizontalDistance(candidate.point, leader);
		if (shortcut > kTrailRewindRadius)
		{
			continue;
		}
		const double alongTrail = (head.odo - candidate.odo) + stepFromHead;
		const double gain = alongTrail - shortcut;
		if (gain > bestGain)
		{
			bestGain = gain;
			best = candidate;
			bestSegmentEnd = end;
		}
	}

	if (best && (!walkable || walkable(best->point, leader)))
	{
		// 剪掉 best 之后的全部点(best 落在 [bestSegmentEnd-1, bestSegmentEnd] 这一段上),再接上新位置。
		nodes.erase(nodes.begin() + static_cast<std::ptrdiff_t>(bestSegmentEnd), nodes.end());
		if (FollowHorizontalDistance(nodes.back().point, best->point) >= kTrailMinStep)
		{
			nodes.push_back(*best);
		}
		if (FollowHorizontalDistance(nodes.back().point, leader) >= kTrailMinStep)
		{
			Append(leader);
		}
		return FeedResult::kRewound;
	}

	Append(leader);
	return FeedResult::kAppended;
}

FollowPoint TeamFollowTrail::PointAt(const double odo) const
{
	if (odo <= nodes.front().odo)
	{
		return nodes.front().point;
	}
	if (odo >= nodes.back().odo)
	{
		return nodes.back().point;
	}
	// 第一个里程 >= odo 的点;上面已排除落在尾部之前,所以它前面一定还有点。
	const auto it = std::lower_bound(nodes.begin(), nodes.end(), odo,
		[](const Node& node, double value) { return node.odo < value; });
	return NodeAtOdo(*std::prev(it), *it, odo).point;
}

std::optional<double> TeamFollowTrail::NearestOdo(const FollowPoint& point, const double maxDistance) const
{
	if (nodes.empty())
	{
		return std::nullopt;
	}
	double bestDistance = FollowHorizontalDistance(nodes.front().point, point);
	double bestOdo = nodes.front().odo;
	for (std::size_t end = 1; end < nodes.size(); ++end)
	{
		const Node projected = ProjectOntoSegment(nodes[end - 1], nodes[end], point);
		const double distance = FollowHorizontalDistance(projected.point, point);
		// 等距时取里程更大的(离队长更近的那一趟):发夹处不必把折返的路再走一遍。
		if (distance <= bestDistance)
		{
			bestDistance = distance;
			bestOdo = projected.odo;
		}
	}
	if (bestDistance > maxDistance)
	{
		return std::nullopt;
	}
	return bestOdo;
}

void TeamFollowTrail::PrependPath(std::span<const FollowPoint> path)
{
	if (nodes.empty())
	{
		return;
	}
	// 从近到远逐点往前接:里程从当前尾部往回减。
	for (auto it = path.rbegin(); it != path.rend(); ++it)
	{
		const Node& tail = nodes.front();
		const double distance = FollowHorizontalDistance(*it, tail.point);
		if (distance < kTrailMinStep)
		{
			continue;
		}
		nodes.push_front(Node{*it, tail.odo - distance});
	}
}

void TeamFollowTrail::TrimTailTo(const double odo)
{
	if (nodes.size() < 2 || odo <= nodes.front().odo)
	{
		return;
	}
	if (odo >= nodes.back().odo)
	{
		const Node head = nodes.back();
		nodes.clear();
		nodes.push_back(head);
		return;
	}
	const FollowPoint cut = PointAt(odo);
	while (nodes.size() >= 2 && nodes[1].odo <= odo)
	{
		nodes.pop_front();
	}
	// 此时 nodes_[0].odo <= odo < nodes_[1].odo:新尾点落在原来那一段上,沿段可走。
	nodes.front() = Node{cut, odo};
}

void TeamFollowTrail::PruneBefore(const double odo)
{
	while (nodes.size() >= 2 && nodes[1].odo <= odo)
	{
		nodes.pop_front();
	}
}

// ---------------------------------------------------------------------------
// TeamFollowFormation
// ---------------------------------------------------------------------------

TeamFollowFormation::TeamFollowFormation(const FollowPoint& leader, const double spacing, const TeamFollowStyle style)
	: spacing(spacing), style(style)
{
	trail.Reset(leader);
}

TeamFollowMember* TeamFollowFormation::Find(const uint64_t playerId)
{
	const auto it = std::ranges::find(members, playerId, &TeamFollowMember::playerId);
	return it != members.end() ? &*it : nullptr;
}

const TeamFollowMember* TeamFollowFormation::Find(const uint64_t playerId) const
{
	const auto it = std::ranges::find(members, playerId, &TeamFollowMember::playerId);
	return it != members.end() ? &*it : nullptr;
}

std::optional<std::size_t> TeamFollowFormation::RankOf(const uint64_t playerId) const
{
	const auto it = std::ranges::find(members, playerId, &TeamFollowMember::playerId);
	if (it == members.end())
	{
		return std::nullopt;
	}
	return static_cast<std::size_t>(std::distance(members.begin(), it));
}

std::size_t TeamFollowFormation::RankFor(const uint32_t joinSeq, const uint64_t playerId) const
{
	std::size_t rank = 0;
	for (const auto& member : members)
	{
		if (member.playerId != playerId && RanksBefore(member.joinSeq, member.playerId, joinSeq, playerId))
		{
			++rank;
		}
	}
	return rank;
}

bool TeamFollowFormation::Add(const TeamFollowMember& member)
{
	if (member.playerId == 0 || Find(member.playerId) != nullptr || members.size() >= kTeamFollowMaxMembers)
	{
		return false;
	}
	const auto position = std::ranges::find_if(members, [&member](const TeamFollowMember& existing) {
		return RanksBefore(member.joinSeq, member.playerId, existing.joinSeq, existing.playerId);
	});
	members.insert(position, member);
	return true;
}

bool TeamFollowFormation::Remove(const uint64_t playerId)
{
	const auto it = std::ranges::find(members, playerId, &TeamFollowMember::playerId);
	if (it == members.end())
	{
		return false;
	}
	members.erase(it);
	return true;
}

void TeamFollowFormation::SetSuspended(const uint64_t playerId, const bool suspended)
{
	if (auto* member = Find(playerId))
	{
		member->suspended = suspended;
	}
}

double TeamFollowFormation::SlotLag(const std::size_t rank) const
{
	switch (style)
	{
	case TeamFollowStyle::kEchelon:
		// 雁行两翼:第 1/2 名并排第一翼,第 3/4 名并排第二翼。
		return static_cast<double>(rank / 2 + 1) * spacing;
	case TeamFollowStyle::kWuxing:
		// 五行聚阵:左右双护翼贴近队长,中军与殿后居中心轴。
		switch (rank)
		{
		case 0:
		case 1:
			return 0.85 * spacing;
		case 2:
			return 1.7 * spacing;
		default:
			return 2.55 * spacing;
		}
	case TeamFollowStyle::kColumn:
	default:
		return static_cast<double>(rank + 1) * spacing;
	}
}

double TeamFollowFormation::TargetLateral(const std::size_t rank) const
{
	switch (style)
	{
	case TeamFollowStyle::kEchelon:
		switch (rank)
		{
		case 0:
			return -2.0;
		case 1:
			return 2.0;
		case 2:
			return -3.6;
		case 3:
			return 3.6;
		default:
			return 0.0;
		}
	case TeamFollowStyle::kWuxing:
		switch (rank)
		{
		case 0:
			return -2.4;
		case 1:
			return 2.4;
		default:
			return 0.0;
		}
	case TeamFollowStyle::kColumn:
	default:
		return 0.0;
	}
}

double TeamFollowFormation::SlotOdo(const std::size_t rank) const
{
	return std::max(trail.TailOdo(), trail.HeadOdo() - SlotLag(rank));
}

FollowPoint TeamFollowFormation::ResolveSlotPoint(const double odo, const double desiredLateral,
	const TeamFollowTrail::WalkableCheck& walkable, double* const appliedLateral) const
{
	const FollowPoint base = trail.PointAt(odo);
	if (appliedLateral != nullptr)
	{
		*appliedLateral = 0.0;
	}
	if (std::abs(desiredLateral) < 1e-4 || trail.Length() < kTrailMinStep)
	{
		return base;
	}
	constexpr double kTangentHalfWindow = 0.5;
	const FollowPoint back = trail.PointAt(std::max(trail.TailOdo(), odo - kTangentHalfWindow));
	const FollowPoint front = trail.PointAt(std::min(trail.HeadOdo(), odo + kTangentHalfWindow));
	const double dx = front.x - back.x;
	const double dy = front.y - back.y;
	const double len = std::hypot(dx, dy);
	if (len < 1e-3)
	{
		return base;
	}
	// Z-up 平面切线 (tx, ty) 对应的右法向量 (ty, -tx)。
	const double rx = dy / len;
	const double ry = -dx / len;
	constexpr double kScales[] = {1.0, 0.5};
	for (const double scale : kScales)
	{
		const double offset = desiredLateral * scale;
		const FollowPoint candidate{base.x + rx * offset, base.y + ry * offset, base.z};
		if (!walkable || walkable(base, candidate))
		{
			if (appliedLateral != nullptr)
			{
				*appliedLateral = offset;
			}
			return candidate;
		}
	}
	// 两侧均贴墙 / 窄道:自动收拢回队长走过的安全轨迹点,永不穿墙或卡死。
	return base;
}

FollowPoint TeamFollowFormation::SlotPosition(const std::size_t rank, const TeamFollowTrail::WalkableCheck& walkable,
	double* const appliedLateral) const
{
	return ResolveSlotPoint(SlotOdo(rank), TargetLateral(rank), walkable, appliedLateral);
}

double TeamFollowFormation::ProtectOdo() const
{
	double protect = std::numeric_limits<double>::lowest();
	for (const auto& member : members)
	{
		if (!member.needsPlacement)
		{
			protect = std::max(protect, member.cursorOdo);
		}
	}
	return protect;
}

double TeamFollowFormation::KeepOdo() const
{
	// 头部之后至少保留到队尾槽位再往后一个间距:第一名队员加入时能落到"队长身后的路上",而不是队长旁边。
	double keep = trail.HeadOdo() - static_cast<double>(kTeamFollowMaxMembers + 1) * spacing;
	for (const auto& member : members)
	{
		if (!member.needsPlacement)
		{
			keep = std::min(keep, member.cursorOdo);
		}
	}
	return keep;
}

void TeamFollowFormation::TrimTailForNewcomer(const std::size_t rank)
{
	double trimTo = trail.HeadOdo() - SlotLag(rank);
	for (const auto& member : members)
	{
		if (!member.needsPlacement)
		{
			trimTo = std::min(trimTo, member.cursorOdo);
		}
	}
	trail.TrimTailTo(trimTo);
}

void TeamFollowFormation::Tick(const FollowPoint& leader, const double deltaSeconds,
	const TeamFollowTrail::WalkableCheck& walkable, std::vector<TeamFollowStep>& out)
{
	out.clear();
	const double headBefore = trail.HeadOdo();
	trail.Feed(leader, ProtectOdo(), walkable);
	// 跟上之后与队长同速:队长这一 tick 走了多远,队员至少能走多远。队长偶尔一 tick 走得比追赶速度还多
	// (一次上报把位置纠正了几十厘米)时,已跟上的队员一起前进,间距不被拉开。
	const double headAdvance = std::max(0.0, trail.HeadOdo() - headBefore);
	const double maxStep = std::max(headAdvance, kTeamFollowCatchUpSpeed * std::max(0.0, deltaSeconds));

	for (std::size_t rank = 0; rank < members.size(); ++rank)
	{
		auto& member = members[rank];
		const double slot = SlotOdo(rank);
		TeamFollowStep step;
		step.playerId = member.playerId;

		if (member.suspended)
		{
			// 原地站着,名次照留;落后太多就不再为他保留轨迹,恢复后直接放进槽位。
			if (!member.needsPlacement && slot - member.cursorOdo > kTeamFollowMaxLag)
			{
				member.needsPlacement = true;
			}
			step.suspended = true;
			out.push_back(step);
			continue;
		}

		if (member.needsPlacement || slot - member.cursorOdo > kTeamFollowMaxLag)
		{
			member.cursorOdo = slot;
			member.needsPlacement = false;
			double appliedLateral = 0.0;
			step.trailPosition = trail.PointAt(slot);
			step.position = ResolveSlotPoint(slot, TargetLateral(rank), walkable, &appliedLateral);
			member.lateralOffset = appliedLateral;
			step.placed = true;
			out.push_back(step);
			continue;
		}

		// 只进不退:在槽位上或比槽位靠前(队长往回走了)时原地等。
		const double next = member.cursorOdo < slot ? std::min(slot, member.cursorOdo + maxStep) : member.cursorOdo;
		const double odoAdvance = next - member.cursorOdo;
		member.cursorOdo = next;
		step.trailPosition = trail.PointAt(next);

		// 目标侧移经导航安全校验后再平滑收敛:切换阵型或进出窄道时平滑滑动,不突兀瞬移。
		double safeTargetLateral = 0.0;
		const FollowPoint safeTargetPoint = ResolveSlotPoint(next, TargetLateral(rank), walkable, &safeTargetLateral);
		const double lateralDiff = safeTargetLateral - member.lateralOffset;
		const double desiredNextLateral = std::abs(lateralDiff) <= maxStep
			? safeTargetLateral
			: member.lateralOffset + std::copysign(maxStep, lateralDiff);
		double appliedLateral = 0.0;
		FollowPoint resolved = ResolveSlotPoint(next, desiredNextLateral, walkable, &appliedLateral);
		// 若平滑过渡中间点撞墙,直接退到已验过的目标侧移(保证不悬停在障碍侧)。
		if (std::abs(appliedLateral - desiredNextLateral) > 1e-4 &&
			std::abs(desiredNextLateral - safeTargetLateral) > 1e-4)
		{
			resolved = safeTargetPoint;
			appliedLateral = safeTargetLateral;
		}
		const double lateralMoved = std::abs(appliedLateral - member.lateralOffset);
		member.lateralOffset = appliedLateral;
		step.moved = (odoAdvance > 0.0 || lateralMoved > 1e-4) ? std::hypot(odoAdvance, lateralMoved) : 0.0;
		step.position = resolved;
		out.push_back(step);
	}

	trail.PruneBefore(KeepOdo());
}

// ---------------------------------------------------------------------------
// 广播节流
// ---------------------------------------------------------------------------

bool TeamFollowShouldBroadcast(const TeamFollowBroadcastState& last, const TeamFollowBroadcastState& now,
	const uint32_t framesSinceLast)
{
	if (last.moving != now.moving)
	{
		return true; // 起步 / 停步
	}
	if (!now.moving)
	{
		return false; // 一直站着
	}
	if (framesSinceLast >= kTeamFollowBroadcastIntervalFrames)
	{
		return true; // 行走中的周期校正
	}
	const double lastSpeed = std::hypot(last.velocityX, last.velocityY);
	const double speed = std::hypot(now.velocityX, now.velocityY);
	if (lastSpeed <= 0.0 || speed <= 0.0)
	{
		return true; // 只有竖直分量的速度切到水平行走,当成起步
	}
	const double cosTurn = (last.velocityX * now.velocityX + last.velocityY * now.velocityY) / (lastSpeed * speed);
	if (cosTurn < kTeamFollowTurnBroadcastCos)
	{
		return true; // 拐弯
	}
	return std::abs(speed - lastSpeed) > kTeamFollowSpeedChangeRatio * lastSpeed; // 追赶 / 跟上
}
