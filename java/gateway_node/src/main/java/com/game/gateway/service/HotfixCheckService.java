package com.game.gateway.service;

import com.game.gateway.config.HotfixProperties;
import com.game.gateway.config.HotfixProperties.PlatformRelease;
import com.game.gateway.dto.HotfixCheckRequest;
import com.game.gateway.dto.HotfixCheckResponse;
import org.springframework.http.HttpStatus;
import org.springframework.stereotype.Service;
import org.springframework.web.server.ResponseStatusException;

import java.util.Map;
import java.util.regex.Pattern;

@Service
public class HotfixCheckService {

    // 数字段版本号,最多四段。每段不超过 9 位,比较时不会溢出 int。
    private static final Pattern VERSION = Pattern.compile("\\d{1,9}(\\.\\d{1,9}){0,3}");

    private final Map<String, PlatformRelease> releases;

    public HotfixCheckService(HotfixProperties props) {
        props.getPlatforms().forEach(HotfixCheckService::validate);
        this.releases = Map.copyOf(props.getPlatforms());
    }

    public HotfixCheckResponse check(HotfixCheckRequest req) {
        String platform = req.getPlatform();
        String clientVersion = req.getClientVersion();
        if (platform == null || platform.isBlank() || clientVersion == null || !VERSION.matcher(clientVersion).matches()) {
            throw new ResponseStatusException(HttpStatus.BAD_REQUEST, "platform and a numeric clientVersion are required");
        }
        HotfixCheckResponse resp = new HotfixCheckResponse();
        PlatformRelease release = releases.get(platform);
        if (release == null) {
            return resp;
        }
        resp.setLatestVersion(release.getLatestVersion());
        if (compare(clientVersion, release.getLatestVersion()) >= 0) {
            return resp;
        }
        resp.setNeedUpdate(true);
        resp.setForceUpdate(release.getMinVersion() != null && compare(clientVersion, release.getMinVersion()) < 0);
        resp.setPatchUrl(release.getPatchUrl());
        resp.setChangelog(release.getChangelog());
        return resp;
    }

    // 配错要在部署时暴露:运行时才发现的话,客户端要么收到一个下载不了的补丁,要么一直收不到更新。
    private static void validate(String platform, PlatformRelease release) {
        String latest = release.getLatestVersion();
        if (latest == null || !VERSION.matcher(latest).matches()) {
            throw new IllegalStateException("hotfix.platforms." + platform + ".latest-version must be numeric segments: " + latest);
        }
        String min = release.getMinVersion();
        if (min != null && (!VERSION.matcher(min).matches() || compare(min, latest) > 0)) {
            throw new IllegalStateException("hotfix.platforms." + platform + ".min-version must be numeric segments, not above latest-version: " + min);
        }
        if (release.getPatchUrl() == null || release.getPatchUrl().isBlank()) {
            throw new IllegalStateException("hotfix.platforms." + platform + ".patch-url is required");
        }
    }

    // 按段比较,缺的段算 0:1.10.0 比 1.9.9 新,1.2 等于 1.2.0。
    static int compare(String left, String right) {
        String[] a = left.split("\\.");
        String[] b = right.split("\\.");
        for (int i = 0; i < Math.max(a.length, b.length); i++) {
            int x = i < a.length ? Integer.parseInt(a[i]) : 0;
            int y = i < b.length ? Integer.parseInt(b[i]) : 0;
            if (x != y) {
                return Integer.compare(x, y);
            }
        }
        return 0;
    }
}
