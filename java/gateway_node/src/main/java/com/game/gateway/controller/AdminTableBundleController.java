package com.game.gateway.controller;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.ObjectReader;
import com.fasterxml.jackson.databind.PropertyNamingStrategies;
import com.fasterxml.jackson.databind.annotation.JsonNaming;
import com.game.gateway.service.TablePointerStore;
import jakarta.servlet.http.HttpServletRequest;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.HttpStatus;
import org.springframework.web.bind.annotation.DeleteMapping;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PutMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RequestParam;
import org.springframework.web.bind.annotation.RestController;
import org.springframework.web.server.ResponseStatusException;

import java.time.Clock;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;
import java.util.function.Supplier;
import java.util.regex.Pattern;

/**
 * 运维/GM 配置表热更轨道指针管理入口(docs/design/config-table-hot-reload.md §6.1、§6.4):
 * <ul>
 *   <li>{@code GET /admin/tables}:列出全部轨道与 schema 线的指针、当前生效包与在线实例回报汇总;</li>
 *   <li>{@code PUT /admin/tables}:按 etcd {@code mod_revision} CAS 设置 {@code next}(原已生效目标折进 {@code current});</li>
 *   <li>{@code DELETE /admin/tables/next?track=...&schema_digest=...}:撤销未到点的 {@code next};</li>
 *   <li>{@code DELETE /admin/tables?track=...&schema_digest=...}:删除已无在线实例回报的旧 schema 线。</li>
 * </ul>
 * 鉴权沿用 {@code AdminApiKeyFilter}(X-Admin-Key);本接口不碰对象存储,只管 etcd 指针与状态汇总。
 */
@RestController
@RequestMapping("/admin/tables")
public class AdminTableBundleController {

    public static final String POINTER_PREFIX = "/mmorpg/tables/pointer/";
    public static final String STATUS_PREFIX = "/mmorpg/tables/status/";
    public static final long MIN_ACTIVATE_LEAD_MS = 60_000L;
    static final int MAX_REASON_CHARS = 256;
    static final int MAX_OPERATOR_CHARS = 64;
    static final String OPERATOR_HEADER = "X-Admin-Operator";

    private static final Set<String> VALID_TRACKS = Set.of("stable", "canary");
    private static final Pattern SCHEMA_DIGEST = Pattern.compile("[0-9a-f]{16}");
    private static final Pattern SHA256_HEX = Pattern.compile("[0-9a-f]{64}");
    private static final Pattern OPERATOR_CHARS = Pattern.compile("[A-Za-z0-9_.@-]+");

    private static final Logger log = LoggerFactory.getLogger(AdminTableBundleController.class);
    private static final ObjectMapper JSON = new ObjectMapper()
            .setSerializationInclusion(JsonInclude.Include.NON_NULL);
    private static final ObjectReader STRICT_JSON = JSON.reader()
            .with(DeserializationFeature.FAIL_ON_TRAILING_TOKENS);

    private final TablePointerStore store;
    private final Clock clock;

    @Autowired
    public AdminTableBundleController(TablePointerStore store) {
        this(store, Clock.systemUTC());
    }

    AdminTableBundleController(TablePointerStore store, Clock clock) {
        this.store = store;
        this.clock = clock;
    }

    @JsonNaming(PropertyNamingStrategies.SnakeCaseStrategy.class)
    public record PointerTarget(
            String contentDigest,
            String bundleSha256,
            long version,
            Long activateAtMs,
            Boolean immediate) {
    }

    @JsonNaming(PropertyNamingStrategies.SnakeCaseStrategy.class)
    public record PointerLine(
            PointerTarget current,
            PointerTarget next,
            long updatedAtMs,
            String updatedBy,
            String reason) {
    }

    @JsonNaming(PropertyNamingStrategies.SnakeCaseStrategy.class)
    public record PublishRequest(
            String track,
            String schemaDigest,
            String contentDigest,
            String bundleSha256,
            Long version,
            Long activateAtMs,
            Boolean immediate,
            String reason) {
    }

    @JsonNaming(PropertyNamingStrategies.SnakeCaseStrategy.class)
    public record PodIssue(String pod, String service, String state, String digest, String reason) {
    }

