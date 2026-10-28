package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const fCalibration = "calibration.json"

type ClassStat struct {
	Samples int     `json:"samples"`
	OSDs    int     `json:"osds"`
	P50MBps float64 `json:"p50_mbps"`
	P90MBps float64 `json:"p90_mbps"`
	UsedMBp float64 `json:"used_mbps"` // перцентиль, взятый в модель
}

// Calibration - скорость одного backfill-слота по истории Prometheus.
// osd: скорость занятого OSD из ceph_osd_recovery_bytes, делённая на osd_max_backfills того периода.
// pool: ceph_pool_recovering_bytes_per_sec / (ceph_pg_backfilling + ceph_pg_recovering) - скорость
// одной PG в recovery напрямую; не зависит от osd_max_backfills и годится для mClock.
type Calibration struct {
	Method           string               `json:"method"` // osd: per-OSD счётчик ceph-exporter; pool: скорость пула / PG в recovery
	Source           string               `json:"source"`
	Since            string               `json:"since"`
	Until            string               `json:"until"`
	Step             string               `json:"step"`
	Percentile       int                  `json:"percentile"`
	HistoryBackfills int                  `json:"history_backfills"`
	Classes          map[string]ClassStat `json:"classes"`
	SlotMBps         map[string]float64   `json:"slot_mbps"`
	// Объектов/с на один слот, по пулам: ceph_pool_recovering_objects_per_sec / PG в recovery.
	ObjClasses map[string]ClassStat `json:"object_classes,omitempty"`
	SlotOps    map[string]float64   `json:"slot_objects_per_sec,omitempty"`
	// Клиентская нагрузка на один OSD класса (среднее по OSD) за весь период и во время recovery.
	Client            map[string]ClientStats `json:"client,omitempty"`
	ClientReplication float64                `json:"client_write_replication,omitempty"`
}

// avgPoolSize - средний size пулов, взвешенный по числу PG.
func avgPoolSize(dir string) float64 {
	var d OSDDump
	if readJSON(dir, fOSDDump, &d) != nil {
		return 3
	}
	var sum, n float64
	for _, p := range d.Pools {
		sum += float64(p.Size * p.PGNum)
		n += float64(p.PGNum)
	}
	if n == 0 {
		return 3
	}
	return sum / n
}

type ClientStats struct {
	P50       ClientLoad `json:"p50"`
	P95       ClientLoad `json:"p95"`
	P99       ClientLoad `json:"p99"`
	DuringCal ClientLoad `json:"during_recovery"`
	CalPoints int        `json:"during_recovery_points"`
	Samples   int        `json:"samples"`
}

// Load возвращает нагрузку по имени: p50, p95, p99, recovery.
func (c ClientStats) Load(name string) (ClientLoad, bool) {
	switch name {
	case "p50":
		return c.P50, true
	case "p95":
		return c.P95, true
	case "p99":
		return c.P99, true
	case "recovery":
		return c.DuringCal, c.CalPoints > 0
	}
	return ClientLoad{}, false
}

// ClientStatsFrom: ряды средней по OSD класса клиентской нагрузки (ops и байты)
// -> перцентили за период и среднее в моменты, когда шёл recovery (active[class][ts]).
func ClientStatsFrom(ops, bytes *promMatrix, active map[string]map[string]bool) map[string]ClientStats {
	series := func(m *promMatrix) map[string]map[string]float64 {
		out := map[string]map[string]float64{}
		for _, s := range m.Data.Result {
			class := s.Metric["device_class"]
			if out[class] == nil {
				out[class] = map[string]float64{}
			}
			for _, v := range s.Values {
				if f, err := strconv.ParseFloat(fmt.Sprint(v[1]), 64); err == nil {
					out[class][fmt.Sprint(v[0])] = f
				}
			}
		}
		return out
	}
	o, b := series(ops), series(bytes)
	res := map[string]ClientStats{}
	for class, byTS := range o {
		var opsV, bytesV []float64
		var calO, calB float64
		n := 0
		for ts, v := range byTS {
			opsV = append(opsV, v)
			bytesV = append(bytesV, b[class][ts])
			if active[class][ts] {
				calO += v
				calB += b[class][ts]
				n++
			}
		}
		sort.Float64s(opsV)
		sort.Float64s(bytesV)
		st := ClientStats{Samples: len(opsV),
			P50: ClientLoad{percentile(opsV, 50), percentile(bytesV, 50)},
			P95: ClientLoad{percentile(opsV, 95), percentile(bytesV, 95)},
			P99: ClientLoad{percentile(opsV, 99), percentile(bytesV, 99)},
		}
		if n > 0 {
			st.DuringCal = ClientLoad{calO / float64(n), calB / float64(n)}
			st.CalPoints = n
		}
		res[class] = st
	}
	return res
}

