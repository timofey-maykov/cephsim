package main

import (
	"fmt"
	"html/template"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// HTML-отчёт: один файл без внешних зависимостей, графики - встроенный SVG.

type kpi struct {
	Label, Value, Note, Level string
}

type hostChip struct {
	Name, Note, Class, Title string
}

type treeBox struct {
	Name, Type string
	Hosts      []hostChip
	Kids       []treeBox
}

type poolRow struct {
	PoolImpact
	Level string
}

type htmlData struct {
	Title, Subtitle, Change string
	Source                  []string
	Verdict, VerdictText    string
	Reasons                 []Reason
	KPIs                    []kpi
	Bottleneck              string
	Tree                    []treeBox
	TreeLegend              []hostChip
	FillSVG, CurveSVG       template.HTML
	FillNote, CurveNote     string
	Clients                 []ClientRow
	Variants                []Variant
	Pools, PoolsMore        []poolRow
	Plan                    []planStep
	Notes                   []string
	GanttSVG                template.HTML
	Batches                 []batchRow
	Blocked                 []BlockedHost
}

// planStep: команды показываются блоком кода с кнопкой копирования, пояснения - текстом.
type planStep struct {
	Text string
	Cmd  bool
}

func planSteps(lines []string) []planStep {
	var out []planStep
	for _, l := range lines {
		out = append(out, planStep{l, strings.HasPrefix(l, "ceph ")})
	}
	return out
}

type ClientRow struct {
	Class, Load, Demand, Cap, Rec string
	Starved                       bool
}

type batchRow struct {
	N                      int
	Start, Hosts, Catchup  string
	OSDs, Degraded, AtRisk int
}

var levelName = map[Level]string{LevelOK: "ok", LevelWarn: "warn", LevelBad: "bad"}
var verdictText = map[Level]string{LevelOK: "Безопасно", LevelWarn: "Можно, с условиями", LevelBad: "Нельзя"}

func pct1(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }

func sourceLines(s *Snapshot, cal *Calibration, sc Scenario) []string {
	src := []string{fmt.Sprintf("снимок osdmap e%d", s.Dump.Epoch)}
	if s.Meta.CollectedAt != "" {
		src[0] += ", снят " + s.Meta.CollectedAt
	}
	if s.Meta.Synthetic {
		src[0] += " (синтетический)"
	}
	var slots []string
	for _, k := range []string{"hdd", "ssd", "nvme"} {
		if v, ok := sc.SlotMBps[k]; ok {
			slots = append(slots, fmt.Sprintf("%s %.0f MB/s", k, v))
		}
	}
	if cal != nil {
		src = append(src, fmt.Sprintf("скорость backfill-слота (%s) - калибровка по истории метрик за %s - %s", strings.Join(slots, ", "), cal.Since[:10], cal.Until[:10]))
	} else {
		src = append(src, fmt.Sprintf("скорость backfill-слота (%s) - значения по умолчанию, без калибровки: время может быть занижено", strings.Join(slots, ", ")))
	}
	if sc.ClientSource != "" {
		src = append(src, "клиентская нагрузка: "+sc.ClientSource)
	}
	src = append(src, "раскладка PG считается портом CRUSH; на каждом новом кластере сверяйте её командой cephsim verify")
	return src
}

func fillClass(v float64, ratios [3]float64) string {
	switch {
	case v >= ratios[2]:
		return "full"
	case v >= ratios[1]:
		return "bf"
	case v >= ratios[0]:
		return "near"
	}
	return "okf"
}

// buildTree: дерево CRUSH до хостов; хосты - фишки с состоянием.
func buildTree(s *Snapshot, r *Result) ([]treeBox, []hostChip) {
	gone := map[int]bool{}
	for _, o := range r.gone {
		gone[o] = true
	}
	added := map[int]bool{}
	for _, o := range r.Added {
		added[o] = true
	}
	after := map[int]float64{}
	for _, p := range r.projs {
		after[p.ID] = p.After
	}
	goneClass, goneNote := "down", "выключается"
	switch r.Scenario.Action {
	case ActDrain:
		goneClass, goneNote = "drain", "выводится"
	case ActFailure:
		goneNote = "отказ"
	}
	var walk func(id int) ([]int, treeBox)
	walk = func(id int) ([]int, treeBox) {
		n := s.Tree[id]
		box := treeBox{Name: n.Name, Type: n.Type}
		var osds []int
		for _, c := range n.Children {
			if c >= 0 {
				osds = append(osds, c)
				continue
			}
			child := s.Tree[c]
			sub, cb := walk(c)
			osds = append(osds, sub...)
			if child.Type == "host" {
				ch := hostChip{Name: child.Name}
				mx, ng, na := 0.0, 0, 0
				for _, o := range sub {
					mx = math.Max(mx, after[o])
					if gone[o] {
						ng++
					}
					if added[o] {
						na++
					}
				}
				switch {
				case ng > 0:
					ch.Class, ch.Note = goneClass, goneNote
				case na > 0:
					ch.Class, ch.Note = "new", fmt.Sprintf("новый, %s", pct1(mx))
				default:
					ch.Class, ch.Note = fillClass(mx, r.Ratios), pct1(mx)
				}
				ch.Title = fmt.Sprintf("%s: %d OSD, максимум после %s", child.Name, len(sub), pct1(mx))
				box.Hosts = append(box.Hosts, ch)
			} else if len(sub) > 0 {
				box.Kids = append(box.Kids, cb)
			}
		}
		sort.Slice(box.Kids, func(i, j int) bool { return box.Kids[i].Name < box.Kids[j].Name })
		sort.Slice(box.Hosts, func(i, j int) bool { return box.Hosts[i].Name < box.Hosts[j].Name })
		return osds, box
	}
	var roots []int
	for id, n := range s.Tree {
		if _, ok := s.Parent[id]; !ok && id < 0 && n.Type != "host" {
			roots = append(roots, id)
		}
	}
	sort.Ints(roots)
	var out []treeBox
	for _, id := range roots {
		if osds, b := walk(id); len(osds) > 0 {
			out = append(out, b)
		}
	}
	legend := []hostChip{{Name: "заполнение < nearfull", Class: "okf"}, {Name: "≥ nearfull", Class: "near"}, {Name: "≥ backfillfull", Class: "bf"}, {Name: "≥ full", Class: "full"}}
	switch r.Scenario.Action {
	case ActDrain:
		legend = append([]hostChip{{Name: "выводится", Class: "drain"}}, legend...)
	case ActExpand:
		legend = append([]hostChip{{Name: "новый хост", Class: "new"}}, legend...)
	case ActMaintenance, ActFailure:
		legend = append([]hostChip{{Name: "выключается", Class: "down"}}, legend...)
	}
	return out, legend
}

// fillSVG: все OSD по убыванию заполненности после; серая метка - до.
func fillSVG(r *Result) template.HTML {
	n := len(r.projs)
	if n == 0 {
		return ""
	}
	const W, H, L, B = 960.0, 240.0, 44.0, 22.0
	top := math.Max(r.Ratios[2]+0.05, r.Max.After*1.05)
	top = math.Min(math.Ceil(top*10)/10, 2)
	y := func(v float64) float64 { return (H - B) - math.Min(v, top)/top*(H-B-8) }
	bw := (W - L) / float64(n)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" class="chart" role="img" aria-label="заполненность OSD после">`, W, H)
	for t := 0.0; t <= top+1e-9; t += 0.2 {
		fmt.Fprintf(&b, `<line x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f" class="grid"/><text x="%.0f" y="%.1f" class="ax" text-anchor="end">%.0f%%</text>`, L, W, y(t), y(t), L-6, y(t)+4, t*100)
	}
	for i, p := range r.projs {
		x := L + float64(i)*bw
		cls := fillClass(p.After, r.Ratios)
		if containsInt(r.Added, p.ID) {
			cls += " newbar"
		}
		fmt.Fprintf(&b, `<rect x="%.2f" y="%.1f" width="%.2f" height="%.1f" class="bar %s"><title>%s (%s): %s → %s</title></rect>`,
			x, y(p.After), math.Max(bw-0.4, 0.6), (H-B)-y(p.After), cls, osdName(p.ID), p.Host, pct1(p.Before), pct1(p.After))
		fmt.Fprintf(&b, `<line x1="%.2f" x2="%.2f" y1="%.1f" y2="%.1f" class="before"/>`, x, x+math.Max(bw-0.4, 0.6), y(p.Before), y(p.Before))
	}
	for i, name := range []string{"nearfull", "backfillfull", "full"} {
		v := r.Ratios[i]
		if v > top {
			continue
		}
		// близкие пороги подписываем в ряд, а не друг на друге
		lx := W - 4
		if (r.Ratios[2]-r.Ratios[0])/top*(H-B-8) < 40 {
			lx = W - 4 - float64(2-i)*120
		}
		dy := -4.0
		fmt.Fprintf(&b, `<line x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f" class="thr t%d"/><text x="%.0f" y="%.1f" class="thl t%d" text-anchor="end">%s %.0f%%</text>`, L, W, y(v), y(v), i, lx, y(v)+dy, i, name, v*100)
	}
	fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" class="ax">OSD, от самого заполненного к наименее (%d шт.)</text></svg>`, L, H-4, n)
	return template.HTML(b.String())
}

// curveSVG: оценка, какая доля данных ещё не переехала.
func curveSVG(curve [][2]float64) template.HTML {
	if len(curve) < 2 {
		return ""
	}
	const W, H, L, B = 960.0, 200.0, 44.0, 26.0
	T := curve[len(curve)-1][0]
	x := func(h float64) float64 { return L + h/T*(W-L-10) }
	y := func(v float64) float64 { return 8 + (1-v)*(H-B-8) }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" class="chart" role="img" aria-label="ход переезда">`, W, H)
	for _, v := range []float64{0, 0.25, 0.5, 0.75, 1} {
		fmt.Fprintf(&b, `<line x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f" class="grid"/><text x="%.0f" y="%.1f" class="ax" text-anchor="end">%.0f%%</text>`, L, W-10, y(v), y(v), L-6, y(v)+4, v*100)
	}
	step := niceStep(T)
	for h := 0.0; h <= T+1e-9; h += step {
		fmt.Fprintf(&b, `<text x="%.1f" y="%.0f" class="ax" text-anchor="middle">%s</text>`, x(h), H-8, hoursLabel(h))
	}
	var pts []string
	for _, p := range curve {
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(p[0]), y(p[1])))
	}
	fmt.Fprintf(&b, `<polygon points="%.1f,%.1f %s %.1f,%.1f" class="area"/>`, x(0), y(0), strings.Join(pts, " "), x(T), y(0))
	fmt.Fprintf(&b, `<polyline points="%s" class="line"/>`, strings.Join(pts, " "))
	// когда останется 10% - хвост узкого места
	for _, p := range curve {
		if p[1] <= 0.1 {
			fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="8" y2="%.0f" class="mark"/><text x="%.1f" y="20" class="ax">90%% переехало за %s</text>`, x(p[0]), x(p[0]), H-B, x(p[0])+6, hoursLabel(p[0]))
			break
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func niceStep(T float64) float64 {
	for _, s := range []float64{0.25, 0.5, 1, 2, 3, 6, 12, 24, 48, 72, 168} {
		if T/s <= 10 {
			return s
		}
	}
	return 336
}

func hoursLabel(h float64) string {
	switch {
	case h == 0:
		return "0"
	case h < 1:
		return fmt.Sprintf("%.0f мин", h*60)
	case h < 48:
		m := int(math.Round(h * 60))
		if m%60 == 0 {
			return fmt.Sprintf("%d ч", m/60)
		}
		return fmt.Sprintf("%d ч %02d", m/60, m%60)
	}
	return fmt.Sprintf("%.1f сут", h/24)
}

func ganttSVG(r *RollingResult) template.HTML {
	if len(r.Batches) == 0 || r.Total <= 0 {
		return ""
	}
	const W, L, RowH, B = 960.0, 150.0, 30.0, 26.0
	H := float64(len(r.Batches))*RowH + B + 8
	T := r.Total.Hours()
	x := func(h float64) float64 { return L + h/T*(W-L-10) }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" class="chart" role="img" aria-label="расписание партий">`, W, H)
	step := niceStep(T)
	for h := 0.0; h <= T+1e-9; h += step {
		fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="4" y2="%.0f" class="grid"/><text x="%.1f" y="%.0f" class="ax" text-anchor="middle">%s</text>`, x(h), x(h), H-B, x(h), H-8, hoursLabel(h))
	}
	for i, bt := range r.Batches {
		yy := 6 + float64(i)*RowH
		s := bt.Start.Hours()
		w := r.Scenario.Window.Hours()
		c := bt.CatchupTime.Hours()
		label := fmt.Sprintf("Партия %d · %d хост.", i+1, len(bt.Hosts))
		fmt.Fprintf(&b, `<text x="8" y="%.1f" class="gl">%s</text>`, yy+18, label)
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.0f" rx="3" class="gw"><title>%s: окно %s</title></rect>`, x(s), yy+4, x(s+w)-x(s), RowH-10, strings.Join(bt.Hosts, ", "), dur(r.Scenario.Window))
		if c > 0 {
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.0f" rx="3" class="gc"><title>догон записей: %s, %s</title></rect>`, x(s+w), yy+4, math.Max(x(s+w+c)-x(s+w), 1.5), RowH-10, bytesH(bt.CatchupB), dur(bt.CatchupTime))
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func poolRows(r *Result) (top, more []poolRow) {
	ps := append([]PoolImpact(nil), r.Pools...)
	score := func(p PoolImpact) int {
		return (p.Inactive+p.Incomplete+p.Undersized)*1_000_000 + p.AtRisk*1000 + p.Degraded + p.Remapped
	}
	sort.SliceStable(ps, func(i, j int) bool { return score(ps[i]) > score(ps[j]) })
	for i, p := range ps {
		row := poolRow{PoolImpact: p, Level: "ok"}
		switch {
		case p.Inactive+p.Incomplete+p.Undersized > 0:
			row.Level = "bad"
		case p.AtRisk+p.Degraded+p.Remapped > 0:
			row.Level = "warn"
		}
		if i < 12 {
			top = append(top, row)
		} else {
			more = append(more, row)
		}
	}
	return top, more
}

func reportData(s *Snapshot, r *Result) *htmlData {
	sc := r.Scenario
	d := &htmlData{
		Title: "cephsim: " + actionTitle(sc), Change: r.Change,
		Verdict: levelName[r.Verdict], VerdictText: verdictText[r.Verdict], Reasons: r.Reasons, Plan: planSteps(r.Plan),
		Source: sourceLines(s, s.Calibration, sc),
	}
	cluster := s.Meta.Cluster
	if cluster == "" {
		cluster = "кластер"
	}
	d.Subtitle = fmt.Sprintf("%s · %d OSD · %s PG · osd_max_backfills=%d", cluster, len(s.DF), num(r.TotalPGs), sc.Backfills)

	lvl := func(bad bool, warn bool) string {
		switch {
		case bad:
			return "bad"
		case warn:
			return "warn"
		}
		return "ok"
	}
	if degrading(sc.Action) {
		lost := r.Inactive + r.Incomplete
		d.KPIs = append(d.KPIs,
			kpi{"Недоступно PG", num(lost), fmt.Sprintf("inactive %s, incomplete %s", num(r.Inactive), num(r.Incomplete)), lvl(lost > 0, false)},
			kpi{"PG без запаса", num(r.AtRisk), "ещё один отказ - и они станут inactive", lvl(false, r.AtRisk > 0)})
	}
	if r.Remapped {
		t := dur(r.RecoveryTime)
		if r.Stalled {
			t = "не завершится"
		}
		moved := bytesH(r.MovedBytes)
		note := objsH(r.MovedObjects) + " объектов, " + num(r.MovedPGs) + " PG"
		if sc.Action == ActMaintenance {
			note += "; туда и столько же обратно"
		}
		d.KPIs = append(d.KPIs,
			kpi{"Переедет", moved, note, "info"},
			kpi{"Время переезда", t, "до конца backfill на узком месте", lvl(r.Stalled, false)})
	} else if r.CatchupBytes > 0 {
		d.KPIs = append(d.KPIs, kpi{"Догон после возврата", dur(r.CatchupTime), bytesH(r.CatchupBytes) + " записей за окно", "info"})
	} else if sc.NoOut {
		d.KPIs = append(d.KPIs, kpi{"Переезд", "нет", "noout: данные остаются на месте", "ok"})
	}
	d.KPIs = append(d.KPIs, kpi{"Макс. OSD после", pct1(r.Max.After), fmt.Sprintf("%s на %s; было %s", osdName(r.Max.ID), r.Max.Host, pct1(r.MaxBefore.Before)),
		lvl(r.Max.After >= r.Ratios[1], r.Max.After >= r.Ratios[0])})

	if b := r.Bottleneck; b != nil && r.Remapped {
		why := map[string]string{"bytes": "в скорость по байтам", "objects": "в число объектов (мелкие объекты)", "mclock": "в ёмкость mClock, которую оставляют клиенты"}[b.Limit]
		d.Bottleneck = fmt.Sprintf("Дольше всех работает %s на %s: принимает %s (%s объектов), отдаёт %s. Время упирается %s.",
			osdName(b.ID), b.Host, bytesH(b.In), objsH(b.InObj), bytesH(b.Out), why)
	}
	if v := r.view; v != nil {
		d.Tree, d.TreeLegend = buildTree(v, r)
	}
	d.FillSVG = fillSVG(r)
	d.FillNote = fmt.Sprintf("Столбик - заполненность OSD после работ, серая черта - сейчас. Выше nearfull: было %d OSD, станет %d.", r.NearfullBefore, r.NearfullAfter)
	if len(r.Added) > 0 {
		d.FillNote += fmt.Sprintf(" Новые OSD (%d шт., с обводкой) заполнятся до %s - %s.", len(r.Added), pct1(r.AddedFill[0]), pct1(r.AddedFill[1]))
	}
	if r.Remapped && !r.Stalled {
		d.CurveSVG = curveSVG(r.curve)
		d.CurveNote = "Оценка: каждый OSD равномерно принимает и отдаёт свои данные за своё время; кривая - худшая из сторон приёма и отдачи. Длинный хвост - узкое место."
	}
	for _, class := range sortedKeys(r.Clients) {
		v := r.Clients[class]
		d.Clients = append(d.Clients, ClientRow{Class: class, Load: fmt.Sprintf("%.0f оп/с, %.1f MB/s", v.Load.Ops, v.Load.Bytes/1e6),
			Demand: pct1(v.Demand), Cap: pct1(v.ClientCap), Rec: pct1(v.RecShare), Starved: v.Starved})
	}
	d.Variants = r.Variants
	d.Pools, d.PoolsMore = poolRows(r)
	if r.PGPerOSD.AfterMax > 0 {
		d.Notes = append(d.Notes, fmt.Sprintf("PG на OSD: в среднем %.0f → %.0f, максимум %d → %d (mon_max_pg_per_osd %d).", r.PGPerOSD.BeforeAvg, r.PGPerOSD.AfterAvg, r.PGPerOSD.BeforeMax, r.PGPerOSD.AfterMax, sc.MaxPGPerOSD))
	}
	return d
}

func rollingData(s *Snapshot, r *RollingResult) *htmlData {
	sc := r.Scenario
	d := &htmlData{
		Title:   fmt.Sprintf("cephsim: обслуживание %d хостов по очереди", r.Hosts),
		Verdict: levelName[r.Verdict], VerdictText: verdictText[r.Verdict], Reasons: r.Reasons,
		Source: sourceLines(s, s.Calibration, sc), Blocked: r.Blocked,
	}
	cluster := s.Meta.Cluster
	if cluster == "" {
		cluster = "кластер"
	}
	par := "без предела"
	if r.Parallel > 0 {
		par = fmt.Sprint(r.Parallel)
	}
	d.Subtitle = fmt.Sprintf("%s · noout · окно %s на партию · хостов в партии: %s · пауза %s", cluster, dur(sc.Window), par, dur(r.Gap))
	if sc.AssumeClean {
		d.Change = "доступность считается по up-наборам: как после завершения текущего recovery (-assume-clean)"
	}
	maxRisk := 0
	for _, b := range r.Batches {
		maxRisk = max(maxRisk, b.AtRisk)
	}
	blk := "ok"
	if len(r.Blocked) > 0 {
		blk = "bad"
	}
	d.KPIs = []kpi{
		{"Партий", fmt.Sprint(len(r.Batches)), fmt.Sprintf("из %d хостов", r.Hosts), "info"},
		{"Всего времени", dur(r.Total), "окна, догон записей и паузы", "info"},
		{"Нельзя выключать", fmt.Sprint(len(r.Blocked)), "хостов даже поодиночке", blk},
		{"PG без запаса", num(maxRisk), "в самой рискованной партии, на время окна", map[bool]string{true: "warn", false: "ok"}[maxRisk > 0]},
	}
	d.GanttSVG = ganttSVG(r)
	d.Plan = []planStep{{fmt.Sprintf("ceph config set osd osd_max_backfills %d", sc.Backfills), true}}
	for i, b := range r.Batches {
		var enter, exit []string
		for _, h := range b.Hosts {
			enter = append(enter, "ceph orch host maintenance enter "+h)
			exit = append(exit, "ceph orch host maintenance exit "+h)
		}
		wait := "дождаться HEALTH_OK"
		if b.CatchupB > 0 {
			wait += fmt.Sprintf(" (догон ~%s)", dur(b.CatchupTime))
		}
		d.Plan = append(d.Plan,
			planStep{fmt.Sprintf("Партия %d (%d хост.): перевести в обслуживание", i+1, len(b.Hosts)), false},
			planStep{strings.Join(enter, "\n"), true},
			planStep{fmt.Sprintf("работы не дольше %s, затем вернуть хосты", dur(sc.Window)), false},
			planStep{strings.Join(exit, "\n"), true},
			planStep{wait, false})
	}
	for i, b := range r.Batches {
		c := "-"
		if b.CatchupB > 0 {
			c = fmt.Sprintf("%s, %s", bytesH(b.CatchupB), dur(b.CatchupTime))
		}
		d.Batches = append(d.Batches, batchRow{N: i + 1, Start: "+" + dur(b.Start), Hosts: strings.Join(b.Hosts, ", "), OSDs: b.OSDs, Degraded: b.Degraded, AtRisk: b.AtRisk, Catchup: c})
	}
	return d
}

func WriteHTML(w io.Writer, d *htmlData) error {
	t := template.Must(template.New("r").Funcs(template.FuncMap{
		"num": num, "bytes": bytesH, "dur": dur, "lvl": func(l Level) string { return levelName[l] }, "add": func(a, b int) int { return a + b }, "css": func() template.CSS { return template.CSS(htmlCSS) },
	}).Parse(htmlTemplate))
	return t.Execute(w, d)
}

const htmlTemplate = `<!doctype html>
<html lang="ru"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>{{css}}</style></head><body><main>
<h1>{{.Title}}</h1><p class="sub">{{.Subtitle}}{{if .Change}}<br>{{.Change}}{{end}}</p>

<section class="card verdict {{.Verdict}}"><div class="badge">{{if eq .Verdict "ok"}}✔{{else if eq .Verdict "warn"}}▲{{else}}✖{{end}} {{.VerdictText}}</div>
<div>{{if .Reasons}}<ul class="reasons">{{range .Reasons}}<li class="{{lvl .Level}}">{{.Text}}</li>{{end}}</ul>{{else}}Данные остаются доступны, места хватает.{{end}}</div></section>

<div class="kpis">{{range .KPIs}}<div class="kpi {{.Level}}"><div class="l">{{.Label}}</div><div class="v">{{.Value}}</div><div class="n">{{.Note}}</div></div>{{end}}</div>

{{if .GanttSVG}}<section class="card"><h2>Расписание</h2>{{.GanttSVG}}
<p class="note">Синий - окно работ партии, оранжевый - догон записей после возврата хостов. Между партиями - пауза.</p>
<div class="tw"><table><thead><tr><th>№</th><th>старт</th><th>хосты</th><th class="r">OSD</th><th class="r">degraded</th><th class="r">без запаса</th><th class="r">догон</th></tr></thead><tbody>
{{range .Batches}}<tr class="ok"><td>{{.N}}</td><td>{{.Start}}</td><td>{{.Hosts}}</td><td class="r">{{.OSDs}}</td><td class="r">{{num .Degraded}}</td><td class="r">{{num .AtRisk}}</td><td class="r">{{.Catchup}}</td></tr>{{end}}
</tbody></table></div>
{{if .Blocked}}<h2 style="margin-top:16px">Нельзя выключать даже поодиночке</h2><ul class="blocked">{{range .Blocked}}<li><b>{{.Host}}</b>: {{.Inactive}} inactive, {{.Incomplete}} incomplete{{if eq .FromUndersized (add .Inactive .Incomplete)}} - только из-за PG, неполных уже сейчас: дождаться HEALTH_OK{{else}} - сначала drain или исправить правила пулов{{end}}</li>{{end}}</ul>{{end}}
</section>{{end}}

{{if .Tree}}<section class="card"><h2>Кластер</h2><div class="tree">{{range .Tree}}{{template "box" .}}{{end}}</div>
<div class="legend">{{range .TreeLegend}}<span class="chip {{.Class}}">{{.Name}}</span>{{end}}</div>
<p class="note">У каждого хоста - максимальная заполненность его OSD после работ.</p></section>{{end}}

{{if .FillSVG}}<section class="card"><h2>Заполненность OSD</h2>{{.FillSVG}}<p class="note">{{.FillNote}}</p></section>{{end}}

{{if .CurveSVG}}<section class="card"><h2>Ход переезда</h2>{{.CurveSVG}}<p class="note">{{.Bottleneck}} {{.CurveNote}}</p></section>{{else if .Bottleneck}}<section class="card"><h2>Узкое место</h2><p>{{.Bottleneck}}</p></section>{{end}}

{{if .Clients}}<section class="card"><h2>Клиенты во время recovery (mClock)</h2><div class="tw"><table><thead><tr><th>класс</th><th>нагрузка на OSD</th><th class="r">нужно клиентам</th><th class="r">профиль даёт</th><th class="r">остаётся recovery</th></tr></thead><tbody>
{{range .Clients}}<tr class="{{if .Starved}}warn{{else}}ok{{end}}"><td>{{.Class}}</td><td>{{.Load}}</td><td class="r">{{.Demand}}</td><td class="r">{{.Cap}}</td><td class="r">{{.Rec}}</td></tr>{{end}}
</tbody></table></div><p class="note">Доли ёмкости OSD по модели mClock. Если клиентам нужно больше, чем даёт профиль, во время recovery у них вырастут задержки.</p></section>{{end}}

{{if .Variants}}<section class="card"><h2>Варианты</h2><div class="tw"><table><thead><tr><th>вариант</th><th class="r">переедет</th><th class="r">время</th><th>итог</th></tr></thead><tbody>
{{range .Variants}}<tr class="{{if .Best}}best {{end}}{{lvl .Verdict}}"><td>{{.Name}}{{if .Best}} - рекомендуем{{end}}</td><td class="r">{{bytes .Moved}}</td><td class="r">{{if .Stalled}}не завершится{{else}}{{dur .Time}}{{end}}</td><td><span class="pill {{lvl .Verdict}}">{{lvl .Verdict}}</span></td></tr>{{end}}
</tbody></table></div></section>{{end}}

{{if .Pools}}<section class="card"><h2>Пулы</h2><div class="tw"><table><thead><tr><th>пул</th><th>тип</th><th class="r">PG</th><th class="r">degraded</th><th class="r">недоступно</th><th class="r">без запаса</th><th class="r">remapped</th><th class="r">undersized</th></tr></thead><tbody>
{{range .Pools}}{{template "pool" .}}{{end}}</tbody></table>
{{if .PoolsMore}}<details><summary>ещё {{len .PoolsMore}} пулов</summary><table><tbody>{{range .PoolsMore}}{{template "pool" .}}{{end}}</tbody></table></details>{{end}}
</div>{{range .Notes}}<p class="note">{{.}}</p>{{end}}</section>{{end}}

{{if .Plan}}<section class="card"><h2>План</h2><ol class="plan">{{range .Plan}}<li>{{if .Cmd}}<div class="cmd"><code>{{.Text}}</code><button type="button" onclick="cp(this)">копировать</button></div>{{else}}{{.Text}}{{end}}</li>{{end}}</ol></section>{{end}}

<footer class="card"><b>Откуда данные</b><ul>{{range .Source}}<li>{{.}}</li>{{end}}</ul></footer>
</main>
<script>function cp(b){var t=b.previousElementSibling.textContent;function ok(){b.textContent='скопировано';setTimeout(function(){b.textContent='копировать'},1500)}
if(navigator.clipboard){navigator.clipboard.writeText(t).then(ok,function(){})}else{var a=document.createElement('textarea');a.value=t;document.body.appendChild(a);a.select();try{document.execCommand('copy');ok()}catch(e){}a.remove()}}</script>
</body></html>
{{define "box"}}<div class="box"><div class="t">{{.Type}} <b>{{.Name}}</b></div>{{if .Hosts}}<div class="chips">{{range .Hosts}}<span class="chip {{.Class}}" title="{{.Title}}">{{.Name}}<small>{{.Note}}</small></span>{{end}}</div>{{end}}{{if .Kids}}<div class="kids">{{range .Kids}}{{template "box" .}}{{end}}</div>{{end}}</div>{{end}}
{{define "pool"}}<tr class="{{.Level}}"><td>{{.Pool}}</td><td>{{.Kind}}</td><td class="r">{{num .PGs}}</td><td class="r">{{num .Degraded}}</td><td class="r">{{num (add .Inactive .Incomplete)}}</td><td class="r">{{num .AtRisk}}</td><td class="r">{{num .Remapped}}</td><td class="r">{{num .Undersized}}</td></tr>{{end}}
`

const htmlCSS = `:root{--bg:#f6f7f9;--card:#fff;--fg:#1d2330;--mut:#667085;--line:#e4e7ec;--ok:#12805c;--okbg:#e7f6ef;--warn:#b54708;--warnbg:#fef4e6;--bad:#c01048;--badbg:#fdecf1;--info:#2e5bd8;--infobg:#ebf0fd;
--f0:#7fb59a;--f1:#e9a23b;--f2:#e2603f;--f3:#b4123b;--new:#2e5bd8;--drain:#e9a23b;--down:#c01048}
@media (prefers-color-scheme:dark){:root{--bg:#11141a;--card:#1a1f28;--fg:#e6e8ec;--mut:#98a2b3;--line:#2a313d;--ok:#47c48f;--okbg:#12291f;--warn:#f0a44b;--warnbg:#2e2213;--bad:#f2668f;--badbg:#33141f;--info:#7c9cf2;--infobg:#172241;
--f0:#3f8a67;--f1:#c98a2e;--f2:#d0583a;--f3:#e23d6b;--new:#7c9cf2}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Arial,sans-serif}
main{max-width:1040px;margin:0 auto;padding:24px 16px 48px}
h1{font-size:22px;margin:0 0 4px}h2{font-size:17px;margin:0 0 12px}.sub{color:var(--mut);margin:0 0 16px}
.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:18px 20px;margin:0 0 16px}
.verdict{display:flex;flex-wrap:wrap;gap:8px 16px;align-items:flex-start;border-width:2px}.verdict.ok{border-color:var(--ok);background:var(--okbg)}.verdict.warn{border-color:var(--warn);background:var(--warnbg)}.verdict.bad{border-color:var(--bad);background:var(--badbg)}
.badge{font-size:20px;font-weight:700;white-space:nowrap}.ok .badge{color:var(--ok)}.warn .badge{color:var(--warn)}.bad .badge{color:var(--bad)}
.reasons{margin:0;padding-left:18px}.reasons li{margin:2px 0}.reasons li.bad::marker{color:var(--bad)}.reasons li.warn::marker{color:var(--warn)}.reasons li.ok::marker{color:var(--mut)}
.kpis{display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:12px;margin:0 0 16px}
.kpi{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:14px 16px;border-top:4px solid var(--line)}
.kpi.ok{border-top-color:var(--ok)}.kpi.warn{border-top-color:var(--warn)}.kpi.bad{border-top-color:var(--bad)}.kpi.info{border-top-color:var(--info)}
.kpi .l{color:var(--mut);font-size:13px}.kpi .v{font-size:24px;font-weight:700;margin:2px 0}.kpi .n{color:var(--mut);font-size:13px}
.note{color:var(--mut);font-size:13px;margin:8px 0 0}
.tree{display:flex;flex-direction:column;gap:10px}.box{border:1px solid var(--line);border-radius:10px;padding:8px 10px}.box>.t{font-size:12px;color:var(--mut);margin-bottom:6px}.box>.t b{color:var(--fg)}
.kids{display:flex;flex-wrap:wrap;gap:8px}.kids>.box{flex:0 1 auto}.tree>.box>.kids{flex-direction:column}.tree>.box>.kids>.box{flex:1 1 auto}
.chips{display:flex;flex-wrap:wrap;gap:6px}.chip{border-radius:8px;padding:4px 8px;font-size:12px;border:1px solid transparent;line-height:1.3}
.chip small{display:block;opacity:.85}.chip.okf{background:color-mix(in srgb,var(--f0) 25%,transparent);border-color:var(--f0)}.chip.near{background:color-mix(in srgb,var(--f1) 30%,transparent);border-color:var(--f1)}
.chip.bf{background:color-mix(in srgb,var(--f2) 35%,transparent);border-color:var(--f2)}.chip.full{background:var(--f3);color:#fff}
.chip.down{background:var(--down);color:#fff}.chip.drain{background:var(--drain);color:#000}.chip.new{background:var(--new);color:#fff}
.legend{display:flex;flex-wrap:wrap;gap:8px;margin-top:10px}
.chart{width:100%;height:auto;display:block}.grid{stroke:var(--line);stroke-width:1}.ax{fill:var(--mut);font-size:11px}
.bar.okf{fill:var(--f0)}.bar.near{fill:var(--f1)}.bar.bf{fill:var(--f2)}.bar.full{fill:var(--f3)}.bar.newbar{stroke:var(--new);stroke-width:1.2}
.before{stroke:var(--fg);stroke-opacity:.55;stroke-width:1.5}.thr{stroke-dasharray:5 4;stroke-width:1.2}.thr.t0,.thl.t0{stroke:var(--f1);fill:var(--f1)}.thr.t1,.thl.t1{stroke:var(--f2);fill:var(--f2)}.thr.t2,.thl.t2{stroke:var(--f3);fill:var(--f3)}.thl{font-size:11px;stroke:none!important}
.area{fill:var(--info);fill-opacity:.15}.line{fill:none;stroke:var(--info);stroke-width:2}.mark{stroke:var(--mut);stroke-dasharray:3 3}
.gw{fill:var(--info)}.gc{fill:var(--warn)}.gl{fill:var(--fg);font-size:12px}
table{width:100%;border-collapse:collapse;font-size:14px}th,td{text-align:left;padding:6px 8px;border-bottom:1px solid var(--line)}th{color:var(--mut);font-weight:600;font-size:12px}
td.r,th.r{text-align:right;font-variant-numeric:tabular-nums}tr.bad td:first-child{border-left:3px solid var(--bad)}tr.warn td:first-child{border-left:3px solid var(--warn)}tr.ok td:first-child{border-left:3px solid transparent}
tr.best td{background:var(--okbg)}.pill{font-size:12px;padding:1px 8px;border-radius:99px}.pill.ok{background:var(--okbg);color:var(--ok)}.pill.warn{background:var(--warnbg);color:var(--warn)}.pill.bad{background:var(--badbg);color:var(--bad)}
.tw{overflow-x:auto}
ol.plan{margin:0;padding-left:22px}ol.plan li{margin:0 0 8px}.cmd{display:flex;gap:8px;align-items:flex-start}
.cmd code{flex:1;font:13px/1.45 ui-monospace,SFMono-Regular,Menlo,monospace;background:var(--bg);border:1px solid var(--line);border-radius:6px;padding:6px 8px;white-space:pre-wrap;word-break:break-word}
button{font:inherit;font-size:12px;border:1px solid var(--line);background:var(--card);color:var(--fg);border-radius:6px;padding:5px 10px;cursor:pointer}button:hover{border-color:var(--info)}
details summary{cursor:pointer;color:var(--info);margin-top:8px}footer{color:var(--mut);font-size:12px}footer li{margin:2px 0}
.blocked li{margin:2px 0}
`

// Сводный отчёт по кластеру.

type riskBox struct {
	Name, Type, Class, Note string
	Hosts                   []hostChip
	Kids                    []riskBox
}

type domainRow struct {
	DomainRisk
	Maint, Fail, Now, FailClass, MaintClass, NowClass, Moved, Time, MaxAfter string
}

type domainGroup struct {
	Type string
	Rows []domainRow
}

type clusterHTML struct {
	Title, Subtitle   string
	Verdict, VText    string
	Summary, Short    string
	Risks             []Risk
	FAQ               []faq
	RiskGroups        []riskGroup
	Glossary          []term
	KPIs              []kpi
	Tree              []riskBox
	Groups            []domainGroup
	FillSVG, GanttSVG template.HTML
	FillNote          string
	Rolling           *htmlData
	PoolGroups        []PoolGroup
	TopPools          []PoolUsage
	Source            []string
}

func riskSummary(rs []Risk) string {
	var bad, warn int
	for _, r := range rs {
		switch r.Level {
		case LevelBad:
			bad++
		case LevelWarn:
			warn++
		}
	}
	var parts []string
	if bad > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", bad, plural(bad, "серьёзный риск", "серьёзных риска", "серьёзных рисков")))
	}
	if warn > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", warn, plural(warn, "предупреждение", "предупреждения", "предупреждений")))
	}
	if len(parts) == 0 {
		return "Кластер переживает отказ любого хоста и любого домена, каждый хост можно обслужить."
	}
	return strings.Join(parts, " и ") + ". Ниже по каждому: что не так, чем грозит и что сделать."
}