    @JsonNaming(PropertyNamingStrategies.SnakeCaseStrategy.class)
    public record StatusSummary(int total, int loaded, int prepared, int rejected, int needsRestart) {
    }

    @JsonNaming(PropertyNamingStrategies.SnakeCaseStrategy.class)
    public record LineView(
            String track,
            String schemaDigest,
            PointerTarget current,
            PointerTarget next,
            PointerTarget active,
            long updatedAtMs,
            String updatedBy,
            String reason,
            boolean valid,
            StatusSummary summary,
            List<PodIssue> rejectedPods,
            List<PodIssue> needsRestartPods) {
    }

    @JsonNaming(PropertyNamingStrategies.SnakeCaseStrategy.class)
    public record DeleteLineResult(String track, String schemaDigest, boolean deleted) {
    }

    private record LineKey(String track, String schemaDigest) implements Comparable<LineKey> {
        @Override
        public int compareTo(LineKey o) {
            int c = track.compareTo(o.track);
            return c != 0 ? c : schemaDigest.compareTo(o.schemaDigest);
        }
    }

    private record PodStatus(
            String pod,
            String service,
            String track,
            String schemaDigest,
            String loadedDigest,
            String pendingState,
            String pendingDigest,
            String pendingReason) {
    }

    @GetMapping
    public List<LineView> list() {
        long nowMs = clock.millis();
        Map<String, TablePointerStore.Entry> pointerRaw = call(() -> store.list(POINTER_PREFIX));
        Map<String, TablePointerStore.Entry> statusRaw = call(() -> store.list(STATUS_PREFIX));

        Map<LineKey, PointerLine> lines = new TreeMap<>();
        Map<LineKey, Boolean> lineValid = new LinkedHashMap<>();

        for (Map.Entry<String, TablePointerStore.Entry> entry : pointerRaw.entrySet()) {
            String rel = entry.getKey().substring(POINTER_PREFIX.length());
            int slash = rel.indexOf('/');
            if (slash <= 0 || slash == rel.length() - 1 || rel.indexOf('/', slash + 1) >= 0) {
                continue;
            }
            LineKey key = new LineKey(rel.substring(0, slash), rel.substring(slash + 1));
            PointerLine parsed = parsePointerLine(entry.getValue().value());
            if (parsed == null) {
                lines.put(key, new PointerLine(null, null, 0, "", ""));
                lineValid.put(key, false);
            } else {
                lines.put(key, parsed);
                lineValid.put(key, true);
            }
        }

        Map<LineKey, List<PodStatus>> podsByLine = new TreeMap<>();
        for (Map.Entry<String, TablePointerStore.Entry> entry : statusRaw.entrySet()) {
            String rel = entry.getKey().substring(STATUS_PREFIX.length());
            int slash = rel.indexOf('/');
            if (slash <= 0 || slash == rel.length() - 1) {
                continue;
            }
            String track = rel.substring(0, slash);
            String pod = rel.substring(slash + 1);
            PodStatus ps = parsePodStatus(track, pod, entry.getValue().value());
            if (ps == null || ps.schemaDigest() == null || ps.schemaDigest().isEmpty()) {
                continue;
            }
            LineKey key = new LineKey(track, ps.schemaDigest());
            podsByLine.computeIfAbsent(key, ignored -> new ArrayList<>()).add(ps);
            lines.putIfAbsent(key, new PointerLine(null, null, 0, "", ""));
            lineValid.putIfAbsent(key, true);
        }

        List<LineView> views = new ArrayList<>(lines.size());
        for (Map.Entry<LineKey, PointerLine> entry : lines.entrySet()) {
            LineKey key = entry.getKey();
            PointerLine line = entry.getValue();
            PointerTarget active = activeTarget(line, nowMs);
            List<PodStatus> pods = podsByLine.getOrDefault(key, List.of());

            int loaded = 0;
            int prepared = 0;
            int rejected = 0;
            int needsRestart = 0;
            List<PodIssue> rejectedPods = new ArrayList<>();
            List<PodIssue> needsRestartPods = new ArrayList<>();

            String desiredDigest = line.next() != null ? line.next().contentDigest()
                    : (active != null ? active.contentDigest() : "");

            for (PodStatus ps : pods) {
                String state = ps.pendingState() == null ? "" : ps.pendingState();
                if ("rejected".equals(state)) {
                    rejected++;
                    rejectedPods.add(new PodIssue(ps.pod(), ps.service(), state, ps.pendingDigest(), ps.pendingReason()));
                } else if ("needs_restart".equals(state)) {
                    needsRestart++;
                    needsRestartPods.add(new PodIssue(ps.pod(), ps.service(), state, ps.pendingDigest(), ps.pendingReason()));
                } else if ("prepared".equals(state)) {
                    prepared++;
                } else if (desiredDigest.isEmpty() || desiredDigest.equals(ps.loadedDigest())) {
                    loaded++;
                }
            }

            views.add(new LineView(
                    key.track(),
                    key.schemaDigest(),
                    line.current(),
                    line.next(),
                    active,
                    line.updatedAtMs(),
                    line.updatedBy(),
                    line.reason(),
                    lineValid.getOrDefault(key, true),
                    new StatusSummary(pods.size(), loaded, prepared, rejected, needsRestart),
                    rejectedPods,
                    needsRestartPods));
        }
        return views;
    }

