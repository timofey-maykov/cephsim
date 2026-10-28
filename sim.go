package main

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/timofey-maykov/cephsim/crush"
)

const (
	ActMaintenance = "maintenance" // хост выключается на время работ и возвращается
	ActDrain       = "drain"       // плановый вывод: OSD живы, пока с них уезжают данные
	ActFailure     = "failure"     // внезапная потеря: хост, стойка, OSD
	ActExpand      = "expand"      // новые хосты с OSD
	ActRule        = "rule"        // пул переводится на другое CRUSH-правило
	ActPGNum       = "pgnum"       // смена pg_num пула
	ActRolling     = "rolling"     // обслуживание хостов по очереди: расписание партий
)

// degrading: сценарии, где копии теряются, и доступность считается по acting-set.
func degrading(a string) bool { return a == ActMaintenance || a == ActFailure || a == ActRolling }

// withTargets: сценарии, которым нужен -target.
func withTargets(a string) bool { return a == ActMaintenance || a == ActFailure || a == ActDrain }

type Scenario struct {
	Action    string             `json:"action"`
	Targets   []string           `json:"targets"`
	NoOut     bool               `json:"noout"`
	Window    time.Duration      `json:"window"`
	Backfills int                `json:"backfills"`
	SlotMBps  map[string]float64 `json:"slot_mbps"`
	// Объектов/с на слот по классам: для пулов с мелкими объектами recovery упирается в них, а не в байты.
	// Нет значения для класса - ограничения по объектам нет.
	SlotOps map[string]float64 `json:"slot_objects_per_sec,omitempty"`
	// Конкуренция с клиентами: планировщик OSD и ожидаемая клиентская нагрузка на OSD по классам.
	MClock       *MClock               `json:"mclock,omitempty"`
	Client       map[string]ClientLoad `json:"client_load,omitempty"`
	ClientSource string                `json:"client_load_source,omitempty"`

	Add         []HostSpec      `json:"add_hosts,omitempty"` // expand
	Pool        string          `json:"pool,omitempty"`      // rule, pgnum
	RuleName    string          `json:"rule,omitempty"`
	NewRule     *crush.RuleSpec `json:"new_rule,omitempty"`
	PGNum       int             `json:"pg_num,omitempty"`
	MaxPGPerOSD int             `json:"mon_max_pg_per_osd,omitempty"`
	// Считать доступность по up, а не acting: как будет после текущего backfill.
	AssumeClean bool `json:"assume_clean,omitempty"`
}

type Level int

const (
	LevelOK Level = iota
	LevelWarn
	LevelBad
)

func (l Level) String() string { return [...]string{"ok", "warn", "bad"}[l] }

func (l Level) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

type Reason struct {
	Level Level  `json:"level"`
	Text  string `json:"text"`
}

type PoolImpact struct {
	Pool       string `json:"pool"`
	Kind       string `json:"kind"`
	MinSize    int    `json:"min_size"`
	PGs        int    `json:"pgs"`
	Degraded   int    `json:"degraded"`
	Inactive   int    `json:"inactive"`
	Incomplete int    `json:"incomplete"`
	AtRisk     int    `json:"at_risk"`
	Remapped   int    `json:"remapped"`
	Undersized int    `json:"undersized_forever"`
}

type OSDProj struct {
	ID     int     `json:"id"`
	Host   string  `json:"host"`
	Rack   string  `json:"rack"`
	Class  string  `json:"class"`
	Before float64 `json:"before"`
	After  float64 `json:"after"`
	In     int64   `json:"in_bytes"`
	Out    int64   `json:"out_bytes"`
	InObj  int64   `json:"in_objects"`
	OutObj int64   `json:"out_objects"`
	Limit  string  `json:"limit,omitempty"` // bytes, objects или mclock: что определяет время этого OSD
	gone   bool
}

// Core - всё, что не зависит от backfills, noout и окна: доступность и новая раскладка.
type Core struct {
	OSDs          []int
	Hosts         []string
	TotalPGs      int
	Degraded      int
	Inactive      int
	Incomplete    int
	AtRisk        int
	DegradedShard float64 // сумма долей шарда по degraded PG: сколько от клиентской записи не попадёт на выключенные OSD
	Pools         map[int]*PoolImpact
	Remapped      bool
	MovedBytes    int64
	MovedObjects  int64
	MovedPGs      int
	Undersized    int
	UpmapStuck    int
	OSDProj       map[int]*OSDProj
	Added         []int
	Change        string
	PGPerOSD      PGPerOSD
	view          *Snapshot // снимок после изменения: дерево с новыми хостами для отчёта
	Undersized0   int       // PG, которые уже сейчас с неполным acting (идёт recovery или backfill)
	LostFromUnder int       // из inactive и incomplete - те, что были неполными ещё до сценария
}

