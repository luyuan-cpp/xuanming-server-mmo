package com.game.gateway.etcd;

import com.fasterxml.jackson.annotation.JsonIgnoreProperties;
import com.fasterxml.jackson.annotation.JsonProperty;
import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.game.gateway.dto.AutoZoneStatus;
import io.etcd.jetcd.ByteSequence;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * 一个网关副本对一个 zone 的健康汇总,写在 etcd 里给所有网关读(2026-11-11,服务器缺口清单第 29 条;
 * docs/design/gateway-zone-health-publish-20261111.md §4.1)。
 *
 * <p>键:{@code /mmorpg/zone_health/zone/<zoneId>/publisher/<instanceId>},挂在发布进程的租约上,进程没了记录随之消失。
 * 值:普通 JSON,字段只做加法演进(读方忽略不认识的字段):
 * <ul>
 *   <li>{@code status}:HEALTHY / DEGRADED / DOWN,判定同原探测(没有 gate → DOWN;有 gate 没 scene → DEGRADED)。
 *       发布者从不写 UNKNOWN:没有记录就是不知道;</li>
 *   <li>{@code onlineCount}:本 zone 全部 gate 的 playerCount 之和;</li>
 *   <li>{@code capacity}:负载档的分母,{@code ZoneHealthProbeService.effectiveCapacity(zone_config.capacity, 本 zone 的 gate)};
 *       0 = 没有上限,负载档恒为流畅。</li>
 * </ul>
 * 构造即校验:不合规的值既写不出去,也读不进来(读方按坏记录处理,整次读失败)。
 */
@JsonIgnoreProperties(ignoreUnknown = true)
public record ZoneHealthRecord(
        @JsonProperty("zoneId") long zoneId,
        @JsonProperty("status") AutoZoneStatus status,
        @JsonProperty("onlineCount") long onlineCount,
        @JsonProperty("capacity") long capacity) {

    /** 记录键的公共前缀;读方 list + watch 它,前缀下只有记录键。 */
    public static final String PREFIX = "/mmorpg/zone_health/zone/";

    private static final Pattern KEY = Pattern.compile(
            "^" + Pattern.quote(PREFIX) + "([1-9][0-9]{0,18})/publisher/([A-Za-z0-9._-]+)$");
    private static final ObjectMapper MAPPER = new ObjectMapper();

    public ZoneHealthRecord {
        if (zoneId <= 0) {
            throw new IllegalArgumentException("zone health record needs a positive zoneId, got " + zoneId);
        }
        if (status == null || status == AutoZoneStatus.UNKNOWN) {
            throw new IllegalArgumentException("zone health record status must be HEALTHY, DEGRADED or DOWN, got " + status);
        }
        if (onlineCount < 0 || capacity < 0) {
            throw new IllegalArgumentException("zone health record counts must not be negative: onlineCount="
                    + onlineCount + ", capacity=" + capacity);
        }
    }

    /** 发布者 {@code publisherId} 为 {@code zoneId} 写的那条记录的键。 */
    public static String key(long zoneId, String publisherId) {
        String key = PREFIX + zoneId + "/publisher/" + publisherId;
        if (!KEY.matcher(key).matches()) {
            throw new IllegalArgumentException("invalid zone health key " + key);
        }
        return key;
    }

    /** 写进 etcd 的值。 */
    public String encode() {
        try {
            return MAPPER.writeValueAsString(this);
        } catch (JsonProcessingException e) {
            throw new IllegalStateException("cannot encode zone health record for zone " + zoneId, e);
        }
    }

    /**
     * 解码 etcd 里的一条记录,供 {@link EtcdPrefixMirror} 用。键形不对、JSON 解析失败、键里的 zone 与值不一致、
     * 值不合规,都抛异常(坏记录);从不返回 null:{@link #PREFIX} 下只应有记录键。
     */
    static ZoneHealthRecord decode(String key, ByteSequence value) throws IOException {
        Matcher m = KEY.matcher(key);
        if (!m.matches()) {
            throw new IllegalArgumentException("unexpected key under " + PREFIX + ": " + key);
        }
        long keyZoneId = Long.parseLong(m.group(1));
        ZoneHealthRecord record = MAPPER.readValue(value.toString(StandardCharsets.UTF_8), ZoneHealthRecord.class);
        if (record == null) {
            throw new IllegalArgumentException("zone health JSON resolved to null");
        }
        if (record.zoneId() != keyZoneId) {
            throw new IllegalArgumentException("zone health record for zone " + record.zoneId()
                    + " is stored under zone " + keyZoneId);
        }
        return record;
    }
}
