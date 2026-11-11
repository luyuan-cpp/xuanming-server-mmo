package com.game.gateway.service;

import com.game.gateway.dto.*;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.repository.ZoneConfigRepository;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.dao.QueryTimeoutException;
import org.springframework.stereotype.Service;

import java.time.LocalDateTime;
import java.time.ZoneOffset;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import java.util.concurrent.atomic.AtomicReference;
import java.util.function.LongSupplier;

/**
 * {@code GET /api/server-list}:zone_config 目录 + 探测态 → 选服列表。
 *
 * <h2>zone_config 行的短缓存(2026-10-15,G2b §9 第 7 条,负责人按「最标准」定)</h2>
 * 这是只读、公开、不限流的列表。以前每个请求一次 {@code findAll}:开服登录潮或被刷时查库次数随请求数线性增长,
 * 而网关的 Hikari 池只有 5 条(application.yaml 的 hikari 段)。现在:
 * <ul>
 *   <li>只缓存库里的行,TTL {@link #ZONE_ROWS_CACHE_TTL_NANOS}(1 秒,与 AssignGateService 的准入缓存同口径:
 *       运维手工开关区要近实时生效)。探测态(ZoneHealthProbeService 的内存快照)与「新服」判定仍按请求现算,
 *       不跟着缓存变旧。管理接口改 zone_config 不主动失效本缓存:多副本下本地失效也管不到别的副本,上限就是 1 秒。</li>
 *   <li>缓存过期瞬间的并发请求合并成一次查库(single-flight):第一个请求查,其余 join 同一个 future。</li>
 *   <li>查库失败不缓存:异常原样抛给本次的查询方与全部等待方(与改动前相同,经 Spring 默认错误处理回 5xx),
 *       future 在 finally 里摘掉,下一个请求重新查。</li>
 *   <li>2026-10-16 补注:等待方有时间预算 {@link #ZONE_ROWS_LOAD_WAIT_MILLIS}(上面「其余 join 同一个 future」现为限时等待)。
 *       查库方挂住(半开 TCP、zone_config 上的 MDL 排队)时,等待方到点抛 {@link QueryTimeoutException}(经默认错误处理回 5xx),
 *       与改动前「取不到连接最多等 3s 回 5xx」同口径,占用的 Tomcat 线程不再随挂住时长无界增长。查库方本身仍只受 JDBC 读超时约束:
 *       jar 与 K8s ConfigMap 的 JDBC URL 当前都没配 socketTimeout、也没配查询超时,挂住时占 1 个线程与 1 条连接,登记为后续
 *       (涉及全部查询,不在本项)。上面「与改动前相同」只指异常语义。
 *       查库方挂在已取到的连接上时,在途 future 只有它自己能在 finally 里摘,这个副本上后到的全部请求都等满预算回 5xx,
 *       直到那条套接字自己断开(半开 TCP 约 15 分钟,keepalive 约 2 小时):改动前这种单连接故障只拖住一个请求,合并查库
 *       把它放大成整个副本的 server-list 不可用。概率低,根治靠 JDBC 读超时,不在本类里另做「超时顶替查库方」。
 *       (2026-10-16 续)JDBC URL 已带 socketTimeout=10000(application.yaml,K8s ConfigMap 镜像):查库方挂住最多约 10s
 *       即以通信异常失败、在 finally 里摘掉在途 future,上面这段不可用窗口随之收到约 10s。</li>
 * </ul>
 * 缓存里的 {@link ZoneConfig} 是脱管实体(open-in-view 关闭,findAll 返回时持久化上下文已关),没有懒加载字段,
 * 跨请求共享,本类只读不写。改成会改实体的写法之前,先把缓存换成不可变快照。
 */
@Service
public class ServerListService {

    /** zone_config 行的缓存有效期。年龄按单调时钟算,写入时刻取查库完成时。 */
    static final long ZONE_ROWS_CACHE_TTL_NANOS = TimeUnit.SECONDS.toNanos(1);

