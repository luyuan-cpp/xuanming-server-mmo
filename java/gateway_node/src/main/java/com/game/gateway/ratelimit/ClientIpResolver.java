package com.game.gateway.ratelimit;

import jakarta.servlet.http.HttpServletRequest;
import java.net.InetAddress;
import java.net.UnknownHostException;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.stereotype.Component;

/**
 * 解析客户端真实 IP,供限流分桶使用。
 *
 * <p><b>为什么不能直接读 X-Forwarded-For。</b>XFF 是一个**任何客户端都能自己写**的
 * 普通请求头。旧实现无条件采信它并取最左元素,而最左元素恰恰是客户端自己填的那一段
 * —— 于是每个请求带一个随机 XFF 就能让 {@link AssignGateRateLimiter} 每次都命中一个
 * 全新的空桶,IP 维度限流(ip-rps/ip-burst)对任何会改 header 的客户端等于不存在;
 * 同时把 Bucket4j 的 {@code rl:ip:*} key 空间变成攻击者可控的无限集合。
 * 旧方法的 javadoc 声称"只在 server.forward-headers-strategy=native 时才采信",
 * 但代码从不查这个设置,配置里也没有该项 —— 契约与实现脱节。
 *
 * <p><b>正确做法(本类实现)。</b>
 * <ul>
 *   <li>默认(未配置可信代理)只用 {@code getRemoteAddr()} 的 socket 对端地址,
 *       完全忽略 XFF。这是 fail-closed:直接暴露端口部署时不会被伪造头骗到。</li>
 *   <li>只有当 socket 对端本身落在配置的可信代理网段内,才解析 XFF;
 *       且**从右往左**剥掉连续的可信跳,取第一个不可信地址 —— 那才是这条链路上
 *       我们能确认没被伪造的最外侧地址。从左往右取等于直接采信客户端输入。</li>
 * </ul>
 *
 * <p>配置示例(application.yaml):
 * <pre>
 * gate:
 *   rate-limit:
 *     trusted-proxies:
 *       - 10.0.0.0/8
 *       - 172.16.0.0/12
 * </pre>
 * 部署在 ingress / LB 之后时必须配置,否则所有请求会共用 LB 的那一个 IP 桶。
 */
@Component
public class ClientIpResolver {

    private static final Logger log = LoggerFactory.getLogger(ClientIpResolver.class);

    private final List<Cidr> trustedProxies;

    public ClientIpResolver(RateLimitProperties props) {
        this.trustedProxies = parseCidrs(props.getTrustedProxies());
        if (trustedProxies.isEmpty()) {
            log.info("ClientIpResolver: no trusted proxies configured — X-Forwarded-For is IGNORED, "
                    + "using socket peer address. If this gateway sits behind an ingress/LB, set "
                    + "gate.rate-limit.trusted-proxies or every request will share one IP bucket.");
        } else {
            log.info("ClientIpResolver: trusting X-Forwarded-For from {} proxy range(s)", trustedProxies.size());
        }
    }

    /** 返回用于限流分桶的客户端地址。永不返回 null。 */
    public String resolve(HttpServletRequest http) {
        String peer = http.getRemoteAddr();
        if (peer == null || peer.isBlank()) {
            return "unknown";
        }
        // 对端不是可信代理 → XFF 一律不可信,直接用 socket 地址。
        if (!isTrusted(peer)) {
            return peer;
        }
        String xff = http.getHeader("X-Forwarded-For");
        if (xff == null || xff.isBlank()) {
            return peer;
        }
        // 从右往左剥可信跳:XFF 的右侧是离我们最近、由可信代理追加的部分。
        String[] hops = xff.split(",");
        for (int i = hops.length - 1; i >= 0; i--) {
            String hop = hops[i].trim();
            if (hop.isEmpty()) {
                continue;
            }
            if (!isTrusted(hop)) {
                return hop;
            }
        }
        // 整条链都是可信代理地址(不该发生),退回 socket 对端。
        return peer;
    }

    private boolean isTrusted(String ip) {
        if (trustedProxies.isEmpty()) {
            return false;
        }
        byte[] addr = toBytes(ip);
        if (addr == null) {
            return false;
        }
        for (Cidr c : trustedProxies) {
            if (c.contains(addr)) {
                return true;
            }
        }
        return false;
    }

    private static byte[] toBytes(String ip) {
        // 只接受字面量地址,绝不做 DNS 解析 —— 否则一个畸形 XFF 就能把 Tomcat
        // 工作线程拖进一次名称解析。
        if (!isLiteralAddress(ip)) {
            return null;
        }
        try {
            return InetAddress.getByName(ip).getAddress();
        } catch (UnknownHostException e) {
            return null;
        }
    }

    private static boolean isLiteralAddress(String s) {
        boolean hasDigitOrColon = false;
        for (int i = 0; i < s.length(); i++) {
            char ch = s.charAt(i);
            boolean ok = (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')
                    || (ch >= 'A' && ch <= 'F') || ch == '.' || ch == ':' || ch == '%';
            if (!ok) {
                return false;
            }
            if (ch != '.' && ch != ':' && ch != '%') {
                hasDigitOrColon = true;
            }
        }
        return hasDigitOrColon;
    }

    private static List<Cidr> parseCidrs(List<String> specs) {
        List<Cidr> out = new ArrayList<>();
        if (specs == null) {
            return out;
        }
        for (String spec : specs) {
            if (spec == null || spec.isBlank()) {
                continue;
            }
            Cidr c = Cidr.parse(spec.trim());
            if (c == null) {
                log.error("ClientIpResolver: ignoring malformed trusted-proxy CIDR '{}'", spec);
                continue;
            }
            out.add(c);
        }
        return out;
    }

    /** 一个 CIDR 网段。按字节前缀比较,IPv4 / IPv6 同构处理。 */
    private static final class Cidr {
        private final byte[] network;
        private final int prefixBits;

        private Cidr(byte[] network, int prefixBits) {
            this.network = network;
            this.prefixBits = prefixBits;
        }

        static Cidr parse(String spec) {
            int slash = spec.indexOf('/');
            String addrPart = slash < 0 ? spec : spec.substring(0, slash);
            byte[] addr = toBytes(addrPart);
            if (addr == null) {
                return null;
            }
            int bits = addr.length * 8;
            if (slash >= 0) {
                try {
                    bits = Integer.parseInt(spec.substring(slash + 1).trim());
                } catch (NumberFormatException e) {
                    return null;
                }
                if (bits < 0 || bits > addr.length * 8) {
                    return null;
                }
            }
            return new Cidr(addr, bits);
        }

        boolean contains(byte[] addr) {
            // 地址族不同(v4 vs v6)直接不匹配,避免跨族误判。
            if (addr.length != network.length) {
                return false;
            }
            int fullBytes = prefixBits / 8;
            for (int i = 0; i < fullBytes; i++) {
                if (addr[i] != network[i]) {
                    return false;
                }
            }
            int remaining = prefixBits % 8;
            if (remaining == 0) {
                return true;
            }
            int mask = 0xFF << (8 - remaining);
            return (addr[fullBytes] & mask) == (network[fullBytes] & mask);
        }
    }

    /** 供单测直接构造,不经 Spring 容器。 */
    static ClientIpResolver forTest(String... cidrs) {
        RateLimitProperties p = new RateLimitProperties();
        p.setTrustedProxies(Arrays.asList(cidrs));
        return new ClientIpResolver(p);
    }
}
