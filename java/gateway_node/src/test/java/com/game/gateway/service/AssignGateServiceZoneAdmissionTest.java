package com.game.gateway.service;

import com.game.gateway.dto.AssignGateRequest;
import com.game.gateway.dto.AssignGateResponse;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.grpc.LoginRpcClient;
import com.game.gateway.repository.ZoneConfigRepository;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;

import java.util.ArrayList;
import java.util.List;
import java.util.Optional;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyInt;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * 区服准入闭环(2026-08):zone_config 的运维状态必须在 /api/assign-gate 和
 * /api/queue-status 上 fail-closed,而不是只影响 /api/server-list 展示。
 */
@ExtendWith(MockitoExtension.class)
class AssignGateServiceZoneAdmissionTest {

    @Mock
    private LoginRpcClient rpc;

    @Mock
    private ZoneConfigRepository zoneConfigRepo;

    private AssignGateService service;

    @BeforeEach
    void setUp() {
        service = new AssignGateService(rpc, zoneConfigRepo);
    }

    private static ZoneConfig zone(long id, int manualStatus) {
        ZoneConfig z = new ZoneConfig();
        z.setZoneId(id);
        z.setName("zone-" + id);
        z.setManualStatus(manualStatus);
        return z;
    }

    private static AssignGateRequest request(int zoneId) {
        AssignGateRequest req = new AssignGateRequest();
        req.setZoneId(zoneId);
        req.setAccount("acc");
        req.setDeviceId("dev");
        return req;
    }

    private static LoginRpcClient.AssignGateResponseProto admitted() {
        var rsp = new LoginRpcClient.AssignGateResponseProto();
        rsp.status = 0;
        rsp.ip = "10.0.0.8";
        rsp.port = 18000;
        rsp.tokenPayload = new byte[] {1};
        rsp.tokenSignature = new byte[] {2};
        rsp.tokenDeadline = 123L;
        return rsp;
    }

    @Test
    void openZone_forwardsToLogin() {
        when(zoneConfigRepo.findById(1L)).thenReturn(Optional.of(zone(1L, 0)));
        when(rpc.assignGate(any(), anyInt())).thenReturn(admitted());

        AssignGateResponse out = service.assignGate(request(1));

        assertEquals(0, out.getCode());
        assertEquals("10.0.0.8", out.getGateIp());
        verify(rpc).assignGate(any(), anyInt());
    }

    @Test
    void maintenanceZone_rejectedWithoutRpc() {
        when(zoneConfigRepo.findById(1L)).thenReturn(Optional.of(zone(1L, 1)));

        AssignGateResponse out = service.assignGate(request(1));

        assertEquals(503, out.getCode());
        assertEquals("zone_maintenance", out.getError());
        verify(rpc, never()).assignGate(any(), anyInt());
    }

    @Test
    void closedZone_rejected() {
        when(zoneConfigRepo.findById(1L)).thenReturn(Optional.of(zone(1L, 2)));

        AssignGateResponse out = service.assignGate(request(1));

        assertEquals(503, out.getCode());
        assertEquals("zone_closed", out.getError());
    }

    @Test
    void previewZone_rejected() {
        when(zoneConfigRepo.findById(1L)).thenReturn(Optional.of(zone(1L, 3)));

        AssignGateResponse out = service.assignGate(request(1));

        assertEquals(503, out.getCode());
        assertEquals("zone_not_open", out.getError());
    }

    @Test
    void unknownZone_notFound() {
        when(zoneConfigRepo.findById(9L)).thenReturn(Optional.empty());

        AssignGateResponse out = service.assignGate(request(9));

        assertEquals(404, out.getCode());
        assertEquals("zone_not_found", out.getError());
        verify(rpc, never()).assignGate(any(), anyInt());
    }

    @Test
    void autoZone_skipsAdmissionCheck() {
        when(rpc.assignGate(any(), anyInt())).thenReturn(admitted());

        AssignGateResponse out = service.assignGate(request(0));

        assertEquals(0, out.getCode());
        verify(rpc).assignGate(any(), anyInt());
        verify(zoneConfigRepo, never()).findById(any());
    }

    @Test
    void repeatedQueueStatusPolls_shareShortAdmissionCache() {
        when(zoneConfigRepo.findById(1L)).thenReturn(Optional.of(zone(1L, 1)));

        AssignGateResponse first = service.queryQueueStatus("tok", 1);
        AssignGateResponse second = service.queryQueueStatus("tok", 1);

        assertEquals(503, first.getCode());
        assertEquals("zone_maintenance", first.getError());
        assertEquals(503, second.getCode());
        assertEquals("zone_maintenance", second.getError());
        verify(zoneConfigRepo, times(1)).findById(1L);
        verify(rpc, never()).queryQueueStatus(any(), anyInt());
    }

    @Test
    void concurrentSameZonePolls_mergeDatabaseLookup() throws Exception {
        int callers = 8;
        CountDownLatch ready = new CountDownLatch(callers);
        CountDownLatch start = new CountDownLatch(1);
        CountDownLatch databaseEntered = new CountDownLatch(1);
        CountDownLatch releaseDatabase = new CountDownLatch(1);
        when(zoneConfigRepo.findById(1L)).thenAnswer(invocation -> {
            databaseEntered.countDown();
            assertTrue(releaseDatabase.await(5, TimeUnit.SECONDS));
            return Optional.of(zone(1L, 1));
        });

        ExecutorService pool = Executors.newFixedThreadPool(callers);
        try {
            List<Future<AssignGateResponse>> results = new ArrayList<>();
            for (int i = 0; i < callers; i++) {
                results.add(pool.submit(() -> {
                    ready.countDown();
                    assertTrue(start.await(5, TimeUnit.SECONDS));
                    return service.queryQueueStatus("tok", 1);
                }));
            }
            assertTrue(ready.await(5, TimeUnit.SECONDS));
            start.countDown();
            assertTrue(databaseEntered.await(5, TimeUnit.SECONDS));
            releaseDatabase.countDown();

            for (Future<AssignGateResponse> result : results) {
                AssignGateResponse out = result.get(5, TimeUnit.SECONDS);
                assertEquals(503, out.getCode());
                assertEquals("zone_maintenance", out.getError());
            }
            verify(zoneConfigRepo, times(1)).findById(1L);
        } finally {
            releaseDatabase.countDown();
            pool.shutdownNow();
        }
    }

    @Test
    void admissionCache_refreshesAfterOneSecondTtl() throws Exception {
        when(zoneConfigRepo.findById(1L)).thenReturn(Optional.of(zone(1L, 1)));

        assertEquals(503, service.queryQueueStatus("tok", 1).getCode());
        Thread.sleep(1100);
        assertEquals(503, service.queryQueueStatus("tok", 1).getCode());

        verify(zoneConfigRepo, times(2)).findById(1L);
    }

    @Test
    void databaseFailure_isFailClosedAndNotCached() {
        when(zoneConfigRepo.findById(1L))
                .thenThrow(new RuntimeException("db down"))
                .thenReturn(Optional.of(zone(1L, 1)));

        AssignGateResponse failed = service.queryQueueStatus("tok", 1);
        AssignGateResponse retried = service.queryQueueStatus("tok", 1);

        assertEquals(500, failed.getCode());
        assertEquals("zone_admission_unavailable", failed.getError());
        assertEquals(503, retried.getCode());
        assertEquals("zone_maintenance", retried.getError());
        verify(zoneConfigRepo, times(2)).findById(1L);
    }
}
