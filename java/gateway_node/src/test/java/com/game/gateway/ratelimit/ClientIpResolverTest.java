package com.game.gateway.ratelimit;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

import jakarta.servlet.http.HttpServletRequest;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * 钉死"X-Forwarded-For 只在可信代理后才采信"这一安全属性。
 *
 * <p>回归的是历史缺陷:旧的 LoginController.extractIp 无条件采信 XFF 并取**最左**元素
 * —— 最左元素是客户端自己写的,于是每请求带一个随机 XFF 就能让每次都命中全新空桶,
 * IP 维度限流形同虚设,并把 Bucket4j 的 rl:ip:* key 空间变成攻击者可控的无限集合。
 */
class ClientIpResolverTest {

    private static HttpServletRequest req(String peer, String xff) {
        HttpServletRequest http = mock(HttpServletRequest.class);
        when(http.getRemoteAddr()).thenReturn(peer);
        when(http.getHeader("X-Forwarded-For")).thenReturn(xff);
        return http;
    }

    @Test
    @DisplayName("未配置可信代理时,伪造的 XFF 必须被完全忽略")
    void ignoresForgedHeaderWhenNoTrustedProxies() {
        ClientIpResolver r = ClientIpResolver.forTest();
        assertEquals("203.0.113.9", r.resolve(req("203.0.113.9", "1.2.3.4")));
        // 攻击者每次换一个随机值也拿不到新桶。
        assertEquals("203.0.113.9", r.resolve(req("203.0.113.9", "9.9.9.9, 8.8.8.8")));
    }

    @Test
    @DisplayName("对端不是可信代理时,即使配了白名单也不采信 XFF")
    void ignoresHeaderFromUntrustedPeer() {
        ClientIpResolver r = ClientIpResolver.forTest("10.0.0.0/8");
        assertEquals("203.0.113.9", r.resolve(req("203.0.113.9", "1.2.3.4")));
    }

    @Test
    @DisplayName("可信代理后:从右往左剥可信跳,取第一个不可信地址")
    void takesRightmostUntrustedHop() {
        ClientIpResolver r = ClientIpResolver.forTest("10.0.0.0/8");
        // 链路:真实客户端 198.51.100.7 → 外层代理 203.0.113.1 → 内网 10.1.1.1 → 我们。
        // 10.1.1.1 是可信跳被剥掉,取 203.0.113.1 —— 它是我们能确认未被伪造的最外侧地址。
        // 绝不能取最左的 198.51.100.7:那一段是客户端可任意书写的。
        assertEquals("203.0.113.1",
                r.resolve(req("10.0.0.5", "198.51.100.7, 203.0.113.1, 10.1.1.1")));
    }

    @Test
    @DisplayName("可信代理后且只有一跳:取该跳")
    void singleHopBehindTrustedProxy() {
        ClientIpResolver r = ClientIpResolver.forTest("10.0.0.0/8");
        assertEquals("198.51.100.7", r.resolve(req("10.0.0.5", "198.51.100.7")));
    }

    @Test
    @DisplayName("可信代理但无 XFF / 空 XFF:退回 socket 对端")
    void fallsBackToPeerWhenHeaderAbsent() {
        ClientIpResolver r = ClientIpResolver.forTest("10.0.0.0/8");
        assertEquals("10.0.0.5", r.resolve(req("10.0.0.5", null)));
        assertEquals("10.0.0.5", r.resolve(req("10.0.0.5", "   ")));
    }

    @Test
    @DisplayName("畸形 XFF 段不被当作可信地址")
    void malformedHopsAreNotTrusted() {
        ClientIpResolver r = ClientIpResolver.forTest("10.0.0.0/8");
        // 非字面量地址(不做 DNS 解析)按"不可信"处理,于是被当成客户端地址返回 ——
        // 它进的是限流桶 key,不是任何安全判定,返回原样字符串是安全的。
        assertEquals("evil.example.com", r.resolve(req("10.0.0.5", "evil.example.com")));
    }

    @Test
    @DisplayName("畸形 CIDR 被忽略,不会把整个白名单变成放行")
    void malformedCidrIsIgnored() {
        ClientIpResolver r = ClientIpResolver.forTest("not-a-cidr");
        // 白名单实际为空 → 完全不信任 XFF。
        assertEquals("10.0.0.5", r.resolve(req("10.0.0.5", "1.2.3.4")));
    }

    @Test
    @DisplayName("CIDR 前缀按位比较,边界不越界")
    void cidrPrefixBoundary() {
        ClientIpResolver r = ClientIpResolver.forTest("192.168.1.0/24");
        // 192.168.1.5 在网段内 → 可信 → 解析 XFF。
        assertEquals("198.51.100.7", r.resolve(req("192.168.1.5", "198.51.100.7")));
        // 192.168.2.5 不在网段内 → 不可信 → 忽略 XFF。
        assertEquals("192.168.2.5", r.resolve(req("192.168.2.5", "198.51.100.7")));
    }
}
