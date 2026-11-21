package com.game.config.controller;

import com.game.config.service.TableService;
import jakarta.servlet.http.HttpServletRequest;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.Map;

/**
 * Hot-reload endpoint for config tables.
 * POST /api/reload triggers a full table reload.
 * 鉴权: 配置了 config.reload.api-key 时强制校验 X-Admin-Key(恒定时间比对);
 * 未配置密钥时仅允许直连本机回环(拒绝经代理转发或远端调用),防止未授权热更。
 */
@RestController
@RequestMapping("/api/reload")
public class ReloadController {

    private final TableService tableService;
    private final String reloadApiKey;

    public ReloadController(TableService tableService,
                            @Value("${config.reload.api-key:}") String reloadApiKey) {
        this.tableService = tableService;
        this.reloadApiKey = reloadApiKey == null ? "" : reloadApiKey.trim();
    }

    @PostMapping
    public ResponseEntity<Map<String, Object>> reload(
            @RequestHeader(value = "X-Admin-Key", required = false) String suppliedKey,
            HttpServletRequest request) {
        if (!reloadApiKey.isEmpty()) {
            if (suppliedKey == null || !MessageDigest.isEqual(
                    reloadApiKey.getBytes(StandardCharsets.UTF_8),
                    suppliedKey.getBytes(StandardCharsets.UTF_8))) {
                return ResponseEntity.status(HttpStatus.UNAUTHORIZED)
                    .body(Map.of("status", "unauthorized"));
            }
        } else {
            String forwarded = request.getHeader("X-Forwarded-For");
            String remoteAddr = request.getRemoteAddr();
            if ((forwarded != null && !forwarded.isBlank()) || !isLoopback(remoteAddr)) {
                return ResponseEntity.status(HttpStatus.FORBIDDEN)
                    .body(Map.of("status", "forbidden"));
            }
        }
        tableService.reload();
        return ResponseEntity.ok(Map.of(
            "status", "ok",
            "loadVersion", tableService.getLoadVersion(),
            "lastLoadTimeMs", tableService.getLastLoadTimeMs()
        ));
    }

    private static boolean isLoopback(String addr) {
        return "127.0.0.1".equals(addr)
            || "::1".equals(addr)
            || "0:0:0:0:0:0:0:1".equals(addr);
    }
}
