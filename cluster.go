package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Сводный отчёт по кластеру - «карта рисков»: для каждого хоста и каждого
// failure domain выше хоста считаются обслуживание с noout и внезапный отказ,
// плюс расписание rolling, запас по месту и конфигурация пулов.

type DomainRisk struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Path  string `json:"path"`
	OSDs  int    `json:"osds"`
	Hosts int    `json:"hosts"`
	Raw   int64  `json:"raw_bytes"`

	// обслуживание с noout: только доступность
	MaintLost   int   `json:"maint_pg_lost"` // inactive + incomplete
	MaintAtRisk int   `json:"maint_pg_at_risk"`
	MaintLevel  Level `json:"maint_level"`
	// сейчас, по acting из pg ls: сколько PG станут недоступны при выключении с учётом текущей деградации
	NowLost int `json:"now_pg_lost"`

	// внезапный отказ: доступность, переезд, место
	FailInactive   int           `json:"fail_pg_inactive"`
	FailIncomplete int           `json:"fail_pg_incomplete"`
	FailMaxAfter   float64       `json:"fail_max_osd_after"`
	FailMaxOSD     int           `json:"fail_max_osd"`
	FailNearfull   int           `json:"fail_nearfull_after"`
	FailFull       int           `json:"fail_full_after"`
	FailBFull      int           `json:"fail_backfillfull_after"`
	FailUnder      int           `json:"fail_pg_undersized"`
	FailMoved      int64         `json:"fail_moved_bytes"`
	FailTime       time.Duration `json:"fail_recovery_time"`
	FailStalled    bool          `json:"fail_stalled"`
	FailLevel      Level         `json:"fail_level"`
	FailWhy        string        `json:"fail_why"`
}

type PoolGroup struct {
	Kind  string `json:"kind"`
	Min   int    `json:"min_size"`
	Pools int    `json:"pools"`
	PGs   int    `json:"pgs"`
	Bytes int64  `json:"bytes"`
}

type ClusterReport struct {
	Scenario    Scenario       `json:"scenario"`
	Epoch       int            `json:"osdmap_epoch"`
	Health      string         `json:"health"`
	Checks      []string       `json:"health_checks"`
	OSDs, InOSD int            `json:"-"`
	Hosts       int            `json:"hosts"`
	PGs         int            `json:"pgs"`
	RawTotal    int64          `json:"raw_total_bytes"`
	RawUsed     int64          `json:"raw_used_bytes"`
	Logical     int64          `json:"logical_bytes"`
	Objects     int64          `json:"objects"`
	FillAvg     float64        `json:"fill_avg"`
	FillMax     float64        `json:"fill_max"`
	FillMaxOSD  int            `json:"fill_max_osd"`
	Headroom    int64          `json:"headroom_raw_bytes"` // сколько сырого места до первого OSD на nearfull при росте как сейчас
	Ratios      [3]float64     `json:"ratios_near_backfill_full"`
	Undersized0 int            `json:"pg_undersized_now"`
	Upmaps      int            `json:"pg_upmap_items"`
	PGPerOSD    [2]float64     `json:"pg_per_osd_avg_max"`
	MaxPGOSD    int            `json:"mon_max_pg_per_osd"`
	FDTypes     []string       `json:"failure_domains"`
	PoolGroups  []PoolGroup    `json:"pool_groups"`
	TopPools    []PoolUsage    `json:"top_pools"`
	Domains     []DomainRisk   `json:"domains"`
	Rolling     *RollingResult `json:"rolling"`
	Risks       []Risk         `json:"risks"`
	Verdict     Level          `json:"verdict"`

	fill []OSDProj // текущая заполненность для графика
}

type PoolUsage struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Min   int    `json:"min_size"`
	PGs   int    `json:"pgs"`
	Bytes int64  `json:"bytes"`
	Objs  int64  `json:"objects"`
}

// domainsOf: все бакеты CRUSH-дерева типа host и выше, кроме корней, с OSD внутри.
func domainsOf(s *Snapshot) []TreeNode {
	var out []TreeNode
	for id, n := range s.Tree {
		if id >= 0 || n.Type == "osd" {
			continue
		}
		if _, ok := s.Parent[id]; !ok {
			continue // корень
		}
		if osds, err := s.ResolveOSDs([]string{n.Name}); err == nil && len(osds) > 0 {
			out = append(out, n)
		}
	}
	return out
}

func treePath(s *Snapshot, id int) string {
	var parts []string
	for {
		parts = append([]string{s.Tree[id].Name}, parts...)
		p, ok := s.Parent[id]
		if !ok {
			break
		}
		id = p
	}
	return joinPath(parts)
}

