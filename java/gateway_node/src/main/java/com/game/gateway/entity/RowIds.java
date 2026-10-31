package com.game.gateway.entity;

import java.util.concurrent.ThreadLocalRandom;
import java.util.concurrent.atomic.AtomicLong;

/**
 * zone_whitelist、announcement 的行号。这两张是运营手工维护的小表,行号只在本表内用,所以不走发号器:
 * 网关不依赖 data_service,也不依赖库的自增(docs/design/player-data-tidb-scaling-plan.md §6.4)。
 *
 * <p>值 = 毫秒时间 × 1024 + 随机低 10 位,同一进程内严格递增。这两个性质都是有意的:
 * <ul>
 *   <li><b>按时间递增,不能打散</b>:zone_whitelist 的 ODKU 只排队不成环,前提是新行的唯一键项排在删除标记项之后,
 *       见 {@code ZoneWhitelistRepository}。副本间时钟偏差超过「删掉到重新加回」的间隔时前提不成立,
 *       ODKU 可能成环,由调用方有上限的 1213 重试吸收,不影响数据。</li>
 *   <li><b>小于 2^53</b>(能用到 2248 年):id 经管理接口的 JSON 给到前端,超过这个值 JavaScript 会丢精度。</li>
 * </ul>
 * 不同副本在同一毫秒发号有 1/1024 的概率撞号,白名单的 ODKU 怎么兜见 {@code AdminWhitelistController#add}。
 * 运营手工操作的频率下可以接受。
 *
 * <p>公告用 {@link #nextInt32()}:{@code /api/announcement} 把 id 发给游戏客户端,以前是个小整数,客户端按什么位宽
 * 解析没有核对过,所以压在 int32 以内。
 */
public final class RowIds {

    /** {@link #nextInt32()} 的起算点:2026-01-01T00:00:00Z。从这里算,int32 够用到 2094 年。 */
    private static final long INT32_EPOCH_SECONDS = 1_767_225_600L;

    private static final AtomicLong LAST = new AtomicLong();
    private static final AtomicLong LAST_INT32 = new AtomicLong();

    private RowIds() {
    }

    public static long next() {
        long candidate = System.currentTimeMillis() * 1024 + ThreadLocalRandom.current().nextInt(1024);
        return LAST.updateAndGet(last -> Math.max(last + 1, candidate));
    }

    /**
     * 秒级行号,同一进程内严格递增,小于 2^31。不同副本在同一秒发号会撞:公告是普通 INSERT,被主键拒绝,接口报错,重试即可。
     */
    public static long nextInt32() {
        long candidate = System.currentTimeMillis() / 1000 - INT32_EPOCH_SECONDS;
        return LAST_INT32.updateAndGet(last -> Math.max(last + 1, candidate));
    }
}