// PGPerOSD - число шардов PG на OSD до и после: mon_max_pg_per_osd ограничивает
// среднее (иначе команда отклоняется), а OSD не создаёт PG сверх лимита × osd_max_pg_per_osd_hard_ratio.
type PGPerOSD struct {
	BeforeAvg float64 `json:"before_avg"`
	BeforeMax int     `json:"before_max"`
	AfterAvg  float64 `json:"after_avg"`
	AfterMax  int     `json:"after_max"`
	MaxOSD    int     `json:"after_max_osd"`
}

type Variant struct {
	Name      string        `json:"name"`
	NoOut     bool          `json:"noout"`
	Backfills int           `json:"backfills"`
	Moved     int64         `json:"moved_bytes"`
	Time      time.Duration `json:"time"`
	Stalled   bool          `json:"stalled"`
	Verdict   Level         `json:"verdict"`
	Best      bool          `json:"best"`
}

type Result struct {
	Scenario          Scenario              `json:"scenario"`
	Epoch             int                   `json:"osdmap_epoch"`
	OSDs              []int                 `json:"osds"`
	Hosts             []string              `json:"hosts"`
	TotalPGs          int                   `json:"total_pgs"`
	Degraded          int                   `json:"pg_degraded"`
	Inactive          int                   `json:"pg_inactive"`
	Incomplete        int                   `json:"pg_incomplete"`
	AtRisk            int                   `json:"pg_at_risk"`
	Remapped          bool                  `json:"remapped"`
	MovedBytes        int64                 `json:"moved_bytes"`
	MovedObjects      int64                 `json:"moved_objects"`
	MovedPGs          int                   `json:"moved_pgs"`
	Undersized        int                   `json:"pg_undersized_forever"`
	Undersized0       int                   `json:"pg_undersized_now"`
	LostFromUnder     int                   `json:"pg_lost_already_undersized"`
	Added             []int                 `json:"added_osds,omitempty"`
	AddedFill         [2]float64            `json:"added_fill_min_max,omitempty"`
	Change            string                `json:"change,omitempty"`
	PGPerOSD          PGPerOSD              `json:"pg_per_osd"`
	UpmapStuck        int                   `json:"pg_upmap_stuck"`
	RecoveryTime      time.Duration         `json:"recovery_time"`
	Stalled           bool                  `json:"recovery_stalled"`
	Bottleneck        *OSDProj              `json:"bottleneck,omitempty"`
	CatchupBytes      int64                 `json:"catchup_bytes"`
	CatchupTime       time.Duration         `json:"catchup_time"`
	Max               OSDProj               `json:"max_osd_after"`
	MaxBefore         OSDProj               `json:"max_osd_before"`
	NearfullBefore    int                   `json:"nearfull_before"`
	NearfullAfter     int                   `json:"nearfull_after"`
	BackfillfullAfter int                   `json:"backfillfull_after"`
	FullAfter         int                   `json:"full_after"`
	Ratios            [3]float64            `json:"ratios_near_backfill_full"`
	Top               []OSDProj             `json:"top_osds"`
	ClassMoves        map[string][2]int64   `json:"class_moves"` // класс -> [байт, объектов], принятых OSD этого класса
	Clients           map[string]ClientView `json:"clients,omitempty"`
	Pools             []PoolImpact          `json:"pools"`
	Verdict           Level                 `json:"verdict"`
	Reasons           []Reason              `json:"reasons"`
	Plan              []string              `json:"plan"`
	Variants          []Variant             `json:"variants,omitempty"`

	// для html-отчёта
	projs []OSDProj    // все OSD после, по убыванию заполненности
	curve [][2]float64 // ход переезда: часы, доля оставшегося
	view  *Snapshot
	gone  []int // выключаемые или выводимые OSD
}

