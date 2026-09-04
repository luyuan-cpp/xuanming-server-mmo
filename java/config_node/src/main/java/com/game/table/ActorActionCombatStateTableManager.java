
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
 * Auto-generated config manager for ActorActionCombatState.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class ActorActionCombatStateTableManager {

    private static final ActorActionCombatStateTableManager INSTANCE = new ActorActionCombatStateTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final ActorActionCombatStateTableData data;
        final Map<Integer, ActorActionCombatStateTable> kvData;





        Snapshot(ActorActionCombatStateTableData data,
                 Map<Integer, ActorActionCombatStateTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            ActorActionCombatStateTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static ActorActionCombatStateTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        ActorActionCombatStateTableData.Builder builder = ActorActionCombatStateTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "actoractioncombatstate.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "actoractioncombatstate.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        ActorActionCombatStateTableData data = builder.build();

        Map<Integer, ActorActionCombatStateTable> kvData = new HashMap<>(data.getDataCount());

        for (ActorActionCombatStateTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public ActorActionCombatStateTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public ActorActionCombatStateTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, ActorActionCombatStateTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }










    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }





    // ---- FindByIds (IN) ----

    public List<ActorActionCombatStateTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<ActorActionCombatStateTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            ActorActionCombatStateTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public ActorActionCombatStateTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<ActorActionCombatStateTable> where(Predicate<ActorActionCombatStateTable> pred) {
        Snapshot snap = this.snapshot;
        List<ActorActionCombatStateTable> result = new ArrayList<>();
        for (ActorActionCombatStateTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public ActorActionCombatStateTable first(Predicate<ActorActionCombatStateTable> pred) {
        Snapshot snap = this.snapshot;
        for (ActorActionCombatStateTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}