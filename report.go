package main

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

type palette struct{ red, yellow, green, dim, bold, reset string }

func colors(on bool) palette {
	if !on {
		return palette{}
	}
	return palette{"\033[31m", "\033[33m", "\033[32m", "\033[2m", "\033[1m", "\033[0m"}
}

func num(n int) string {
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteRune(' ')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func objsH(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1f млрд", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f млн", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1f тыс", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func osdName(id int) string { return fmt.Sprintf("osd.%d", id) }

func bytesH(b int64) string {
	v := float64(b)
	for _, u := range []string{"B", "KiB", "MiB", "GiB", "TiB"} {
		if v < 1024 {
			return fmt.Sprintf("%.1f %s", v, u)
		}
		v /= 1024
	}
	return fmt.Sprintf("%.2f PiB", v)
}

func dur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0 мин"
	case d < time.Hour:
		return fmt.Sprintf("%d мин", max(1, int(d.Minutes()+0.5)))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d ч %02d мин", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%.1f сут", d.Hours()/24)
	}
}

func bar(before, after float64, near, full float64, w int) string {
	n := func(v float64) int { return min(w, int(v*float64(w)+0.5)) }
	var b strings.Builder
	for i := 0; i < w; i++ {
		switch {
		case i < n(before):
			b.WriteString("█")
		case i < n(after):
			b.WriteString("▓")
		case i == n(near) || i == n(full):
			b.WriteString("┊")
		default:
			b.WriteString("░")
		}
	}
	return b.String()
}

func actionTitle(sc Scenario) string {
	t := strings.Join(sc.Targets, ", ")
	switch sc.Action {
	case ActMaintenance:
		mode := "без noout"
		if sc.NoOut {
			mode = "noout"
		}
		return fmt.Sprintf("обслуживание %s, %s, окно %s", t, mode, dur(sc.Window))
	case ActDrain:
		return "вывод навсегда (drain) " + t
	case ActExpand:
		var hs []string
		for _, h := range sc.Add {
			hs = append(hs, fmt.Sprintf("%s (%s, %d×%s %s)", h.Name, h.Loc[len(h.Loc)-1].Name, h.OSDs, bytesH(h.Size), h.Class))
		}
		return "расширение: " + strings.Join(hs, ", ")
	case ActRule:
		return "смена CRUSH-правила пула " + sc.Pool
	case ActPGNum:
		return fmt.Sprintf("pg_num пула %s → %d", sc.Pool, sc.PGNum)
	default:
		return "внезапная потеря " + t
	}
}

