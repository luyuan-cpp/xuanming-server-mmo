package com.game.gateway.service;

import com.game.gateway.config.HotfixProperties;
import com.game.gateway.config.HotfixProperties.PlatformRelease;
import com.game.gateway.dto.HotfixCheckRequest;
import com.game.gateway.dto.HotfixCheckResponse;
import org.junit.jupiter.api.Test;
import org.springframework.http.HttpStatus;
import org.springframework.web.server.ResponseStatusException;

import static org.junit.jupiter.api.Assertions.*;

class HotfixCheckServiceTest {

    private static PlatformRelease release(String latest, String min, String url) {
        PlatformRelease release = new PlatformRelease();
        release.setLatestVersion(latest);
        release.setMinVersion(min);
        release.setPatchUrl(url);
        release.setChangelog("修复若干问题");
        return release;
    }

    private static HotfixCheckService service(PlatformRelease android) {
        HotfixProperties props = new HotfixProperties();
        props.getPlatforms().put("android", android);
        return new HotfixCheckService(props);
    }

    private static HotfixCheckRequest request(String platform, String version) {
        HotfixCheckRequest req = new HotfixCheckRequest();
        req.setPlatform(platform);
        req.setClientVersion(version);
        return req;
    }

    @Test
    void platformWithoutReleaseLineNeedsNoUpdate() {
        HotfixCheckResponse resp = new HotfixCheckService(new HotfixProperties()).check(request("android", "1.0.0"));
        assertFalse(resp.isNeedUpdate());
        assertFalse(resp.isForceUpdate());
        assertNull(resp.getLatestVersion(), "没有发布线就不能编一个版本号");
    }

    @Test
    void upToDateClientNeedsNoUpdate() {
        HotfixCheckResponse resp = service(release("1.2.3", "1.2.0", "https://cdn/p.zip")).check(request("android", "1.2.3"));
        assertFalse(resp.isNeedUpdate());
        assertEquals("1.2.3", resp.getLatestVersion());
        assertNull(resp.getPatchUrl());
    }

    @Test
    void olderClientGetsPatchWithoutForceAboveMinVersion() {
        HotfixCheckResponse resp = service(release("1.2.3", "1.2.0", "https://cdn/p.zip")).check(request("android", "1.2.1"));
        assertTrue(resp.isNeedUpdate());
        assertFalse(resp.isForceUpdate());
        assertEquals("https://cdn/p.zip", resp.getPatchUrl());
        assertEquals("修复若干问题", resp.getChangelog());
    }

    @Test
    void clientBelowMinVersionIsForced() {
        HotfixCheckResponse resp = service(release("1.2.3", "1.2.0", "https://cdn/p.zip")).check(request("android", "1.1.9"));
        assertTrue(resp.isNeedUpdate());
        assertTrue(resp.isForceUpdate());
    }

    @Test
    void noMinVersionMeansNoForce() {
        HotfixCheckResponse resp = service(release("1.2.3", null, "https://cdn/p.zip")).check(request("android", "0.0.1"));
        assertTrue(resp.isNeedUpdate());
        assertFalse(resp.isForceUpdate());
    }

    @Test
    void versionsCompareBySegmentNotAsText() {
        assertTrue(HotfixCheckService.compare("1.10.0", "1.9.9") > 0);
        assertEquals(0, HotfixCheckService.compare("1.2", "1.2.0"));
        assertTrue(HotfixCheckService.compare("1.2.0.1", "1.2") > 0);
    }

    @Test
    void malformedRequestIsRejected() {
        HotfixCheckService service = service(release("1.2.3", null, "https://cdn/p.zip"));
        for (HotfixCheckRequest bad : new HotfixCheckRequest[] {
                request(null, "1.0.0"), request(" ", "1.0.0"), request("android", null),
                request("android", "1.0.0-beta"), request("android", "1.2.3.4.5"), request("android", "1234567890") }) {
            ResponseStatusException e = assertThrows(ResponseStatusException.class, () -> service.check(bad));
            assertEquals(HttpStatus.BAD_REQUEST, e.getStatusCode());
        }
    }

    @Test
    void misconfiguredReleaseRefusesToStart() {
        assertThrows(IllegalStateException.class, () -> service(release(null, null, "https://cdn/p.zip")));
        assertThrows(IllegalStateException.class, () -> service(release("v1", null, "https://cdn/p.zip")));
        assertThrows(IllegalStateException.class, () -> service(release("1.0.0", "x", "https://cdn/p.zip")));
        assertThrows(IllegalStateException.class, () -> service(release("1.0.0", "1.0.1", "https://cdn/p.zip")));
        assertThrows(IllegalStateException.class, () -> service(release("1.0.0", null, " ")));
    }
}
