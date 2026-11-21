package org.jeecg.modules.gm;

import java.math.BigInteger;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.ArrayList;
import java.util.HexFormat;
import java.util.List;
import java.util.TreeSet;

/**
 * 与 go/mail/internal/data/campaign.go 逐字节一致的活动名单分片与摘要计算器。
 * 每片至多 1000 个去重并按 uint64 升序排列的十进制玩家 ID，摘要算法：
 *   shardHash    = sha256(每个 "id\n" 拼接)
 *   manifestHash = sha256("v1\n" + 按 shard_index 升序拼接 "index:shardHash:count\n")
 */
final class MailCampaignManifest {
    static final int SHARD_SIZE = 1000;
    static final int MAX_WEB_RECIPIENTS = 10_000;
    private static final BigInteger UINT64_MAX = new BigInteger("18446744073709551615");

    record Plan(List<List<String>> shards, int recipientCount, String manifestHash) { }

    private MailCampaignManifest() { }

    static Plan build(List<String> rawIds) {
        if (rawIds == null || rawIds.isEmpty()) {
            throw new IllegalArgumentException("收件人名单不能为空");
        }
        TreeSet<BigInteger> unique = new TreeSet<>();
        for (String raw : rawIds) {
            BigInteger id = parsePositiveUint64(raw);
            unique.add(id);
            if (unique.size() > MAX_WEB_RECIPIENTS) {
                throw new IllegalArgumentException("单次网页活动邮件收件人不能超过 " + MAX_WEB_RECIPIENTS + " 人");
            }
        }
        List<String> sorted = new ArrayList<>(unique.size());
        for (BigInteger id : unique) sorted.add(id.toString());

        List<List<String>> shards = new ArrayList<>();
        for (int start = 0; start < sorted.size(); start += SHARD_SIZE) {
            int end = Math.min(start + SHARD_SIZE, sorted.size());
            shards.add(List.copyOf(sorted.subList(start, end)));
        }
        String manifestHash = manifestHashOf(shards);
        return new Plan(List.copyOf(shards), sorted.size(), manifestHash);
    }

    static String manifestHashOf(List<List<String>> shards) {
        if (shards == null || shards.isEmpty() || shards.size() > 10_000) {
            throw new IllegalArgumentException("活动名单分片数量非法");
        }
        StringBuilder manifest = new StringBuilder("v1\n");
        for (int index = 0; index < shards.size(); index++) {
            List<String> shard = shards.get(index);
            if (shard == null || shard.isEmpty() || shard.size() > SHARD_SIZE) {
                throw new IllegalArgumentException("活动名单分片大小非法");
            }
            String hash = shardHashOf(shard);
            manifest.append(index).append(':').append(hash).append(':').append(shard.size()).append('\n');
        }
        return sha256Hex(manifest.toString());
    }

    static String shardHashOf(List<String> ids) {
        if (ids == null || ids.isEmpty() || ids.size() > SHARD_SIZE) {
            throw new IllegalArgumentException("收件人分片大小非法");
        }
        TreeSet<BigInteger> sorted = new TreeSet<>();
        for (String id : ids) sorted.add(parsePositiveUint64(id));
        if (sorted.size() != ids.size()) {
            throw new IllegalArgumentException("收件人分片包含重复玩家 ID");
        }
        StringBuilder payload = new StringBuilder(sorted.size() * 20);
        for (BigInteger id : sorted) {
            payload.append(id).append('\n');
        }
        return sha256Hex(payload.toString());
    }

    private static BigInteger parsePositiveUint64(String raw) {
        if (raw == null) throw new IllegalArgumentException("收件人玩家 ID 非法");
        String text = raw.strip();
        if (!text.matches("[1-9][0-9]{0,19}")) throw new IllegalArgumentException("收件人玩家 ID 必须是非零 uint64 十进制字符串");
        BigInteger value = new BigInteger(text);
        if (value.compareTo(UINT64_MAX) > 0) throw new IllegalArgumentException("收件人玩家 ID 超出 uint64 范围");
        return value;
    }

    private static String sha256Hex(String text) {
        try {
            byte[] digest = MessageDigest.getInstance("SHA-256").digest(text.getBytes(StandardCharsets.UTF_8));
            return HexFormat.of().formatHex(digest);
        } catch (NoSuchAlgorithmException error) {
            throw new IllegalStateException("SHA-256 unavailable", error);
        }
    }
}
