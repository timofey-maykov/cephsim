package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Batch - партия хостов, которые обслуживаются одновременно с noout.
type Batch struct {
	Hosts       []string      `json:"hosts"`
	OSDs        int           `json:"osds"`
	Degraded    int           `json:"pg_degraded"`
	AtRisk      int           `json:"pg_at_risk"`
	CatchupB    int64         `json:"catchup_bytes"`
	CatchupTime time.Duration `json:"catchup_time"`
	Start       time.Duration `json:"start"`
	Time        time.Duration `json:"time"`
}

type BlockedHost struct {
	Host       string `json:"host"`
	Inactive   int    `json:"pg_inactive"`
	Incomplete int    `json:"pg_incomplete"`
	// сколько из них неполные уже сейчас: блокировку снимет завершение recovery
	FromUndersized int `json:"pg_already_undersized"`
}

type RollingResult struct {
	Scenario Scenario      `json:"scenario"`
	Epoch    int           `json:"osdmap_epoch"`
	Hosts    int           `json:"hosts"`
	Parallel int           `json:"max_parallel"`
	Gap      time.Duration `json:"gap"`
	Batches  []Batch       `json:"batches"`
	Blocked  []BlockedHost `json:"blocked"`
	Total    time.Duration `json:"total_time"`
	// PG с неполным acting в снимке
	Undersized0 int      `json:"pg_undersized_now"`
	Verdict     Level    `json:"verdict"`
	Reasons     []Reason `json:"reasons"`
	Plan        []string `json:"plan"`
}

// rollingHosts: хосты под целями (по умолчанию все), по пути в CRUSH-дереве
// (row, rack, host), чтобы хосты одного failure domain шли подряд.
func rollingHosts(s *Snapshot, targets []string) ([]string, error) {
	seen := map[string]bool{}
	var hosts []string
	if len(targets) == 0 {
		for id := range s.Tree {
			if h := s.Ancestor(id, "host"); id >= 0 && h != "-" && !seen[h] {
				seen[h] = true
				hosts = append(hosts, h)
			}
		}
	} else {
		osds, err := s.ResolveOSDs(targets)
		if err != nil {
			return nil, err
		}
		for _, o := range osds {
			if h := s.Ancestor(o, "host"); !seen[h] {
				seen[h] = true
				hosts = append(hosts, h)
			}
		}
	}
	path := map[string]string{}
	for _, h := range hosts {
		var parts []string
		for id := s.ByName[h].ID; ; {
			parts = append([]string{s.Tree[id].Name}, parts...)
			p, ok := s.Parent[id]
			if !ok {
				break
			}
			id = p
		}
		path[h] = strings.Join(parts, "/")
	}
	sort.Slice(hosts, func(i, j int) bool { return path[hosts[i]] < path[hosts[j]] })
	return hosts, nil
}

// Rolling строит расписание обслуживания: каждый хост идёт в первую партию,
// где одновременное выключение с noout не делает PG inactive или incomplete.
// parallel - предел хостов в партии (0 - без предела), gap - пауза между партиями.
func Rolling(s *Snapshot, sc Scenario, parallel int, gap time.Duration) (*RollingResult, error) {
	hosts, err := rollingHosts(s, sc.Targets)
	if err != nil {
		return nil, err
	}
	r := &RollingResult{Scenario: sc, Epoch: s.Dump.Epoch, Hosts: len(hosts), Parallel: parallel, Gap: gap}
	m := sc
	m.Action, m.NoOut = ActMaintenance, true
	avail := func(hs []string) (*Core, error) {
		q := m
		q.Targets = hs
		return Analyze(s, q, false)
	}
	var batches [][]string
next:
	for _, h := range hosts {
		c, err := avail([]string{h})
		if err != nil {
			return nil, err
		}
		r.Undersized0 = c.Undersized0
		if c.Inactive+c.Incomplete > 0 {
			r.Blocked = append(r.Blocked, BlockedHost{h, c.Inactive, c.Incomplete, c.LostFromUnder})
			continue
		}
		for i, b := range batches {
			if parallel > 0 && len(b) >= parallel {
				continue
			}
			c, err := avail(append(append([]string(nil), b...), h))
			if err != nil {
				return nil, err
			}
			if c.Inactive+c.Incomplete == 0 {
				batches[i] = append(b, h)
				continue next
			}
		}
		batches = append(batches, []string{h})
	}

	var start time.Duration
	maxRisk := 0
	for i, hs := range batches {
		q := m
		q.Targets = hs
		c, err := Analyze(s, q, false)
		if err != nil {
			return nil, err
		}
		f := Finalize(s, c, q)
		b := Batch{Hosts: hs, OSDs: len(c.OSDs), Degraded: c.Degraded, AtRisk: c.AtRisk, CatchupB: f.CatchupBytes, CatchupTime: f.CatchupTime, Start: start}
		b.Time = sc.Window + f.CatchupTime
		if i < len(batches)-1 {
			b.Time += gap
		}
		start += b.Time
		maxRisk = max(maxRisk, c.AtRisk)
		r.Batches = append(r.Batches, b)
	}
	r.Total = start

	add := func(l Level, f string, a ...any) { r.Reasons = append(r.Reasons, Reason{l, fmt.Sprintf(f, a...)}) }
	transient := 0
	for _, b := range r.Blocked {
		if b.FromUndersized == b.Inactive+b.Incomplete {
			transient++
			continue
		}
		add(LevelBad, "%s нельзя обслуживать с noout: %s PG станут inactive, %s incomplete. Сначала drain этого хоста или исправить правила пулов", b.Host, num(b.Inactive), num(b.Incomplete))
	}
	if transient > 0 {
		add(LevelBad, "%d хост(ов) сейчас нельзя выключать: на них единственные живые копии PG, которые уже неполные (в кластере %s PG с неполным acting, идёт recovery/backfill). Дождитесь HEALTH_OK или посчитайте с -assume-clean", transient, num(r.Undersized0))
	}
	if maxRisk > 0 && sc.Window > 4*time.Hour {
		add(LevelWarn, "в партиях до %s PG без запаса на %s: ещё один отказ, и они станут inactive. Сократите окно", num(maxRisk), dur(sc.Window))
	}
	if len(batches) > 1 && len(batches) == len(hosts)-len(r.Blocked) {
		add(LevelOK, "по одному хосту: любые два хоста вместе дают inactive PG (failure domain пулов - host)")
	}
	r.Verdict = LevelOK
	for _, x := range r.Reasons {
		if x.Level > r.Verdict {
			r.Verdict = x.Level
		}
	}
	r.Plan = append(r.Plan, fmt.Sprintf("ceph config set osd osd_max_backfills %d", sc.Backfills))
	for i, b := range r.Batches {
		var enter, exit []string
		for _, h := range b.Hosts {
			enter = append(enter, "ceph orch host maintenance enter "+h)
			exit = append(exit, "ceph orch host maintenance exit "+h)
		}
		wait := "ждать HEALTH_OK"
		if b.CatchupB > 0 {
			wait += fmt.Sprintf(" (~%s)", dur(b.CatchupTime))
		}
		r.Plan = append(r.Plan, fmt.Sprintf("партия %d: %s; работы ≤ %s; %s; %s", i+1, strings.Join(enter, " && "), dur(sc.Window), strings.Join(exit, " && "), wait))
	}
	return r, nil
}

