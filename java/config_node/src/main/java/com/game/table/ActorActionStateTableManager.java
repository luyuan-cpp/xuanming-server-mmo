
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
 * Auto-generated config manager for ActorActionState.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class ActorActionStateTableManager {

    private static final ActorActionStateTableManager INSTANCE = new ActorActionStateTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final ActorActionStateTableData data;
        final Map<Integer, List<ActorActionStateTable>> kvData;





        Snapshot(ActorActionStateTableData data,
                 Map<Integer, List<ActorActionStateTable>> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            ActorActionStateTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static ActorActionStateTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        ActorActionStateTableData.Builder builder = ActorActionStateTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "actoractionstate.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "actoractionstate.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        ActorActionStateTableData data = builder.build();

        Map<Integer, List<ActorActionStateTable>> kvData = new HashMap<>(data.getDataCount());

        for (ActorActionStateTable row : data.getDataList()) {
            kvData.computeIfAbsent(row.getId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public ActorActionStateTableData findAll() {
        return snapshot.data;
    }

    /**
     * 返回该 id 命中的全部行。这张表的主键声明了 (cfg_multi)，单值语义不成立，
     * 因此不提供 findById —— 把可重复主键当单值用会在编译期断，而不是悄悄只看第一行。
     *
     * <p>热更契约：返回的对象属于当前快照，只许当场用，调用方**只存 id**。
     */
    public List<ActorActionStateTable> findAllById(int id) {
        return snapshot.kvData.getOrDefault(id, Collections.emptyList());
    }

    public Map<Integer, List<ActorActionStateTable>> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }










    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.data.getDataCount();
    }





    // ---- FindByIds (IN) ----

    public List<ActorActionStateTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<ActorActionStateTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            result.addAll(snap.kvData.getOrDefault(id, Collections.emptyList()));
        }
        return result;
    }

    // ---- RandOne ----

    public ActorActionStateTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<ActorActionStateTable> where(Predicate<ActorActionStateTable> pred) {
        Snapshot snap = this.snapshot;
        List<ActorActionStateTable> result = new ArrayList<>();
        for (ActorActionStateTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public ActorActionStateTable first(Predicate<ActorActionStateTable> pred) {
        Snapshot snap = this.snapshot;
        for (ActorActionStateTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}