func joinPath(p []string) string {
	out := ""
	for i, x := range p {
		if i > 0 {
			out += " / "
		}
		out += x
	}
	return out
}

// BuildCluster считает карту рисков. sc задаёт backfills, скорости, нагрузку и окно;
// Action и Targets подставляются для каждого домена. Риски конструкции (правила,
// размещение, место) считаются по up-наборам, как после HEALTH_OK; отдельно -
// что блокирует текущая деградация (acting из pg ls).
func BuildCluster(s *Snapshot, sc Scenario, gap time.Duration) (*ClusterReport, error) {
	cr := &ClusterReport{Scenario: sc, Epoch: s.Dump.Epoch, PGs: len(s.PGs), MaxPGOSD: sc.MaxPGPerOSD,
		Ratios: [3]float64{s.Dump.NearFullRatio, s.Dump.BackfillFullRatio, s.Dump.FullRatio}}
	s.baseMapping()

	// Состояние и место.
	var st struct {
		Health struct {
			Status string                    `json:"status"`
			Checks map[string]map[string]any `json:"checks"`
		} `json:"health"`
	}
	if readJSON(s.Dir, fStatus, &st) == nil {
		cr.Health = st.Health.Status
		for k := range st.Health.Checks {
			cr.Checks = append(cr.Checks, k)
		}
		sort.Strings(cr.Checks)
	}
	var sumFill float64
	for id, n := range s.DF {
		if n.KB <= 0 {
			continue
		}
		cr.OSDs++
		cr.RawTotal += n.KB * 1024
		cr.RawUsed += n.KBUsed * 1024
		f := float64(n.KBUsed) / float64(n.KB)
		sumFill += f
		if f > cr.FillMax {
			cr.FillMax, cr.FillMaxOSD = f, id
		}
		cr.fill = append(cr.fill, OSDProj{ID: id, Host: s.Ancestor(id, "host"), Class: s.Class(id), Before: f, After: f})
	}
	sort.Slice(cr.fill, func(i, j int) bool { return cr.fill[i].After > cr.fill[j].After })
	if cr.OSDs > 0 {
		cr.FillAvg = sumFill / float64(cr.OSDs)
	}
	if cr.FillMax > 0 {
		// При росте пропорционально текущему первый OSD дойдёт до nearfull, когда всё вырастет в near/max раз.
		cr.Headroom = int64(float64(cr.RawUsed) * (cr.Ratios[0]/cr.FillMax - 1))
	}
	hosts := map[string]bool{}
	for _, n := range s.Tree {
		if n.Type == "host" {
			hosts[n.Name] = true
		}
	}
	cr.Hosts = len(hosts)

	// Пулы.
	poolBytes := map[int][2]int64{}
	for _, pg := range s.PGs {
		b := poolBytes[pg.PoolID()]
		poolBytes[pg.PoolID()] = [2]int64{b[0] + pg.StatSum.NumBytes, b[1] + pg.StatSum.NumObjects}
		cr.Logical += pg.StatSum.NumBytes
		cr.Objects += pg.StatSum.NumObjects
		set := pg.Acting
		if len(set) == 0 {
			set = pg.Up
		}
		if p, ok := s.Pools[pg.PoolID()]; ok && alive(set, nil) < p.Size {
			cr.Undersized0++
		}
	}
	groups := map[string]*PoolGroup{}
	fds := map[string]bool{}
	for _, p := range s.Pools {
		key := fmt.Sprintf("%s/%d", p.Kind(), p.MinSize)
		g := groups[key]
		if g == nil {
			g = &PoolGroup{Kind: p.Kind(), Min: p.MinSize}
			groups[key] = g
		}
		g.Pools++
		g.PGs += p.PGNum
		g.Bytes += poolBytes[p.ID][0]
		cr.TopPools = append(cr.TopPools, PoolUsage{p.Name, p.Kind(), p.MinSize, p.PGNum, poolBytes[p.ID][0], poolBytes[p.ID][1]})
		if fd := s.Placement.Crush.FailureDomain(p.CrushRule); fd != "" {
			fds[fd] = true
		}
	}
	for _, g := range groups {
		cr.PoolGroups = append(cr.PoolGroups, *g)
	}
	sort.Slice(cr.PoolGroups, func(i, j int) bool { return cr.PoolGroups[i].Bytes > cr.PoolGroups[j].Bytes })
	sort.Slice(cr.TopPools, func(i, j int) bool { return cr.TopPools[i].Bytes > cr.TopPools[j].Bytes })
	if len(cr.TopPools) > 10 {
		cr.TopPools = cr.TopPools[:10]
	}
	cr.FDTypes = sortedKeys(fds)
	var d osdDumpExtra
	if readJSON(s.Dir, fOSDDump, &d) == nil {
		cr.Upmaps = len(d.PGUpmapItems)
	}
	pp := pgPerOSD(s.baseMapping(), s.baseMapping(), s.Placement, s.Placement)
	cr.PGPerOSD = [2]float64{pp.BeforeAvg, float64(pp.BeforeMax)}

	// Домены: обслуживание и отказ.
	for _, n := range domainsOf(s) {
		osds, _ := s.ResolveOSDs([]string{n.Name})
		dr := DomainRisk{Name: n.Name, Type: n.Type, Path: treePath(s, n.ID), OSDs: len(osds)}
		for _, o := range osds {
			dr.Raw += s.DF[o].KB * 1024
		}
		for h := range hosts {
			if hn, ok := s.ByName[h]; ok && (hn.ID == n.ID || isUnder(s, hn.ID, n.ID)) {
				dr.Hosts++
			}
		}
		m := sc
		m.Action, m.NoOut, m.Targets, m.AssumeClean = ActMaintenance, true, []string{n.Name}, true
		mc, err := Analyze(s, m, false)
		if err != nil {
			return nil, err
		}
		dr.MaintLost, dr.MaintAtRisk = mc.Inactive+mc.Incomplete, mc.AtRisk
		switch {
		case dr.MaintLost > 0:
			dr.MaintLevel = LevelBad
		case mc.AtRisk > 0 && sc.Window > 4*time.Hour:
			dr.MaintLevel = LevelWarn
		}
		m.AssumeClean = false
		if now, err := Analyze(s, m, false); err == nil {
			dr.NowLost = now.Inactive + now.Incomplete
		}

		f := sc
		f.AssumeClean = true
		f.Action, f.NoOut, f.Targets = ActFailure, false, []string{n.Name}
		fc, err := Analyze(s, f, true)
		if err != nil {
			return nil, err
		}
		fr := Finalize(s, fc, f)
		dr.FailInactive, dr.FailIncomplete = fr.Inactive, fr.Incomplete
		dr.FailMaxAfter, dr.FailMaxOSD = fr.Max.After, fr.Max.ID
		dr.FailNearfull, dr.FailBFull, dr.FailFull = fr.NearfullAfter, fr.BackfillfullAfter, fr.FullAfter
		dr.FailUnder, dr.FailMoved, dr.FailTime, dr.FailStalled = fr.Undersized, fr.MovedBytes, fr.RecoveryTime, fr.Stalled
		switch {
		case fr.Incomplete > 0:
			dr.FailLevel, dr.FailWhy = LevelBad, fmt.Sprintf("%s PG теряют все копии", num(fr.Incomplete))
		case fr.Inactive > 0:
			dr.FailLevel, dr.FailWhy = LevelBad, fmt.Sprintf("%s PG inactive", num(fr.Inactive))
		case fr.FullAfter > 0:
			dr.FailLevel, dr.FailWhy = LevelBad, fmt.Sprintf("не хватит места, встанет запись (%d OSD выше full)", fr.FullAfter)
		case fr.BackfillfullAfter > 0:
			dr.FailLevel, dr.FailWhy = LevelBad, fmt.Sprintf("не хватит места, восстановление не завершится (%d OSD выше backfillfull)", fr.BackfillfullAfter)
		case fr.NearfullAfter > fr.NearfullBefore:
			dr.FailLevel, dr.FailWhy = LevelWarn, fmt.Sprintf("%d OSD выше nearfull", fr.NearfullAfter)
		case fr.Undersized > 0:
			dr.FailLevel, dr.FailWhy = LevelWarn, fmt.Sprintf("%s PG undersized до возврата", num(fr.Undersized))
		default:
			dr.FailWhy = "переживает"
		}
		if lim, ok := s.Placement.Crush.Types[s.Meta.DownOutSubtreeLimit]; ok && s.Placement.Crush.Types[n.Type] >= lim && dr.FailLevel == LevelBad && fr.Inactive+fr.Incomplete == 0 {
			// Ceph сам не выведет OSD целого домена в out: переезд будет только вручную.
			dr.FailLevel = LevelWarn
			dr.FailWhy += " - если вывести его OSD вручную"
		}
		cr.Domains = append(cr.Domains, dr)
	}
	typeRank := map[string]int{}
	for name, id := range s.Placement.Crush.Types {
		typeRank[name] = id
	}
	sort.Slice(cr.Domains, func(i, j int) bool {
		a, b := cr.Domains[i], cr.Domains[j]
		if typeRank[a.Type] != typeRank[b.Type] {
			return typeRank[a.Type] > typeRank[b.Type]
		}
		return a.Path < b.Path
	})

	roll := sc
	roll.Action, roll.Targets, roll.AssumeClean = ActRolling, nil, true
	var err error
	if cr.Rolling, err = Rolling(s, roll, 0, gap); err != nil {
		return nil, err
	}

	cr.assess(s)
	return cr, nil
}

