#pragma once

// 组队跟随的纯逻辑层:队长轨迹 + 队员槽位(docs/design/team-follow-server-trail.md)。
//
// 职责:只回答"队员此刻该站在哪"。不碰 ECS、导航、网络与时钟;导航只经 WalkableCheck 回调注入
// (ECS 层 TeamFollowSystem 用导航射线实现,单测用桩)。调用方:TeamFollowSystem(每 tick 一次)。
// 线程模型:无内部同步,由 scene 节点 loop 线程独占使用。
//
// 模型:队员不自己寻路,而是站在"队长走过的路"上、离队长固定路程的地方。
//   - 轨迹:队长每 tick 的服务器位置连成的折线,按"里程"(沿折线累计的水平长度)寻址。相邻两点之间是
//     队长实际走过的直线段,移动裁决(导航射线)已经保证可走,所以沿折线走不会穿墙,也不需要再寻路。
//   - 槽位:第 k 名(k 从 0 起)队员的目标里程 = 队长里程 - (k+1) × 间距。队长走多少,目标就前进多少:
//     全队同速、同起停、在同一个拐角拐弯、沿路间距恒定("同动作、同间隔")。
//   - 只进不退:目标跑到队员身后时(队长往回走),队员原地站着等,不倒着走。
//   - 回收(队长原地小范围来回走):队长回到自己刚走过、且还没有队员走到的那段路附近时,
//     把那段回路从轨迹上剪掉、直接连到新位置。配合"只进不退",队长在第一名队员前面怎么折腾,
//     队员都原地不动;队长走远时队员走的是剪干净的路,不会跟着画之字。
//   - 回收不越过最前面那名队员的游标:队长真掉头、穿过队伍往回走时,轨迹在第一名队员处折成发夹,
//     队员依次走到折返点再掉头,顺序不乱(玩家之间没有碰撞,互相穿过)。
//
// 复杂度:每 tick O(队员数 + 回收搜索段数),回收只搜头部 kTrailRewindSearchLength 之内;
// 轨迹长度受 kTeamFollowMaxLag 与 kTrailKeepBehindHead 约束,点数有界。

#include <cstddef>
#include <cstdint>
#include <deque>
#include <functional>
#include <optional>
#include <span>
#include <vector>

// 槽位间距(米,沿轨迹量)。沿用客户端跟随一直用的 1.4(team-system.md V.4)。
// 视觉上偏紧是待定项(team-system.md V.7 第 5 条);改大时要一并核对 kTeamRegroupDistance 与可见半径 kMaxViewRadius。
constexpr double kTeamFollowSpacing = 1.4;
// 队形里最多的队员数:队伍上限 5 人,队长之外 4 人。
constexpr std::size_t kTeamFollowMaxMembers = 4;
// 队员追赶速度上限(米/秒):加入、补位、恢复后赶回槽位时用;跟上之后与队长同速。
// 取客户端基础移速 9 的 1.2 倍,比队长稍快才能收敛。
constexpr double kTeamFollowCatchUpSpeed = 10.8;
// 落后超过这个路程(米)就不再沿路追,下一次能动时直接放进槽位(由 ECS 层瞬移)。
// 也是轨迹为暂停中的队员保留历史的上限:轨迹长度因此有界。
constexpr double kTeamFollowMaxLag = 24.0;
// 队长移动小于这个水平距离(米)不加点:站着时的上报抖动、纯高度变化不进轨迹。
constexpr double kTrailMinStep = 0.05;
// 回收判定一:队长新位置离轨迹上某段路的水平距离不超过它(米)。
constexpr double kTrailRewindRadius = 0.6;
// 回收判定二:直接连过去比沿轨迹绕过去至少近这么多(米)。正常前进(直走、缓弯、不超过 90° 的拐弯)时
// 两者相等或只差毫米级弦高,不会误剪;往回走、绕小圈回到原处时差值约为折返距离的两倍或圈长。
// 取得小(5cm):队长横着来回晃、每次穿过岔口时留下的小残段(残段长的两倍就是收益)也要剪掉,
// 不然队员每晃一次就被残段往前带一点,积少成多。超过 90° 的急转顺带被切掉几厘米的角,捷径照样校验可走。
constexpr double kTrailRewindMinGain = 0.05;
// 回收只在头部这么长(米)的一段轨迹里找:小范围来回都发生在队长脚下。
constexpr double kTrailRewindSearchLength = 4.0;
// 头部之后至少保留的历史(米):第一名队员加入时能落到"队长身后的路上",而不是队长旁边。
constexpr double kTrailKeepBehindHead = static_cast<double>(kTeamFollowMaxMembers + 1) * kTeamFollowSpacing;

