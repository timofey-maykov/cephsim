// cephsim: «что будет, если…» для Ceph на копии osdmap, без изменений в кластере.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// version задаётся при сборке: go build -ldflags "-X main.version=0.1.0".
var version = "dev"

const usage = `cephsim - симулятор изменений и отказов Ceph на копии osdmap

  cephsim collect   -o DIR                    снять снимок кластера (нужен доступ к ceph CLI)
  cephsim calibrate -snap DIR -prometheus URL скорость recovery по истории в Prometheus
  cephsim simulate  -snap DIR -action A -target T [флаги]
  cephsim verify    -snap DIR                   сверить раскладку с osdmaptool и pg ls
  cephsim report    -snap DIR > cluster.html    карта рисков: отказ и обслуживание каждого хоста и домена, rolling, место
  cephsim version                               версия

Действия (-action):
  maintenance   хост выключается на окно работ и возвращается (-noout, -window)
  drain         вывод навсегда: OSD живы, пока с них уезжают данные
  failure       внезапная потеря хоста, стойки или OSD
  expand        новые хосты с OSD (-add, можно несколько раз)
  rule          пул на другое CRUSH-правило (-pool и -rule или -new-rule)
  pgnum         новый pg_num пула (-pool, -pg-num)
  rolling       обслуживание хостов по очереди: партии и расписание (-target, -window, -parallel)

-target - имена из CRUSH-дерева через запятую: host-07, rack2, osd.12

Примеры:
  cephsim simulate -snap snap -action maintenance -target host-07 -noout -window 3h -compare
  cephsim simulate -snap snap -action failure -target rack2
  cephsim simulate -snap snap -action drain -target host-03 -backfills 3 -format json
  cephsim simulate -snap snap -action expand -add host=new,count=2,rack=rack2,osds=20,size=16T,class=hdd
  cephsim simulate -snap snap -action rule -pool rbd -new-rule replicated,fd=rack,class=hdd
  cephsim simulate -snap snap -action rule -pool ec84 -new-rule msr,fd=host,per=3
  cephsim simulate -snap snap -action pgnum -pool rgw.buckets.data -pg-num 4096
  cephsim simulate -snap snap -action rolling -target default -window 2h -gap 15m
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "simulate", "sim":
		err = cmdSimulate(os.Args[2:])
	case "collect":
		err = cmdCollect(os.Args[2:])
	case "calibrate":
		err = cmdCalibrate(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "report":
		err = cmdReport(os.Args[2:])
	case "version", "-version", "--version":
		fmt.Println("cephsim", version)
		return
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		err = fmt.Errorf("неизвестная команда %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, " ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func parseSlots(s string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			return nil, fmt.Errorf("ожидаю class=число, получил %q", kv)
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 {
			return nil, fmt.Errorf("неверное значение %q", kv)
		}
		out[k] = f
	}
	return out, nil
}

func cmdSimulate(args []string) error {
	fs := flag.NewFlagSet("simulate", flag.ExitOnError)
	snap := fs.String("snap", "", "каталог снимка (cephsim collect)")
	action := fs.String("action", ActMaintenance, "maintenance | drain | failure | expand | rule | pgnum | rolling")
	var adds multiFlag
	fs.Var(&adds, "add", "expand: host=ИМЯ[,count=N],rack=..|root=..,osds=N,size=16T,class=hdd (повторяется)")
	poolName := fs.String("pool", "", "rule, pgnum: имя или id пула")
	ruleName := fs.String("rule", "", "rule: существующее CRUSH-правило")
	newRule := fs.String("new-rule", "", "rule: новое правило replicated|erasure|msr,fd=host,class=hdd[,root=default][,per=N][,name=..]")
	pgNum := fs.Int("pg-num", 0, "pgnum: новый pg_num (pgp_num станет таким же)")
	maxPG := fs.Int("max-pg-per-osd", 0, "mon_max_pg_per_osd (по умолчанию из снимка, иначе 250)")
	parallel := fs.Int("parallel", 0, "rolling: не больше N хостов в партии (0 - сколько позволяет доступность)")
	gap := fs.Duration("gap", 15*time.Minute, "rolling: пауза между партиями")
	assumeClean := fs.Bool("assume-clean", false, "доступность по up-наборам, как после завершения текущего recovery/backfill")
	target := fs.String("target", "", "хост, стойка или OSD из CRUSH-дерева, через запятую")
	noout := fs.Bool("noout", false, "maintenance: не переносить данные, пока хост выключен")
	window := fs.Duration("window", 3*time.Hour, "maintenance: длительность работ")
	backfills := fs.Int("backfills", 0, "osd_max_backfills (по умолчанию действующий из снимка, иначе 2)")
	slots := fs.String("slot-mbps", "", "скорость одного backfill-слота по классам, MB/s: hdd=40,ssd=150 (по умолчанию из calibration.json снимка, иначе hdd=40,ssd=150,nvme=300)")
	slotOps := fs.String("slot-ops", "", "предел объектов/с на один backfill-слот по классам: hdd=100,ssd=500 (по умолчанию из calibration.json, иначе без предела)")
	clientLoad := fs.String("client-load", "p95", "клиентская нагрузка во время работ из калибровки: p50 | p95 | p99 | recovery | none")
	clientOps := fs.Float64("client-ops", -1, "клиентские операции/с на один OSD вместо калибровки (для всех классов)")
	clientMBps := fs.Float64("client-mbps", -1, "клиентские MB/s на один OSD вместо калибровки (для всех классов)")
	compare := fs.Bool("compare", false, "сравнить варианты (noout, backfills)")
	format := fs.String("format", "text", "text | json | html (один файл для браузера: cephsim simulate ... -format html > report.html)")
	noColor := fs.Bool("no-color", false, "без цвета")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage+"\nФлаги:\n"); fs.PrintDefaults() }
	fs.Parse(args)

	switch *action {
	case ActMaintenance, ActDrain, ActFailure, ActExpand, ActRule, ActPGNum, ActRolling:
	default:
		return fmt.Errorf("-action: %q", *action)
	}
	if *snap == "" || (withTargets(*action) && *target == "") {
		fs.Usage()
		return fmt.Errorf("нужны -snap и -target")
	}
	if *backfills < 0 {
		return fmt.Errorf("-backfills должен быть ≥ 1")
	}
	s, err := LoadSnapshot(*snap)
	if err != nil {
		return err
	}
	slotMap, err := parseSlots(defaultSlots)
	if err != nil {
		return err
	}
	opsMap := map[string]float64{}
	if cal, err := LoadCalibration(*snap); err == nil {
		for k, v := range cal.SlotMBps {
			slotMap[k] = v
		}
		for k, v := range cal.SlotOps {
			opsMap[k] = v
		}
		s.Calibration = cal
	} else if !os.IsNotExist(err) {
		return err
	}
	if *slots != "" {
		user, err := parseSlots(*slots)
		if err != nil {
			return err
		}
		for k, v := range user {
			slotMap[k] = v
		}
		s.Calibration = nil
	}
	if *slotOps != "" {
		user, err := parseSlots(*slotOps)
		if err != nil {
			return err
		}
		for k, v := range user {
			opsMap[k] = v
		}
	}
	if *backfills == 0 {
		*backfills = 2
		if s.Meta.Backfills > 0 {
			*backfills = s.Meta.Backfills
		}
	}
	sc := Scenario{Action: *action, NoOut: *noout, Window: *window, Backfills: *backfills, SlotMBps: slotMap, SlotOps: opsMap, MClock: s.Meta.MClock, Client: map[string]ClientLoad{},
		Pool: *poolName, RuleName: *ruleName, PGNum: *pgNum, MaxPGPerOSD: *maxPG, AssumeClean: *assumeClean}
	if sc.MaxPGPerOSD == 0 {
		sc.MaxPGPerOSD = 250
		if s.Meta.MaxPGPerOSD > 0 {
			sc.MaxPGPerOSD = s.Meta.MaxPGPerOSD
		}
	}
	switch *action {
	case ActExpand:
		if len(adds) == 0 {
			return fmt.Errorf("expand: нужен -add")
		}
		for _, a := range adds {
			hs, err := parseHosts(a, s.Placement.Crush.Types)
			if err != nil {
				return err
			}
			sc.Add = append(sc.Add, hs...)
		}
	case ActRule:
		pool, err := s.poolByName(*poolName)
		if err != nil {
			return fmt.Errorf("rule: %w", err)
		}
		if (*ruleName == "") == (*newRule == "") {
			return fmt.Errorf("rule: нужен ровно один из -rule и -new-rule")
		}
		if *newRule != "" {
			sp, err := parseRuleSpec(*newRule, pool)
			if err != nil {
				return err
			}
			sc.NewRule = &sp
		}
	case ActPGNum:
		if *poolName == "" || *pgNum <= 0 {
			return fmt.Errorf("pgnum: нужны -pool и -pg-num")
		}
	}
	switch {
	case *clientOps >= 0 || *clientMBps >= 0:
		cl := ClientLoad{Ops: math.Max(0, *clientOps), Bytes: math.Max(0, *clientMBps) * 1e6}
		for _, c := range []string{"hdd", "ssd", "nvme"} {
			sc.Client[c] = cl
		}
		sc.ClientSource = fmt.Sprintf("задана: %.0f оп/с, %.1f MB/s на OSD", cl.Ops, cl.Bytes/1e6)
	case *clientLoad != "none" && s.Calibration != nil && len(s.Calibration.Client) > 0:
		for c, st := range s.Calibration.Client {
			l, ok := st.Load(*clientLoad)
			if !ok && *clientLoad != "recovery" {
				return fmt.Errorf("-client-load: %q", *clientLoad)
			}
			if ok {
				sc.Client[c] = l
			}
		}
		sc.ClientSource = *clientLoad + " по истории"
	default:
		sc.ClientSource = "не учтена"
	}
	for _, t := range strings.Split(*target, ",") {
		if t = strings.TrimSpace(t); t != "" {
			sc.Targets = append(sc.Targets, t)
		}
	}
	if sc.Action == ActRolling {
		r, err := Rolling(s, sc, *parallel, *gap)
		if err != nil {
			return err
		}
		if *format == "json" {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(r)
		}
		if *format == "html" {
			return WriteHTML(os.Stdout, rollingData(s, r))
		}
		fi, _ := os.Stdout.Stat()
		PrintRolling(os.Stdout, s, r, !*noColor && fi.Mode()&os.ModeCharDevice != 0)
		return nil
	}
	needRemap := !(sc.Action == ActMaintenance && sc.NoOut) || *compare
	core, err := Analyze(s, sc, needRemap)
	if err != nil {
		return err
	}
	r := Finalize(s, core, sc)
	if *compare {
		r.Variants = Variants(s, core, sc)
	}

	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	if *format == "html" {
		return WriteHTML(os.Stdout, reportData(s, r))
	}
	fi, _ := os.Stdout.Stat()
	PrintReport(os.Stdout, s, r, !*noColor && fi.Mode()&os.ModeCharDevice != 0)
	return nil
}

const defaultSlots = "hdd=40,ssd=150,nvme=300"

// cmdVerify сверяет Go-порт CRUSH с osdmaptool (вся карта и out одного OSD)
// и с up-наборами из pg ls. Расхождение - повод не доверять симуляции на этом кластере.
func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	snap := fs.String("snap", "", "каталог снимка")
	osd := fs.Int("out-osd", -1, "OSD для проверки out (по умолчанию первый)")
	bin := fs.String("ceph-bin", "", "каталог с osdmaptool и crushtool (по умолчанию PATH, иначе docker)")
	image := fs.String("image", "quay.io/ceph/ceph:v20", "образ Ceph для osdmaptool через docker")
	fs.Parse(args)
	if *snap == "" {
		return fmt.Errorf("нужен -snap")
	}
	s, err := LoadSnapshot(*snap)
	if err != nil {
		return err
	}
	m := Mapper{Bin: *bin, Image: *image, ExtraArgs: s.Meta.OSDMapToolArg}
	osdmap := filepath.Join(*snap, fOSDMap)
	cmp := func(name string, want, got Mapping) bool {
		diff, first := 0, ""
		for pg, w := range want {
			if fmt.Sprint(w) != fmt.Sprint(got[pg]) {
				diff++
				if first == "" {
					first = fmt.Sprintf("%s: ожидалось %v, получено %v", pg, w, got[pg])
				}
			}
		}
		if diff == 0 {
			fmt.Printf("  ✔ %-34s %d PG совпали\n", name, len(want))
			return true
		}
		fmt.Printf("  ✖ %-34s %d из %d PG расходятся, например %s\n", name, diff, len(want), first)
		return false
	}
	ok := true

	ref, err := m.Map(osdmap, nil)
	if err != nil {
		return err
	}
	ok = cmp("текущая раскладка vs osdmaptool", ref, s.Placement.MapAll(s.Pools)) && ok

	if *osd < 0 {
		for id := range s.Placement.Exists {
			if s.Placement.Exists[id] {
				*osd = id
				break
			}
		}
	}
	refOut, err := m.Map(osdmap, nil, "--mark-out", fmt.Sprint(*osd))
	if err != nil {
		return err
	}
	p := s.Placement.Clone()
	p.MarkOut([]int{*osd}, false)
	ok = cmp(fmt.Sprintf("out osd.%d vs osdmaptool --mark-out", *osd), refOut, p.MapAll(s.Pools)) && ok

	if !s.Meta.Synthetic {
		fromLs := Mapping{}
		for _, pg := range s.PGs {
			fromLs[pg.PGID] = pg.Up
		}
		ok = cmp("текущая раскладка vs up из pg ls", fromLs, s.Placement.MapAll(s.Pools)) && ok
	}
	if !ok {
		return fmt.Errorf("раскладка не совпала: результаты simulate на этом кластере неточны")
	}
	return nil
}

// collectMClock снимает планировщик и ёмкость mClock с одного OSD каждого класса:
// у SSD она часто задана для отдельных OSD (osd_mclock_max_capacity_iops_ssd).
func collectMClock(run func(string, ...string) ([]byte, error), dir string) *MClock {
	var df struct {
		Nodes []DFNode `json:"nodes"`
	}
	if readJSON(dir, fOSDDF, &df) != nil || len(df.Nodes) == 0 {
		return nil
	}
	get := func(who, key string) string {
		b, err := run("", "config", "show", who, key)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	num := func(who, key string) float64 {
		f, _ := strconv.ParseFloat(get(who, key), 64)
		return f
	}
	first := fmt.Sprintf("osd.%d", df.Nodes[0].ID)
	m := &MClock{Scheduler: get(first, "osd_op_queue"), Profile: get(first, "osd_mclock_profile"), CapIOPS: map[string]float64{}, CapBW: map[string]float64{}}
	seen := map[string]bool{}
	for _, n := range df.Nodes {
		key := "ssd"
		if n.DeviceClass == "hdd" {
			key = "hdd"
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		who := fmt.Sprintf("osd.%d", n.ID)
		m.CapIOPS[key] = num(who, "osd_mclock_max_capacity_iops_"+key)
		m.CapBW[key] = num(who, "osd_mclock_max_sequential_bandwidth_"+key)
	}
	if m.Profile == "custom" {
		k := "osd_mclock_scheduler_"
		m.Custom = &MClockProfile{
			ClientRes: num(first, k+"client_res"), ClientWgt: num(first, k+"client_wgt"), ClientLim: num(first, k+"client_lim"),
			RecRes: num(first, k+"background_recovery_res"), RecWgt: num(first, k+"background_recovery_wgt"), RecLim: num(first, k+"background_recovery_lim"),
		}
	}
	return m
}

func cmdCollect(args []string) error {
	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	out := fs.String("o", "", "каталог для снимка")
	cephCmd := fs.String("ceph", "ceph", "команда ceph, например \"cephadm shell -- ceph\"")
	cluster := fs.String("name", "", "имя кластера для отчёта")
	fs.Parse(args)
	if *out == "" {
		return fmt.Errorf("нужен -o")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	base := strings.Fields(*cephCmd)
	run := func(file string, a ...string) ([]byte, error) {
		cmd := exec.Command(base[0], append(base[1:], a...)...)
		cmd.Stderr = os.Stderr
		b, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", *cephCmd, strings.Join(a, " "), err)
		}
		if file != "" {
			err = os.WriteFile(filepath.Join(*out, file), b, 0o644)
		}
		fmt.Fprintf(os.Stderr, "  %-18s %s\n", file, strings.Join(a, " "))
		return b, err
	}
	// osdmap бинарный: пишем через stdout, чтобы работало и внутри cephadm shell.
	if _, err := run(fOSDMap, "osd", "getmap"); err != nil {
		return err
	}
	for _, x := range [][]string{
		{fOSDDump, "osd", "dump", "-f", "json"},
		{fOSDTree, "osd", "tree", "-f", "json"},
		{fCrush, "osd", "crush", "dump", "-f", "json"},
		{fOSDDF, "osd", "df", "-f", "json"},
		{fPGLs, "pg", "ls", "-f", "json"},
		{fStatus, "status", "-f", "json"},
	} {
		if _, err := run(x[0], x[1:]...); err != nil {
			return err
		}
	}
	var dump OSDDump
	if err := readJSON(*out, fOSDDump, &dump); err != nil {
		return err
	}
	profiles := map[string]json.RawMessage{}
	for _, p := range dump.Pools {
		if p.EC() && profiles[p.ECProfile] == nil {
			b, err := run("", "osd", "erasure-code-profile", "get", p.ECProfile, "-f", "json")
			if err != nil {
				return err
			}
			profiles[p.ECProfile] = b
		}
	}
	pb, _ := json.MarshalIndent(profiles, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, fECProfiles), pb, 0o644); err != nil {
		return err
	}
	meta := Meta{CollectedAt: time.Now().Format("2006-01-02 15:04"), Cluster: *cluster}
	// Действующее значение на OSD: при mClock без override оно отличается от config db.
	if len(dump.OSDs) > 0 {
		who := fmt.Sprintf("osd.%d", dump.OSDs[0].OSD)
		if b, err := run("", "config", "show", who, "osd_max_backfills"); err == nil {
			meta.Backfills, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if b, err := run("", "config", "show", who, "osd_mclock_profile"); err == nil {
			meta.MClockProfile = strings.TrimSpace(string(b))
		}
		if b, err := run("", "config", "get", "mon", "mon_max_pg_per_osd"); err == nil {
			meta.MaxPGPerOSD, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if b, err := run("", "config", "get", "mon", "mon_osd_down_out_subtree_limit"); err == nil {
			meta.DownOutSubtreeLimit = strings.TrimSpace(string(b))
		}
		if b, err := run("", "config", "get", "mon", "mon_osd_min_in_ratio"); err == nil {
			meta.MinInRatio, _ = strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
		}
	}
	meta.MClock = collectMClock(run, *out)
	mb, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, fMeta), mb, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "снимок готов: %s (osdmap e%d)\n", *out, dump.Epoch)
	return nil
}

// cmdReport - сводный отчёт по кластеру (карта рисков) в HTML или JSON.
func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	snap := fs.String("snap", "", "каталог снимка")
	window := fs.Duration("window", 2*time.Hour, "окно обслуживания одного хоста или партии")
	gap := fs.Duration("gap", 15*time.Minute, "пауза между партиями в расписании")
	backfills := fs.Int("backfills", 0, "osd_max_backfills (по умолчанию из снимка, иначе 2)")
	clientLoad := fs.String("client-load", "p95", "клиентская нагрузка из калибровки: p50 | p95 | p99 | recovery | none")
	maxPG := fs.Int("max-pg-per-osd", 0, "mon_max_pg_per_osd (по умолчанию из снимка, иначе 250)")
	format := fs.String("format", "html", "html | json")
	fs.Parse(args)
	if *snap == "" {
		return fmt.Errorf("нужен -snap")
	}
	s, err := LoadSnapshot(*snap)
	if err != nil {
		return err
	}
	slotMap, _ := parseSlots(defaultSlots)
	opsMap := map[string]float64{}
	if cal, err := LoadCalibration(*snap); err == nil {
		for k, v := range cal.SlotMBps {
			slotMap[k] = v
		}
		for k, v := range cal.SlotOps {
			opsMap[k] = v
		}
		s.Calibration = cal
	}
	sc := Scenario{Window: *window, Backfills: *backfills, SlotMBps: slotMap, SlotOps: opsMap, MClock: s.Meta.MClock,
		Client: map[string]ClientLoad{}, MaxPGPerOSD: *maxPG, NoOut: true, ClientSource: "не учтена"}
	if sc.Backfills == 0 {
		sc.Backfills = 2
		if s.Meta.Backfills > 0 {
			sc.Backfills = s.Meta.Backfills
		}
	}
	if sc.MaxPGPerOSD == 0 {
		sc.MaxPGPerOSD = 250
		if s.Meta.MaxPGPerOSD > 0 {
			sc.MaxPGPerOSD = s.Meta.MaxPGPerOSD
		}
	}
	if *clientLoad != "none" && s.Calibration != nil {
		for c, st := range s.Calibration.Client {
			if l, ok := st.Load(*clientLoad); ok {
				sc.Client[c] = l
			}
		}
		if len(sc.Client) > 0 {
			sc.ClientSource = *clientLoad + " по истории"
		}
	}
	start := time.Now()
	cr, err := BuildCluster(s, sc, *gap)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "карта рисков: %d доменов за %s\n", len(cr.Domains), time.Since(start).Round(time.Millisecond))
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cr)
	}
	return WriteClusterHTML(os.Stdout, clusterData(s, cr))
}
