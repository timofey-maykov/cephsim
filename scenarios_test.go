package main

import (
	"math"
	"testing"
	"time"
)

func TestSplitShares(t *testing.T) {
	// 8 → 16: каждая PG делится пополам на ps и ps+8.
	s := splitShares(8, 16)
	for o := uint32(0); o < 8; o++ {
		if len(s[o]) != 2 || s[o][o] != 0.5 || s[o][o+8] != 0.5 {
			t.Fatalf("8→16, PG %d: %v", o, s[o])
		}
	}
	// 6 → 10 (не степени двойки): доли каждой старой PG в сумме 1, новые PG покрыты все.
	s = splitShares(6, 10)
	seen := map[uint32]bool{}
	for o, m := range s {
		sum := 0.0
		for n, v := range m {
			sum += v
			seen[n] = true
			if stableMod(n, 6, 7) != o {
				t.Fatalf("6→10: новая PG %d не потомок %d", n, o)
			}
		}
		if math.Abs(sum-1) > 1e-9 {
			t.Fatalf("6→10, PG %d: сумма долей %v", o, sum)
		}
	}
	if len(seen) != 10 {
		t.Fatalf("6→10: покрыто %d новых PG", len(seen))
	}
	// 16 → 8: слияние, каждая старая PG целиком уходит в одну.
	for o, m := range splitShares(16, 8) {
		if len(m) != 1 || m[o%8] != 1 {
			t.Fatalf("16→8, PG %d: %v", o, m)
		}
	}
}

func synth(t *testing.T) *Snapshot {
	s, err := LoadSnapshot("testdata/snap-synthetic")
	if err != nil {
		t.Skip(err)
	}
	return s
}

func baseScenario(action string) Scenario {
	return Scenario{Action: action, Backfills: 2, Window: time.Hour, SlotMBps: map[string]float64{"hdd": 40}, MaxPGPerOSD: 250}
}

func TestExpandFillsNewHosts(t *testing.T) {
	s := synth(t)
	hs, err := parseHosts("host=host-13,count=2,rack=rack2,osds=12,size=16T,weight=1", s.Placement.Crush.Types)
	if err != nil {
		t.Fatal(err)
	}
	sc := baseScenario(ActExpand)
	sc.Add = hs
	c, err := Analyze(s, sc, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Added) != 24 || c.Added[0] != 144 || c.Degraded != 0 {
		t.Fatalf("добавлено %v, degraded %d", c.Added, c.Degraded)
	}
	// Новые OSD с тем же весом, что старые, заполняются примерно как старые после.
	var newIn int64
	var newFill, oldFill float64
	for id, p := range c.OSDProj {
		if id >= 144 {
			newIn += p.In
			newFill += p.After / 24
		} else {
			oldFill += p.After / 144
		}
	}
	if newIn == 0 || c.MovedBytes < newIn || math.Abs(newFill-oldFill) > 0.15*oldFill {
		t.Fatalf("на новые OSD приехало %d из %d, заполненность новых %.3f, старых %.3f", newIn, c.MovedBytes, newFill, oldFill)
	}
	if r := Finalize(s, c, sc); r.Verdict == LevelBad || len(r.Plan) == 0 {
		t.Fatalf("вердикт %v, план %v", r.Verdict, r.Plan)
	}
	if hs[0].Loc[1].Name != "rack2" || c.OSDProj[144].Host != "host-13-01" || c.OSDProj[144].Rack != "rack2" {
		t.Fatalf("расположение: %+v %+v", hs[0].Loc, c.OSDProj[144])
	}
}

func TestRuleChangeToRack(t *testing.T) {
	s := synth(t)
	pool := s.Pools[1]
	sp, err := parseRuleSpec("replicated,fd=rack,root=root", pool)
	if err != nil {
		t.Fatal(err)
	}
	sc := baseScenario(ActRule)
	sc.Pool, sc.NewRule = "rbd", &sp
	c, err := Analyze(s, sc, true)
	if err != nil {
		t.Fatal(err)
	}
	// Три стойки на size 3: всё размещается, но почти каждая PG переезжает хотя бы одной копией.
	if c.Undersized != 0 || c.MovedPGs < pool.PGNum/2 {
		t.Fatalf("undersized %d, moved %d", c.Undersized, c.MovedPGs)
	}
	sp.FailureDomain = "root"
	sp.Name = "bad"
	sc.NewRule = &sp
	if c, err = Analyze(s, sc, true); err != nil || c.Undersized != pool.PGNum {
		t.Fatalf("failure domain root для size 3: undersized %d, err %v", c.Undersized, err)
	}
}