func LoadCalibration(dir string) (*Calibration, error) {
	b, err := os.ReadFile(filepath.Join(dir, fCalibration))
	if err != nil {
		return nil, err
	}
	var c Calibration
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", fCalibration, err)
	}
	return &c, nil
}

type promMatrix struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func promQueryRange(client *http.Client, base, token, query string, start, end time.Time, step time.Duration) (*promMatrix, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(end.Unix(), 10))
	q.Set("step", strconv.Itoa(int(step.Seconds())))
	req, err := http.NewRequest("GET", strings.TrimRight(base, "/")+"/api/v1/query_range?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var m promMatrix
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("Prometheus %s: %s", resp.Status, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	if m.Status != "success" {
		return nil, fmt.Errorf("Prometheus: %s", m.Error)
	}
	return &m, nil
}

func percentile(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * float64(p) / 100)
	return sorted[i]
}

// Calibrate: замеры скорости по OSD, где идёт recovery (rate выше minMBps),
// перцентиль по классу дисков, делённый на historyBackfills.
func Calibrate(m *promMatrix, minMBps float64, pct, historyBackfills int) (map[string]ClassStat, map[string]float64, error) {
	samples := map[string][]float64{}
	osds := map[string]map[string]bool{}
	for _, s := range m.Data.Result {
		class := s.Metric["device_class"]
		if class == "" {
			class = "hdd"
		}
		for _, v := range s.Values {
			str, _ := v[1].(string)
			bps, err := strconv.ParseFloat(str, 64)
			if err != nil || bps/1e6 < minMBps {
				continue
			}
			samples[class] = append(samples[class], bps/1e6)
			if osds[class] == nil {
				osds[class] = map[string]bool{}
			}
			osds[class][s.Metric["ceph_daemon"]] = true
		}
	}
	if len(samples) == 0 {
		return nil, nil, fmt.Errorf("в истории нет recovery быстрее %.0f MB/s ни на одном OSD: увеличьте -since или уменьшите -min-mbps", minMBps)
	}
	stats, slots := map[string]ClassStat{}, map[string]float64{}
	for class, xs := range samples {
		sort.Float64s(xs)
		st := ClassStat{Samples: len(xs), OSDs: len(osds[class]), P50MBps: percentile(xs, 50), P90MBps: percentile(xs, 90), UsedMBp: percentile(xs, pct)}
		stats[class] = st
		slots[class] = st.UsedMBp / float64(historyBackfills)
	}
	return stats, slots, nil
}