func contains(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func alive(set []int, down map[int]bool) int {
	n := 0
	for _, o := range set {
		if o != crushNone && o >= 0 && !down[o] {
			n++
		}
	}
	return n
}

// Analyze считает доступность по реальным acting-set из `ceph pg ls`
// и, если нужно, новую раскладку.
func Analyze(s *Snapshot, sc Scenario, needRemap bool) (*Core, error) {
	var osds []int
	if withTargets(sc.Action) || len(sc.Targets) > 0 {
		var err error
		if osds, err = s.ResolveOSDs(sc.Targets); err != nil {
			return nil, err
		}
	}
	ch, err := prepare(s, sc, osds)
	if err != nil {
		return nil, err
	}
	down := ch.down
	hostSeen := map[string]bool{}
	c := &Core{OSDs: osds, Pools: map[int]*PoolImpact{}, OSDProj: map[int]*OSDProj{}, Added: ch.added, Change: ch.note, view: ch.s}
	for _, o := range osds {
		if h := s.Ancestor(o, "host"); !hostSeen[h] {
			hostSeen[h] = true
			c.Hosts = append(c.Hosts, h)
		}
	}
	for _, o := range ch.added {
		if h := ch.s.Ancestor(o, "host"); !hostSeen[h] {
			hostSeen[h] = true
			c.Hosts = append(c.Hosts, h)
		}
	}
	sort.Ints(c.OSDs)

	for id, p := range s.Pools {
		c.Pools[id] = &PoolImpact{Pool: p.Name, Kind: p.Kind(), MinSize: p.MinSize}
	}

	// Доступность: копии теряются только при выключении OSD.
	pgBytes := map[string]int64{}
	pgObjs := map[string]int64{}
	for _, pg := range s.PGs {
		pool, ok := s.Pools[pg.PoolID()]
		if !ok {
			continue
		}
		pi := c.Pools[pool.ID]
		pi.PGs++
		c.TotalPGs++
		pgBytes[pg.PGID] = pg.StatSum.NumBytes
		pgObjs[pg.PGID] = pg.StatSum.NumObjects
		if !ch.degrade {
			continue
		}
		set := pg.Acting
		if len(set) == 0 || sc.AssumeClean {
			set = pg.Up
		}
		whole := alive(set, nil)
		if whole < pool.Size {
			c.Undersized0++
		}
		a := alive(set, down)
		if a == whole {
			continue
		}
		c.Degraded++
		pi.Degraded++
		c.DegradedShard += pool.shardFactor()
		need := 1
		if pool.EC() {
			need = pool.K
		}
		if a < pool.MinSize && whole < pool.Size {
			c.LostFromUnder++
		}
		switch {
		case a < need:
			c.Incomplete++
			pi.Incomplete++
		case a < pool.MinSize:
			c.Inactive++
			pi.Inactive++
		case a == pool.MinSize:
			c.AtRisk++
			pi.AtRisk++
		}
	}

	for id, n := range ch.s.DF {
		if n.KB <= 0 {
			continue
		}
		u := float64(n.KBUsed) / float64(n.KB)
		c.OSDProj[id] = &OSDProj{ID: id, Host: ch.s.Ancestor(id, "host"), Rack: ch.s.Ancestor(id, "rack"), Class: ch.s.Class(id), Before: u, After: u, gone: down[id]}
	}

	if !needRemap {
		return c, nil
	}
	c.Remapped = true
	// Раскладка до и после считается своим портом CRUSH: osdmaptool не умеет out для группы OSD.
	base := s.baseMapping()
	next := ch.after.MapAll(ch.pools)
	c.PGPerOSD = pgPerOSD(base, next, s.Placement, ch.after)

	// Куски данных: PG целиком, а при смене pg_num - доли старых PG в новых.
	type piece struct {
		pool    Pool
		pgid    string // PG после
		old, nw []int
		bytes   int64
		objs    int64
	}
	var pieces []piece
	for id, pool := range s.Pools {
		np := ch.pools[id]
		if np.PGNum == pool.PGNum {
			for ps := 0; ps < pool.PGNum; ps++ {
				pgid := fmt.Sprintf("%d.%x", id, ps)
				pieces = append(pieces, piece{pool, pgid, base[pgid], next[pgid], pgBytes[pgid], pgObjs[pgid]})
			}
			continue
		}
		for o, m := range splitShares(pool.PGNum, np.PGNum) {
			old := fmt.Sprintf("%d.%x", id, o)
			for n, share := range m {
				pgid := fmt.Sprintf("%d.%x", id, n)
				pieces = append(pieces, piece{pool, pgid, base[old], next[pgid], int64(float64(pgBytes[old]) * share), int64(float64(pgObjs[old]) * share)})
			}
		}
	}
	for pgid, nw := range next {
		var poolID int
		fmt.Sscanf(pgid, "%d.", &poolID)
		pool := ch.pools[poolID]
		real, stuck := 0, false
		for _, o := range nw {
			if o != crushNone {
				real++
			}
			if down[o] {
				stuck = true
			}
		}
		if real < pool.Size {
			c.Undersized++
			c.Pools[poolID].Undersized++
		}
		if stuck {
			c.UpmapStuck++
		}
	}

	delta := map[int]int64{}
	movedPG := map[string]bool{}
	for _, pc := range pieces {
		old, nw, pool := pc.old, pc.nw, pc.pool
		shard := int64(float64(pc.bytes) * pool.shardFactor())
		// Шард EC хранит кусок каждого объекта, так что объектов в шарде столько же, сколько в PG.
		objs := pc.objs

		// Источник для догона: выживший член старого набора (при drain - сам выводимый OSD).
		src := -1
		for _, o := range old {
			if o != crushNone && !down[o] && contains(nw, o) {
				src = o
				break
			}
		}
		if src < 0 {
			for _, o := range old {
				if o != crushNone && !down[o] {
					src = o
					break
				}
			}
		}
		moved := false
		for i, o := range nw {
			if o == crushNone || contains(old, o) {
				continue
			}
			moved = true
			c.MovedBytes += shard
			c.MovedObjects += objs
			delta[o] += shard
			if p := c.OSDProj[o]; p != nil {
				p.In += shard
				p.InObj += objs
			}
			from := src
			if sc.Action == ActDrain && i < len(old) && down[old[i]] {
				from = old[i]
			}
			if p := c.OSDProj[from]; p != nil {
				p.Out += shard
				p.OutObj += objs
			}
		}
		for _, o := range old {
			if o != crushNone && !contains(nw, o) && !down[o] {
				delta[o] -= shard
			}
		}
		if moved && !movedPG[pc.pgid] {
			movedPG[pc.pgid] = true
			c.MovedPGs++
			c.Pools[pool.ID].Remapped++
		}
	}
	for id, p := range c.OSDProj {
		if p.gone {
			continue
		}
		kb := float64(ch.s.DF[id].KB)
		p.After = (float64(ch.s.DF[id].KBUsed)*1024 + float64(delta[id])) / (kb * 1024)
	}
	return c, nil
}

// pgPerOSD: среднее по OSD, которые in и up, и максимум.
func pgPerOSD(base, next Mapping, pb, pa *Placement) PGPerOSD {
	count := func(m Mapping, p *Placement) (float64, int, int) {
		n := map[int]int{}
		total := 0
		for _, set := range m {
			for _, o := range set {
				if o != crushNone && o >= 0 {
					n[o]++
					total++
				}
			}
		}
		in := 0
		for o := range p.Exists {
			if p.Exists[o] && p.Up[o] && p.Weight[o] > 0 {
				in++
			}
		}
		mx, who := 0, -1
		for o, k := range n {
			if k > mx || (k == mx && o < who) {
				mx, who = k, o
			}
		}
		return float64(total) / float64(max(in, 1)), mx, who
	}
	var r PGPerOSD
	r.BeforeAvg, r.BeforeMax, _ = count(base, pb)
	r.AfterAvg, r.AfterMax, r.MaxOSD = count(next, pa)
	return r
}

// recoveryTime: узкое место - OSD с наибольшим приёмом или отдачей. На каждом не больше
// backfills параллельных PG, у слота два предела: SlotMBps по байтам и SlotOps по объектам.
// Третий предел - ёмкость mClock, которую оставляют клиенты: recovery тратит на объект
// cost_per_io плюс байты. Время OSD определяет самый медленный из трёх.
func recoveryTime(c *Core, sc Scenario) (time.Duration, *OSDProj) {
	var worst float64
	var who *OSDProj
	for _, p := range c.OSDProj {
		if t, limit := osdTime(p, sc); t > worst {
			q := *p
			q.Limit = limit
			worst, who = t, &q
		}
	}
	return time.Duration(worst * float64(time.Second)), who
}

// osdTime - секунды, за которые OSD примет и отдаст свои данные, и какой предел их задал.
func osdTime(p *OSDProj, sc Scenario) (float64, string) {
	b := float64(sc.Backfills)
	bytes := math.Max(float64(p.In), float64(p.Out))
	objs := math.Max(float64(p.InObj), float64(p.OutObj))
	if bytes == 0 && objs == 0 {
		return 0, ""
	}
	rate := sc.SlotMBps[p.Class]
	if rate == 0 {
		rate = sc.SlotMBps["hdd"]
	}
	t, limit := bytes/(b*rate*1e6), "bytes"
	if ops := sc.SlotOps[p.Class]; ops > 0 {
		if to := objs / (b * ops); to > t {
			t, limit = to, "objects"
		}
	}
	if v := sc.MClock.view(p.Class, sc.Client[p.Class]); v.Configured && v.RecShare > 0 {
		if tm := (objs*v.CostPerIO + bytes) / (v.RecShare * v.Capacity); tm > t {
			t, limit = tm, "mclock"
		}
	}
	return t, limit
}

// remainingCurve - оценка хода переезда: каждый OSD равномерно принимает и отдаёт
// свои данные за своё время. Доля оставшегося - худшая из сторон приёма и отдачи:
// при drain всё упирается в отдающие OSD, при expand - в принимающие.
// Точки (часы, доля оставшегося).
func remainingCurve(c *Core, sc Scenario, total time.Duration) [][2]float64 {
	type part struct{ in, out, t float64 }
	var ps []part
	var sumIn, sumOut float64
	for _, p := range c.OSDProj {
		if p.In == 0 && p.Out == 0 {
			continue
		}
		t, _ := osdTime(p, sc)
		ps = append(ps, part{float64(p.In), float64(p.Out), t})
		sumIn += float64(p.In)
		sumOut += float64(p.Out)
	}
	if sumIn == 0 || total <= 0 {
		return nil
	}
	const n = 80
	out := make([][2]float64, 0, n+1)
	T := total.Seconds()
	for i := 0; i <= n; i++ {
		t := T * float64(i) / n
		var leftIn, leftOut float64
		for _, p := range ps {
			if p.t > 0 {
				f := math.Max(0, 1-t/p.t)
				leftIn += p.in * f
				leftOut += p.out * f
			}
		}
		v := leftIn / sumIn
		if sumOut > 0 {
			v = math.Max(v, leftOut/sumOut)
		}
		out = append(out, [2]float64{t / 3600, v})
	}
	return out
}

// Finalize собирает итог для конкретных backfills/noout/окна.
func Finalize(s *Snapshot, c *Core, sc Scenario) *Result {
	r := &Result{
		Added: c.Added, Change: c.Change, PGPerOSD: c.PGPerOSD, Undersized0: c.Undersized0, LostFromUnder: c.LostFromUnder,
		Scenario: sc, Epoch: s.Dump.Epoch, OSDs: c.OSDs, Hosts: c.Hosts, TotalPGs: c.TotalPGs,
		Degraded: c.Degraded, Inactive: c.Inactive, Incomplete: c.Incomplete, AtRisk: c.AtRisk,
		Ratios: [3]float64{s.Dump.NearFullRatio, s.Dump.BackfillFullRatio, s.Dump.FullRatio},
	}
	noRemap := sc.Action == ActMaintenance && sc.NoOut
	for _, pi := range c.Pools {
		if pi.PGs > 0 {
			p := *pi
			if noRemap {
				p.Remapped = 0 // с noout данные не двигаются, даже если remap считали для -compare
			}
			r.Pools = append(r.Pools, p)
		}
	}
	sort.Slice(r.Pools, func(i, j int) bool { return r.Pools[i].Pool < r.Pools[j].Pool })

	near, bf, full := s.Dump.NearFullRatio, s.Dump.BackfillFullRatio, s.Dump.FullRatio
	var projs []OSDProj
	for _, p := range c.OSDProj {
		if p.gone {
			continue
		}
		q := *p
		if noRemap || !c.Remapped {
			q.After, q.In, q.Out = q.Before, 0, 0
		}
		projs = append(projs, q)
		if q.Before >= near {
			r.NearfullBefore++
		}
		if q.After >= near {
			r.NearfullAfter++
		}
		if q.After >= bf {
			r.BackfillfullAfter++
		}
		if q.After >= full {
			r.FullAfter++
		}
		if q.Before > r.MaxBefore.Before {
			r.MaxBefore = q
		}
	}
	if len(c.Added) > 0 {
		r.AddedFill = [2]float64{1, 0}
		for _, id := range c.Added {
			if p := c.OSDProj[id]; p != nil {
				r.AddedFill = [2]float64{math.Min(r.AddedFill[0], p.After), math.Max(r.AddedFill[1], p.After)}
			}
		}
	}
	sort.Slice(projs, func(i, j int) bool { return projs[i].After > projs[j].After })
	if len(projs) > 0 {
		r.Max = projs[0]
	}
	r.projs, r.view, r.gone = projs, c.view, c.OSDs
	if len(projs) > 8 {
		r.Top = projs[:8]
	} else {
		r.Top = projs
	}

	if !noRemap && c.Remapped {
		r.Remapped = true
		r.MovedBytes, r.MovedObjects, r.MovedPGs, r.Undersized, r.UpmapStuck = c.MovedBytes, c.MovedObjects, c.MovedPGs, c.Undersized, c.UpmapStuck
		r.RecoveryTime, r.Bottleneck = recoveryTime(c, sc)
		r.curve = remainingCurve(c, sc, r.RecoveryTime)
		r.ClassMoves = map[string][2]int64{}
		r.Clients = map[string]ClientView{}
		for _, p := range c.OSDProj {
			m := r.ClassMoves[p.Class]
			r.ClassMoves[p.Class] = [2]int64{m[0] + p.In, m[1] + p.InObj}
			if v := sc.MClock.view(p.Class, sc.Client[p.Class]); v.Configured {
				r.Clients[p.Class] = v
			}
		}
		r.Stalled = r.BackfillfullAfter > 0
	}
	if noRemap && s.WriteBytesSec > 0 {
		// За окно на выключенные OSD не попадут записи; их догонят после возврата.
		r.CatchupBytes = int64(s.WriteBytesSec * sc.Window.Seconds() * c.DegradedShard / float64(max(c.TotalPGs, 1)))
		rate := sc.SlotMBps[s.Class(c.OSDs[0])]
		if rate == 0 {
			rate = sc.SlotMBps["hdd"]
		}
		r.CatchupTime = time.Duration(float64(r.CatchupBytes) / (float64(len(c.OSDs)*sc.Backfills) * rate * 1e6) * float64(time.Second))
	}

	r.Reasons = assess(r, sc)
	r.Verdict = LevelOK
	for _, x := range r.Reasons {
		if x.Level > r.Verdict {
			r.Verdict = x.Level
		}
	}
	r.Plan = plan(r, sc)
	return r
}

func assess(r *Result, sc Scenario) []Reason {
	var out []Reason
	add := func(l Level, f string, a ...any) { out = append(out, Reason{l, fmt.Sprintf(f, a...)}) }
	pct := func(v float64) float64 { return v * 100 }

	if r.Incomplete > 0 {
		add(LevelBad, "%s PG потеряют данные: осталось меньше k шардов (EC) или ни одной копии", num(r.Incomplete))
	}
	if r.Inactive > 0 {
		add(LevelBad, "%s PG станут inactive: живых копий меньше min_size, I/O к ним встанет до конца recovery", num(r.Inactive))
	}
	if r.LostFromUnder > 0 {
		add(LevelWarn, "из них %s PG уже сейчас с неполным acting (в кластере %s таких, идёт recovery/backfill): после HEALTH_OK картина лучше, проверьте с -assume-clean", num(r.LostFromUnder), num(r.Undersized0))
	}
	if r.FullAfter > 0 {
		add(LevelBad, "%d OSD превысят full_ratio %.0f%%: запись встанет на всём кластере", r.FullAfter, pct(r.Ratios[2]))
	} else if r.BackfillfullAfter > 0 {
		add(LevelBad, "%d OSD превысят backfillfull_ratio %.0f%%: backfill на них остановится, recovery не завершится", r.BackfillfullAfter, pct(r.Ratios[1]))
	}
	if r.NearfullAfter > r.NearfullBefore {
		add(LevelWarn, "OSD выше nearfull %.0f%%: было %d, станет %d (максимум %s, %.1f%%)", pct(r.Ratios[0]), r.NearfullBefore, r.NearfullAfter, osdName(r.Max.ID), pct(r.Max.After))
	} else if r.Remapped && r.Max.After >= r.Ratios[0]-0.03 && r.Max.After < r.Ratios[0] {
		add(LevelWarn, "%s дойдёт до %.1f%%, в %.1f п.п. от nearfull", osdName(r.Max.ID), pct(r.Max.After), pct(r.Ratios[0]-r.Max.After))
	}
	if r.Undersized > 0 {
		if degrading(sc.Action) {
			add(LevelWarn, "%s PG останутся undersized: CRUSH не находит достаточно failure domain", num(r.Undersized))
		} else {
			add(LevelBad, "%s PG останутся undersized навсегда: CRUSH не находит достаточно failure domain для size пула", num(r.Undersized))
		}
	}
	if lim := sc.MaxPGPerOSD; lim > 0 && r.Remapped {
		pp := r.PGPerOSD
		switch {
		case sc.Action == ActPGNum && pp.AfterAvg > float64(lim):
			add(LevelBad, "в среднем %.0f PG на OSD > mon_max_pg_per_osd %d: монитор отклонит pg_num", pp.AfterAvg, lim)
		case pp.AfterMax > 3*lim:
			add(LevelBad, "%s получит %d PG > %d (mon_max_pg_per_osd × osd_max_pg_per_osd_hard_ratio 3): лишние PG не создадутся и останутся activating", osdName(pp.MaxOSD), pp.AfterMax, 3*lim)
		case pp.AfterMax > lim && pp.AfterMax > pp.BeforeMax:
			add(LevelWarn, "на %s станет %d PG > mon_max_pg_per_osd %d: health warning TOO_MANY_PGS", osdName(pp.MaxOSD), pp.AfterMax, lim)
		}
	}
	if sc.Action == ActPGNum {
		if sc.PGNum&(sc.PGNum-1) != 0 {
			add(LevelWarn, "pg_num %d не степень двойки: часть PG будет вдвое больше остальных, раскладка по OSD неровная", sc.PGNum)
		}
		if r.PGPerOSD.AfterAvg < r.PGPerOSD.BeforeAvg {
			add(LevelWarn, "уменьшение pg_num идёт слиянием пар PG по одной, с peering каждой: дольше расчётного времени переезда")
		}
	}
	if r.UpmapStuck > 0 {
		add(LevelWarn, "%s PG по-прежнему указывают на выводимые OSD (pg_upmap_items), их нужно проверить вручную", num(r.UpmapStuck))
	}
	for _, class := range sortedKeys(r.ClassMoves) {
		m := r.ClassMoves[class]
		// При mClock цена объекта уже учтена (cost_per_io), предупреждать не о чем.
		if m[1] > 0 && sc.SlotOps[class] == 0 && !r.Clients[class].Configured && m[0]/m[1] < 256<<10 {
			add(LevelWarn, "на %s переезжают мелкие объекты (в среднем %s), а предела по объектам для %s нет: время recovery занижено, задайте -slot-ops или откалибруйте", class, bytesH(m[0]/m[1]), class)
		}
	}
	for _, class := range sortedKeys(r.Clients) {
		v := r.Clients[class]
		if v.Starved && r.Remapped {
			add(LevelWarn, "клиентам на %s не хватит ёмкости во время recovery: их нагрузка (%s) ≈ %.0f%% ёмкости mClock, а профиль %s оставляет им %.0f%%: задержки вырастут, выберите окно с меньшей нагрузкой",
				class, sc.ClientSource, v.Demand*100, sc.MClock.Profile, v.ClientCap*100)
		}
	}
	exposure := sc.Window
	if sc.Action != ActMaintenance || !sc.NoOut {
		exposure = r.RecoveryTime
	}
	if r.AtRisk > 0 && exposure > 4*time.Hour {
		add(LevelWarn, "%s PG %s без запаса: ещё один отказ, и они станут inactive", num(r.AtRisk), dur(exposure))
	}
	if sc.Action == ActMaintenance && !sc.NoOut && r.Remapped {
		if sc.Window < r.RecoveryTime {
			add(LevelWarn, "окно %s короче выноса данных (%s): данные начнут уезжать и вернутся обратно зря", dur(sc.Window), dur(r.RecoveryTime))
		} else {
			add(LevelWarn, "без noout %s переедут дважды: с хоста и обратно", bytesH(r.MovedBytes))
		}
	}
	return out
}

func plan(r *Result, sc Scenario) []string {
	bf := fmt.Sprintf("ceph config set osd osd_max_backfills %d", sc.Backfills)
	h := "<host>"
	if len(r.Hosts) > 0 {
		h = r.Hosts[0]
	}
	switch sc.Action {
	case ActMaintenance:
		if sc.NoOut {
			p := []string{
				fmt.Sprintf("ceph orch host maintenance enter %s   # cephadm сам ставит noout на хост", h),
				fmt.Sprintf("работы не дольше %s", dur(sc.Window)),
				fmt.Sprintf("ceph orch host maintenance exit %s", h),
				bf,
			}
			if r.CatchupBytes > 0 {
				p = append(p, fmt.Sprintf("ждать HEALTH_OK: догон записей за окно ~%s, ~%s", bytesH(r.CatchupBytes), dur(r.CatchupTime)))
			} else {
				p = append(p, "ждать HEALTH_OK")
			}
			return p
		}
		return []string{bf, fmt.Sprintf("выключить %s: через mon_osd_down_out_interval данные начнут уезжать (~%s)", h, dur(r.RecoveryTime)), "после возврата хоста данные поедут обратно", "лучше вариант с noout: --noout"}
	case ActDrain:
		p := []string{bf}
		for _, x := range r.Hosts {
			p = append(p, fmt.Sprintf("ceph orch host drain %s", x))
		}
		return append(p, fmt.Sprintf("ждать, пока `ceph orch osd rm status` опустеет (~%s)", dur(r.RecoveryTime)), fmt.Sprintf("ceph orch host rm %s", h))
	case ActExpand:
		p := []string{bf, "ceph osd set norebalance   # чтобы данные не поехали, пока OSD создаются по одному"}
		for _, x := range sc.Add {
			p = append(p, fmt.Sprintf("ceph orch host add %s && ceph orch daemon add osd %s:<устройства>   # %d OSD, класс %s", x.Name, x.Name, x.OSDs, x.Class))
		}
		return append(p, "ceph osd unset norebalance", fmt.Sprintf("ждать HEALTH_OK (~%s); затем ceph balancer status", dur(r.RecoveryTime)))
	case ActRule:
		p := []string{bf}
		if sp := sc.NewRule; sp != nil {
			switch sp.Mode {
			case "replicated":
				p = append(p, fmt.Sprintf("ceph osd crush rule create-replicated %s %s %s %s", sp.Name, sp.Root, sp.FailureDomain, sp.Class))
			case "erasure":
				p = append(p, fmt.Sprintf("ceph osd erasure-code-profile set <профиль> k=.. m=.. crush-failure-domain=%s crush-device-class=%s && ceph osd crush rule create-erasure %s <профиль>", sp.FailureDomain, sp.Class, sp.Name))
			case "msr":
				p = append(p, fmt.Sprintf("ceph osd erasure-code-profile set <профиль> k=.. m=.. crush-failure-domain=%s crush-osds-per-failure-domain=%d crush-num-failure-domains=%d && ceph osd crush rule create-erasure %s <профиль>", sp.FailureDomain, sp.OSDsPerDomain, sp.Width/max(sp.OSDsPerDomain, 1), sp.Name))
			}
		}
		rule := sc.RuleName
		if sc.NewRule != nil {
			rule = sc.NewRule.Name
		}
		return append(p, fmt.Sprintf("ceph osd pool set %s crush_rule %s", sc.Pool, rule), fmt.Sprintf("ждать HEALTH_OK (~%s)", dur(r.RecoveryTime)))
	case ActPGNum:
		return []string{bf,
			fmt.Sprintf("ceph osd pool set %s pg_autoscale_mode off   # иначе autoscaler вернёт своё значение", sc.Pool),
			fmt.Sprintf("ceph osd pool set %s pg_num %d   # mgr сам поднимает pgp_num шагами, держа misplaced ≤ target_max_misplaced_ratio (5%%)", sc.Pool, sc.PGNum),
			fmt.Sprintf("ждать, пока pgp_num дойдёт до %d и HEALTH_OK (~%s)", sc.PGNum, dur(r.RecoveryTime))}
	default:
		p := []string{}
		if r.Inactive+r.Incomplete > 0 {
			p = append(p, "правила CRUSH не переживают этот отказ: поднять failure domain пулов из таблицы до уровня цели (например, rack) или добавить стоек")
		}
		if r.BackfillfullAfter+r.FullAfter > 0 || r.NearfullAfter > r.NearfullBefore {
			p = append(p, "добавить ёмкость или освободить место, чтобы после recovery остаться ниже nearfull")
		}
		if len(p) == 0 {
			p = append(p, "кластер переживает этот отказ; держать osd_max_backfills на "+fmt.Sprint(sc.Backfills))
		}
		return p
	}
}

// Variants: те же данные, другие backfills и noout. Remap не пересчитывается.
func Variants(s *Snapshot, c *Core, sc Scenario) []Variant {
	type v struct {
		name  string
		noout bool
		b     int
	}
	var list []v
	if sc.Action == ActMaintenance {
		list = []v{{"с noout", true, 2}, {"без noout, backfills 2", false, 2}, {"без noout, backfills 6", false, 6}}
	} else {
		list = []v{{"бережно, backfills 1", false, 1}, {"баланс, backfills 3", false, 3}, {"быстро, backfills 6", false, 6}}
	}
	var out []Variant
	best, bestScore := -1, math.Inf(1)
	for i, x := range list {
		q := sc
		q.NoOut, q.Backfills = x.noout, x.b
		r := Finalize(s, c, q)
		moved := r.MovedBytes
		if q.Action == ActMaintenance && !q.NoOut {
			moved *= 2
		}
		t := r.RecoveryTime
		if q.NoOut {
			t = r.CatchupTime
		}
		out = append(out, Variant{Name: x.name, NoOut: x.noout, Backfills: x.b, Moved: moved, Time: t, Stalled: r.Stalled, Verdict: r.Verdict})
		// Сначала итог, потом меньше переезда, потом быстрее, потом бережнее к клиентам.
		score := float64(r.Verdict)*1e9 + float64(moved)/1e12*10 + t.Hours() + float64(x.b)*0.5
		if score < bestScore {
			best, bestScore = i, score
		}
	}
	if best >= 0 {
		out[best].Best = true
	}
	return out
}
