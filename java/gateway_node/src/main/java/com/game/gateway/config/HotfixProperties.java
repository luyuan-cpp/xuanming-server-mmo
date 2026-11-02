package com.game.gateway.config;

import org.springframework.boot.context.properties.ConfigurationProperties;
import org.springframework.context.annotation.Configuration;

import java.util.HashMap;
import java.util.Map;

/** 客户端热更检查的发布线,键是客户端上报的 platform。校验在 HotfixCheckService 的构造函数里,配错拒绝启动。 */
@Configuration
@ConfigurationProperties(prefix = "hotfix")
public class HotfixProperties {

    private Map<String, PlatformRelease> platforms = new HashMap<>();

    public Map<String, PlatformRelease> getPlatforms() { return platforms; }
    public void setPlatforms(Map<String, PlatformRelease> platforms) { this.platforms = platforms; }

    public static class PlatformRelease {
        private String latestVersion;
        private String minVersion;
        private String patchUrl;
        private String changelog;

        public String getLatestVersion() { return latestVersion; }
        public void setLatestVersion(String latestVersion) { this.latestVersion = latestVersion; }

        /** 低于它的客户端必须更新。不配表示没有强更线。 */
        public String getMinVersion() { return minVersion; }
        public void setMinVersion(String minVersion) { this.minVersion = minVersion; }

        public String getPatchUrl() { return patchUrl; }
        public void setPatchUrl(String patchUrl) { this.patchUrl = patchUrl; }

        public String getChangelog() { return changelog; }
        public void setChangelog(String changelog) { this.changelog = changelog; }
    }
}
