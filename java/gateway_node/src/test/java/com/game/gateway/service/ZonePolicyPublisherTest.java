package com.game.gateway.service;

import com.game.gateway.config.HomeZoneProperties;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.repository.ZoneConfigRepository;
import io.micrometer.core.instrument.simple.SimpleMeterRegistry;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.Mockito.*;

class ZonePolicyPublisherTest {

    private static final long ZONE = 101L;

    /** 内存版 etcd:记下每次写。 */
    private final Map<String, byte[]> etcd = new HashMap<>();
    private int puts;
    private RuntimeException failGetFor101;

    private final ZonePolicyStore store = new ZonePolicyStore() {
        @Override
        public Optional<byte[]> get(String key) {
            if (failGetFor101 != null && key.equals(ZonePolicyPublisher.key(ZONE))) {
                throw failGetFor101;
            }
            return Optional.ofNullable(etcd.get(key));
        }

        @Override
        public void put(String key, byte[] value) {
            puts++;
            etcd.put(key, value);
        }
    };

    private ZoneConfigRepository repo;
    private SimpleMeterRegistry registry;

    @BeforeEach
    void setUp() {
        repo = mock(ZoneConfigRepository.class);
        registry = new SimpleMeterRegistry();
    }

    private ZonePolicyPublisher publisher(Long... homeZones) {
        HomeZoneProperties props = new HomeZoneProperties();
        props.setHomeZoneIds(List.of(homeZones));
        return new ZonePolicyPublisher(repo, props, store, registry, () -> 0L);
    }

    private void zoneRow(long zoneId, int capacity, boolean createRoleClosed) {
        ZoneConfig z = new ZoneConfig();
        z.setZoneId(zoneId);
        z.setName("zone-" + zoneId);
        z.setCapacity(capacity);
        z.setCreateRoleClosed(createRoleClosed);
        when(repo.findById(zoneId)).thenReturn(Optional.of(z));
    }

    private String stored(long zoneId) {
        byte[] raw = etcd.get(ZonePolicyPublisher.key(zoneId));
        return raw == null ? null : new String(raw, StandardCharsets.UTF_8);
    }

    private double count(String outcome) {
        return registry.get("gateway.zone_policy.publish").tag("outcome", outcome).counter().count();
    }

    // 写出来的值必须逐字等于 Go 侧的金样:字段名、顺序、类型都是两种语言之间的契约。
    @Test
    void writesTheGoContractShapeWhenAbsent() throws IOException {
        zoneRow(ZONE, 45000, true);

        publisher(ZONE).publish();

        String golden = Files.readString(findRepoFile("go/login/internal/logic/pkg/zonepolicy/testdata/policy.golden.json")).trim();
        assertEquals(golden, stored(ZONE));
        assertEquals(1, puts);
        assertEquals(1.0, count("written"));
    }

    // 键前缀与 Go 的 zonepolicy.KeyPrefix 逐字相同,改一边就要改另一边。
    @Test
    void keyPrefixMatchesGoReader() throws IOException {
        String source = Files.readString(findRepoFile("go/login/internal/logic/pkg/zonepolicy/zonepolicy.go"));
        Matcher m = Pattern.compile("(?m)^const KeyPrefix = \"([^\"]+)\"\\s*$").matcher(source);
        assertTrue(m.find(), "const KeyPrefix not found in zonepolicy.go");
        assertEquals(m.group(1), ZonePolicyPublisher.KEY_PREFIX);
        assertEquals("/mmorpg/zone_policy/zone/101", ZonePolicyPublisher.key(101));
    }

    // 现值与区服目录相同就不写(写法不同也算相同,比较的是内容);稳态零写入。
    @Test
    void unchangedValueIsNotRewritten() {
        zoneRow(ZONE, 45000, false);
        etcd.put(ZonePolicyPublisher.key(ZONE),
                "{ \"createRoleClosed\": false, \"capacity\": 45000 }".getBytes(StandardCharsets.UTF_8));
        ZonePolicyPublisher p = publisher(ZONE);

        p.publish();
        p.publish();

        assertEquals(0, puts);
        assertEquals(2.0, count("unchanged"));
    }

    // 运维改了开关或容量,下一拍写新值。
    @Test
    void directoryChangeIsRepublished() {
        zoneRow(ZONE, 45000, false);
        ZonePolicyPublisher p = publisher(ZONE);
        p.publish();

        zoneRow(ZONE, 45000, true);
        p.publish();

        assertEquals(2, puts);
        assertEquals("{\"capacity\":45000,\"createRoleClosed\":true}", stored(ZONE));
    }

    // 值被写坏(或是旧格式)时整值重写,不卡住。
    @Test
    void corruptValueIsOverwritten() {
        zoneRow(ZONE, 36000, false);
        etcd.put(ZonePolicyPublisher.key(ZONE), "not json".getBytes(StandardCharsets.UTF_8));

        publisher(ZONE).publish();

        assertEquals("{\"capacity\":36000,\"createRoleClosed\":false}", stored(ZONE));
    }

    // 每个副本只写自己所在的区,别的区一行都不读。
    @Test
    void onlyHomeZonesArePublished() {
        zoneRow(ZONE, 45000, false);

        publisher(ZONE).publish();

        verify(repo).findById(ZONE);
        verify(repo, never()).findById(102L);
        assertEquals(List.of(ZonePolicyPublisher.key(ZONE)), List.copyOf(etcd.keySet()));
    }

    // 区服目录里没有本区:不写也不删。
    @Test
    void missingZoneWritesNothing() {
        when(repo.findById(ZONE)).thenReturn(Optional.empty());

        publisher(ZONE).publish();

        assertTrue(etcd.isEmpty());
        assertEquals(1.0, count("zone_missing"));
    }

    // 一个区失败不影响下一个区;失败的区下一拍重试。
    @Test
    void failureInOneZoneDoesNotStopTheNext() {
        zoneRow(ZONE, 45000, false);
        zoneRow(102L, 20000, false);
        failGetFor101 = new IllegalStateException("etcd get timed out");
        ZonePolicyPublisher p = publisher(ZONE, 102L);

        p.publish();
        assertNull(stored(ZONE));
        assertEquals("{\"capacity\":20000,\"createRoleClosed\":false}", stored(102L));
        assertEquals(1.0, count("error"));

        failGetFor101 = null;
        p.publish();
        assertEquals("{\"capacity\":45000,\"createRoleClosed\":false}", stored(ZONE));
    }

    // 库里手改出负容量时按 0 发布:Go 侧容量是 uint32,负数会解析失败、login 一直用旧值。
    @Test
    void negativeCapacityIsPublishedAsZero() {
        zoneRow(ZONE, -5, false);

        publisher(ZONE).publish();

        assertEquals("{\"capacity\":0,\"createRoleClosed\":false}", stored(ZONE));
    }

    /** 从工作目录(surefire 下是模块目录 java/gateway_node)往上找仓库里的文件。 */
    private static Path findRepoFile(String relative) {
        for (Path dir = Path.of(System.getProperty("user.dir")).toAbsolutePath(); dir != null; dir = dir.getParent()) {
            Path candidate = dir.resolve(relative);
            if (Files.isRegularFile(candidate)) {
                return candidate;
            }
        }
        throw new AssertionError(relative + " not found above " + System.getProperty("user.dir"));
    }
}