// CalibratePools: замеры по пулу и моменту времени, где идёт recovery,
// скорость пула делится на число PG в backfill/recovery. Класс пула - из CRUSH-правила.
// scale переводит единицы метрики: 1e6 для байт в MB/s, 1 для объектов/с; minRate - в этих единицах.
func CalibratePools(rate, pgs *promMatrix, poolClass map[string]string, minRate, scale float64, pct int) (map[string]ClassStat, map[string]float64, error) {
	type key struct {
		pool string
		ts   string
	}
	active := map[key]float64{}
	for _, s := range pgs.Data.Result {
		for _, v := range s.Values {
			n, _ := strconv.ParseFloat(fmt.Sprint(v[1]), 64)
			if n >= 1 {
				active[key{s.Metric["pool_id"], fmt.Sprint(v[0])}] = n
			}
		}
	}
	samples := map[string][]float64{}
	pools := map[string]map[string]bool{}
	for _, s := range rate.Data.Result {
		pool := s.Metric["pool_id"]
		class := poolClass[pool]
		if class == "" {
			class = "hdd"
		}
		for _, v := range s.Values {
			bps, err := strconv.ParseFloat(fmt.Sprint(v[1]), 64)
			n := active[key{pool, fmt.Sprint(v[0])}]
			if err != nil || n == 0 || bps/scale < minRate {
				continue
			}
			samples[class] = append(samples[class], bps/scale/n)
			if pools[class] == nil {
				pools[class] = map[string]bool{}
			}
			pools[class][pool] = true
		}
	}
	if len(samples) == 0 {
		return nil, nil, fmt.Errorf("в истории нет recovery быстрее %g в единицах метрики ни в одном пуле: увеличьте -since или уменьшите -min-mbps", minRate)
	}
	stats, slots := map[string]ClassStat{}, map[string]float64{}
	for class, xs := range samples {
		sort.Float64s(xs)
		st := ClassStat{Samples: len(xs), OSDs: len(pools[class]), P50MBps: percentile(xs, 50), P90MBps: percentile(xs, 90), UsedMBp: percentile(xs, pct)}
		stats[class] = st
		slots[class] = st.UsedMBp
	}
	return stats, slots, nil
}