func PrintRolling(w io.Writer, s *Snapshot, r *RollingResult, color bool) {
	c := colors(color)
	p := func(f string, a ...any) { fmt.Fprintf(w, f, a...) }
	head := func(t string) { p("\n%s%s%s\n", c.bold, t, c.reset) }
	src := "osdmap e" + fmt.Sprint(r.Epoch)
	if s.Meta.CollectedAt != "" {
		src += ", снят " + s.Meta.CollectedAt
	}
	par := "без предела"
	if r.Parallel > 0 {
		par = fmt.Sprintf("до %d", r.Parallel)
	}
	p("%scephsim%s · %s · %d OSD\n", c.bold, c.reset, src, len(s.DF))
	p("Сценарий: обслуживание %d хостов по очереди, noout, окно %s на партию, хостов в партии %s, пауза %s\n", r.Hosts, dur(r.Scenario.Window), par, dur(r.Gap))
	verdict(w, c, r.Verdict, r.Reasons)

	head(fmt.Sprintf("Расписание: %d партий, всего ~%s", len(r.Batches), dur(r.Total)))
	p("  %-3s %-13s %-44s %5s %9s %9s  %s\n", "№", "старт", "хосты", "OSD", "degraded", "без зап.", "догон")
	for i, b := range r.Batches {
		hs := strings.Join(b.Hosts, ", ")
		if len(hs) > 44 {
			hs = hs[:41] + "..."
		}
		catch := "-"
		if b.CatchupB > 0 {
			catch = fmt.Sprintf("%s, %s", bytesH(b.CatchupB), dur(b.CatchupTime))
		}
		p("  %-3d +%-12s %-44s %5d %9s %9s  %s\n", i+1, dur(b.Start), hs, b.OSDs, num(b.Degraded), num(b.AtRisk), catch)
	}
	for _, b := range r.Blocked {
		why := ""
		if b.FromUndersized == b.Inactive+b.Incomplete {
			why = ": только из-за PG, неполных уже сейчас"
		}
		p("  %s✖ %s - нельзя с noout (%d inactive, %d incomplete%s)%s\n", c.red, b.Host, b.Inactive, b.Incomplete, why, c.reset)
	}
	head("План")
	for i, x := range r.Plan {
		p("  %d. %s\n", i+1, x)
	}
	p("\n%sПартия = хосты, которые можно выключить вместе без inactive PG. Время партии = окно + догон записей + пауза.%s\n", c.dim, c.reset)
}

func verdict(w io.Writer, c palette, v Level, reasons []Reason) {
	var vc, vt string
	switch v {
	case LevelBad:
		vc, vt = c.red, "✖ НЕЛЬЗЯ"
	case LevelWarn:
		vc, vt = c.yellow, "▲ МОЖНО, С УСЛОВИЯМИ"
	default:
		vc, vt = c.green, "✔ БЕЗОПАСНО"
	}
	fmt.Fprintf(w, "\n%s%s%s%s\n", c.bold, vc, vt, c.reset)
	n := 0
	for _, x := range reasons {
		col := c.yellow
		switch x.Level {
		case LevelBad:
			col = c.red
		case LevelOK:
			col = c.dim
		}
		fmt.Fprintf(w, "  %s•%s %s\n", col, c.reset, x.Text)
		n++
	}
	if n == 0 {
		fmt.Fprintf(w, "  данные доступны, места хватает\n")
	}
}