// 服务器坐标(Z-up,x/y 为地面,z 为高度)。所有距离与里程只量水平面。
struct FollowPoint
{
	double x{0.0};
	double y{0.0};
	double z{0.0};
};

double FollowHorizontalDistance(const FollowPoint& a, const FollowPoint& b);

// 队长走过的路:按里程寻址的折线。
// 不变量:非空时里程严格递增(相邻点水平距离 >= kTrailMinStep,PrependPath / TrimTailTo 例外处
// 也保证不减);Head() 是最近一次被记录的队长位置。除 Reset / Feed 外,所有读接口前置:非空。
class TeamFollowTrail
{
public:
	// 回收时要把 from 直接连到 to:返回 false 表示这条捷径不可走,不回收、照常追加。
	// 空函数视为可走(场景没有导航数据时与移动裁决同样 fail-open)。
	using WalkableCheck = std::function<bool(const FollowPoint& from, const FollowPoint& to)>;

	enum class FeedResult : uint8_t
	{
		kIgnored = 0,  // 离头部太近,没加点
		kAppended = 1, // 追加到头部
		kRewound = 2,  // 剪掉了一段回路再接上
	};

	// 轨迹只剩 head 一个点,里程从 0 重新开始。
	void Reset(const FollowPoint& head);

	[[nodiscard]] bool Empty() const { return nodes_.empty(); }
	[[nodiscard]] std::size_t PointCount() const { return nodes_.size(); }
	[[nodiscard]] const FollowPoint& Head() const { return nodes_.back().point; }
	[[nodiscard]] const FollowPoint& Tail() const { return nodes_.front().point; }
	[[nodiscard]] double HeadOdo() const { return nodes_.back().odo; }
	[[nodiscard]] double TailOdo() const { return nodes_.front().odo; }
	[[nodiscard]] double Length() const { return HeadOdo() - TailOdo(); }

	// 记录队长的新位置。protectOdo:回收不得越过的里程(最前面那名队员的游标;没有队员时取极小值)。
	// 空轨迹等同 Reset。
	FeedResult Feed(const FollowPoint& leader, double protectOdo, const WalkableCheck& walkable);

	// 轨迹上里程 odo 处的点(夹到 [TailOdo, HeadOdo],段内线性插值,含高度)。
	[[nodiscard]] FollowPoint PointAt(double odo) const;

	// 离 point 最近的轨迹点的里程;水平距离超过 maxDistance 返回 nullopt。
	[[nodiscard]] std::optional<double> NearestOdo(const FollowPoint& point, double maxDistance) const;

	// 在尾部之前接一段路。path 从远到近:path.front() 是新的尾部(加入者的位置),
	// path.back() 应当就是(或紧挨着)当前尾部。调用方保证 path 相邻点之间可走。
	void PrependPath(std::span<const FollowPoint> path);

	// 把尾部剪到里程 odo(段内插值出新尾点);odo 不大于尾部时不动,不小于头部时只留头部。
	void TrimTailTo(double odo);

	// 删掉不再需要的旧点:保留覆盖 odo 的那一段的起点,PointAt(>= odo) 不受影响。
	void PruneBefore(double odo);

private:
	struct Node
	{
		FollowPoint point;
		double odo{0.0};
	};

	void Append(const FollowPoint& point);

	std::deque<Node> nodes_;
};

// 队形里的一名队员(纯数据)。
struct TeamFollowMember
{
	uint64_t playerId{0};
	uint32_t joinSeq{0};          // 入队序号(TeamMemberRecord.join_seq):决定在队伍里排第几,与组队界面同序
	double cursorOdo{0.0};        // 自己站在轨迹上的里程
	bool suspended{false};        // 本 tick 不能动(战斗中 / 断线保留):原地站着,名次照留。由调用方每 tick 写入
	bool needsPlacement{false};   // 落后超过 kTeamFollowMaxLag:不再为他保留轨迹,能动时直接放进槽位
};

