
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
 * Auto-generated config manager for PlayMode.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class PlayModeTableManager {

    private static final PlayModeTableManager INSTANCE = new PlayModeTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final PlayModeTableData data;
        final Map<Integer, PlayModeTable> kvData;




        final Map<Integer, List<PlayModeTable>> idxBattleConfigId;


        Snapshot(PlayModeTableData data,
                 Map<Integer, PlayModeTable> kvData,
                 Map<Integer, List<PlayModeTable>> idxBattleConfigId) {
            this.data = data;
            this.kvData = kvData;
            this.idxBattleConfigId = idxBattleConfigId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            PlayModeTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static PlayModeTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        PlayModeTableData.Builder builder = PlayModeTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "playmode.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "playmode.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        PlayModeTableData data = builder.build();

        Map<Integer, PlayModeTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<PlayModeTable>> idxBattleConfigId = new HashMap<>();

        for (PlayModeTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxBattleConfigId.computeIfAbsent(row.getBattleConfigId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxBattleConfigId);
    }

    public PlayModeTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public PlayModeTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, PlayModeTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<PlayModeTable> getByBattleConfigId(int key) {
        return snapshot.idxBattleConfigId.getOrDefault(key, Collections.emptyList());
    }



    // FK: battle_config_id → Dungeon.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByBattleConfigIdIndex(int key) {
        return snapshot.idxBattleConfigId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<PlayModeTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<PlayModeTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            PlayModeTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public PlayModeTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<PlayModeTable> where(Predicate<PlayModeTable> pred) {
        Snapshot snap = this.snapshot;
        List<PlayModeTable> result = new ArrayList<>();
        for (PlayModeTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public PlayModeTable first(Predicate<PlayModeTable> pred) {
        Snapshot snap = this.snapshot;
        for (PlayModeTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}