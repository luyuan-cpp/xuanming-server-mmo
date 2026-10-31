package com.game.gateway.service;

import com.game.gateway.entity.Announcement;
import com.game.gateway.repository.AnnouncementRepository;
import org.springframework.stereotype.Service;

import java.time.LocalDateTime;
import java.util.List;

@Service
public class AnnouncementService {

    private final AnnouncementRepository announcementRepo;

    public AnnouncementService(AnnouncementRepository announcementRepo) {
        this.announcementRepo = announcementRepo;
    }

    public List<Announcement> getActiveAnnouncements() {
        return announcementRepo.findActive(LocalDateTime.now());
    }

    public List<Announcement> getAll() {
        return announcementRepo.findAll();
    }

    public Announcement create(Announcement announcement) {
        // 请求体里的 id 不算数:带 id 的 save() 走 merge,会按主键改掉已有的公告,或者按客户端给的号插一行。
        announcement.setId(null);
        return announcementRepo.save(announcement);
    }

    public void delete(Long id) {
        announcementRepo.deleteById(id);
    }
}