// poolClasses: класс устройств пула по шагу take его CRUSH-правила (default~ssd → ssd),
// для правил без класса - самый частый класс OSD в кластере.
func poolClasses(dir string) (map[string]string, error) {
	var d OSDDump
	if err := readJSON(dir, fOSDDump, &d); err != nil {
		return nil, err
	}
	var c struct {
		Rules []struct {
			ID    int `json:"rule_id"`
			Steps []struct {
				Op       string `json:"op"`
				ItemName string `json:"item_name"`
			} `json:"steps"`
		} `json:"rules"`
	}
	if err := readJSON(dir, fCrush, &c); err != nil {
		return nil, err
	}
	var df struct {
		Nodes []DFNode `json:"nodes"`
	}
	_ = readJSON(dir, fOSDDF, &df)
	count := map[string]int{}
	common := "hdd"
	for _, n := range df.Nodes {
		count[n.DeviceClass]++
		if count[n.DeviceClass] > count[common] {
			common = n.DeviceClass
		}
	}
	ruleClass := map[int]string{}
	for _, r := range c.Rules {
		ruleClass[r.ID] = common
		for _, st := range r.Steps {
			if _, cls, ok := strings.Cut(st.ItemName, "~"); st.Op == "take" && ok {
				ruleClass[r.ID] = cls
			}
		}
	}
	out := map[string]string{}
	for _, p := range d.Pools {
		out[strconv.Itoa(p.ID)] = ruleClass[p.CrushRule]
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func parseSince(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		return time.Duration(n) * 24 * time.Hour, err
	}
	return time.ParseDuration(s)
}

func cmdCalibrate(args []string) error {
	fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
	snap := fs.String("snap", "", "каталог снимка: туда пишется calibration.json")
	prom := fs.String("prometheus", "", "адрес Prometheus, например http://prometheus:9090")
	since := fs.String("since", "30d", "глубина истории: 30d, 72h")
	stepF := fs.Duration("step", 5*time.Minute, "шаг выборки и окно rate()")
	selector := fs.String("selector", "", "доп. матчеры меток, например cluster=\"prod-s3\"")
	metric := fs.String("metric", "ceph_osd_recovery_bytes", "счётчик байт recovery на OSD (ceph-mgr prometheus)")
	backfills := fs.Int("backfills", 1, "osd_max_backfills, действовавший в этот период")
	pct := fs.Int("percentile", 75, "какой перцентиль скорости занятого OSD брать в модель")
	minMBps := fs.Float64("min-mbps", 1, "ниже - OSD или пул считается не занятым recovery")
	source := fs.String("source", "auto", "auto | osd (ceph_osd_recovery_bytes из ceph-exporter) | pool (метрики пулов из mgr)")
	fs.Parse(args)
	if *snap == "" || *prom == "" {
		return fmt.Errorf("нужны -snap и -prometheus")
	}
	if *backfills < 1 || *pct < 1 || *pct > 99 {
		return fmt.Errorf("-backfills ≥ 1, -percentile 1..99")
	}
	dur, err := parseSince(*since)
	if err != nil {
		return fmt.Errorf("-since: %w", err)
	}
	end := time.Now()
	start := end.Add(-dur)
	step := *stepF
	// Prometheus отдаёт не больше 11 000 точек на ряд.
	if minStep := dur / 10000; step < minStep {
		step = minStep.Round(time.Minute)
	}
	sel := ""
	if *selector != "" {
		sel = "{" + *selector + "}"
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	token := os.Getenv("CEPHSIM_PROM_TOKEN")
	cal := Calibration{Source: *prom, Since: start.Format(time.RFC3339), Until: end.Format(time.RFC3339), Step: step.String(), Percentile: *pct}
	var stats map[string]ClassStat
	var slots map[string]float64
	var osdErr error

	if *source == "auto" || *source == "osd" {
		query := fmt.Sprintf(`rate(%s%s[%ds]) * on(ceph_daemon) group_left(device_class) max by (ceph_daemon, device_class) (ceph_osd_metadata%s)`,
			*metric, sel, int(step.Seconds()), sel)
		m, err := promQueryRange(client, *prom, token, query, start, end, step)
		switch {
		case err != nil:
			osdErr = err
		case len(m.Data.Result) == 0:
			osdErr = fmt.Errorf("нет рядов %s: ceph-exporter не собирается этим Prometheus?", *metric)
		default:
			stats, slots, osdErr = Calibrate(m, *minMBps, *pct, *backfills)
			cal.Method, cal.HistoryBackfills = "osd", *backfills
		}
		if osdErr != nil && *source == "osd" {
			return osdErr
		}
	}
	classes, err := poolClasses(*snap)
	if err != nil {
		return err
	}
	w := int(step.Seconds())
	pgs, err := promQueryRange(client, *prom, token, fmt.Sprintf(`sum by (pool_id) (avg_over_time(ceph_pg_backfilling%s[%ds])) + sum by (pool_id) (avg_over_time(ceph_pg_recovering%s[%ds]))`, sel, w, sel, w), start, end, step)
	if err != nil {
		return err
	}
	poolRate := func(metric string) (*promMatrix, error) {
		m, err := promQueryRange(client, *prom, token, fmt.Sprintf(`sum by (pool_id) (avg_over_time(%s%s[%ds]))`, metric, sel, w), start, end, step)
		if err == nil && len(m.Data.Result) == 0 {
			err = fmt.Errorf("нет рядов %s: проверьте -selector и сбор mgr prometheus", metric)
		}
		return m, err
	}
	if slots == nil {
		if osdErr != nil {
			fmt.Fprintf(os.Stderr, "per-OSD метрика не подошла (%v), считаю по пулам\n", osdErr)
		}
		rate, err := poolRate("ceph_pool_recovering_bytes_per_sec")
		if err != nil {
			return err
		}
		if stats, slots, err = CalibratePools(rate, pgs, classes, *minMBps, 1e6, *pct); err != nil {
			return err
		}
		cal.Method = "pool"
	}
	active := map[string]map[string]bool{}
	for _, sr := range pgs.Data.Result {
		class := classes[sr.Metric["pool_id"]]
		if class == "" {
			class = "hdd"
		}
		for _, v := range sr.Values {
			if n, _ := strconv.ParseFloat(fmt.Sprint(v[1]), 64); n >= 1 {
				if active[class] == nil {
					active[class] = map[string]bool{}
				}
				active[class][fmt.Sprint(v[0])] = true
			}
		}
	}
	// Клиентская нагрузка: операции и байты на OSD из ceph-exporter. Счётчиков subop
	// (запись реплик) ceph-exporter не отдаёт, поэтому запись primary умножается на
	// средний size пулов: каждая клиентская запись ложится на size OSD.
	repl := avgPoolSize(*snap)
	join := fmt.Sprintf(`* on(ceph_daemon) group_left(device_class) max by (ceph_daemon, device_class) (ceph_osd_metadata%s)`, sel)
	r := func(m string) string { return fmt.Sprintf("rate(%s%s[%ds])", m, sel, w) }
	opsQ := fmt.Sprintf("avg by (device_class) ((%s + %s + %s * %g) %s)", r("ceph_osd_op_r"), r("ceph_osd_op_rw"), r("ceph_osd_op_w"), repl, join)
	bytesQ := fmt.Sprintf("avg by (device_class) ((%s + %s * %g) %s)", r("ceph_osd_op_r_out_bytes"), r("ceph_osd_op_w_in_bytes"), repl, join)
	if om, err := promQueryRange(client, *prom, token, opsQ, start, end, step); err != nil || len(om.Data.Result) == 0 {
		fmt.Fprintf(os.Stderr, "клиентская нагрузка не снята (нет ceph_osd_op_* от ceph-exporter): %v\n", err)
	} else if bm, err := promQueryRange(client, *prom, token, bytesQ, start, end, step); err == nil {
		cal.Client = ClientStatsFrom(om, bm, active)
		cal.ClientReplication = repl
	}

	// Предел по объектам считается только по пулам: per-OSD счётчика объектов recovery у OSD нет.
	if objRate, err := poolRate("ceph_pool_recovering_objects_per_sec"); err != nil {
		fmt.Fprintf(os.Stderr, "предел по объектам не откалиброван: %v\n", err)
	} else if cal.ObjClasses, cal.SlotOps, err = CalibratePools(objRate, pgs, classes, 1, 1, *pct); err != nil {
		fmt.Fprintf(os.Stderr, "предел по объектам не откалиброван: %v\n", err)
	}
	cal.Classes, cal.SlotMBps = stats, slots
	b, _ := json.MarshalIndent(cal, "", "  ")
	if err := os.WriteFile(filepath.Join(*snap, fCalibration), b, 0o644); err != nil {
		return err
	}
	unit := "OSD"
	if cal.Method == "pool" {
		unit = "пулов"
	}
	fmt.Printf("Калибровка (%s) по %s за %s\n", cal.Method, *prom, *since)
	fmt.Printf("  %-6s %8s %6s %10s %10s %14s\n", "класс", "замеров", unit, "p50 MB/s", "p90 MB/s", "слот MB/s")
	for _, c := range sortedKeys(stats) {
		st := stats[c]
		fmt.Printf("  %-6s %8d %6d %10.1f %10.1f %14.1f\n", c, st.Samples, st.OSDs, st.P50MBps, st.P90MBps, slots[c])
	}
	if len(cal.SlotOps) > 0 {
		fmt.Printf("  %-6s %8s %6s %10s %10s %14s\n", "класс", "замеров", "пулов", "p50 об/с", "p90 об/с", "слот об/с")
		for _, c := range sortedKeys(cal.ObjClasses) {
			st := cal.ObjClasses[c]
			fmt.Printf("  %-6s %8d %6d %10.1f %10.1f %14.1f\n", c, st.Samples, st.OSDs, st.P50MBps, st.P90MBps, cal.SlotOps[c])
		}
	}
	if len(cal.Client) > 0 {
		fmt.Printf("  клиенты на OSD: %-6s %16s %16s %16s %18s\n", "класс", "p50", "p95", "p99", "во время recovery")
		f := func(l ClientLoad) string { return fmt.Sprintf("%.0f оп/с %.1f MB/s", l.Ops, l.Bytes/1e6) }
		for _, c := range sortedKeys(cal.Client) {
			st := cal.Client[c]
			during := "recovery не было"
			if st.CalPoints > 0 {
				during = f(st.DuringCal)
			}
			fmt.Printf("                  %-6s %16s %16s %16s %18s\n", c, f(st.P50), f(st.P95), f(st.P99), during)
		}
	}
	fmt.Printf("записано в %s; simulate подхватит его автоматически\n", filepath.Join(*snap, fCalibration))
	return nil
}
