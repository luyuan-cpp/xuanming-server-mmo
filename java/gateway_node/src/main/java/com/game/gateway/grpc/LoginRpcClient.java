package com.game.gateway.grpc;

import com.game.gateway.config.LoginGrpcProperties;
import com.game.gateway.dto.LoginResponse;
import com.google.protobuf.CodedInputStream;
import com.google.protobuf.CodedOutputStream;
import io.grpc.CallOptions;
import io.grpc.ManagedChannel;
import io.grpc.ManagedChannelBuilder;
import io.grpc.MethodDescriptor;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import io.grpc.stub.ClientCalls;
import jakarta.annotation.PostConstruct;
import jakarta.annotation.PreDestroy;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.boot.context.properties.EnableConfigurationProperties;
import org.springframework.stereotype.Component;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;

/**
 * Thin gRPC client for {@code go/login}'s {@code ClientPlayerLogin.Login} RPC.
 *
 * <p><b>Design choice — no protoc-generated Java classes.</b> The gateway
 * already has hundreds of proto files in {@code proto/}; pulling them all
 * into the Java build (or carefully selecting just login.proto and its
 * imports) creates a heavy dependency on the C++/Go-centric proto pipeline.
 *
 * <p>Instead we hand-code the wire format for two messages
 * ({@code LoginRequest} and {@code LoginResponse}) using the protobuf-java
 * runtime that's already on the classpath. Field tags and types come straight
 * from {@code proto/login/login.proto}. As long as the proto fields stay
 * additive (which is the wire-format contract), this client keeps working
 * across upgrades. Any breaking field change shows up at runtime in
 * {@code parseResponse} — same blast radius as a generated stub anyway.
 *
 * <p>Channel lifecycle: one persistent {@link ManagedChannel} per endpoint,
 * created at startup and reused for every call (HTTP/2 multiplexing). This
 * is the same long-lived-connection contract the C++ {@code GrpcChannelCache}
 * holds for cpp gate→login.
 */
@Component
@EnableConfigurationProperties(LoginGrpcProperties.class)
public class LoginRpcClient {

    private static final Logger log = LoggerFactory.getLogger(LoginRpcClient.class);

    /** Fully-qualified names from proto/login/login.proto.
     *
     * The proto file declares {@code package loginpb;} (carried by the
     * Go output's {@code FullMethodName} constants), so the on-wire service
     * name must be {@code loginpb.ClientPlayerLogin}. Without the package
     * prefix, go-zero's grpc server returns UNIMPLEMENTED.
     */
    private static final String SERVICE = "loginpb.ClientPlayerLogin";
    private static final String METHOD_LOGIN         = SERVICE + "/Login";
    private static final String METHOD_REFRESH_TOKEN = SERVICE + "/RefreshToken";

    /** Pre-gate service: gate selection + login queue (added 2026-05). */
    private static final String PREGATE_SERVICE      = "loginpb.LoginPreGate";
    private static final String METHOD_ASSIGN_GATE   = PREGATE_SERVICE + "/AssignGate";
    private static final String METHOD_QUEUE_STATUS  = PREGATE_SERVICE + "/QueryQueueStatus";

    private final LoginGrpcProperties props;
    private final List<ManagedChannel> channels = new ArrayList<>();
    /**
     * Zone-aware routing map: {@code zoneId -> channel}. Populated when an
     * endpoint is configured as {@code "<zoneId>=host:port"}; left empty when
     * all endpoints are bare {@code host:port}. Bare endpoints are only valid
     * for truly zone-less calls such as RefreshToken.
     *
     * <p><b>Why zone-aware routing matters.</b> Each {@code login.rpc} instance
     * watches only its own zone's gate registrations in etcd
     * (prefix {@code GateNodeService.rpc/zone/{ZoneId}/...}). A pure
     * round-robin client therefore has a 2-in-3 chance of dispatching a
     * {@code zone_id=1} AssignGate to a {@code z2_login} or {@code z3_login},
     * which sees zero gate candidates for that zone and replies
     * {@code "no gate available for requested zone"}. Stress run 2026-05-24
     * caught this on the first 3-zone × 15000 attempt — see
     * {@code docs/design/stress-3zone-2026-05-23-postmortem.md §I}.
     */
    private final Map<Integer, ManagedChannel> channelByZone = new LinkedHashMap<>();
    private final AtomicInteger rr = new AtomicInteger();