    @PutMapping
    public LineView publish(@RequestBody PublishRequest body, HttpServletRequest request) {
        if (body == null) {
            throw badRequest("request body is required");
        }
        String track = requireTrack(body.track());
        String schemaDigest = requireSchemaDigest(body.schemaDigest());
        String contentDigest = requireSha256(body.contentDigest(), "content_digest");
        String bundleSha256 = requireSha256(body.bundleSha256(), "bundle_sha256");
        long version = body.version() == null ? 0L : body.version();
        if (version < 0) {
            throw badRequest("version must be non-negative");
        }
        String reason = validReason(body.reason());
        boolean immediate = Boolean.TRUE.equals(body.immediate());
        long nowMs = clock.millis();

        Long activateAtMs;
        if (immediate) {
            activateAtMs = null;
        } else {
            long minActivate = nowMs + MIN_ACTIVATE_LEAD_MS;
            long requested = body.activateAtMs() == null ? minActivate : body.activateAtMs();
            if (requested < minActivate) {
                throw badRequest("activate_at_ms must be at least 60 seconds in the future unless immediate=true");
            }
            activateAtMs = requested;
        }

        String key = pointerKey(track, schemaDigest);
        TablePointerStore.Entry existing = call(() -> store.get(key)).orElse(null);
        long expectedRev = existing == null ? 0L : existing.modRevision();
        PointerLine prev = existing == null ? null : parsePointerLine(existing.value());

        // 原来已生效的那一包折进 current(设计 §6.4)。
        PointerTarget foldedCurrent = prev == null ? null : activeTarget(prev, nowMs);
        if (foldedCurrent != null) {
            foldedCurrent = new PointerTarget(
                    foldedCurrent.contentDigest(),
                    foldedCurrent.bundleSha256(),
                    foldedCurrent.version(),
                    null,
                    null);
        }
        PointerTarget next = new PointerTarget(
                contentDigest,
                bundleSha256,
                version,
                activateAtMs,
                immediate ? Boolean.TRUE : null);

        String operator = operator(request);
        PointerLine updated = new PointerLine(foldedCurrent, next, nowMs, operator, reason.isEmpty() ? null : reason);
        String raw = toJson(updated);

        boolean ok = call(() -> store.compareAndPut(key, expectedRev, raw));
        if (!ok) {
            throw new ResponseStatusException(HttpStatus.CONFLICT,
                    "concurrent table pointer update detected; retry");
        }
        log.warn("AUDIT table bundle pointer set track={} schema_digest={} content_digest={} version={} immediate={} activate_at_ms={} reason=\"{}\" operator={} remote={}",
                track, schemaDigest, contentDigest.substring(0, 12), version, immediate, activateAtMs,
                reason, operator, request.getRemoteAddr());

        return new LineView(
                track,
                schemaDigest,
                updated.current(),
                updated.next(),
                activeTarget(updated, nowMs),
                updated.updatedAtMs(),
                updated.updatedBy(),
                updated.reason(),
                true,
                new StatusSummary(0, 0, 0, 0, 0),
                List.of(),
                List.of());
    }