func isUnder(s *Snapshot, id, ancestor int) bool {
	for {
		p, ok := s.Parent[id]
		if !ok {
			return false
		}
		if p == ancestor {
			return true
		}
		id = p
	}
}

// Risk - риск для человека, который будет работать по отчёту: что не так,
// чем грозит и что сделать. Без внутренних терминов модели.
type Risk struct {
	Level  Level    `json:"level"`
	When   string   `json:"when"` // now - сделать сейчас, plan - запланировать, good - хорошие новости
	Title  string   `json:"title"`
	Impact string   `json:"impact"`
	Action []string `json:"action"`
	Cmds   []string `json:"commands,omitempty"`
	weight int      // порядок внутри уровня: недоступность данных важнее нехватки места
}

// healthText - что значат частые health-проверки.
var healthText = map[string]string{
	"PG_DEGRADED":                   "часть данных хранится не во всех копиях (идёт восстановление)",
	"PG_AVAILABILITY":               "часть данных недоступна",
	"PG_DAMAGED":                    "найдены повреждённые данные",
	"OSD_DOWN":                      "часть OSD выключена",
	"OSD_NEARFULL":                  "OSD близки к заполнению",
	"OSD_BACKFILLFULL":              "OSD заполнены до порога, переезд данных на них остановлен",
	"OSD_FULL":                      "OSD заполнены, запись остановлена",
	"OSD_TOO_MANY_REPAIRS":          "OSD часто исправляют ошибки чтения: возможна деградация дисков",
	"LARGE_OMAP_OBJECTS":            "есть слишком большие omap-объекты (обычно индексы бакетов RGW без решардинга)",
	"CEPHADM_FAILED_DAEMON":         "упали службы под управлением cephadm",
	"CEPHADM_HOST_CHECK_FAILED":     "cephadm не может проверить один из хостов",
	"MDS_DAMAGE":                    "повреждены метаданные CephFS",
	"AUTH_INSECURE_CLIENT_KEY_TYPE": "у клиентов небезопасный тип ключей",
	"AUTH_INSECURE_KEYS_ALLOWED":    "кластер разрешает небезопасные ключи",
	"POOL_NO_REDUNDANCY":            "есть пулы без избыточности (size 1)",
	"TOO_MANY_PGS":                  "на OSD слишком много PG",
	"PG_NOT_SCRUBBED":               "PG давно не проверялись scrub",
	"PG_NOT_DEEP_SCRUBBED":          "PG давно не проверялись deep-scrub",
	"OSDMAP_FLAGS":                  "установлены флаги, меняющие поведение кластера",
	"RECENT_CRASH":                  "недавно падали демоны Ceph",
	"SLOW_OPS":                      "есть медленные операции",
	"BLUESTORE_SLOW_OP_ALERT":       "BlueStore сообщает о медленных операциях",
	"MON_DISK_LOW":                  "на мониторах мало места",
}