    // ── etcd 动态发现(多节点部署)────────────────────────────────
    //
    // LoginNodeDiscovery 周期性把 etcd 里 LoginNodeService.rpc/ 的注册结果
    // 通过 updateDynamicEndpoints() 推进来。动态池优先于静态配置:同一 zone
    // 有多个 login 实例时按轮询分摊;etcd 里没有该 zone 的注册时回落到
    // 静态 channelByZone(兼容纯静态配置的旧部署)。

    /** endpoint("host:port") -> 动态发现创建的 channel(创建一次,复用)。 */
    private final ConcurrentHashMap<String, ManagedChannel> dynamicChannels = new ConcurrentHashMap<>();

    /** zone -> 动态 channel 列表。整表原子替换,读方无锁。 */
    private volatile Map<Integer, List<ManagedChannel>> dynamicByZone = Map.of();

    private final MethodDescriptor<LoginRequestProto, LoginResponseProto> loginMethod;
    private final MethodDescriptor<RefreshTokenRequestProto, RefreshTokenResponseProto> refreshTokenMethod;
    private final MethodDescriptor<AssignGateRequestProto, AssignGateResponseProto> assignGateMethod;
    private final MethodDescriptor<QueryQueueStatusRequestProto, QueryQueueStatusResponseProto> queueStatusMethod;

    public LoginRpcClient(LoginGrpcProperties props) {
        this.props = props;
        this.loginMethod = MethodDescriptor.<LoginRequestProto, LoginResponseProto>newBuilder()
                .setType(MethodDescriptor.MethodType.UNARY)
                .setFullMethodName(METHOD_LOGIN)
                .setRequestMarshaller(new LoginRequestMarshaller())
                .setResponseMarshaller(new LoginResponseMarshaller())
                .build();
        this.refreshTokenMethod = MethodDescriptor.<RefreshTokenRequestProto, RefreshTokenResponseProto>newBuilder()
                .setType(MethodDescriptor.MethodType.UNARY)
                .setFullMethodName(METHOD_REFRESH_TOKEN)
                .setRequestMarshaller(new RefreshTokenRequestMarshaller())
                .setResponseMarshaller(new RefreshTokenResponseMarshaller())
                .build();
        this.assignGateMethod = MethodDescriptor.<AssignGateRequestProto, AssignGateResponseProto>newBuilder()
                .setType(MethodDescriptor.MethodType.UNARY)
                .setFullMethodName(METHOD_ASSIGN_GATE)
                .setRequestMarshaller(new AssignGateRequestMarshaller())
                .setResponseMarshaller(new AssignGateResponseMarshaller())
                .build();
        this.queueStatusMethod = MethodDescriptor.<QueryQueueStatusRequestProto, QueryQueueStatusResponseProto>newBuilder()
                .setType(MethodDescriptor.MethodType.UNARY)
                .setFullMethodName(METHOD_QUEUE_STATUS)
                .setRequestMarshaller(new QueryQueueStatusRequestMarshaller())
                .setResponseMarshaller(new QueryQueueStatusResponseMarshaller())
                .build();
    }

    @PostConstruct
    void start() {
        String[] eps = props.getEndpoints().split(",");
        for (String raw : eps) {
            String ep = raw.trim();
            if (ep.isEmpty()) continue;

            // Two accepted shapes:
            //   "host:port"            — bare endpoint, joins the round-robin pool only
            //   "<zoneId>=host:port"   — also indexed by zone for zone-aware routing
            // Mixing both is allowed: zone-tagged endpoints route all zone-bound
            // calls, unzoned ones only serve RefreshToken (which has no zone).
            Integer zoneTag = null;
            int eq = ep.indexOf('=');
            if (eq > 0) {
                String prefix = ep.substring(0, eq).trim();
                try {
                    zoneTag = Integer.parseInt(prefix);
                } catch (NumberFormatException nfe) {
                    log.warn("login.grpc endpoint zone prefix not a number, ignoring zone tag: {}", ep);
                    zoneTag = null;
                }
                ep = ep.substring(eq + 1).trim();
            }

            String[] hp = ep.split(":");
            if (hp.length != 2) {
                log.warn("login.grpc endpoint malformed, skipped: {}", ep);
                continue;
            }
            ManagedChannel ch = ManagedChannelBuilder.forAddress(hp[0], Integer.parseInt(hp[1]))
                    .usePlaintext()
                    .keepAliveTime(30, TimeUnit.SECONDS)
                    .keepAliveTimeout(10, TimeUnit.SECONDS)
                    .keepAliveWithoutCalls(true)
                    .build();
            channels.add(ch);
            if (zoneTag != null) {
                ManagedChannel prev = channelByZone.put(zoneTag, ch);
                if (prev != null) {
                    log.warn("login.grpc duplicate zone tag {} — overriding previous channel", zoneTag);
                }
                log.info("login.rpc channel up (zone={}): {}", zoneTag, ep);
            } else {
                log.info("login.rpc channel up: {}", ep);
            }
        }
        if (channels.isEmpty()) {
            log.warn("LoginRpcClient has no endpoints; /api/login will fail");
        }
    }

