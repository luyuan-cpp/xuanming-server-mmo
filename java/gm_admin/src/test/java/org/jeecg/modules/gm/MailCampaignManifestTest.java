package org.jeecg.modules.gm;

import static org.junit.jupiter.api.Assertions.*;

import java.util.ArrayList;
import java.util.List;
import org.junit.jupiter.api.Test;

class MailCampaignManifestTest {
    @Test
    void manifestAndShardHashMatchGoCampaignContract() {
        // 黄金向量由独立 Python 按 go/mail/internal/data/campaign.go 口径计算。
        var plan = MailCampaignManifest.build(List.of("9007199254740995", "1002", "1001", "1001"));
        assertEquals(3, plan.recipientCount());
        assertEquals(1, plan.shards().size());
        assertEquals(List.of("1001", "1002", "9007199254740995"), plan.shards().get(0));
        assertEquals("11269e20a25ea42638fcc4785b3d51aea3f810c92565363d2783730a172f1535",
                MailCampaignManifest.shardHashOf(plan.shards().get(0)));
        assertEquals("5b7e8f4bbd42c63231ae335896bda86cce9e5d4e70a138352ae97c349c3b70bf", plan.manifestHash());
    }

    @Test
    void numericUnsignedOrderingAndShardingWorkAcrossBoundaries() {
        List<String> ids = new ArrayList<>();
        for (int i = 1005; i >= 1; i--) ids.add(Integer.toString(i));
        ids.add("18446744073709551615");
        var plan = MailCampaignManifest.build(ids);
        assertEquals(1006, plan.recipientCount());
        assertEquals(2, plan.shards().size());
        assertEquals(1000, plan.shards().get(0).size());
        assertEquals(6, plan.shards().get(1).size());
        assertEquals("1", plan.shards().get(0).get(0));
        assertEquals("2", plan.shards().get(0).get(1));
        assertEquals("10", plan.shards().get(0).get(9));
        assertEquals("18446744073709551615", plan.shards().get(1).get(5));
        assertEquals(plan.manifestHash(), MailCampaignManifest.manifestHashOf(plan.shards()));
    }

    @Test
    void rejectsInvalidEmptyOrOverCapRecipients() {
        assertThrows(IllegalArgumentException.class, () -> MailCampaignManifest.build(List.of()));
        assertThrows(IllegalArgumentException.class, () -> MailCampaignManifest.build(List.of("0")));
        assertThrows(IllegalArgumentException.class, () -> MailCampaignManifest.build(List.of("-1")));
        assertThrows(IllegalArgumentException.class, () -> MailCampaignManifest.build(List.of("18446744073709551616")));
        List<String> tooMany = new ArrayList<>(MailCampaignManifest.MAX_WEB_RECIPIENTS + 1);
        for (int i = 1; i <= MailCampaignManifest.MAX_WEB_RECIPIENTS + 1; i++) tooMany.add(Integer.toString(i));
        assertThrows(IllegalArgumentException.class, () -> MailCampaignManifest.build(tooMany));
    }
}