    @DeleteMapping("/next")
    public LineView cancelNext(
            @RequestParam("track") String rawTrack,
            @RequestParam("schema_digest") String rawSchemaDigest,
            HttpServletRequest request) {
        String track = requireTrack(rawTrack);
        String schemaDigest = requireSchemaDigest(rawSchemaDigest);
        long nowMs = clock.millis();

        String key = pointerKey(track, schemaDigest);
        TablePointerStore.Entry existing = call(() -> store.get(key))
                .orElseThrow(() -> new ResponseStatusException(HttpStatus.NOT_FOUND, "pointer line not found"));
        PointerLine prev = parsePointerLine(existing.value());
        if (prev == null || prev.next() == null) {
            throw new ResponseStatusException(HttpStatus.NOT_FOUND, "no pending next target to cancel");
        }
        PointerTarget next = prev.next();
        if (Boolean.TRUE.equals(next.immediate()) || (next.activateAtMs() != null && nowMs >= next.activateAtMs())) {
            throw new ResponseStatusException(HttpStatus.CONFLICT,
                    "next target has already reached activate_at; publish a rollback instead");
        }

        String operator = operator(request);
        PointerLine updated = new PointerLine(prev.current(), null, nowMs, operator, "cancelled");
        boolean ok = call(() -> store.compareAndPut(key, existing.modRevision(), toJson(updated)));
        if (!ok) {
            throw new ResponseStatusException(HttpStatus.CONFLICT,
                    "concurrent table pointer update detected; retry");
        }
        log.warn("AUDIT table bundle next cancelled track={} schema_digest={} cancelled_digest={} operator={} remote={}",
                track, schemaDigest,
                next.contentDigest() != null && next.contentDigest().length() >= 12
                        ? next.contentDigest().substring(0, 12) : next.contentDigest(),
                operator, request.getRemoteAddr());

        return new LineView(
                track,
                schemaDigest,
                updated.current(),
                null,
                activeTarget(updated, nowMs),
                updated.updatedAtMs(),
                updated.updatedBy(),
                updated.reason(),
                true,
                new StatusSummary(0, 0, 0, 0, 0),
                List.of(),
                List.of());
    }

    @DeleteMapping
    public DeleteLineResult deleteLine(
            @RequestParam("track") String rawTrack,
            @RequestParam("schema_digest") String rawSchemaDigest,
            HttpServletRequest request) {
        String track = requireTrack(rawTrack);
        String schemaDigest = requireSchemaDigest(rawSchemaDigest);

        // 只要这条线还有在线实例在回报就拒绝删除(设计 §6.4:否则重启的进程会退回镜像里更旧的表)。
        String trackStatusPrefix = STATUS_PREFIX + track + "/";
        Map<String, TablePointerStore.Entry> statuses = call(() -> store.list(trackStatusPrefix));
        List<String> activePods = new ArrayList<>();
        for (Map.Entry<String, TablePointerStore.Entry> entry : statuses.entrySet()) {
            String pod = entry.getKey().substring(trackStatusPrefix.length());
            PodStatus ps = parsePodStatus(track, pod, entry.getValue().value());
            if (ps != null && schemaDigest.equals(ps.schemaDigest())) {
                activePods.add(pod);
            }
        }
        if (!activePods.isEmpty()) {
            throw new ResponseStatusException(HttpStatus.CONFLICT,
                    "schema line still has " + activePods.size() + " active reporting pod(s), e.g. " + activePods.get(0));
        }

        String key = pointerKey(track, schemaDigest);
        TablePointerStore.Entry existing = call(() -> store.get(key))
                .orElseThrow(() -> new ResponseStatusException(HttpStatus.NOT_FOUND, "pointer line not found"));
        boolean deleted = call(() -> store.compareAndDelete(key, existing.modRevision()));
        if (!deleted) {
            throw new ResponseStatusException(HttpStatus.CONFLICT,
                    "concurrent table pointer update detected; retry");
        }
        log.warn("AUDIT table bundle schema line deleted track={} schema_digest={} operator={} remote={}",
                track, schemaDigest, operator(request), request.getRemoteAddr());
        return new DeleteLineResult(track, schemaDigest, true);
    }