    /**
     * 加入在途查库的等待方最多等多久(毫秒)。与 Hikari connection-timeout(application.yaml 的 hikari 段)同口径:
     * 改动前每个请求自己 findAll,池满时最多等这么久就回 5xx;合并查库后等待方也不能比这更久。
     * 刻意不从配置读:Hikari 把 0 当无限等,照搬会让等待方退回无上限。改 connection-timeout 时同步改这里。
     */
    static final long ZONE_ROWS_LOAD_WAIT_MILLIS = 3000L;

    private final ZoneConfigRepository zoneConfigRepo;
    private final ZoneHealthProbeService probeService;
    /** 单调时钟(纳秒),只用来算缓存年龄。 */
    private final LongSupplier nanoClock;
    /** 等待方加入在途查库后的时间预算(毫秒),见 {@link #ZONE_ROWS_LOAD_WAIT_MILLIS}。 */
    private final long loadWaitMillis;

    /** 最近一次成功查库的结果(已按 sort_order 排好、不可变);null = 还没成功过。失败从不写这里。 */
    private volatile ZoneRowsSnapshot cachedRows;
    /** 正在进行的那次查库;null = 没有。由发起查库的请求在 finally 里摘掉,不留失败结果。 */
    private final AtomicReference<CompletableFuture<List<ZoneConfig>>> inflightLoad = new AtomicReference<>();

    @Autowired
    public ServerListService(ZoneConfigRepository zoneConfigRepo,
                             ZoneHealthProbeService probeService) {
        this(zoneConfigRepo, probeService, System::nanoTime, ZONE_ROWS_LOAD_WAIT_MILLIS);
    }

    // 包级时钟注入点(与 ZoneHealthProbeService 同一做法):TTL 用例推进假时钟,不依赖墙钟。
    // 等待预算同样可注入:超时用例用短预算,并发用例用远大于屏障上限的预算。
    ServerListService(ZoneConfigRepository zoneConfigRepo,
                      ZoneHealthProbeService probeService,
                      LongSupplier nanoClock,
                      long loadWaitMillis) {
        this.zoneConfigRepo = zoneConfigRepo;
        this.probeService = probeService;
        this.nanoClock = nanoClock;
        this.loadWaitMillis = loadWaitMillis;
    }

    public ServerListResponse getServerList() {
        List<ZoneInfoDto> dtos = getZoneRows().stream().map(this::toDto).toList();
        applyRecommendation(dtos);
        return new ServerListResponse(dtos);
    }

    /**
     * 推荐区(2026-11-10 起;此前 recommended 原样下发 zone_config 的手工值)。客户端与机器人按它默认选区,所以它决定新玩家去哪:
     * <ul>
     *   <li>运维勾的推荐只在区服显示 OPEN、且负载档不是爆满时保留。维护、关闭、预告中的区进不去(assign-gate 回 503),
     *       推荐它会让默认选中的区登不上;爆满的区要排队,应把新玩家引到别处。负载档未知(探测态不新鲜)时不判爆满,照留。</li>
     *   <li>一个都不剩时,改推荐负载率(在线 / 负载档分母,见 {@link ZoneHealthProbeService#getLoadRatio})最低的那个开放区,
     *       并列取列表里靠前的(sort_order 小)。开放区全都爆满时仍推荐最空的那个:排队照样能进,只是要等。
     *       没有任何开放区的负载率可知时不补推荐,没有依据挑。</li>
     * </ul>
     * 多副本各按自己的探测快照算,快照最多差一个探测周期(5 秒),推荐偶尔不一致无害。
     */
    private void applyRecommendation(List<ZoneInfoDto> dtos) {
        boolean anyKept = false;
        for (ZoneInfoDto dto : dtos) {
            boolean keep = dto.isRecommended()
                    && dto.getStatus() == ZoneDisplayStatus.OPEN
                    && dto.getLoadLevel() != LoadLevel.FULL;
            dto.setRecommended(keep);
            anyKept |= keep;
        }
        if (anyKept) {
            return;
        }

        ZoneInfoDto emptiest = null;
        double emptiestRatio = Double.POSITIVE_INFINITY;
        for (ZoneInfoDto dto : dtos) {
            // loadLevel 只在显示 OPEN 且探测态已知时才填(见 toDto),为 null 就是没有依据。
            if (dto.getStatus() != ZoneDisplayStatus.OPEN || dto.getLoadLevel() == null) {
                continue;
            }
            Double ratio = probeService.getLoadRatio(dto.getZoneId());
            if (ratio != null && ratio < emptiestRatio) {
                emptiest = dto;
                emptiestRatio = ratio;
            }
        }
        if (emptiest != null) {
            emptiest.setRecommended(true);
        }
    }