func PrintReport(w io.Writer, s *Snapshot, r *Result, color bool) {
	c := colors(color)
	sc := r.Scenario
	p := func(f string, a ...any) { fmt.Fprintf(w, f, a...) }
	pct := func(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }
	head := func(t string) { p("\n%s%s%s\n", c.bold, t, c.reset) }

	src := "osdmap e" + fmt.Sprint(r.Epoch)
	if s.Meta.CollectedAt != "" {
		src += ", снят " + s.Meta.CollectedAt
	}
	if s.Meta.Synthetic {
		src += ", синтетический"
	}
	p("%scephsim%s · %s · %d OSD, %s PG\n", c.bold, c.reset, src, len(s.DF), num(r.TotalPGs))
	if withTargets(sc.Action) {
		p("Сценарий: %s · %d OSD на %d хост(ах) · osd_max_backfills=%d\n", actionTitle(sc), len(r.OSDs), len(r.Hosts), sc.Backfills)
	} else {
		p("Сценарий: %s · osd_max_backfills=%d\n", actionTitle(sc), sc.Backfills)
	}
	if r.Change != "" {
		p("Изменение: %s\n", r.Change)
	}

	verdict(w, c, r.Verdict, r.Reasons)

	share := func(n int) string { return fmt.Sprintf("%5.1f%%", float64(n)*100/float64(max(r.TotalPGs, 1))) }
	if degrading(sc.Action) {
		head("Доступность (по acting-set из pg ls)")
		p("  PG degraded        %8s  %s\n", num(r.Degraded), share(r.Degraded))
		p("  PG inactive        %8s  %s\n", num(r.Inactive), share(r.Inactive))
		p("  PG incomplete      %8s  %s\n", num(r.Incomplete), share(r.Incomplete))
		p("  PG без запаса      %8s  %s   %sещё один отказ - inactive%s\n", num(r.AtRisk), share(r.AtRisk), c.dim, c.reset)
	} else {
		head("Доступность")
		p("  копии не теряются: данные переезжают, пока старые OSD работают (PG remapped, не degraded)\n")
	}

	how := map[string]string{ActDrain: "CRUSH-вес 0", ActExpand: "новые бакеты и OSD, вес = размер в TiB", ActRule: "новое правило пула", ActPGNum: "новый pg_num и pgp_num"}[sc.Action]
	if how == "" {
		how = "out и down, reweight 0"
	}
	head("Перебалансировка (CRUSH на копии карты: " + how + ")")
	if !r.Remapped {
		p("  данные не двигаются: noout\n")
		if r.CatchupBytes > 0 {
			p("  догон после возврата  %s за ~%s  %s(запись %s/с за окно)%s\n", bytesH(r.CatchupBytes), dur(r.CatchupTime), c.dim, bytesH(int64(s.WriteBytesSec)), c.reset)
		}
	} else {
		moved := bytesH(r.MovedBytes)
		if sc.Action == ActMaintenance {
			moved += " туда и столько же обратно"
		}
		p("  переедет           %s, %s объектов, %s PG\n", moved, objsH(r.MovedObjects), num(r.MovedPGs))
		t := dur(r.RecoveryTime)
		if r.Stalled {
			t = c.red + "не завершится" + c.reset + " (упрётся в backfillfull)"
		}
		p("  время recovery     %s\n", t)
		if b := r.Bottleneck; b != nil {
			limit := fmt.Sprintf("%d×%.0f MB/s", sc.Backfills, sc.SlotMBps[b.Class])
			if ops := sc.SlotOps[b.Class]; ops > 0 {
				limit += fmt.Sprintf(" и %d×%.0f об/с", sc.Backfills, ops)
			}
			by := "упирается в байты"
			switch b.Limit {
			case "objects":
				by = "упирается в число объектов"
			case "mclock":
				v := r.Clients[b.Class]
				by = fmt.Sprintf("упирается в ёмкость mClock: клиенты занимают %.0f%%, recovery остаётся %.0f%%", math.Min(v.Demand, v.ClientCap)*100, v.RecShare*100)
			}
			p("  %sузкое место %s (%s): приём %s / %s об., отдача %s / %s об. при %s - %s%s\n", c.dim, osdName(b.ID), b.Host,
				bytesH(b.In), objsH(b.InObj), bytesH(b.Out), objsH(b.OutObj), limit, by, c.reset)
		}
	}
	p("  макс. OSD          %s → %s  (%s, %s)\n", pct(r.MaxBefore.Before), pct(r.Max.After), osdName(r.Max.ID), r.Max.Host)
	p("  выше nearfull      %d → %d OSD\n", r.NearfullBefore, r.NearfullAfter)
	if r.Remapped {
		lim := ""
		if sc.MaxPGPerOSD > 0 {
			lim = fmt.Sprintf("  %s(mon_max_pg_per_osd %d)%s", c.dim, sc.MaxPGPerOSD, c.reset)
		}
		p("  PG на OSD          в среднем %.0f → %.0f, максимум %d → %d%s\n", r.PGPerOSD.BeforeAvg, r.PGPerOSD.AfterAvg, r.PGPerOSD.BeforeMax, r.PGPerOSD.AfterMax, lim)
	}
	if len(r.Added) > 0 {
		p("  новые OSD          %d шт. (osd.%d..osd.%d), заполнятся до %s..%s\n", len(r.Added), r.Added[0], r.Added[len(r.Added)-1], pct(r.AddedFill[0]), pct(r.AddedFill[1]))
	}

	if r.Remapped {
		head("Самые заполненные OSD после")
		for _, o := range r.Top {
			col := ""
			switch {
			case o.After >= r.Ratios[1]:
				col = c.red
			case o.After >= r.Ratios[0]:
				col = c.yellow
			}
			p("  %-8s %-10s %-8s %s%s%s %5.1f%% → %s%5.1f%%%s\n", osdName(o.ID), o.Host, o.Rack, col, bar(o.Before, o.After, r.Ratios[0], r.Ratios[2], 30), c.reset, o.Before*100, col, o.After*100, c.reset)
		}
		p("  %s█ сейчас  ▓ прирост  ┊ nearfull и full%s\n", c.dim, c.reset)
	}

	if len(r.Clients) > 0 {
		head(fmt.Sprintf("Конкуренция с клиентским I/O (mClock %s, нагрузка %s)", sc.MClock.Profile, sc.ClientSource))
		for _, class := range sortedKeys(r.Clients) {
			v := r.Clients[class]
			col := ""
			if v.Starved {
				col = c.yellow
			}
			p("  %-5s клиенты %.0f оп/с, %.1f MB/s на OSD ≈ %s%.1f%%%s ёмкости (%.0f MB/s, %.0f KB на операцию); при recovery им до %.0f%%, recovery %.0f%%\n",
				class, v.Load.Ops, v.Load.Bytes/1e6, col, v.Demand*100, c.reset, v.Capacity/1e6, v.CostPerIO/1e3, v.ClientCap*100, v.RecShare*100)
		}
	} else if sc.MClock != nil && sc.MClock.Scheduler != "mclock_scheduler" {
		p("\n%sПланировщик %s: конкуренция с клиентами не моделируется.%s\n", c.dim, sc.MClock.Scheduler, c.reset)
	}

	head("Пулы")
	nw := 22
	for _, x := range r.Pools {
		nw = max(nw, len(x.Pool))
	}
	p("  %-*s %-8s %5s %8s %8s %8s %8s %9s\n", nw, "пул", "тип", "min", "PG", "degraded", "inactive", "без зап.", "remapped")
	for _, x := range r.Pools {
		ia := num(x.Inactive + x.Incomplete)
		if x.Inactive+x.Incomplete > 0 {
			ia = c.red + fmt.Sprintf("%8s", ia) + c.reset
		}
		p("  %-*s %-8s %5d %8s %8s %8s %8s %9s\n", nw, x.Pool, x.Kind, x.MinSize, num(x.PGs), num(x.Degraded), ia, num(x.AtRisk), num(x.Remapped))
	}

	if len(r.Variants) > 0 {
		head("Сравнение вариантов")
		p("  %-26s %-26s %-18s %s\n", "вариант", "переедет", "время", "итог")
		for _, v := range r.Variants {
			t := dur(v.Time)
			if v.Stalled {
				t = "не завершится"
			}
			mark := ""
			if v.Best {
				mark = c.green + "  ← рекомендуем" + c.reset
			}
			p("  %-26s %-26s %-18s %s%s\n", v.Name, bytesH(v.Moved), t, v.Verdict, mark)
		}
	}

	head("План")
	for i, x := range r.Plan {
		p("  %d. %s\n", i+1, x)
	}
	var slots []string
	for _, k := range []string{"hdd", "ssd", "nvme"} {
		if v, ok := sc.SlotMBps[k]; ok {
			slots = append(slots, fmt.Sprintf("%s %.0f", k, v))
		}
	}
	var ops []string
	for _, k := range []string{"hdd", "ssd", "nvme"} {
		if v, ok := sc.SlotOps[k]; ok && v > 0 {
			ops = append(ops, fmt.Sprintf("%s %.0f", k, v))
		}
	}
	if len(ops) > 0 {
		rest := "для классов без предела время считается только по байтам"
		if _, ok := sc.MClock.profile(); ok {
			rest = "для остальных классов цену объекта задаёт mClock (cost_per_io)"
		}
		p("\n%sПредел объектов на слот: %s об/с (из ceph_pool_recovering_objects_per_sec); %s.%s", c.dim, strings.Join(ops, ", "), rest, c.reset)
	} else {
		p("\n%sПредела по объектам нет: время считается только по байтам, для мелких объектов это занижает его. Задайте -slot-ops или откалибруйте.%s", c.dim, c.reset)
	}
	if cal := s.Calibration; cal != nil {
		how := fmt.Sprintf("p%d скорости одной PG в recovery по пулам", cal.Percentile)
		if cal.Method == "osd" {
			how = fmt.Sprintf("p%d занятого OSD, делённый на osd_max_backfills=%d того периода", cal.Percentile, cal.HistoryBackfills)
		}
		p("\n%sСкорость слота (%s MB/s) - %s, Prometheus за %s..%s.%s\n",
			c.dim, strings.Join(slots, ", "), how, cal.Since[:10], cal.Until[:10], c.reset)
	} else {
		p("\n%sСкорость слота (%s MB/s) - значения по умолчанию. Откалибруйте: cephsim calibrate -snap DIR -prometheus URL.%s\n", c.dim, strings.Join(slots, ", "), c.reset)
	}
}