    static PointerTarget activeTarget(PointerLine line, long nowMs) {
        if (line == null) {
            return null;
        }
        if (line.next() != null && line.next().contentDigest() != null && !line.next().contentDigest().isEmpty()) {
            boolean imm = Boolean.TRUE.equals(line.next().immediate());
            long actAt = line.next().activateAtMs() == null ? 0L : line.next().activateAtMs();
            if (imm || nowMs >= actAt) {
                return line.next();
            }
        }
        if (line.current() != null && line.current().contentDigest() != null && !line.current().contentDigest().isEmpty()) {
            return line.current();
        }
        return null;
    }

    static String pointerKey(String track, String schemaDigest) {
        return POINTER_PREFIX + track + "/" + schemaDigest;
    }

    private static String requireTrack(String raw) {
        String track = raw == null ? "" : raw.strip();
        if (!VALID_TRACKS.contains(track)) {
            throw badRequest("track must be 'stable' or 'canary'");
        }
        return track;
    }

    private static String requireSchemaDigest(String raw) {
        String s = raw == null ? "" : raw.strip();
        if (!SCHEMA_DIGEST.matcher(s).matches()) {
            throw badRequest("schema_digest must be 16 lowercase hex characters");
        }
        return s;
    }

    private static String requireSha256(String raw, String field) {
        String s = raw == null ? "" : raw.strip();
        if (!SHA256_HEX.matcher(s).matches()) {
            throw badRequest(field + " must be 64 lowercase hex characters");
        }
        return s;
    }

    private static String validReason(String raw) {
        String reason = raw == null ? "" : raw.strip();
        if (reason.codePointCount(0, reason.length()) > MAX_REASON_CHARS
                || reason.codePoints().anyMatch(Character::isISOControl)) {
            throw badRequest("reason must be at most " + MAX_REASON_CHARS + " characters without control characters");
        }
        return reason;
    }

    private static PointerLine parsePointerLine(String raw) {
        try {
            return STRICT_JSON.forType(PointerLine.class).readValue(raw);
        } catch (JsonProcessingException e) {
            return null;
        }
    }

    private static PodStatus parsePodStatus(String track, String pod, String raw) {
        try {
            JsonNode node = STRICT_JSON.readTree(raw);
            if (node == null || !node.isObject()) {
                return null;
            }
            return new PodStatus(
                    pod,
                    textOf(node, "service"),
                    track,
                    textOf(node, "schema_digest"),
                    textOf(node, "loaded_digest"),
                    textOf(node, "pending_state"),
                    textOf(node, "pending_digest"),
                    textOf(node, "pending_reason"));
        } catch (JsonProcessingException e) {
            return null;
        }
    }

    private static String textOf(JsonNode node, String field) {
        JsonNode v = node.get(field);
        return (v != null && v.isTextual()) ? v.asText() : "";
    }

    private static String toJson(Object obj) {
        try {
            return JSON.writeValueAsString(obj);
        } catch (JsonProcessingException e) {
            throw new IllegalStateException(e);
        }
    }

    private static String operator(HttpServletRequest request) {
        String raw = request.getHeader(OPERATOR_HEADER);
        if (raw == null || raw.isBlank()) {
            return "-";
        }
        String text = raw.strip();
        if (text.length() > MAX_OPERATOR_CHARS || !OPERATOR_CHARS.matcher(text).matches()) {
            return "invalid";
        }
        return text;
    }

    private static ResponseStatusException badRequest(String message) {
        return new ResponseStatusException(HttpStatus.BAD_REQUEST, message);
    }

    private static <T> T call(Supplier<T> action) {
        try {
            return action.get();
        } catch (ResponseStatusException e) {
            throw e;
        } catch (RuntimeException e) {
            log.warn("Table bundle admin call failed because etcd is unavailable: {}", e.toString());
            throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE, "etcd unavailable", e);
        }
    }
}
