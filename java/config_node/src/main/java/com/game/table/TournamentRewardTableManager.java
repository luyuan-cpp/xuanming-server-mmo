
package com.game.table;

import com.google.protobuf.util.JsonFormat;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ThreadLocalRandom;
import java.util.function.Predicate;

/**
 * Auto-generated config manager for TournamentReward.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class TournamentRewardTableManager {

    private static final TournamentRewardTableManager INSTANCE = new TournamentRewardTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final TournamentRewardTableData data;
        final Map<Integer, TournamentRewardTable> kvData;



        final Map<Integer, List<TournamentRewardTable>> idxItemIds;

        final Map<Integer, List<TournamentRewardTable>> idxItemCounts;


        final Map<Integer, List<TournamentRewardTable>> idxTierKind;


        Snapshot(TournamentRewardTableData data,
                 Map<Integer, TournamentRewardTable> kvData,
                 Map<Integer, List<TournamentRewardTable>> idxItemIds,
                 Map<Integer, List<TournamentRewardTable>> idxItemCounts,
                 Map<Integer, List<TournamentRewardTable>> idxTierKind) {
            this.data = data;
            this.kvData = kvData;
            this.idxItemIds = idxItemIds;
            this.idxItemCounts = idxItemCounts;
            this.idxTierKind = idxTierKind;
        }
    }

    private Snapshot snapshot = new Snapshot(
            TournamentRewardTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static TournamentRewardTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        TournamentRewardTableData.Builder builder = TournamentRewardTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "tournamentreward.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "tournamentreward.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        TournamentRewardTableData data = builder.build();

        Map<Integer, TournamentRewardTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<TournamentRewardTable>> idxItemIds = new HashMap<>();
        Map<Integer, List<TournamentRewardTable>> idxItemCounts = new HashMap<>();
        Map<Integer, List<TournamentRewardTable>> idxTierKind = new HashMap<>();

        for (TournamentRewardTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            for (Integer elem : row.getItemIdsList()) {
                idxItemIds.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            for (Integer elem : row.getItemCountsList()) {
                idxItemCounts.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            idxTierKind.computeIfAbsent(row.getTierKind(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxItemIds, idxItemCounts, idxTierKind);
    }

    public TournamentRewardTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public TournamentRewardTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, TournamentRewardTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }





    public List<TournamentRewardTable> findByItemIdsIndex(int key) {
        return snapshot.idxItemIds.getOrDefault(key, Collections.emptyList());
    }

    public List<TournamentRewardTable> findByItemCountsIndex(int key) {
        return snapshot.idxItemCounts.getOrDefault(key, Collections.emptyList());
    }



    public List<TournamentRewardTable> getByTierKind(int key) {
        return snapshot.idxTierKind.getOrDefault(key, Collections.emptyList());
    }



    // FK: item_ids → Item.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }



    public int countByItemIdsIndex(int key) {
        return snapshot.idxItemIds.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByItemCountsIndex(int key) {
        return snapshot.idxItemCounts.getOrDefault(key, Collections.emptyList()).size();
    }


    public int countByTierKindIndex(int key) {
        return snapshot.idxTierKind.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<TournamentRewardTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<TournamentRewardTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            TournamentRewardTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public TournamentRewardTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<TournamentRewardTable> where(Predicate<TournamentRewardTable> pred) {
        Snapshot snap = this.snapshot;
        List<TournamentRewardTable> result = new ArrayList<>();
        for (TournamentRewardTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public TournamentRewardTable first(Predicate<TournamentRewardTable> pred) {
        Snapshot snap = this.snapshot;
        for (TournamentRewardTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}