var mark = map[Level]string{LevelOK: "✔", LevelWarn: "▲", LevelBad: "✖"}

func clusterData(s *Snapshot, cr *ClusterReport) *clusterHTML {
	name := s.Meta.Cluster
	if name == "" {
		name = "кластер"
	}
	d := &clusterHTML{
		Title:    "cephsim: карта рисков " + name,
		Subtitle: fmt.Sprintf("%d OSD · %d %s · %s PG · %d %s · копии разносятся по: %s", cr.OSDs, cr.Hosts, plural(cr.Hosts, "хост", "хоста", "хостов"), num(cr.PGs), len(s.Pools), plural(len(s.Pools), "пул", "пула", "пулов"), joinList(cr.FDTypes)),
		Verdict:  levelName[cr.Verdict], VText: map[Level]string{LevelOK: "Рисков не найдено", LevelWarn: "Есть риски", LevelBad: "Есть серьёзные риски"}[cr.Verdict],
		Risks: cr.Risks, Summary: riskSummary(cr.Risks), PoolGroups: cr.PoolGroups, TopPools: cr.TopPools,
		Source: sourceLines(s, s.Calibration, cr.Scenario),
	}
	health := "ok"
	if cr.Health != "HEALTH_OK" {
		health = "warn"
	}
	if cr.Health == "HEALTH_ERR" {
		health = "bad"
	}
	hc := joinList(cr.Checks)
	if len(hc) > 90 {
		hc = hc[:87] + "..."
	}
	fillLvl := "ok"
	switch {
	case cr.FillMax >= cr.Ratios[0]:
		fillLvl = "bad"
	case cr.FillMax >= cr.Ratios[0]-0.1:
		fillLvl = "warn"
	}
	under := "ok"
	if cr.Undersized0 > 0 {
		under = "warn"
	}
	d.KPIs = []kpi{
		{"Состояние", cr.Health, hc, health},
		{"Занято", pct1(float64(cr.RawUsed) / float64(max(cr.RawTotal, 1))), fmt.Sprintf("%s из %s сырой ёмкости; %s данных", bytesH(cr.RawUsed), bytesH(cr.RawTotal), bytesH(cr.Logical)), "info"},
		{"Самый заполненный OSD", pct1(cr.FillMax), fmt.Sprintf("%s; nearfull %.0f%%", osdName(cr.FillMaxOSD), cr.Ratios[0]*100), fillLvl},
		{"Запас до nearfull", bytesH(cr.Headroom), "сырого места, если всё растёт как сейчас", fillLvl},
		{"PG без полного числа копий", num(cr.Undersized0), "сейчас; 0 - кластер восстановлен", under},
	}
	if cr.Rolling != nil {
		lv := "info"
		if len(cr.Rolling.Blocked) > 0 {
			lv = "warn"
		}
		d.KPIs = append(d.KPIs, kpi{"Обслужить все хосты", dur(cr.Rolling.Total), fmt.Sprintf("%d %s по %s; нельзя выключить: %d", len(cr.Rolling.Batches), plural(len(cr.Rolling.Batches), "партия", "партии", "партий"), dur(cr.Scenario.Window), len(cr.Rolling.Blocked)), lv})
	}

	byName := map[string]DomainRisk{}
	for _, dr := range cr.Domains {
		byName[dr.Name] = dr
	}
	var walk func(id int) riskBox
	walk = func(id int) riskBox {
		n := s.Tree[id]
		b := riskBox{Name: n.Name, Type: n.Type}
		if dr, ok := byName[n.Name]; ok {
			b.Class = "lv-" + levelName[dr.FailLevel]
			b.Note = fmt.Sprintf("отказ %s %s", mark[dr.FailLevel], dr.FailWhy)
		}
		for _, c := range n.Children {
			if c >= 0 {
				continue
			}
			ch := s.Tree[c]
			if ch.Type == "host" {
				dr := byName[ch.Name]
				hc := hostChip{Name: ch.Name, Class: "lv-" + levelName[max(dr.FailLevel, dr.MaintLevel)]}
				hc.Note = fmt.Sprintf("обсл. %s · отказ %s", mark[dr.MaintLevel], mark[dr.FailLevel])
				if dr.NowLost > 0 && dr.MaintLevel != LevelBad {
					hc.Note += " · сейчас ✖"
				}
				hc.Title = fmt.Sprintf("%s, %d OSD. Обслуживание с noout: недоступно %d PG, без запаса %d. Отказ: %s, макс. OSD после %s", ch.Name, dr.OSDs, dr.MaintLost, dr.MaintAtRisk, dr.FailWhy, pct1(dr.FailMaxAfter))
				b.Hosts = append(b.Hosts, hc)
			} else if _, ok := byName[ch.Name]; ok {
				b.Kids = append(b.Kids, walk(c))
			}
		}
		sort.Slice(b.Kids, func(i, j int) bool { return b.Kids[i].Name < b.Kids[j].Name })
		sort.Slice(b.Hosts, func(i, j int) bool { return b.Hosts[i].Name < b.Hosts[j].Name })
		return b
	}
	var roots []int
	for id, n := range s.Tree {
		if _, ok := s.Parent[id]; !ok && id < 0 && n.Type != "host" {
			roots = append(roots, id)
		}
	}
	sort.Ints(roots)
	for _, id := range roots {
		if b := walk(id); len(b.Kids)+len(b.Hosts) > 0 {
			d.Tree = append(d.Tree, b)
		}
	}

	groups := map[string]*domainGroup{}
	var order []string
	for _, dr := range cr.Domains {
		g := groups[dr.Type]
		if g == nil {
			g = &domainGroup{Type: dr.Type}
			groups[dr.Type] = g
			order = append(order, dr.Type)
		}
		row := domainRow{DomainRisk: dr, FailClass: levelName[dr.FailLevel], MaintClass: levelName[dr.MaintLevel],
			Moved: bytesH(dr.FailMoved), Time: dur(dr.FailTime), MaxAfter: pct1(dr.FailMaxAfter)}
		if dr.FailStalled {
			row.Time = "не завершится"
		}
		row.Now, row.NowClass = "✔", "ok"
		if dr.NowLost > 0 {
			row.Now, row.NowClass = fmt.Sprintf("✖ %s PG", num(dr.NowLost)), "warn"
		}
		switch {
		case dr.MaintLevel == LevelBad:
			row.Maint = fmt.Sprintf("✖ нельзя: %s PG недоступны", num(dr.MaintLost))
		default:
			row.Maint = fmt.Sprintf("✔ можно; без запаса %s PG", num(dr.MaintAtRisk))
		}
		row.Fail = mark[dr.FailLevel] + " " + dr.FailWhy
		g.Rows = append(g.Rows, row)
	}
	for _, t := range order {
		g := groups[t]
		sort.SliceStable(g.Rows, func(i, j int) bool {
			a, b := g.Rows[i], g.Rows[j]
			if max(a.FailLevel, a.MaintLevel) != max(b.FailLevel, b.MaintLevel) {
				return max(a.FailLevel, a.MaintLevel) > max(b.FailLevel, b.MaintLevel)
			}
			return a.Path < b.Path
		})
		d.Groups = append(d.Groups, *g)
	}

	if len(cr.fill) > 0 {
		fr := &Result{projs: cr.fill, Ratios: cr.Ratios, Max: cr.fill[0]}
		d.FillSVG = fillSVG(fr)
		d.FillNote = fmt.Sprintf("Заполненность каждого OSD сейчас. В среднем %s, максимум %s.", pct1(cr.FillAvg), pct1(cr.FillMax))
	}
	if cr.Rolling != nil {
		d.Rolling = rollingData(s, cr.Rolling)
		d.GanttSVG = d.Rolling.GanttSVG
	}
	friendlyCluster(s, cr, d)
	return d
}