func TestPGNumKeepsBytes(t *testing.T) {
	s := synth(t)
	sc := baseScenario(ActPGNum)
	sc.Pool, sc.PGNum = "rbd", 9216
	c, err := Analyze(s, sc, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.MovedBytes == 0 || c.MovedPGs == 0 || c.PGPerOSD.AfterAvg <= c.PGPerOSD.BeforeAvg*1.9 {
		t.Fatalf("moved %d PG %d, PG на OSD %+v", c.MovedBytes, c.MovedPGs, c.PGPerOSD)
	}
	// Данных после не больше, чем до: переезд - не больше трёх копий пула.
	var total int64
	for _, pg := range s.PGs {
		total += pg.StatSum.NumBytes
	}
	if c.MovedBytes > 3*total {
		t.Fatalf("переезд %d больше трёх копий пула %d", c.MovedBytes, total)
	}
	r := Finalize(s, c, sc)
	if r.PGPerOSD.AfterAvg > 250 && r.Verdict != LevelBad {
		t.Fatalf("PG на OSD %.0f > 250, а вердикт %v", r.PGPerOSD.AfterAvg, r.Verdict)
	}
}

// Две стойки по два хоста, пул size 2 с failure domain rack: оба хоста стойки можно выключить вместе.
func TestRollingBatchesByRack(t *testing.T) {
	s := &Snapshot{Tree: map[int]TreeNode{}, ByName: map[string]TreeNode{}, Parent: map[int]int{}, DF: map[int]DFNode{}, Pools: map[int]Pool{}}
	add := func(n TreeNode) {
		s.Tree[n.ID] = n
		s.ByName[n.Name] = n
		for _, c := range n.Children {
			s.Parent[c] = n.ID
		}
	}
	add(TreeNode{ID: -1, Name: "root", Type: "root", Children: []int{-2, -3}})
	add(TreeNode{ID: -2, Name: "r1", Type: "rack", Children: []int{-4, -5}})
	add(TreeNode{ID: -3, Name: "r2", Type: "rack", Children: []int{-6, -7}})
	for i, h := range []string{"a", "b", "c", "d"} {
		add(TreeNode{ID: -4 - i, Name: h, Type: "host", Children: []int{i}})
		add(TreeNode{ID: i, Name: osdName(i), Type: "osd"})
		s.DF[i] = DFNode{ID: i, KB: 1 << 30, KBUsed: 1 << 28}
	}
	s.Pools[1] = Pool{ID: 1, Name: "p", Type: 1, Size: 2, MinSize: 1}
	for i, set := range [][]int{{0, 2}, {1, 3}, {0, 3}, {1, 2}} {
		pg := PGStat{PGID: "1." + string(rune('0'+i)), Acting: set, Up: set}
		s.PGs = append(s.PGs, pg)
	}
	sc := baseScenario(ActRolling)
	r, err := Rolling(s, sc, 0, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Batches) != 2 || len(r.Batches[0].Hosts) != 2 || r.Batches[0].Hosts[0] != "a" || r.Batches[1].Hosts[1] != "d" {
		t.Fatalf("партии %+v", r.Batches)
	}
	if r.Total != 2*time.Hour+10*time.Minute {
		t.Fatalf("всего %v", r.Total)
	}
	if r, _ := Rolling(s, sc, 1, 0); len(r.Batches) != 4 {
		t.Fatalf("parallel 1: %d партий", len(r.Batches))
	}
}

func TestMSRRuleSpec(t *testing.T) {
	ec := Pool{Name: "ec", Type: poolErasure, Size: 6, K: 4}
	sp, err := parseRuleSpec("msr,fd=host,per=2,class=hdd", ec)
	if err != nil || sp.Width != 6 || sp.OSDsPerDomain != 2 || sp.Name != "ec_host_hdd" {
		t.Fatalf("%+v %v", sp, err)
	}
	if _, err := parseRuleSpec("msr,fd=host,per=2", Pool{Name: "r", Type: 1, Size: 3}); err == nil {
		t.Fatal("msr для replicated пула должно отклоняться")
	}
}