func plural(n int, one, few, many string) string {
	n10, n100 := n%10, n%100
	switch {
	case n10 == 1 && n100 != 11:
		return one
	case n10 >= 2 && n10 <= 4 && (n100 < 10 || n100 >= 20):
		return few
	}
	return many
}

var typeWord = map[string][3]string{
	"host": {"хост", "хоста", "хостов"}, "rack": {"стойка", "стойки", "стоек"},
	"row": {"ряд", "ряда", "рядов"}, "datacenter": {"ЦОД", "ЦОДа", "ЦОДов"}, "room": {"зал", "зала", "залов"},
}

// one - «одного хоста», «одной стойки»: родительный падеж с правильным родом.
func one(t string) string {
	w, ok := typeWord[t]
	if !ok {
		return t
	}
	if t == "rack" {
		return "одной " + w[1]
	}
	return "одного " + w[1]
}

// countGen - число в родительном падеже: «касается 5 хостов», «3 стоек».
var typePlural = map[string]string{"host": "хосты", "rack": "стойки", "row": "ряды", "datacenter": "ЦОДы", "room": "залы"}

func countGen(n int, t string) string {
	w, ok := typeWord[t]
	if !ok {
		return fmt.Sprintf("%d %s", n, t)
	}
	if n%10 == 1 && n%100 != 11 {
		return fmt.Sprintf("%d %s", n, w[1])
	}
	return fmt.Sprintf("%d %s", n, w[2])
}

func countOf(n int, t string) string {
	w, ok := typeWord[t]
	if !ok {
		return fmt.Sprintf("%d %s", n, t)
	}
	return fmt.Sprintf("%d %s", n, plural(n, w[0], w[1], w[2]))
}

