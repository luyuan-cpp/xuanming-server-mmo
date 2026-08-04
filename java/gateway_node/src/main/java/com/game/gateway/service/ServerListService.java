package com.game.gateway.service;

import com.game.gateway.dto.*;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.repository.ZoneConfigRepository;
import org.springframework.stereotype.Service;

import java.time.LocalDateTime;
import java.time.ZoneOffset;
import java.util.Comparator;
import java.util.List;

@Service
public class ServerListService {

    private final ZoneConfigRepository zoneConfigRepo;
    private final ZoneHealthProbeService probeService;

    public ServerListService(ZoneConfigRepository zoneConfigRepo,
                             ZoneHealthProbeService probeService) {
        this.zoneConfigRepo = zoneConfigRepo;
        this.probeService = probeService;
    }

    public ServerListResponse getServerList() {
        List<ZoneConfig> zones = new java.util.ArrayList<>(zoneConfigRepo.findAll());
        zones.sort(Comparator.comparingInt(ZoneConfig::getSortOrder));

        List<ZoneInfoDto> dtos = zones.stream().map(this::toDto).toList();
        return new ServerListResponse(dtos);
    }

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
