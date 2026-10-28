// genfixture превращает синтетический osdmap (crushtool --build + osdmaptool --createsimple)
// в снимок того же вида, что пишет `cephsim collect`. Объёмы PG случайные, раскладка - настоящая.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func readJSON(path string, v any) {
	b, err := os.ReadFile(path)
	must(err)
	must(json.Unmarshal(b, v))
}

func writeJSON(path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	must(err)
	must(os.WriteFile(path, b, 0o644))
}

func main() {
	raw := flag.String("raw", "", "каталог с dump.json, tree.json, mapping.txt")
	out := flag.String("out", "", "каталог снимка")
	osdTB := flag.Float64("osd-tb", 16, "размер OSD, TB")
	fill := flag.Float64("fill", 0.62, "средняя заполненность")
	writeMBps := flag.Float64("write-mbps", 1200, "клиентская запись для status.json, MB/s")
	flag.Parse()

	var dump map[string]any
	readJSON(filepath.Join(*raw, "dump.json"), &dump)
	dump["full_ratio"], dump["backfillfull_ratio"], dump["nearfull_ratio"] = 0.95, 0.90, 0.85
	// У --createsimple OSD не существуют; в снимке они как в живом кластере: up, in.
	var osdList []map[string]any
	for i := 0; i < int(dump["max_osd"].(float64)); i++ {
		osdList = append(osdList, map[string]any{"osd": i, "up": 1, "in": 1, "weight": 1.0, "primary_affinity": 1.0, "state": []string{"exists", "up"}})
	}
	dump["osds"] = osdList
	writeJSON(filepath.Join(*out, "osd_dump.json"), dump)

	var tree struct {
		Nodes []map[string]any `json:"nodes"`
	}
	readJSON(filepath.Join(*raw, "tree.json"), &tree)
	var osds []int
	for _, n := range tree.Nodes {
		if n["type"] == "osd" {
			n["device_class"], n["status"], n["reweight"], n["exists"] = "hdd", "up", 1.0, 1
			osds = append(osds, int(n["id"].(float64)))
		}
	}
	writeJSON(filepath.Join(*out, "osd_tree.json"), tree)

	f, err := os.Open(filepath.Join(*raw, "mapping.txt"))
	must(err)
	re := regexp.MustCompile(`^(\d+\.[0-9a-f]+)\s+\[([0-9,]*)\]`)
	type pg struct {
		id  string
		set []int
	}
	var pgs []pg
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := re.FindStringSubmatch(sc.Text()); m != nil {
			var set []int
			for _, x := range strings.Split(m[2], ",") {
				id, _ := strconv.Atoi(x)
				set = append(set, id)
			}
			pgs = append(pgs, pg{m[1], set})
		}
	}
	must(sc.Err())

	osdBytes := *osdTB * 1e12
	replicas := 0
	for _, p := range pgs {
		replicas += len(p.set)
	}
	mean := *fill * osdBytes * float64(len(osds)) / float64(replicas)
	rng := rand.New(rand.NewSource(42))
	used := map[int]float64{}
	var stats []map[string]any
	for _, p := range pgs {
		b := mean * math.Exp(rng.NormFloat64()*0.25-0.03125)
		for _, o := range p.set {
			used[o] += b
		}
		stats = append(stats, map[string]any{
			"pgid": p.id, "state": "active+clean", "up": p.set, "acting": p.set,
			"stat_sum": map[string]any{"num_bytes": int64(b), "num_objects": int64(b / 4e6)},
		})
	}
	writeJSON(filepath.Join(*out, "pg_ls.json"), map[string]any{"pg_ready": true, "pg_stats": stats})

	var df []map[string]any
	for _, o := range osds {
		kb := int64(osdBytes / 1024)
		df = append(df, map[string]any{
			"id": o, "name": "osd." + strconv.Itoa(o), "device_class": "hdd", "crush_weight": 1.0, "reweight": 1.0,
			"kb": kb, "kb_used": int64(used[o]/1024) + kb/100, "utilization": (used[o]/osdBytes + 0.01) * 100,
		})
	}
	writeJSON(filepath.Join(*out, "osd_df.json"), map[string]any{"nodes": df})
	writeJSON(filepath.Join(*out, "status.json"), map[string]any{"pgmap": map[string]any{"write_bytes_sec": *writeMBps * 1e6}})
	writeJSON(filepath.Join(*out, "ec_profiles.json"), map[string]any{})
	writeJSON(filepath.Join(*out, "meta.json"), map[string]any{
		"collected_at": time.Now().Format("2006-01-02 15:04"), "cluster": "synthetic", "synthetic": true,
		// у созданного --createsimple osdmap все OSD down/out
		"osdmaptool_args": []string{"--mark-up-in"},
	})
	log.Printf("снимок: %d OSD, %d PG", len(osds), len(pgs))
}
