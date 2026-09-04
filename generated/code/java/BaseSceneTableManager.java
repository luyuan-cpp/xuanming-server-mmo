
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
 * Auto-generated config manager for BaseScene.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class BaseSceneTableManager {

    private static final BaseSceneTableManager INSTANCE = new BaseSceneTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final BaseSceneTableData data;
        final Map<Integer, BaseSceneTable> kvData;





        Snapshot(BaseSceneTableData data,
                 Map<Integer, BaseSceneTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            BaseSceneTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static BaseSceneTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        BaseSceneTableData.Builder builder = BaseSceneTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "basescene.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "basescene.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        BaseSceneTableData data = builder.build();

        Map<Integer, BaseSceneTable> kvData = new HashMap<>(data.getDataCount());

        for (BaseSceneTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public BaseSceneTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public BaseSceneTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, BaseSceneTable> getKvData() {
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

    public List<BaseSceneTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<BaseSceneTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            BaseSceneTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public BaseSceneTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<BaseSceneTable> where(Predicate<BaseSceneTable> pred) {
        Snapshot snap = this.snapshot;
        List<BaseSceneTable> result = new ArrayList<>();
        for (BaseSceneTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public BaseSceneTable first(Predicate<BaseSceneTable> pred) {
        Snapshot snap = this.snapshot;
        for (BaseSceneTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}