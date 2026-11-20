package com.game.startup;

import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.*;

/** 启动时钟核对的纯判定(偏差怎么算、什么时候拒启、warn 开关);发请求读 Date 头不在这里跑。 */
class ClockCheckTest {

    @Test
    void offsetComparesTheMidpointOfTheRequestWithTheMidpointOfTheDateSecond() {
        // 请求前后本机是 1000.000 秒、1000.200 秒,Date 是第 996 秒:本机中点 1000.100,参照中点 996.500,本机快 3.6 秒。
        assertEquals(3_600, ClockCheck.offsetMillis(1_000_000, 1_000_200, 996));
    }

    @Test
    void judgeRefusesBeyondTheLimitUnlessTheModeIsWarn() {
        long oneYear = 365L * 24 * 3600 * 1000;
        assertNull(ClockCheck.judge(ClockCheck.MAX_OFFSET_MILLIS, null));
        assertNull(ClockCheck.judge(-ClockCheck.MAX_OFFSET_MILLIS, null));
        assertNotNull(ClockCheck.judge(ClockCheck.MAX_OFFSET_MILLIS + 1, null));
        assertNotNull(ClockCheck.judge(-ClockCheck.MAX_OFFSET_MILLIS - 1, "enforce"));
        assertNull(ClockCheck.judge(-oneYear, "warn"));
        assertNotNull(ClockCheck.judge(-oneYear, "WARN"));
    }
}