func WriteClusterHTML(w io.Writer, d *clusterHTML) error {
	t := template.Must(template.New("c").Funcs(template.FuncMap{
		"num": num, "bytes": bytesH, "dur": dur, "lvl": func(l Level) string { return levelName[l] }, "add": func(a, b int) int { return a + b },
		"css": func() template.CSS { return template.CSS(htmlCSS + clusterCSS) }, "g": gloss,
	}).Parse(clusterTemplate))
	return t.Execute(w, d)
}

const clusterCSS = `
.lv-ok{background:color-mix(in srgb,var(--ok) 18%,transparent);border-color:var(--ok)}
.lv-warn{background:color-mix(in srgb,var(--warn) 22%,transparent);border-color:var(--warn)}
.lv-bad{background:color-mix(in srgb,var(--bad) 25%,transparent);border-color:var(--bad)}
.box.lv-ok,.box.lv-warn,.box.lv-bad{background:transparent;border-width:2px}
.box>.t .bn{font-size:12px;margin-left:6px}.box.lv-bad>.t .bn{color:var(--bad)}.box.lv-warn>.t .bn{color:var(--warn)}.box.lv-ok>.t .bn{color:var(--ok)}
td.lv{white-space:nowrap}td.ok{color:var(--ok)}td.warn{color:var(--warn)}td.bad{color:var(--bad)}
h3{font-size:14px;margin:16px 0 6px;color:var(--mut);text-transform:uppercase;letter-spacing:.04em}
.vsum{align-self:center}
.short{border-width:2px;font-size:17px}.short p{margin:4px 0 0}.short .sl{font-size:12px;text-transform:uppercase;letter-spacing:.06em;color:var(--mut);font-weight:700}
.short.ok{border-color:var(--ok)}.short.warn{border-color:var(--warn)}.short.bad{border-color:var(--bad)}
.faq{display:grid;grid-template-columns:repeat(auto-fit,minmax(280px,1fr));gap:12px}
.fq{border:1px solid var(--line);border-radius:10px;padding:12px 14px}.fq .q{font-weight:700;margin-bottom:6px}.fq .a{padding-left:18px;position:relative}
.dot{position:absolute;left:0;top:.45em;width:10px;height:10px;border-radius:50%;background:var(--mut)}.fq.ok .dot{background:var(--ok)}.fq.warn .dot{background:var(--warn)}.fq.bad .dot{background:var(--bad)}.fq.info .dot{background:var(--info)}
h2.gh{margin:24px 0 10px}h2.gh span{font-size:13px;font-weight:400;color:var(--mut);margin-left:6px}
abbr[title]{text-decoration:underline dotted;text-underline-offset:3px;cursor:help}
details.cmds summary{margin-top:6px;font-size:13px}details.eng>summary{list-style:none;cursor:pointer}details.eng>summary h2{display:inline;margin:0}
details.eng>summary::before{content:"▸ ";color:var(--mut)}details.eng[open]>summary::before{content:"▾ "}details.eng[open]>summary{margin-bottom:12px}
dl.gl{display:grid;grid-template-columns:max-content 1fr;gap:6px 14px;margin:0}dl.gl dt{font-weight:700}dl.gl dd{margin:0;color:var(--mut)}
.risk{background:var(--card);border:1px solid var(--line);border-left:5px solid var(--line);border-radius:12px;padding:14px 18px;margin:0 0 12px}
.risk.bad{border-left-color:var(--bad)}.risk.warn{border-left-color:var(--warn)}.risk.ok{border-left-color:var(--ok)}
.risk .rt{font-size:17px;font-weight:700;margin-bottom:6px}.risk .ri{margin-right:8px}.risk.bad .ri{color:var(--bad)}.risk.warn .ri{color:var(--warn)}.risk.ok .ri{color:var(--ok)}
.risk p{margin:6px 0}.risk ul{margin:4px 0 8px;padding-left:20px}.risk li{margin:2px 0}.risk .cmd{margin-top:6px}
`