    @PreDestroy
    void stop() {
        var all = new ArrayList<>(channels);
        all.addAll(dynamicChannels.values());
        for (var ch : all) {
            ch.shutdown();
            try {
                ch.awaitTermination(5, TimeUnit.SECONDS);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
        }
    }

    /**
     * 用 etcd 发现结果整体替换动态路由表(由 {@link LoginNodeDiscovery}
     * 周期调用)。endpoint 形如 {@code host:port};已存在的 channel 复用,
     * 新出现的创建,从 etcd 消失的关停回收。调用方保证 etcd 查询失败时
     * 不调用本方法(保留 last-known-good 路由)。
     */
    public void updateDynamicEndpoints(Map<Integer, List<String>> endpointsByZone) {
        Map<Integer, List<ManagedChannel>> next = new LinkedHashMap<>();
        Set<String> live = new HashSet<>();
        for (var entry : endpointsByZone.entrySet()) {
            List<ManagedChannel> chs = new ArrayList<>();
            for (String ep : entry.getValue()) {
                if (ep == null || ep.isBlank()) continue;
                live.add(ep);
                ManagedChannel ch = dynamicChannels.computeIfAbsent(ep, this::buildDynamicChannel);
                if (ch != null) chs.add(ch);
            }
            if (!chs.isEmpty()) next.put(entry.getKey(), chs);
        }
        dynamicByZone = next;

        // 回收 etcd 里已消失的节点。grpc shutdown() 是优雅关闭,在途调用
        // 会完成;新路由表已不含该 channel,不会再被选中。
        for (var it = dynamicChannels.entrySet().iterator(); it.hasNext(); ) {
            var entry = it.next();
            if (!live.contains(entry.getKey())) {
                it.remove();
                entry.getValue().shutdown();
                log.info("login.rpc dynamic channel retired: {}", entry.getKey());
            }
        }
    }

    /** 当前动态路由表快照(仅测试/观测用)。 */
    public Map<Integer, Integer> dynamicRoutingSnapshot() {
        Map<Integer, Integer> out = new LinkedHashMap<>();
        for (var e : dynamicByZone.entrySet()) out.put(e.getKey(), e.getValue().size());
        return out;
    }

    private ManagedChannel buildDynamicChannel(String ep) {
        int colon = ep.lastIndexOf(':');
        if (colon <= 0 || colon == ep.length() - 1) {
            log.warn("login.rpc dynamic endpoint malformed, skipped: {}", ep);
            return null;
        }
        final int port;
        try {
            port = Integer.parseInt(ep.substring(colon + 1));
        } catch (NumberFormatException nfe) {
            log.warn("login.rpc dynamic endpoint port malformed, skipped: {}", ep);
            return null;
        }
        ManagedChannel ch = ManagedChannelBuilder.forAddress(ep.substring(0, colon), port)
                .usePlaintext()
                .keepAliveTime(30, TimeUnit.SECONDS)
                .keepAliveTimeout(10, TimeUnit.SECONDS)
                .keepAliveWithoutCalls(true)
                .build();
        log.info("login.rpc dynamic channel up: {}", ep);
        return ch;
    }

    /**
     * Zone-aware Login: routes to the {@code login.rpc} instance that watches
     * gates for the given zone. A missing zone mapping is a configuration error
     * and never falls back to another zone.
     */
    public LoginResponseProto login(LoginRequestProto req, int zoneId) {
        // Login 可能创建账号壳并签发 access/refresh token。响应丢失时结果
        // 不确定；没有幂等键就自动重试会重复执行这些副作用。
        return unaryCall(loginMethod, req, zoneId, true, false);
    }

    /** Calls {@code ClientPlayerLogin.RefreshToken}. Throws on terminal failure. */
    public RefreshTokenResponseProto refreshToken(RefreshTokenRequestProto req) {
        // RefreshToken 是**非幂等**的一次性轮换:服务端每次成功调用都会签发新
        // access/refresh 对并原子删除旧 refresh token。响应在回程丢失(典型:
        // DEADLINE_EXCEEDED)时服务端可能已经消费掉旧 token —— 自动重试会拿着
        // 已作废的 token 再打一次,得到鉴权失败,把玩家会话彻底烧掉。
        // 所以这条方法禁用自动重试,失败直接上抛,由客户端走完整登录兜底。
        return unaryCall(refreshTokenMethod, req, 0, false, false);
    }

    /**
     * Zone-aware AssignGate: see {@link #login(LoginRequestProto, int)} for
     * routing semantics.
     */
    public AssignGateResponseProto assignGate(AssignGateRequestProto req, int zoneId) {
        // AssignGate 可能入队/放行账号并签发 Gate token，因此不是纯读。
        return unaryCall(assignGateMethod, req, zoneId, true, false);
    }

    /**
     * Zone-aware QueryQueueStatus: when the queue token was issued by a
     * specific zone's login, the status poll has to come back to the same
     * instance — otherwise the lookup can't find the queue entry.
     */
    public QueryQueueStatusResponseProto queryQueueStatus(QueryQueueStatusRequestProto req, int zoneId) {
        // QueryQueueStatus 也不是纯读：轮询已放行条目可能消费/推进队列状态，
        // 并返回新签发的 Gate 连接材料。
        return unaryCall(queueStatusMethod, req, zoneId, true, false);
    }

    private <Q, R> R unaryCall(
            MethodDescriptor<Q, R> method,
            Q req,
            int zoneId,
            boolean zoneRequired,
            boolean retrySafe) {
        if (zoneRequired && zoneId <= 0) {
            throw new IllegalArgumentException("login.rpc zone-aware call requires a positive zone id: "
                    + method.getFullMethodName());
        }

        int attempts = retrySafe ? 1 + Math.max(0, props.getRetry()) : 1;
        StatusRuntimeException last = null;
        for (int i = 0; i < attempts; i++) {
            // 每次尝试重新选 channel:同 zone 多 login 实例时,重试能落到
            // 另一个实例上(单实例故障的自然 failover)。
            ManagedChannel ch = pickChannel(method, zoneId, zoneRequired);
            // deadline 必须**每次尝试**单独计算:withDeadlineAfter 产出的是绝对
            // 时间点,若在循环外只算一次,首次 DEADLINE_EXCEEDED 时预算已经
            // 耗尽,后续所有重试都会瞬时再报 DEADLINE_EXCEEDED —— 重试形同虚设,
            // 还向调用方伪装成"重试过了"。
            CallOptions opts = CallOptions.DEFAULT.withDeadlineAfter(props.getTimeoutMs(), TimeUnit.MILLISECONDS);
            try {
                return ClientCalls.blockingUnaryCall(ch, method, opts, req);
            } catch (StatusRuntimeException e) {
                last = e;
                Status.Code c = e.getStatus().getCode();
                if (!retrySafe || (c != Status.Code.UNAVAILABLE && c != Status.Code.DEADLINE_EXCEEDED)) {
                    throw e;
                }
                log.warn("login.rpc retriable error on {} attempt {}/{}: {}",
                        method.getFullMethodName(), i + 1, attempts, c);
            }
        }
        throw last;
    }

    /**
     * 选路顺序:zone 动态池(etcd 发现,轮询)→ zone 静态配置 → 报错。
     * zone 无关调用(RefreshToken)在「动态全体 + 静态全体」里轮询。
     * 任何情况下都不做跨 zone 兜底(见 channelByZone 注释的压测事故)。
     */
    private ManagedChannel pickChannel(MethodDescriptor<?, ?> method, int zoneId, boolean zoneRequired) {
        if (zoneRequired) {
            List<ManagedChannel> dyn = dynamicByZone.get(zoneId);
            if (dyn != null && !dyn.isEmpty()) {
                return dyn.get(Math.floorMod(rr.getAndIncrement(), dyn.size()));
            }
            ManagedChannel ch = channelByZone.get(zoneId);
            if (ch == null) {
                String message = "login.rpc no endpoint for zone=" + zoneId
                        + " (etcd discovery empty, no static zone mapping)"
                        + " method=" + method.getFullMethodName();
                log.error("{}; refusing cross-zone round-robin fallback", message);
                throw new IllegalStateException(message);
            }
            return ch;
        }

        // RefreshToken 的 token 当前不携带 zone，才允许在全部 channel 间轮询。
        List<ManagedChannel> pool = new ArrayList<>(dynamicChannels.values());
        pool.addAll(channels);
        if (pool.isEmpty()) {
            throw new IllegalStateException("login.rpc no endpoint");
        }
        return pool.get(Math.floorMod(rr.getAndIncrement(), pool.size()));
    }

    // ──────────────────── Protobuf wire encoding ─────────────────────

    /**
     * proto/login/login.proto LoginRequest:
     *   string account = 1;
     *   string password = 2;
     *   string auth_type = 3;
     *   string auth_token = 4;
     *
     * (Gateway also forwards device_id/zone_id via metadata; the proto today
     * does not have those fields, so we don't write them — additive proto
     * upgrade later can pick them up without changing this file's wire output.)
     */
    public static final class LoginRequestProto {
        // Public so cross-package integration tests can capture and assert
        // these via Mockito's ArgumentCaptor — production code never reads
        // them after toBytes() runs. final keeps the value immutable.
        public final String account;
        public final String password;
        public final String authType;
        public final String authToken;

        public LoginRequestProto(String account, String password, String authType, String authToken) {
            this.account   = nullToEmpty(account);
            this.password  = nullToEmpty(password);
            this.authType  = nullToEmpty(authType);
            this.authToken = nullToEmpty(authToken);
        }

        byte[] toBytes() throws IOException {
            ByteArrayOutputStream baos = new ByteArrayOutputStream(64);
            CodedOutputStream out = CodedOutputStream.newInstance(baos);
            if (!account.isEmpty())   out.writeString(1, account);
            if (!password.isEmpty())  out.writeString(2, password);
            if (!authType.isEmpty())  out.writeString(3, authType);
            if (!authToken.isEmpty()) out.writeString(4, authToken);
            out.flush();
            return baos.toByteArray();
        }
    }

    /**
     * proto/login/login.proto LoginResponse:
     *   TipInfoMessage error_message = 1;                    (group/message — skipped, parsed lazily)
     *   repeated AccountSimplePlayerWrapper players = 2;     (解析 wrapper.player.player_id;proto 目前无 name/level)
     *   string access_token = 3;
     *   string refresh_token = 4;
     *   int64  access_token_expire = 5;
     *   int64  refresh_token_expire = 6;
     */
    public static final class LoginResponseProto {
        public Integer errorCode;       // populated if error_message present and we can crack it
        public String  errorMessage;
        public List<LoginResponse.PlayerInfo> players = new ArrayList<>();
        public String  accessToken;
        public String  refreshToken;
        public long    accessTokenExpire;
        public long    refreshTokenExpire;

        public boolean hasError() {
            return (errorCode != null && errorCode != 0)
                    || (errorMessage != null && !errorMessage.isEmpty() && (errorCode == null || errorCode != 0));
        }
    }

    private static String nullToEmpty(String s) { return s == null ? "" : s; }

    private static final class LoginRequestMarshaller implements MethodDescriptor.Marshaller<LoginRequestProto> {
        @Override
        public InputStream stream(LoginRequestProto value) {
            try {
                return new java.io.ByteArrayInputStream(value.toBytes());
            } catch (IOException e) {
                throw new RuntimeException(e);
            }
        }

        @Override
        public LoginRequestProto parse(InputStream stream) {
            // gateway never receives a request — server-side parsing happens in go-zero
            throw new UnsupportedOperationException();
        }
    }

    private static final class LoginResponseMarshaller implements MethodDescriptor.Marshaller<LoginResponseProto> {
        @Override
        public InputStream stream(LoginResponseProto value) {
            throw new UnsupportedOperationException();
        }

        @Override
        public LoginResponseProto parse(InputStream stream) {
            try {
                byte[] bytes = stream.readAllBytes();
                return parseLoginResponse(bytes);
            } catch (IOException e) {
                throw new RuntimeException(e);
            }
        }
    }

    // ── RefreshToken wire codec ──────────────────────────────────────

    /**
     * proto/login/login.proto RefreshTokenRequest:
     *   string refresh_token = 1;
     */
    public static final class RefreshTokenRequestProto {
        // Public for cross-package test capture, mirroring LoginRequestProto.
        public final String refreshToken;

        public RefreshTokenRequestProto(String refreshToken) {
            this.refreshToken = nullToEmpty(refreshToken);
        }

        byte[] toBytes() throws IOException {
            ByteArrayOutputStream baos = new ByteArrayOutputStream(64);
            CodedOutputStream out = CodedOutputStream.newInstance(baos);
            if (!refreshToken.isEmpty()) out.writeString(1, refreshToken);
            out.flush();
            return baos.toByteArray();
        }
    }

    /**
     * proto/login/login.proto RefreshTokenResponse:
     *   TipInfoMessage error_message      = 1;
     *   string          access_token      = 2;
     *   string          refresh_token     = 3;
     *   int64           access_token_expire  = 4;
     *   int64           refresh_token_expire = 5;
     */
    public static final class RefreshTokenResponseProto {
        public Integer errorCode;
        public String  errorMessage;
        public String  accessToken;
        public String  refreshToken;
        public long    accessTokenExpire;
        public long    refreshTokenExpire;

        public boolean hasError() {
            return (errorCode != null && errorCode != 0)
                    || (errorMessage != null && !errorMessage.isEmpty() && (errorCode == null || errorCode != 0));
        }
    }

    private static final class RefreshTokenRequestMarshaller implements MethodDescriptor.Marshaller<RefreshTokenRequestProto> {
        @Override
        public InputStream stream(RefreshTokenRequestProto value) {
            try {
                return new java.io.ByteArrayInputStream(value.toBytes());
            } catch (IOException e) {
                throw new RuntimeException(e);
            }
        }

        @Override
        public RefreshTokenRequestProto parse(InputStream stream) {
            throw new UnsupportedOperationException();
        }
    }

    private static final class RefreshTokenResponseMarshaller implements MethodDescriptor.Marshaller<RefreshTokenResponseProto> {
        @Override
        public InputStream stream(RefreshTokenResponseProto value) {
            throw new UnsupportedOperationException();
        }

        @Override
        public RefreshTokenResponseProto parse(InputStream stream) {
            try {
                byte[] bytes = stream.readAllBytes();
                return parseRefreshResponse(bytes);
            } catch (IOException e) {
                throw new RuntimeException(e);
            }
        }
    }

    /**
     * Hand-rolled parser that only reads fields we care about. Unknown fields
     * are skipped using {@link CodedInputStream#skipField}, so additive proto
     * upgrades remain wire-compatible.
     */
    private static LoginResponseProto parseLoginResponse(byte[] bytes) throws IOException {
        LoginResponseProto resp = new LoginResponseProto();
        CodedInputStream in = CodedInputStream.newInstance(bytes);
        while (!in.isAtEnd()) {
            int tag = in.readTag();
            int field = tag >>> 3;
            switch (field) {
                case 1 -> {
                    int len = in.readRawVarint32();
                    int oldLimit = in.pushLimit(len);
                    while (!in.isAtEnd()) {
                        int subTag = in.readTag();
                        int subField = subTag >>> 3;
                        int wire = subTag & 0x7;
                        if (subField == 1 && wire == 0) {
                            resp.errorCode = (int) in.readInt64();
                        } else if (subField == 2 && wire == 2) {
                            resp.errorMessage = in.readString();
                        } else {
                            in.skipField(subTag);
                        }
                    }
                    in.popLimit(oldLimit);
                }
                case 2 -> {
                    // repeated AccountSimplePlayerWrapper players:
                    //   wrapper.player = 1 (message) -> AccountSimplePlayer.player_id = 1 (varint)
                    // proto 里 AccountSimplePlayer 目前只有 player_id 一个字段,
                    // PlayerInfo 的 name/level 等昵称体系落地后再补。
                    int len = in.readRawVarint32();
                    int oldLimit = in.pushLimit(len);
                    long playerId = 0;
                    while (!in.isAtEnd()) {
                        int subTag = in.readTag();
                        int subField = subTag >>> 3;
                        int wire = subTag & 0x7;
                        if (subField == 1 && wire == 2) {
                            int innerLen = in.readRawVarint32();
                            int innerLimit = in.pushLimit(innerLen);
                            while (!in.isAtEnd()) {
                                int pTag = in.readTag();
                                if ((pTag >>> 3) == 1 && (pTag & 0x7) == 0) {
                                    playerId = in.readUInt64();
                                } else {
                                    in.skipField(pTag);
                                }
                            }
                            in.popLimit(innerLimit);
                        } else {
                            in.skipField(subTag);
                        }
                    }
                    in.popLimit(oldLimit);
                    LoginResponse.PlayerInfo p = new LoginResponse.PlayerInfo();
                    p.setPlayerId(playerId);
                    resp.players.add(p);
                }
                case 3 -> resp.accessToken = in.readString();
                case 4 -> resp.refreshToken = in.readString();
                case 5 -> resp.accessTokenExpire = in.readInt64();
                case 6 -> resp.refreshTokenExpire = in.readInt64();
                default -> in.skipField(tag);
            }
        }
        return resp;
    }

    private static RefreshTokenResponseProto parseRefreshResponse(byte[] bytes) throws IOException {
        RefreshTokenResponseProto resp = new RefreshTokenResponseProto();
        CodedInputStream in = CodedInputStream.newInstance(bytes);
        while (!in.isAtEnd()) {
            int tag = in.readTag();
            int field = tag >>> 3;
            switch (field) {
                case 1 -> {
                    int len = in.readRawVarint32();
                    int oldLimit = in.pushLimit(len);
                    while (!in.isAtEnd()) {
                        int subTag = in.readTag();
                        int subField = subTag >>> 3;
                        int wire = subTag & 0x7;
                        if (subField == 1 && wire == 0) {
                            resp.errorCode = (int) in.readInt64();
                        } else if (subField == 2 && wire == 2) {
                            resp.errorMessage = in.readString();
                        } else {
                            in.skipField(subTag);
                        }
                    }
                    in.popLimit(oldLimit);
                }
                case 2 -> resp.accessToken = in.readString();
                case 3 -> resp.refreshToken = in.readString();
                case 4 -> resp.accessTokenExpire = in.readInt64();
                case 5 -> resp.refreshTokenExpire = in.readInt64();
                default -> in.skipField(tag);
            }
        }
        return resp;
    }

    // ── AssignGate / QueryQueueStatus wire codecs ────────────────────
    //
    // proto/login/login.proto AssignGateRequest:
    //   uint32 zone_id     = 1
    //   string queue_token = 2
    //   string account     = 3
    //   string device_id   = 4
    //
    // AssignGateResponse: see proto for full layout. Tags here MUST match
    // the proto exactly — that's the entire wire-format contract this
    // hand-coded client relies on. Additive proto changes (new field tags)
    // are forward-compatible because parseAssignGateResponse falls through
    // to skipField for unknown tags.

    public static final class AssignGateRequestProto {
        public final int zoneId;
        public final String queueToken;
        public final String account;
        public final String deviceId;

        public AssignGateRequestProto(int zoneId, String queueToken, String account, String deviceId) {
            this.zoneId = zoneId;
            this.queueToken = nullToEmpty(queueToken);
            this.account = nullToEmpty(account);
            this.deviceId = nullToEmpty(deviceId);
        }

        byte[] toBytes() throws IOException {
            ByteArrayOutputStream baos = new ByteArrayOutputStream(64);
            CodedOutputStream out = CodedOutputStream.newInstance(baos);
            if (zoneId != 0)             out.writeUInt32(1, zoneId);
            if (!queueToken.isEmpty())   out.writeString(2, queueToken);
            if (!account.isEmpty())      out.writeString(3, account);
            if (!deviceId.isEmpty())     out.writeString(4, deviceId);
            out.flush();
            return baos.toByteArray();
        }
    }

    public static final class AssignGateResponseProto {
        public String ip;
        public int    port;
        public byte[] tokenPayload;
        public byte[] tokenSignature;
        public long   tokenDeadline;
        public String error;

        public int    status;          // 0=ADMITTED, 1=QUEUEING, 2=ERROR, 3=EXPIRED
        public String queueToken;
        public int    queueRank;
        public int    queueTotal;
        public int    retryAfterMs;
    }

    private static final class AssignGateRequestMarshaller implements MethodDescriptor.Marshaller<AssignGateRequestProto> {
        @Override
        public InputStream stream(AssignGateRequestProto value) {
            try {
                return new java.io.ByteArrayInputStream(value.toBytes());
            } catch (IOException e) {
                throw new RuntimeException(e);
            }
        }

        @Override
        public AssignGateRequestProto parse(InputStream stream) {
            throw new UnsupportedOperationException();
        }
    }

    private static final class AssignGateResponseMarshaller implements MethodDescriptor.Marshaller<AssignGateResponseProto> {
        @Override
        public InputStream stream(AssignGateResponseProto value) {
            throw new UnsupportedOperationException();
        }

        @Override
        public AssignGateResponseProto parse(InputStream stream) {
            try {
                return parseAssignGateResponse(stream.readAllBytes());
            } catch (IOException e) {
                throw new RuntimeException(e);
            }
        }
    }

    private static AssignGateResponseProto parseAssignGateResponse(byte[] bytes) throws IOException {
        AssignGateResponseProto resp = new AssignGateResponseProto();
        CodedInputStream in = CodedInputStream.newInstance(bytes);
        while (!in.isAtEnd()) {
            int tag = in.readTag();
            int field = tag >>> 3;
            switch (field) {
                case 1 -> resp.ip             = in.readString();
                case 2 -> resp.port           = in.readUInt32();
                case 3 -> resp.tokenPayload   = in.readByteArray();
                case 4 -> resp.tokenSignature = in.readByteArray();
                case 5 -> resp.tokenDeadline  = in.readInt64();
                case 6 -> resp.error          = in.readString();
                case 7 -> resp.status         = in.readUInt32();
                case 8 -> resp.queueToken     = in.readString();
                case 9 -> resp.queueRank      = in.readUInt32();
                case 10 -> resp.queueTotal    = in.readUInt32();
                case 11 -> resp.retryAfterMs  = in.readUInt32();
                default -> in.skipField(tag);
            }
        }
        return resp;
    }

    public static final class QueryQueueStatusRequestProto {
        public final String queueToken;

        public QueryQueueStatusRequestProto(String queueToken) {
            this.queueToken = nullToEmpty(queueToken);
        }

        byte[] toBytes() throws IOException {
            ByteArrayOutputStream baos = new ByteArrayOutputStream(64);
            CodedOutputStream out = CodedOutputStream.newInstance(baos);
            if (!queueToken.isEmpty()) out.writeString(1, queueToken);
            out.flush();
            return baos.toByteArray();
        }
    }

    public static final class QueryQueueStatusResponseProto {
        public int    status;
        public String ip;
        public int    port;
        public byte[] tokenPayload;
        public byte[] tokenSignature;
        public long   tokenDeadline;
        public int    queueRank;
        public int    queueTotal;
        public int    retryAfterMs;
        public String error;
    }

    private static final class QueryQueueStatusRequestMarshaller implements MethodDescriptor.Marshaller<QueryQueueStatusRequestProto> {
        @Override
        public InputStream stream(QueryQueueStatusRequestProto value) {
            try {
                return new java.io.ByteArrayInputStream(value.toBytes());
            } catch (IOException e) {
                throw new RuntimeException(e);
            }
        }

        @Override
        public QueryQueueStatusRequestProto parse(InputStream stream) {
            throw new UnsupportedOperationException();
        }
    }

    private static final class QueryQueueStatusResponseMarshaller implements MethodDescriptor.Marshaller<QueryQueueStatusResponseProto> {
        @Override
        public InputStream stream(QueryQueueStatusResponseProto value) {
            throw new UnsupportedOperationException();
        }

        @Override
        public QueryQueueStatusResponseProto parse(InputStream stream) {
            try {
                return parseQueueStatusResponse(stream.readAllBytes());
            } catch (IOException e) {
                throw new RuntimeException(e);
            }
        }
    }

    private static QueryQueueStatusResponseProto parseQueueStatusResponse(byte[] bytes) throws IOException {
        QueryQueueStatusResponseProto resp = new QueryQueueStatusResponseProto();
        CodedInputStream in = CodedInputStream.newInstance(bytes);
        while (!in.isAtEnd()) {
            int tag = in.readTag();
            int field = tag >>> 3;
            switch (field) {
                case 1 -> resp.status         = in.readUInt32();
                case 2 -> resp.ip             = in.readString();
                case 3 -> resp.port           = in.readUInt32();
                case 4 -> resp.tokenPayload   = in.readByteArray();
                case 5 -> resp.tokenSignature = in.readByteArray();
                case 6 -> resp.tokenDeadline  = in.readInt64();
                case 7 -> resp.queueRank      = in.readUInt32();
                case 8 -> resp.queueTotal     = in.readUInt32();
                case 9 -> resp.retryAfterMs   = in.readUInt32();
                case 10 -> resp.error         = in.readString();
                default -> in.skipField(tag);
            }
        }
        return resp;
    }
}