// 一名队员这一 tick 的结果。
struct TeamFollowStep
{
	uint64_t playerId{0};
	FollowPoint position;   // 这一 tick 之后应在的位置
	double moved{0.0};      // 沿轨迹走过的路程;0 = 站着
	bool placed{false};     // 直接放进槽位(调用方按瞬移处理,不当成走路)
	bool suspended{false};  // 暂停中,position 无意义
};

// 一个队长的队形:轨迹 + 按名次排好的队员。
class TeamFollowFormation
{
public:
	explicit TeamFollowFormation(const FollowPoint& leader, double spacing = kTeamFollowSpacing);

	[[nodiscard]] TeamFollowTrail& Trail() { return trail_; }
	[[nodiscard]] const TeamFollowTrail& Trail() const { return trail_; }
	[[nodiscard]] double Spacing() const { return spacing_; }

	// 按 (joinSeq, playerId) 升序;下标就是名次,0 紧跟队长。
	[[nodiscard]] const std::vector<TeamFollowMember>& Members() const { return members_; }
	[[nodiscard]] TeamFollowMember* Find(uint64_t playerId);
	[[nodiscard]] const TeamFollowMember* Find(uint64_t playerId) const;
	[[nodiscard]] std::optional<std::size_t> RankOf(uint64_t playerId) const;
	// (joinSeq, playerId) 这名新队员加入后会排第几(不修改队形)。
	[[nodiscard]] std::size_t RankFor(uint32_t joinSeq, uint64_t playerId) const;

	// 按名次插入。已在队形里或已满员返回 false,队形不变。
	bool Add(const TeamFollowMember& member);
	bool Remove(uint64_t playerId);
	// 调用方每 tick 在 Tick 之前写入:这名队员本 tick 能不能动。不在队形里时什么都不做。
	void SetSuspended(uint64_t playerId, bool suspended);

	// 名次 rank 的槽位里程:max(尾部, 头部 - (rank+1) × 间距)。轨迹不够长时落在尾部。
	[[nodiscard]] double SlotOdo(std::size_t rank) const;
	// 回收下限:所有仍受保护的队员游标的最大值;没有这样的队员时为极小值。
	[[nodiscard]] double ProtectOdo() const;
	// 修剪下限:min(头部 - kTrailKeepBehindHead, 所有仍受保护的队员游标)。
	[[nodiscard]] double KeepOdo() const;

	// 给即将以 rank 名次加入的新队员腾出路尾:把谁都不需要的旧历史剪到新队员的槽位处,
	// 接下来接在尾部的那段路(PrependPath)就刚好通到槽位,不会绕回更早的历史。
	void TrimTailForNewcomer(std::size_t rank);

	// 一个 tick:先把队长位置喂进轨迹(含回收),再推进全体队员,最后修剪轨迹。
	// out 先清空,再按名次给每名队员写一条结果。
	void Tick(const FollowPoint& leader, double deltaSeconds, const TeamFollowTrail::WalkableCheck& walkable,
			  std::vector<TeamFollowStep>& out);

private:
	TeamFollowTrail trail_;
	std::vector<TeamFollowMember> members_;
	double spacing_;
};

// ---- 被驱动队员的移动广播节流(纯函数) ----
// 观察者按最后一次广播的速度外推,所以只在外推会出错的时候发:起步 / 停步、拐弯超过约 20°、
// 速度变化超过 15%(追赶与跟上),行走中另每 kTeamFollowBroadcastIntervalFrames 帧补一条校正
// (与客户端每 0.25s 一条 MoveSync 同一节奏)。站着不动时不发。
constexpr uint32_t kTeamFollowBroadcastIntervalFrames = 5;  // 20 FPS 下 250ms
constexpr double kTeamFollowTurnBroadcastCos = 0.94;        // cos(20°)
constexpr double kTeamFollowSpeedChangeRatio = 0.15;

struct TeamFollowBroadcastState
{
	bool moving{false};
	double velocityX{0.0};
	double velocityY{0.0};
};

bool TeamFollowShouldBroadcast(const TeamFollowBroadcastState& last, const TeamFollowBroadcastState& now,
							   uint32_t framesSinceLast);