    /**
     * 返回 zone_config 的全部行(按 sort_order 升序,不可变)。缓存新鲜直接返回;否则发起或加入那一次查库。
     * 查库异常原样抛出(等待方拿到的是同一个异常),不写缓存。
     */
    private List<ZoneConfig> getZoneRows() {
        ZoneRowsSnapshot cached = cachedRows;
        if (isFresh(cached)) {
            return cached.rows();
        }

        CompletableFuture<List<ZoneConfig>> mine = new CompletableFuture<>();
        CompletableFuture<List<ZoneConfig>> active = inflightLoad.compareAndExchange(null, mine);
        if (active != null) {
            return awaitLoad(active);
        }

        try {
            // 在上面的新鲜度检查与成为查库方之间,上一次查库可能刚写完缓存并摘掉 future;
            // 成为查库方后再看一次,避免紧邻的重复查库。
            cached = cachedRows;
            if (isFresh(cached)) {
                mine.complete(cached.rows());
                return cached.rows();
            }

            List<ZoneConfig> sorted = new ArrayList<>(zoneConfigRepo.findAll());
            sorted.sort(Comparator.comparingInt(ZoneConfig::getSortOrder));
            List<ZoneConfig> rows = List.copyOf(sorted);
            cachedRows = new ZoneRowsSnapshot(rows, nanoClock.getAsLong());
            mine.complete(rows);
            return rows;
        } catch (RuntimeException | Error e) {
            mine.completeExceptionally(e);
            throw e;
        } finally {
            inflightLoad.compareAndSet(mine, null);
        }
    }

    /** 年龄用差值比较(System.nanoTime 的约定写法,数值回绕时仍正确)。 */
    private boolean isFresh(ZoneRowsSnapshot snapshot) {
        return snapshot != null
                && nanoClock.getAsLong() - snapshot.loadedAtNanos() < ZONE_ROWS_CACHE_TTL_NANOS;
    }

