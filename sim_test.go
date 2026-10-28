package main

import (
	"strings"
	"testing"
	"time"
)

// Два хоста по три OSD, EC 2+2 (min_size 3) и rep 3 (min_size 2).
func testSnapshot() *Snapshot {
	s := &Snapshot{Tree: map[int]TreeNode{}, ByName: map[string]TreeNode{}, Parent: map[int]int{}, DF: map[int]DFNode{}, Pools: map[int]Pool{}}
	add := func(n TreeNode) {
		s.Tree[n.ID] = n
		s.ByName[n.Name] = n
		for _, c := range n.Children {
			s.Parent[c] = n.ID
		}
	}
	add(TreeNode{ID: -1, Name: "root", Type: "root", Children: []int{-2, -3}})
	add(TreeNode{ID: -2, Name: "h1", Type: "host", Children: []int{0, 1, 2}})
	add(TreeNode{ID: -3, Name: "h2", Type: "host", Children: []int{3, 4, 5}})
	for i := 0; i < 6; i++ {
		add(TreeNode{ID: i, Name: osdName(i), Type: "osd"})
		s.DF[i] = DFNode{ID: i, KB: 1 << 30, KBUsed: 1 << 29}
	}
	s.Pools[1] = Pool{ID: 1, Name: "ec", Type: poolErasure, Size: 4, MinSize: 3, K: 2}
	s.Pools[2] = Pool{ID: 2, Name: "rep", Type: 1, Size: 3, MinSize: 2}
	pg := func(id string, set ...int) PGStat {
		p := PGStat{PGID: id, Acting: set, Up: set}
		p.StatSum.NumBytes = 1 << 30
		return p
	}
	s.PGs = []PGStat{
		pg("1.0", 0, 1, 3, 4), // теряет 2 шарда: 2 = k, но < min_size → inactive
		pg("1.1", 0, 1, 2, 3), // теряет 3: остался 1 < k → incomplete
		pg("1.2", 0, 3, 4, 5), // теряет 1: 3 = min_size → без запаса
		pg("2.0", 0, 1, 3),    // остался 1 < min_size → inactive
		pg("2.1", 3, 4, 5),    // не затронута
	}
	return s
}

func TestAvailabilityHostDown(t *testing.T) {
	s := testSnapshot()
	sc := Scenario{Action: ActFailure, Targets: []string{"h1"}, Backfills: 2, Window: time.Hour, SlotMBps: map[string]float64{"hdd": 40}}
	c, err := Analyze(s, sc, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Degraded != 4 || c.Inactive != 2 || c.Incomplete != 1 || c.AtRisk != 1 {
		t.Fatalf("degraded=%d inactive=%d incomplete=%d atrisk=%d", c.Degraded, c.Inactive, c.Incomplete, c.AtRisk)
	}
	if r := Finalize(s, c, sc); r.Verdict != LevelBad {
		t.Fatalf("verdict %v, ожидаю bad", r.Verdict)
	}
}

func TestDrainKeepsAvailability(t *testing.T) {
	s := testSnapshot()
	sc := Scenario{Action: ActDrain, Targets: []string{"osd.0"}, Backfills: 2, SlotMBps: map[string]float64{"hdd": 40}}
	c, err := Analyze(s, sc, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Degraded+c.Inactive+c.Incomplete != 0 {
		t.Fatalf("drain не должен деградировать PG: %+v", c)
	}
}

func TestParseMapping(t *testing.T) {
	m, err := parseMapping(strings.NewReader("pool 1 pg_num 2\n1.0\t[3,2147483647,5]\t3\n1.1\t[]\t-1\nosd.3\t1\t1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m["1.0"]) != 3 || m["1.0"][1] != crushNone || len(m["1.1"]) != 0 || len(m) != 2 {
		t.Fatalf("%v", m)
	}
	// -dump-all: берём up, а не acting с pg_temp.
	m, err = parseMapping(strings.NewReader("428.2 raw ([420,325], p420) up ([420,325], p420) acting ([420], p420)\n"))
	if err != nil || len(m["428.2"]) != 2 || m["428.2"][1] != 325 {
		t.Fatalf("%v %v", m, err)
	}
}
