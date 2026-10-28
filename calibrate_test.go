package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCalibrateFromPrometheus(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		// osd.0 hdd: простой и 4 замера recovery 60..120 MB/s; osd.1 ssd: 300 MB/s.
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{
			map[string]any{"metric": map[string]string{"ceph_daemon": "osd.0", "device_class": "hdd"},
				"values": [][2]any{{1, "0"}, {2, "60e6"}, {3, "80e6"}, {4, "100e6"}, {5, "120e6"}, {6, "0.2e6"}}},
			map[string]any{"metric": map[string]string{"ceph_daemon": "osd.1", "device_class": "ssd"},
				"values": [][2]any{{1, "300e6"}, {2, "300e6"}}},
		}}})
	}))
	defer srv.Close()

	m, err := promQueryRange(srv.Client(), srv.URL, "", "q", time.Unix(0, 0), time.Unix(60, 0), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	stats, slots, err := Calibrate(m, 1, 75, 2)
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "q" {
		t.Fatalf("query %q", gotQuery)
	}
	if h := stats["hdd"]; h.Samples != 4 || h.OSDs != 1 || h.P50MBps != 80 {
		t.Fatalf("hdd %+v", h)
	}
	// p75 из [60 80 100 120] = 100, делим на backfills=2.
	if slots["hdd"] != 50 || slots["ssd"] != 150 {
		t.Fatalf("slots %v", slots)
	}
}

func TestCalibrateNoRecovery(t *testing.T) {
	m := &promMatrix{}
	m.Data.Result = append(m.Data.Result, struct {
		Metric map[string]string `json:"metric"`
		Values [][2]any          `json:"values"`
	}{Metric: map[string]string{"ceph_daemon": "osd.0"}, Values: [][2]any{{1, "0"}}})
	if _, _, err := Calibrate(m, 1, 75, 1); err == nil || !strings.Contains(err.Error(), "нет recovery") {
		t.Fatalf("ожидаю ошибку про отсутствие recovery, получил %v", err)
	}
}

func TestCalibratePools(t *testing.T) {
	mk := func(series ...map[string]any) *promMatrix {
		b, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": series}})
		var m promMatrix
		json.Unmarshal(b, &m)
		return &m
	}
	// пул 7 (hdd): 400 MB/s при 4 PG в backfill = 100 на PG; в момент 2 recovery нет.
	// пул 6 (ssd): 600 MB/s при 2 PG = 300.
	rate := mk(
		map[string]any{"metric": map[string]string{"pool_id": "7"}, "values": [][2]any{{1, "400e6"}, {2, "0"}}},
		map[string]any{"metric": map[string]string{"pool_id": "6"}, "values": [][2]any{{1, "600e6"}}},
	)
	pgs := mk(
		map[string]any{"metric": map[string]string{"pool_id": "7"}, "values": [][2]any{{1, "4"}, {2, "0"}}},
		map[string]any{"metric": map[string]string{"pool_id": "6"}, "values": [][2]any{{1, "2"}}},
	)
	stats, slots, err := CalibratePools(rate, pgs, map[string]string{"7": "hdd", "6": "ssd"}, 1, 1e6, 75)
	if err != nil {
		t.Fatal(err)
	}
	if slots["hdd"] != 100 || slots["ssd"] != 300 || stats["hdd"].Samples != 1 {
		t.Fatalf("slots %v stats %v", slots, stats)
	}
}

func TestPoolClasses(t *testing.T) {
	c, err := poolClasses("testdata/snap-synthetic")
	if err != nil {
		t.Skip("нет снимка testdata/snap-synthetic")
	}
	// правило без класса: класс пула - самый частый класс OSD
	if c["1"] != "hdd" {
		t.Fatalf("%v", c)
	}
}

func TestObjectLimitDominates(t *testing.T) {
	// 1 GiB мелкими объектами по 4 KiB: по байтам 40 MB/s - секунды, по объектам 100 об/с - минуты.
	c := &Core{OSDProj: map[int]*OSDProj{0: {ID: 0, Class: "ssd", In: 1 << 30, InObj: 262144}}}
	sc := Scenario{Backfills: 2, SlotMBps: map[string]float64{"ssd": 40}}
	tb, _ := recoveryTime(c, sc)
	sc.SlotOps = map[string]float64{"ssd": 100}
	to, who := recoveryTime(c, sc)
	if who.Limit != "objects" || to <= tb || to.Round(time.Second) != 1311*time.Second {
		t.Fatalf("bytes %v, objects %v, limit %q", tb, to, who.Limit)
	}
}