    /**
     * 等待别人发起的那次查库,最多等 {@link #loadWaitMillis}。查库失败时抛查库方的原异常;到点未完成抛
     * {@link QueryTimeoutException}(查库方不受影响,继续查,结果照常写缓存)。限时等待可被中断,中断时恢复中断标记后失败。
     */
    private List<ZoneConfig> awaitLoad(CompletableFuture<List<ZoneConfig>> active) {
        try {
            return active.get(loadWaitMillis, TimeUnit.MILLISECONDS);
        } catch (ExecutionException e) {
            Throwable cause = e.getCause();
            if (cause instanceof RuntimeException runtime) {
                throw runtime;
            }
            if (cause instanceof Error error) {
                throw error;
            }
            throw new IllegalStateException("zone_config load failed", cause);
        } catch (TimeoutException e) {
            throw new QueryTimeoutException(
                    "zone_config load still in flight after " + loadWaitMillis + "ms", e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new IllegalStateException("interrupted while waiting for zone_config load", e);
        }
    }

    /**
     * 仅供同包测试(与包级时钟注入同一做法,只读不写):当前在途查库 future 上挂着的等待方数;没有在途查库时为 0。
     * 等待方只经 {@link #awaitLoad} 的限时 get 挂上,get 在 park 之前先把自己的等待节点压进 future 的依赖栈;
     * 本类不给在途 future 挂任何其他回调,所以完成之前这个数就是已挂上的请求数。等待方超时或被中断离开时会把自己的节点
     * 清出栈,计数随之下降,所以只在等待方不会超时的用例里当屏障用(等人齐再放行查库)。
     * 不参与任何业务判断(JDK 文档也写明 getNumberOfDependents 是监控用途,不作同步控制)。
     */
    int inflightWaiterCountForTest() {
        CompletableFuture<List<ZoneConfig>> active = inflightLoad.get();
        return active == null ? 0 : active.getNumberOfDependents();
    }

    private record ZoneRowsSnapshot(List<ZoneConfig> rows, long loadedAtNanos) {}

    private ZoneInfoDto toDto(ZoneConfig zone) {
        ZoneInfoDto dto = new ZoneInfoDto();
        dto.setZoneId(zone.getZoneId());
        dto.setName(zone.getName());
        dto.setRecommended(zone.isRecommended());

        ManualZoneStatus manualStatus = ManualZoneStatus.fromCode(zone.getManualStatus());
        AutoZoneStatus autoStatus = probeService.getAutoStatus(zone.getZoneId());
        LoadLevel loadLevel = probeService.getLoadLevel(zone.getZoneId());

        // Manual status takes priority
        ZoneDisplayStatus displayStatus = resolveDisplayStatus(manualStatus, autoStatus);
        dto.setStatus(displayStatus);

        if (displayStatus == ZoneDisplayStatus.OPEN && autoStatus != AutoZoneStatus.UNKNOWN) {
            dto.setLoadLevel(loadLevel);
        }

        if (displayStatus == ZoneDisplayStatus.MAINTENANCE || displayStatus == ZoneDisplayStatus.CLOSED) {
            dto.setMaintenanceMsg(zone.getMaintenanceMsg());
        }

        if (manualStatus == ManualZoneStatus.PREVIEW && zone.getOpenTime() != null) {
            dto.setOpenTime(zone.getOpenTime().toEpochSecond(ZoneOffset.UTC));
        }

        // Mark as "new" if created within 7 days
        if (zone.getCreatedAt() != null &&
                zone.getCreatedAt().isAfter(LocalDateTime.now().minusDays(7))) {
            dto.setNew(true);
        }

        return dto;
    }

    /**
     * Merge manual override with auto-detected status.
     * Manual status always wins when not OPEN.
     * When manual is OPEN, auto DOWN overrides to MAINTENANCE.
     */
    private ZoneDisplayStatus resolveDisplayStatus(ManualZoneStatus manual, AutoZoneStatus auto) {
        return switch (manual) {
            case CLOSED -> ZoneDisplayStatus.CLOSED;
            case MAINTENANCE -> ZoneDisplayStatus.MAINTENANCE;
            case PREVIEW -> ZoneDisplayStatus.PREVIEW;
            case OPEN -> {
                if (auto == AutoZoneStatus.DOWN) {
                    yield ZoneDisplayStatus.MAINTENANCE;
                }
                if (auto == AutoZoneStatus.UNKNOWN) {
                    // 对外 status 是既有四值协议（OPEN/MAINTENANCE/CLOSED/PREVIEW），
                    // 不能把内部探测态 UNKNOWN 直接扩成新的 wire enum；旧客户端会
                    // 无法解析。探测面不可用不等于区服 DOWN，因此保持手工 OPEN，
                    // 同时 getLoadLevel() 在 UNKNOWN 时返回 null，不发布伪造负载。
                    // 真正不可登录仍由 AssignGate 的实时节点选择 fail-closed。
                    yield ZoneDisplayStatus.OPEN;
                }
                yield ZoneDisplayStatus.OPEN;
            }
        };
    }
}