// assess - главные риски кластера простым языком, с последствиями и действиями.
func (cr *ClusterReport) assess(s *Snapshot) {
	add := func(r Risk) { cr.Risks = append(cr.Risks, r) }
	byType := map[string][]DomainRisk{}
	for _, d := range cr.Domains {
		byType[d.Type] = append(byType[d.Type], d)
	}
	rank := s.Placement.Crush.Types
	limit, hasLimit := rank[s.Meta.DownOutSubtreeLimit]

	// 1. Потеря домена делает данные недоступными или переполняет кластер.
	for _, t := range sortedKeys(byType) {
		var lost, full []DomainRisk
		for _, d := range byType[t] {
			switch {
			case d.FailInactive+d.FailIncomplete > 0:
				lost = append(lost, d)
			case d.FailFull+d.FailBFull > 0:
				full = append(full, d)
			}
		}
		n := len(byType[t])
		autoOut := !hasLimit || rank[t] < limit
		if len(lost) > 0 {
			var names []string
			pg := 0
			for _, d := range lost {
				names = append(names, d.Name)
				pg = max(pg, d.FailInactive+d.FailIncomplete)
			}
			add(Risk{Level: LevelBad, When: "plan", weight: 3,
				Title:  fmt.Sprintf("Отказ %s сделает часть данных недоступной", one(t)),
				Impact: fmt.Sprintf("Это касается %s из %d (%s). В худшем случае недоступны %s %s данных (PG) - клиенты этих пулов получат ошибки или зависнут до возврата оборудования.", countGen(len(lost), t), n, short(names), num(pg), plural(pg, "группа", "группы", "групп")),
				Action: []string{fmt.Sprintf("Проверить CRUSH-правила пулов: копии должны разноситься по уровню не ниже «%s».", t), "Пока не исправлено - не выключать эти домены даже на обслуживание."}})
		}
		if len(full) > 0 {
			add(cr.overflowRisk(s, t, full, byType[t], autoOut))
		}
		// обслуживание невозможно по конструкции
		var maintBad []string
		for _, d := range byType[t] {
			if d.MaintLevel == LevelBad {
				maintBad = append(maintBad, d.Name)
			}
		}
		if len(maintBad) > 0 {
			title := fmt.Sprintf("Часть %s нельзя выключить даже на обслуживание", typeWord[t][2])
			if len(maintBad) == n {
				title = fmt.Sprintf("%s нельзя выключить даже на обслуживание", map[string]string{"host": "Ни один хост", "rack": "Ни одну стойку", "row": "Ни один ряд"}[t])
			}
			add(Risk{Level: LevelBad, When: "plan", weight: 2, Title: title,
				Impact: fmt.Sprintf("%s из %d (%s): при выключении даже с noout часть данных станет недоступной.", countOf(len(maintBad), t), n, short(maintBad)),
				Action: []string{"Перед работами выводить такой домен через drain, а не выключать.", "Проверить CRUSH-правила пулов и размещение копий."}})
		}
	}

	// 2. Текущее восстановление блокирует работы.
	if cr.Undersized0 > 0 {
		var hostsNow, hostsOK []string
		for _, d := range byType["host"] {
			if d.NowLost > 0 {
				hostsNow = append(hostsNow, d.Name)
			} else if d.MaintLevel != LevelBad {
				hostsOK = append(hostsOK, d.Name)
			}
		}
		r := Risk{Level: LevelWarn, When: "now", Title: "Идёт восстановление данных - выключать хосты сейчас опасно",
			Impact: fmt.Sprintf("Кластер сейчас досоздаёт недостающие копии для %s %s данных (PG). У части из них живая копия осталась только одна. Если выключить хост, на котором она лежит, эти данные станут недоступны.", num(cr.Undersized0), plural(cr.Undersized0, "группы", "групп", "групп"))}
		if len(hostsNow) > 0 {
			r.Impact += fmt.Sprintf(" Сейчас это касается %s из %d.", countGen(len(hostsNow), "host"), len(byType["host"]))
		}
		r.Action = []string{"Дождаться HEALTH_OK и только потом начинать плановые работы."}
		if len(hostsOK) > 0 {
			r.Action = append(r.Action, fmt.Sprintf("Если работы не ждут - прямо сейчас можно выключать только: %s.", short(hostsOK)))
		}
		r.Cmds = []string{"ceph -s", "ceph pg ls undersized"}
		add(r)
	}

	// 3. Пороги заполнения.
	if cr.FillMax >= cr.Ratios[0] {
		add(Risk{Level: LevelBad, When: "now", Title: "Диск уже почти заполнен",
			Impact: fmt.Sprintf("%s заполнен на %s при nearfull %.0f%%. На %.0f%% Ceph остановит на нём переезд данных, на %.0f%% - запись во весь кластер.", osdName(cr.FillMaxOSD), pct1(cr.FillMax), cr.Ratios[0]*100, cr.Ratios[1]*100, cr.Ratios[2]*100),
			Action: []string{"Добавить ёмкость или выровнять заполнение (balancer в режиме upmap)."},
			Cmds:   []string{"ceph osd df tree", "ceph balancer status"}})
	}
	if cr.Ratios[1] >= cr.Ratios[2] {
		add(Risk{Level: LevelWarn, When: "plan", Title: "Нет запаса между остановкой переезда данных и остановкой записи",
			Impact: fmt.Sprintf("Пороги backfillfull и full оба %.0f%% (nearfull %.0f%%). Когда OSD заполнится, Ceph одновременно перестанет переносить на него данные и принимать запись - времени отреагировать не будет.", cr.Ratios[2]*100, cr.Ratios[0]*100),
			Action: []string{fmt.Sprintf("Развести пороги, например стандартные 85/90/95%%. Сейчас самый полный OSD - %s, так что это безопасно.", pct1(cr.FillMax))},
			Cmds:   []string{"ceph osd set-nearfull-ratio 0.85", "ceph osd set-backfillfull-ratio 0.90", "ceph osd set-full-ratio 0.95"}})
	}

	// 4. Пулы с min_size 1.
	for _, g := range cr.PoolGroups {
		if g.Min == 1 && g.Kind != "rep 1" {
			add(Risk{Level: LevelWarn, When: "plan", Title: fmt.Sprintf("%s %s в двух копиях и пишут даже в одну", num(g.Pools), plural(g.Pools, "пул хранит данные", "пула хранят данные", "пулов хранят данные")),
				Impact: fmt.Sprintf("У пулов %s - %d копии данных и min_size 1, в них %s. Пока одна копия недоступна - при отказе или обслуживании хоста - запись идёт в единственную оставшуюся. Если в это время откажет и её диск, записанное за это время пропадёт.", g.Kind, poolSize(g.Kind), bytesH(g.Bytes)),
				Action: []string{"Для важных данных перейти на size 3 / min_size 2 (нужно ещё столько же сырого места, сколько эти пулы занимают в одной копии).",
					"Для остальных - держать хосты выключенными как можно меньше и не обслуживать два домена подряд без HEALTH_OK между ними."},
				Cmds: []string{"ceph osd pool set <пул> size 3", "ceph osd pool set <пул> min_size 2"}})
		}
	}

	// 5. Флаги и состояние.
	for _, f := range []struct{ flag, what string }{{"noout", "OSD не выводятся из раскладки при отказе"}, {"norebalance", "данные не перебалансируются"}, {"nobackfill", "backfill остановлен"}, {"norecover", "восстановление остановлено"}, {"noscrub", "scrub выключен: ошибки на дисках не ищутся"}, {"nodeep-scrub", "deep-scrub выключен: тихая порча данных не обнаруживается"}, {"pause", "запись и чтение остановлены"}} {
		if containsFlag(s.Dump.Flags, f.flag) {
			add(Risk{Level: LevelWarn, When: "now", Title: "Установлен флаг " + f.flag, Impact: capitalize(f.what) + ". Флаг ставят на время работ и часто забывают снять.",
				Action: []string{"Убедиться, что флаг нужен, иначе снять."}, Cmds: []string{"ceph osd unset " + f.flag}})
		}
	}
	if cr.Health != "" && cr.Health != "HEALTH_OK" {
		var items []string
		for _, c := range cr.Checks {
			if t, ok := healthText[c]; ok {
				items = append(items, t+" ("+c+")")
			} else {
				items = append(items, c)
			}
		}
		lv := LevelWarn
		if cr.Health == "HEALTH_ERR" {
			lv = LevelBad
		}
		add(Risk{Level: lv, When: "now", Title: "Ceph сообщает о проблемах (" + cr.Health + ")",
			Impact: "Что именно: " + strings.Join(items, "; ") + ".",
			Action: []string{"Разобрать каждую проверку до начала плановых работ."}, Cmds: []string{"ceph health detail"}})
	}
	if cr.MaxPGOSD > 0 && cr.PGPerOSD[1] > float64(cr.MaxPGOSD) {
		add(Risk{Level: LevelWarn, When: "plan", Title: "На части дисков слишком много групп данных (PG)",
			Impact: fmt.Sprintf("До %.0f PG на OSD при пределе %d: такие OSD тратят больше памяти и дольше восстанавливаются, а новые PG на них могут не создаться.", cr.PGPerOSD[1], cr.MaxPGOSD),
			Action: []string{"Уменьшить pg_num у пулов с избытком PG или добавить OSD."}})
	}

	// 6. Хорошее - что можно делать.
	if r := cr.Rolling; r != nil && len(r.Batches) > 0 {
		one := true
		for _, b := range r.Batches {
			one = one && len(b.Hosts) == 1
		}
		how := fmt.Sprintf("%d %s, всего около %s при окне %s на партию.", len(r.Batches), plural(len(r.Batches), "партия", "партии", "партий"), dur(r.Total), dur(cr.Scenario.Window))
		act := "Порядок, хосты и команды - в разделе «План обслуживания всех хостов» ниже."
		if one {
			add(Risk{Level: LevelOK, When: "good", Title: "Плановое обслуживание - строго по одному хосту", Impact: "Любые два хоста, выключенные вместе, делают часть данных недоступной. " + how, Action: []string{act}})
		} else {
			add(Risk{Level: LevelOK, When: "good", Title: "Плановое обслуживание можно делать партиями", Impact: fmt.Sprintf("Хосты можно выключать группами, не теряя доступа к данным: %d %s укладываются в %s", cr.Hosts, plural(cr.Hosts, "хост", "хоста", "хостов"), how), Action: []string{act}})
		}
	}

	cr.Verdict = LevelOK
	for _, r := range cr.Risks {
		if r.Level > cr.Verdict {
			cr.Verdict = r.Level
		}
	}
	sort.SliceStable(cr.Risks, func(i, j int) bool {
		a, b := cr.Risks[i], cr.Risks[j]
		if a.Level != b.Level {
			return a.Level > b.Level
		}
		return a.weight > b.weight
	})
}

