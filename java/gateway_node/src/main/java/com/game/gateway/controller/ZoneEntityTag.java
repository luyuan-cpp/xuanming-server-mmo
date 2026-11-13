package com.game.gateway.controller;

import org.springframework.http.ETag;

import java.util.List;
import java.util.OptionalLong;
import java.util.regex.Pattern;

/**
 * 区服行版本({@code zone_config.version})与 HTTP 实体标签的换算,以及整行 PUT 的 If-Match 解析(2026-11-12)。
 *
 * <p>ETag 是强标签 {@code "<版本号>"}(RFC 9110 §8.8.3)。这一行每被写一次版本号加一,所以它的 JSON 表示一变 ETag 就变,
 * {@code If-None-Match} 的 304 也因此可信。调用方只把 GET 拿到的 ETag 原样放进 If-Match,不要自己拼:引号里写什么不是契约。
 */
final class ZoneEntityTag {

    /** 本服务发出的版本号:正的十进制整数,不带前导零。 */
    private static final Pattern VERSION = Pattern.compile("[1-9][0-9]{0,18}");

    private ZoneEntityTag() {
    }

    /** 这一版本的强 ETag。 */
    static String of(long version) {
        return "\"" + version + "\"";
    }

    /**
     * 整行 PUT 的 If-Match 前提,返回它指明的版本号。只接受一个实体标签:
     * <ul>
     *   <li>没带、空白、或 {@code *}:428。{@code *} 只要求「行存在」,不指明基于哪个版本,挡不住两个运维先后 GET 再 PUT 时
     *       后写覆盖前写 —— 这正是要求 If-Match 的原因。</li>
     *   <li>不是实体标签(例如没加引号)、或带了不止一个:400。</li>
     *   <li>弱标签,或引号里不是本服务发出的版本号:返回 empty。If-Match 用强比较(RFC 9110 §13.1.1),这种标签与任何版本
     *       都不相等;调用方照样先判区服在不在(不在回 404 优先,§13.2.1),在就回 412。</li>
     * </ul>
     *
     * @param ifMatch If-Match 头的原值;请求里有多行 If-Match 时是 Spring 用逗号连起来的整串
     */
    static OptionalLong requiredVersion(String ifMatch) {
        if (ifMatch == null || ifMatch.isBlank()) {
            throw AdminProblems.ifMatchRequired("If-Match is required: send the ETag returned by GET /admin/zones/{id}");
        }
        List<ETag> tags = ETag.parse(ifMatch);
        if (tags.stream().anyMatch(ETag::isWildcard)) {
            throw AdminProblems.ifMatchRequired(
                    "If-Match must name the ETag returned by GET /admin/zones/{id}; * is not accepted");
        }
        if (tags.size() != 1) {
            throw AdminProblems.ifMatchInvalid("If-Match must carry exactly one quoted entity tag, as returned by GET");
        }
        ETag tag = tags.get(0);
        if (tag.weak() || !VERSION.matcher(tag.tag()).matches()) {
            return OptionalLong.empty();
        }
        try {
            return OptionalLong.of(Long.parseLong(tag.tag()));
        } catch (NumberFormatException beyondLongRange) {
            return OptionalLong.empty();
        }
    }
}
