
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
 * Auto-generated config manager for SpiritShopGoods.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class SpiritShopGoodsTableManager {

    private static final SpiritShopGoodsTableManager INSTANCE = new SpiritShopGoodsTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final SpiritShopGoodsTableData data;
        final Map<Integer, SpiritShopGoodsTable> kvData;




        final Map<Integer, List<SpiritShopGoodsTable>> idxItemId1;


        Snapshot(SpiritShopGoodsTableData data,
                 Map<Integer, SpiritShopGoodsTable> kvData,
                 Map<Integer, List<SpiritShopGoodsTable>> idxItemId1) {
            this.data = data;
            this.kvData = kvData;
            this.idxItemId1 = idxItemId1;
        }
    }

    private Snapshot snapshot = new Snapshot(
            SpiritShopGoodsTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static SpiritShopGoodsTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        SpiritShopGoodsTableData.Builder builder = SpiritShopGoodsTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "spiritshopgoods.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "spiritshopgoods.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        SpiritShopGoodsTableData data = builder.build();

        Map<Integer, SpiritShopGoodsTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<SpiritShopGoodsTable>> idxItemId1 = new HashMap<>();

        for (SpiritShopGoodsTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxItemId1.computeIfAbsent(row.getItemId1(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxItemId1);
    }

    public SpiritShopGoodsTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public SpiritShopGoodsTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, SpiritShopGoodsTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<SpiritShopGoodsTable> getByItemId1(int key) {
        return snapshot.idxItemId1.getOrDefault(key, Collections.emptyList());
    }



    // FK: item_id_1 → Item.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByItemId1Index(int key) {
        return snapshot.idxItemId1.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<SpiritShopGoodsTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<SpiritShopGoodsTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            SpiritShopGoodsTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public SpiritShopGoodsTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<SpiritShopGoodsTable> where(Predicate<SpiritShopGoodsTable> pred) {
        Snapshot snap = this.snapshot;
        List<SpiritShopGoodsTable> result = new ArrayList<>();
        for (SpiritShopGoodsTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public SpiritShopGoodsTable first(Predicate<SpiritShopGoodsTable> pred) {
        Snapshot snap = this.snapshot;
        for (SpiritShopGoodsTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}