const clusterTemplate = `<!doctype html>
<html lang="ru"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title><style>{{css}}</style></head><body><main>
<h1>{{.Title}}</h1><p class="sub">{{.Subtitle}}</p>

<section class="card short {{.Verdict}}"><div class="sl">Коротко</div><p>{{g .Short}}</p></section>

<section class="card"><h2>Ответы на частые вопросы</h2><div class="faq">{{range .FAQ}}<div class="fq {{.Level}}"><div class="q">{{.Q}}</div><div class="a"><span class="dot"></span>{{g .A}}</div></div>{{end}}</div></section>

{{range .RiskGroups}}<h2 class="gh {{.Class}}">{{.Title}} <span>{{len .Risks}} · {{.Hint}}</span></h2>
{{range .Risks}}<section class="risk {{lvl .Level}}"><div class="rt"><span class="ri">{{if eq (lvl .Level) "ok"}}✔{{else if eq (lvl .Level) "warn"}}▲{{else}}✖{{end}}</span>{{.Title}}</div>
<p><b>{{if eq (lvl .Level) "ok"}}Что известно.{{else}}Чем грозит.{{end}}</b> {{g .Impact}}</p>
{{if .Action}}<p class="rh"><b>{{if eq (lvl .Level) "ok"}}Как делать.{{else}}Что сделать.{{end}}</b></p><ul>{{range .Action}}<li>{{g .}}</li>{{end}}</ul>{{end}}
{{if .Cmds}}<details class="cmds"><summary>Команды для инженера</summary>{{range .Cmds}}<div class="cmd"><code>{{.}}</code><button type="button" onclick="cp(this)">копировать</button></div>{{end}}</details>{{end}}
</section>{{end}}{{end}}

<div class="kpis">{{range .KPIs}}<div class="kpi {{.Level}}"><div class="l">{{.Label}}</div><div class="v">{{.Value}}</div><div class="n">{{.Note}}</div></div>{{end}}</div>

<section class="card"><h2>Карта отказов</h2><div class="tree">{{range .Tree}}{{template "rbox" .}}{{end}}</div>
<div class="legend"><span class="chip lv-ok">✔ переживает</span><span class="chip lv-warn">▲ с условиями</span><span class="chip lv-bad">✖ опасно</span></div>
<p class="note">Цвет хоста - худшее из двух для здорового кластера: обслуживание с noout («обсл.») и внезапный отказ («отказ»). «сейчас ✖» - пока идёт recovery, выключать нельзя. Рамка ряда или стойки - что будет при потере всего этого домена. Наведите на хост, чтобы увидеть детали.</p></section>

<details class="card eng"><summary><h2>Подробно по каждому ряду, стойке и хосту</h2></summary>
{{range .Groups}}<h3>{{.Type}}</h3><div class="tw"><table><thead><tr><th>{{.Type}}</th><th class="r">OSD</th><th>обслуживание с noout</th><th>сейчас</th><th>внезапный отказ</th><th class="r">макс. OSD после</th><th class="r">переедет</th><th class="r">время</th></tr></thead><tbody>
{{range .Rows}}<tr class="{{if or (eq .FailClass "bad") (eq .MaintClass "bad")}}bad{{else if or (eq .FailClass "warn") (eq .MaintClass "warn")}}warn{{else}}ok{{end}}"><td title="{{.Path}}">{{.Name}}</td><td class="r">{{.OSDs}}</td><td class="lv {{.MaintClass}}">{{.Maint}}</td><td class="lv {{.NowClass}}" title="выключение сейчас, с учётом текущей деградации">{{.Now}}</td><td class="lv {{.FailClass}}">{{.Fail}}</td><td class="r">{{.MaxAfter}}</td><td class="r">{{.Moved}}</td><td class="r">{{.Time}}</td></tr>{{end}}
</tbody></table></div>{{end}}
<p class="note">Обслуживание - хост или домен выключается с noout, данные не двигаются. Отказ - OSD выходят в out, CRUSH перекладывает данные на остальные; время - до конца recovery на узком месте. Обе колонки - для здорового кластера (как после HEALTH_OK). «Сейчас» - сколько PG станут недоступны при выключении прямо сейчас, с учётом идущего recovery.</p></details>

{{if .GanttSVG}}<section class="card"><h2>План обслуживания всех хостов</h2>{{.GanttSVG}}
<p class="note">Партии - хосты, которые можно выключить вместе без недоступных PG. Синий - окно работ, оранжевый - догон записей.</p>
{{with .Rolling}}<div class="tw"><table><thead><tr><th>№</th><th>старт</th><th>хосты</th><th class="r">OSD</th><th class="r">без запаса</th><th class="r">догон</th></tr></thead><tbody>
{{range .Batches}}<tr class="ok"><td>{{.N}}</td><td>{{.Start}}</td><td>{{.Hosts}}</td><td class="r">{{.OSDs}}</td><td class="r">{{num .AtRisk}}</td><td class="r">{{.Catchup}}</td></tr>{{end}}</tbody></table></div>
{{if .Blocked}}<p class="note">Нельзя выключать даже поодиночке: {{range $i, $b := .Blocked}}{{if $i}}, {{end}}{{$b.Host}}{{end}}.</p>{{end}}{{end}}</section>{{end}}

{{if .FillSVG}}<details class="card eng"><summary><h2>Заполненность дисков</h2></summary>{{.FillSVG}}<p class="note">{{.FillNote}}</p></details>{{end}}

<details class="card eng"><summary><h2>Пулы</h2></summary><div class="tw"><table><thead><tr><th>тип</th><th class="r">min_size</th><th class="r">пулов</th><th class="r">PG</th><th class="r">данных</th></tr></thead><tbody>
{{range .PoolGroups}}<tr class="{{if eq .Min 1}}warn{{else}}ok{{end}}"><td>{{.Kind}}</td><td class="r">{{.Min}}</td><td class="r">{{num .Pools}}</td><td class="r">{{num .PGs}}</td><td class="r">{{bytes .Bytes}}</td></tr>{{end}}</tbody></table></div>
<h3>Самые большие пулы</h3><div class="tw"><table><thead><tr><th>пул</th><th>тип</th><th class="r">min_size</th><th class="r">PG</th><th class="r">данных</th></tr></thead><tbody>
{{range .TopPools}}<tr class="ok"><td>{{.Name}}</td><td>{{.Kind}}</td><td class="r">{{.Min}}</td><td class="r">{{num .PGs}}</td><td class="r">{{bytes .Bytes}}</td></tr>{{end}}</tbody></table></div></details>

<section class="card"><h2>Словарь</h2><dl class="gl">{{range .Glossary}}<dt>{{.Short}}</dt><dd>{{.Long}}</dd>{{end}}</dl></section>

<footer class="card"><b>Откуда данные</b><ul>{{range .Source}}<li>{{.}}</li>{{end}}</ul></footer>
</main>
<script>function cp(b){var t=b.previousElementSibling.textContent;function ok(){b.textContent='скопировано';setTimeout(function(){b.textContent='копировать'},1500)}
if(navigator.clipboard){navigator.clipboard.writeText(t).then(ok,function(){})}else{var a=document.createElement('textarea');a.value=t;document.body.appendChild(a);a.select();try{document.execCommand('copy');ok()}catch(e){}a.remove()}}</script>
</body></html>
{{define "rbox"}}<div class="box {{.Class}}"><div class="t">{{.Type}} <b>{{.Name}}</b>{{if .Note}}<span class="bn">{{.Note}}</span>{{end}}</div>{{if .Hosts}}<div class="chips">{{range .Hosts}}<span class="chip {{.Class}}" title="{{.Title}}">{{.Name}}<small>{{.Note}}</small></span>{{end}}</div>{{end}}{{if .Kids}}<div class="kids">{{range .Kids}}{{template "rbox" .}}{{end}}</div>{{end}}</div>{{end}}
`