func poolSize(kind string) int {
	var n int
	fmt.Sscanf(kind, "rep %d", &n)
	return n
}

func containsFlag(flags, f string) bool {
	for _, x := range strings.Split(flags, ",") {
		if x == f {
			return true
		}
	}
	return false
}

func short(xs []string) string {
	if len(xs) <= 4 {
		return joinList(xs)
	}
	return joinList(xs[:4]) + fmt.Sprintf(" и ещё %d", len(xs)-4)
}

func joinList(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

func capitalize(x string) string {
	r := []rune(x)
	if len(r) == 0 {
		return x
	}
	return strings.ToUpper(string(r[:1])) + string(r[1:])
}

// overflowRisk - после отказа домена данные не помещаются на остальные OSD.
// Различает остановку записи (выше full) и остановку восстановления (выше backfillfull),
// а причину - разную ёмкость доменов, общую заполненность или неровное заполнение дисков.
func (cr *ClusterReport) overflowRisk(s *Snapshot, t string, full, all []DomainRisk, autoOut bool) Risk {
	w := typeWord[t]
	n := len(all)
	var names []string
	worst := full[0]
	writes := false
	for _, d := range full {
		names = append(names, d.Name)
		if d.FailMaxAfter > worst.FailMaxAfter {
			worst = d
		}
		writes = writes || d.FailFull > 0
	}
	which := fmt.Sprintf("%s из %d (%s)", countGen(len(full), t), n, short(names))
	if len(full) == n {
		which = map[bool]string{true: "любой стойки", false: "любого " + w[1]}[t == "rack"]
	}
	osds := func(k int) string {
		return fmt.Sprintf("%d OSD %s", k, plural(k, "заполнится", "заполнятся", "заполнятся"))
	}
	r := Risk{Level: LevelBad, When: "plan", weight: 1}
	if writes {
		r.weight = 2
		r.Title = fmt.Sprintf("Отказ %s остановит запись: на остальных не хватит места", one(t))
		r.Impact = fmt.Sprintf("Это касается %s. %s, и места не хватит: %s выше порога full %.0f%%, самый полный - до %s. После этого Ceph остановит запись во все пулы.",
			which, moveStart(s, autoOut), osds(worst.FailFull), cr.Ratios[2]*100, pct1(worst.FailMaxAfter))
	} else {
		r.Title = fmt.Sprintf("После отказа %s восстановление не завершится: не хватит места", one(t))
		r.Impact = fmt.Sprintf("Это касается %s. %s, и %s выше порога backfillfull %.0f%% (самый полный - до %s). На такие диски Ceph перестаёт переносить данные: часть данных так и останется с неполным числом копий, пока не освободится место. Запись продолжится, но до порога full %.0f%% останется совсем немного.",
			which, moveStart(s, autoOut), osds(worst.FailBFull), cr.Ratios[1]*100, pct1(worst.FailMaxAfter), cr.Ratios[2]*100)
	}

	// Причина.
	var small []string
	var bigMin int64
	for _, d := range all {
		if d.Raw > 0 && d.Raw*3 < worst.Raw {
			small = append(small, fmt.Sprintf("%s - %s", d.Name, bytesH(d.Raw)))
		} else if bigMin == 0 || d.Raw < bigMin {
			bigMin = d.Raw
		}
	}
	// Средняя заполненность, при которой равные домены переживают потерю одного с запасом до nearfull.
	target := cr.Ratios[0] * float64(n-1) / float64(n)
	dense := cr.FillAvg > target
	// Неровное заполнение дисков называем причиной, только когда домены равны по ёмкости.
	uneven := cr.FillMax-cr.FillAvg > 0.08 && len(small) == 0
	var causes []string
	if len(small) > 0 {
		causes = append(causes, fmt.Sprintf("%s разной ёмкости: %s, а в остальных от %s", typePlural[t], joinList(small), bytesH(bigMin)))
		r.Action = append(r.Action, fmt.Sprintf("Выровнять ёмкость %s: любые %d из %d должны вмещать все данные с запасом до nearfull.", w[2], n-1, n))
	}
	if dense {
		causes = append(causes, fmt.Sprintf("кластер заполнен слишком плотно: в среднем %s, а чтобы пережить потерю %s из %d, нужно не выше %.0f%%", pct1(cr.FillAvg), one(t), n, target*100))
		need := float64(cr.RawUsed)/target - float64(cr.RawTotal)
		r.Action = append(r.Action, fmt.Sprintf("Добавить около %s сырой ёмкости или освободить место, чтобы средняя заполненность опустилась до %.0f%%.", bytesH(int64(need)), target*100))
	}
	if uneven {
		causes = append(causes, fmt.Sprintf("диски заполнены неравномерно: самый полный - %s при среднем %s, он и переполняется первым. В среднем места хватает", pct1(cr.FillMax), pct1(cr.FillAvg)))
		r.Action = append(r.Action, fmt.Sprintf("Выровнять заполнение дисков балансировщиком в режиме upmap: при среднем %s самые полные диски должны опуститься ближе к нему.", pct1(cr.FillAvg)))
		r.Cmds = append(r.Cmds, "ceph balancer mode upmap", "ceph balancer on", "ceph osd df tree")
	}
	if uneven && (len(small) > 0 || dense) {
		// «в среднем места хватает» верно только когда неровность - единственная причина
		causes[len(causes)-1] = strings.TrimSuffix(causes[len(causes)-1], ". В среднем места хватает")
	}
	if len(causes) == 1 {
		r.Impact += " Причина - " + causes[0] + "."
	} else if len(causes) > 1 {
		r.Impact += " Причины: " + strings.Join(causes, "; ") + "."
	}
	if !autoOut {
		r.Level = LevelWarn
		r.Title = fmt.Sprintf("Если при аварии %s вывести его OSD вручную, %s", w[1], map[bool]string{true: "встанет запись", false: "восстановление не завершится"}[writes])
		r.Impact = strings.Replace(r.Impact, "Это касается", fmt.Sprintf("Сам Ceph при отказе целого %s его диски из раскладки не выводит (mon_osd_down_out_subtree_limit = %s): данные остаются на месте до возврата, и места хватает. Риск - в ручных действиях. Это касается", w[1], s.Meta.DownOutSubtreeLimit), 1)
		r.Action = append([]string{fmt.Sprintf("При аварии %s не выводить его диски вручную (ceph osd out) и не менять mon_osd_down_out_subtree_limit - чинить и возвращать оборудование.", w[1])}, r.Action...)
	} else {
		r.Action = append(r.Action, fmt.Sprintf("Пока это не сделано: если оборудование после отказа быстро не вернуть, поставить noout в первые %s. Данные останутся с неполным числом копий, но место не закончится. Снять noout после возврата.", downOutInterval(s)))
		r.Cmds = append(r.Cmds, "ceph osd set noout", "ceph osd unset noout")
	}
	return r
}

// downOutInterval - через сколько Ceph выводит упавшие OSD из раскладки (по умолчанию 10 минут).
func downOutInterval(s *Snapshot) string {
	if s.Meta.DownOutInterval > 0 {
		return dur(time.Duration(s.Meta.DownOutInterval) * time.Second)
	}
	return "10 минут"
}

// moveStart - с чего начинается переезд после отказа: сам Ceph через интервал или только вручную.
func moveStart(s *Snapshot, autoOut bool) string {
	if autoOut {
		return fmt.Sprintf("Через %s после отказа Ceph начнёт переносить данные на остальные диски", downOutInterval(s))
	}
	return "Если вывести диски вручную, Ceph начнёт переносить данные на остальные"
}
