package com.game.gateway.entity;

import org.junit.jupiter.api.Test;

import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.stream.IntStream;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

class RowIdsTest {

    /** 2^53:再大前端的 JavaScript 就丢精度了。 */
    private static final long MAX_SAFE_INTEGER = 1L << 53;

    /** zone_whitelist 的 ODKU 要求新行主键大于删除标记那条记录的主键,所以不能打散,见 ZoneWhitelistRepository。 */
    @Test
    void strictlyIncreasingWithinOneProcess() {
        long previous = RowIds.next();
        for (int i = 0; i < 10_000; i++) {
            long id = RowIds.next();
            assertTrue(id > previous, "行号必须严格递增");
            previous = id;
        }
    }

    /** 公告 id 发给游戏客户端,要压在 int32 以内。 */
    @Test
    void int32IdsAreStrictlyIncreasingAndFit() {
        long previous = RowIds.nextInt32();
        for (int i = 0; i < 1_000; i++) {
            long id = RowIds.nextInt32();
            assertTrue(id > previous, "行号必须严格递增");
            assertTrue(id > 0 && id <= Integer.MAX_VALUE, "公告 id 必须放得进 int32");
            previous = id;
        }
    }

    @Test
    void uniqueUnderConcurrencyAndWithinSafeRange() {
        Set<Long> ids = ConcurrentHashMap.newKeySet();
        IntStream.range(0, 20_000).parallel().forEach(i -> ids.add(RowIds.next()));
        assertEquals(20_000, ids.size());
        assertTrue(ids.stream().allMatch(id -> id > 0 && id < MAX_SAFE_INTEGER));
    }
}