// Дружелюбная подача карты рисков: коротко, ответы на частые вопросы, риски по срочности, словарь.

type faq struct {
	Q, A, Level string
}

type riskGroup struct {
	Title, Hint, Class string
	Risks              []Risk
}

type term struct{ Word, Short, Long string }

// glossary: термины, которые подсвечиваются в тексте рисков с подсказкой при наведении.
var glossary = []term{
	{"OSD", "OSD", "один диск в Ceph со своей службой. На хосте их обычно 10-20."},
	{"PG", "PG (placement group)", "группа данных. Ceph делит данные на тысячи таких групп и хранит каждую в нескольких копиях на разных хостах."},
	{"noout", "noout", "режим, в котором выключенный диск не выводится из раскладки: данные не переносятся, Ceph ждёт его возвращения."},
	{"nearfull", "nearfull", "порог заполнения диска, после которого Ceph предупреждает о нехватке места."},
	{"backfillfull", "backfillfull", "порог, после которого Ceph перестаёт переносить данные на диск."},
	{"min_size", "min_size", "сколько копий данных должно быть доступно, чтобы Ceph разрешал запись."},
	{"HEALTH_OK", "HEALTH_OK", "состояние Ceph без предупреждений: все копии на месте."},
	{"out", "out", "диск выведен из раскладки, его данные переносятся на другие диски."},
}

var glossaryRe = func() *regexp.Regexp {
	var ws []string
	for _, t := range glossary {
		ws = append(ws, regexp.QuoteMeta(t.Word))
	}
	return regexp.MustCompile(`(^|[^\pL\pN_~-])(` + strings.Join(ws, "|") + `)([^\pL\pN_-]|$)`)
}()

// gloss экранирует текст и оборачивает первое упоминание каждого термина в подсказку.
func gloss(text string) template.HTML {
	esc := template.HTMLEscapeString(text)
	seen := map[string]bool{}
	out := glossaryRe.ReplaceAllStringFunc(esc, func(m string) string {
		sm := glossaryRe.FindStringSubmatch(m)
		w := sm[2]
		if seen[w] {
			return m
		}
		seen[w] = true
		for _, t := range glossary {
			if t.Word == w {
				return sm[1] + `<abbr title="` + template.HTMLEscapeString(t.Short+" - "+t.Long) + `">` + w + `</abbr>` + sm[3]
			}
		}
		return m
	})
	return template.HTML(out)
}

func friendlyCluster(s *Snapshot, cr *ClusterReport, d *clusterHTML) {
	byType := map[string][]DomainRisk{}
	for _, dr := range cr.Domains {
		byType[dr.Type] = append(byType[dr.Type], dr)
	}
	hosts := byType["host"]
	var nowOK []string
	for _, h := range hosts {
		if h.NowLost == 0 && h.MaintLevel != LevelBad {
			nowOK = append(nowOK, h.Name)
		}
	}
	sort.Strings(nowOK)
	limit, hasLimit := s.Placement.Crush.Types[s.Meta.DownOutSubtreeLimit]

	// Коротко.
	var short []string
	if cr.Undersized0 > 0 {
		short = append(short, "Кластер работает и данные доступны, но сейчас он восстанавливает недостающие копии части данных.")
		if len(nowOK) < len(hosts) {
			short = append(short, fmt.Sprintf("Пока восстановление не закончится, на обслуживание можно выключать только %d %s из %d - остальные держат последние копии данных.", len(nowOK), plural(len(nowOK), "хост", "хоста", "хостов"), len(hosts)))
		}
	} else if cr.Health == "HEALTH_OK" {
		short = append(short, "Кластер в порядке: все копии данных на месте.")
	} else {
		short = append(short, "Кластер работает, все копии данных на месте, но Ceph сообщает о проблемах - они ниже.")
	}
	var plan []string
	for _, r := range cr.Risks {
		if r.When == "plan" {
			rs := []rune(r.Title)
			plan = append(plan, strings.ToLower(string(rs[:1]))+string(rs[1:]))
		}
	}
	switch len(plan) {
	case 0:
		short = append(short, "Конструкция кластера надёжная: отказ любого хоста или домена он переживает.")
	case 1:
		short = append(short, "Запланировать стоит одно: "+plan[0]+".")
	default:
		short = append(short, fmt.Sprintf("Запланировать стоит %d %s, главное - %s.", len(plan), plural(len(plan), "изменение", "изменения", "изменений"), plan[0]))
	}
	d.Short = strings.Join(short, " ")

	// Ответы на частые вопросы.
	if cr.Undersized0 > 0 && len(nowOK) < len(hosts) {
		a := fmt.Sprintf("Только %d из %d, пока идёт восстановление: %s. Остальные - после HEALTH_OK.", len(nowOK), len(hosts), short3(nowOK))
		lv := "warn"
		if len(nowOK) == 0 {
			a, lv = "Нет, пока идёт восстановление. Дождитесь HEALTH_OK.", "bad"
		}
		d.FAQ = append(d.FAQ, faq{"Можно ли прямо сейчас выключить хост на обслуживание?", a, lv})
	} else {
		bad := 0
		for _, h := range hosts {
			if h.MaintLevel == LevelBad {
				bad++
			}
		}
		if bad == 0 {
			d.FAQ = append(d.FAQ, faq{"Можно ли прямо сейчас выключить хост на обслуживание?", "Да, любой - данные останутся доступны.", "ok"})
		} else {
			d.FAQ = append(d.FAQ, faq{"Можно ли прямо сейчас выключить хост на обслуживание?", fmt.Sprintf("Не любой: %d из %d нельзя даже с noout.", bad, len(hosts)), "bad"})
		}
	}
	for _, t := range []string{"host", "rack", "row", "room", "datacenter"} {
		ds := byType[t]
		if len(ds) == 0 {
			continue
		}
		var bad, warn []string
		var maxT time.Duration
		for _, x := range ds {
			switch x.FailLevel {
			case LevelBad:
				bad = append(bad, x.Name)
			case LevelWarn:
				warn = append(warn, x.Name)
			}
			if !x.FailStalled && x.FailTime > maxT {
				maxT = x.FailTime
			}
		}
		w := typeWord[t]
		q := fmt.Sprintf("Переживёт ли кластер внезапный отказ одного %s?", w[1])
		switch {
		case len(bad) > 0:
			d.FAQ = append(d.FAQ, faq{q, fmt.Sprintf("Не любого: опасен отказ %s.", short3(bad)), "bad"})
		case len(warn) > 0 && hasLimit && s.Placement.Crush.Types[t] >= limit:
			d.FAQ = append(d.FAQ, faq{q, fmt.Sprintf("Да, данные останутся доступны. Но если после отказа %s вручную вывести его диски (out), места на остальных не хватит.", plural(len(warn), "этого "+w[1], "такого "+w[1], "такого "+w[1])), "warn"})
		case len(warn) > 0:
			d.FAQ = append(d.FAQ, faq{q, fmt.Sprintf("Данные останутся доступны, но после отказа %s места будет впритык.", short3(warn)), "warn"})
		default:
			a := "Да, любого: данные останутся доступны."
			if maxT > 0 {
				a += fmt.Sprintf(" Ceph восстановит копии сам, максимум за %s.", dur(maxT))
			}
			d.FAQ = append(d.FAQ, faq{q, a, "ok"})
		}
	}
	space := "ok"
	if cr.FillMax >= cr.Ratios[0] {
		space = "bad"
	} else if cr.FillMax >= cr.Ratios[0]-0.1 {
		space = "warn"
	}
	d.FAQ = append(d.FAQ, faq{"Хватает ли места?", fmt.Sprintf("Занято %s ёмкости. Самый заполненный диск - %s при пороге предупреждения %.0f%%; до него ещё около %s сырого места.",
		pct1(float64(cr.RawUsed)/float64(max(cr.RawTotal, 1))), pct1(cr.FillMax), cr.Ratios[0]*100, bytesH(cr.Headroom)), space})
	if r := cr.Rolling; r != nil && len(r.Batches) > 0 {
		d.FAQ = append(d.FAQ, faq{"Сколько займёт обслуживание всех хостов?", fmt.Sprintf("Около %s: %d %s по %s на работы. План - ниже.", dur(r.Total), len(r.Batches), plural(len(r.Batches), "заход", "захода", "заходов"), dur(cr.Scenario.Window)), "info"})
	}
	var two, three int64
	for _, g := range cr.PoolGroups {
		switch {
		case g.Min <= 1:
			two += g.Bytes
		default:
			three += g.Bytes
		}
	}
	if two+three > 0 {
		lv, a := "ok", fmt.Sprintf("Все %s данных хранятся так, что один отказ не мешает записи и не ставит данные под угрозу.", bytesH(two+three))
		if two > 0 {
			lv = "warn"
			a = fmt.Sprintf("%.0f%% данных (%s) хранятся так, что при одном отказе запись идёт в единственную копию; остальные %s - надёжно.", float64(two)*100/float64(two+three), bytesH(two), bytesH(three))
		}
		d.FAQ = append(d.FAQ, faq{"Насколько надёжно хранятся данные?", a, lv})
	}

	// Риски по срочности.
	groups := []riskGroup{
		{Title: "Сделать сейчас", Hint: "касается текущего состояния кластера", Class: "now"},
		{Title: "Запланировать", Hint: "особенности конструкции: сами не пройдут, но и не горят", Class: "plan"},
		{Title: "Хорошие новости", Hint: "что уже можно делать", Class: "good"},
	}
	for _, r := range cr.Risks {
		i := map[string]int{"now": 0, "plan": 1, "good": 2}[r.When]
		groups[i].Risks = append(groups[i].Risks, r)
	}
	for _, g := range groups {
		if len(g.Risks) > 0 {
			d.RiskGroups = append(d.RiskGroups, g)
		}
	}
	d.Glossary = glossary
}

func short3(xs []string) string {
	if len(xs) <= 3 {
		return joinList(xs)
	}
	return joinList(xs[:3]) + fmt.Sprintf(" и ещё %d", len(xs)-3)
}
