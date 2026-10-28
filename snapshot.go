package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Снимок кластера: файлы, которые пишет `cephsim collect`.
const (
	fOSDMap     = "osdmap.bin"
	fOSDDump    = "osd_dump.json"
	fOSDTree    = "osd_tree.json"
	fOSDDF      = "osd_df.json"
	fPGLs       = "pg_ls.json"
	fStatus     = "status.json"
	fECProfiles = "ec_profiles.json"
	fMeta       = "meta.json"
)

const poolErasure = 3

type Pool struct {
	ID        int    `json:"pool"`
	Name      string `json:"pool_name"`
	Type      int    `json:"type"`
	Size      int    `json:"size"`
	MinSize   int    `json:"min_size"`
	CrushRule int    `json:"crush_rule"`
	ECProfile string `json:"erasure_code_profile"`
	PGNum     int    `json:"pg_num"`
	PGPNum    int    `json:"pg_placement_num"`
	Flags     int    `json:"flags"`
	K         int    `json:"-"`
}

func (p Pool) EC() bool { return p.Type == poolErasure }

func (p Pool) Kind() string {
	if p.EC() {
		return fmt.Sprintf("EC %d+%d", p.K, p.Size-p.K)
	}
	return fmt.Sprintf("rep %d", p.Size)
}

// shardFactor: какая доля логических байт PG лежит на одном OSD.
func (p Pool) shardFactor() float64 {
	if p.EC() && p.K > 0 {
		return 1 / float64(p.K)
	}
	return 1
}

type OSDDump struct {
	Epoch             int     `json:"epoch"`
	Flags             string  `json:"flags"`
	FullRatio         float64 `json:"full_ratio"`
	BackfillFullRatio float64 `json:"backfillfull_ratio"`
	NearFullRatio     float64 `json:"nearfull_ratio"`
	Pools             []Pool  `json:"pools"`
	OSDs              []struct {
		OSD int `json:"osd"`
	} `json:"osds"`
}

type TreeNode struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Children    []int  `json:"children"`
	DeviceClass string `json:"device_class"`
}

type DFNode struct {
	ID          int     `json:"id"`
	Name        string  `json:"name"`
	DeviceClass string  `json:"device_class"`
	KB          int64   `json:"kb"`
	KBUsed      int64   `json:"kb_used"`
	CrushWeight float64 `json:"crush_weight"`
	Reweight    float64 `json:"reweight"`
}

type PGStat struct {
	PGID    string `json:"pgid"`
	State   string `json:"state"`
	Up      []int  `json:"up"`
	Acting  []int  `json:"acting"`
	StatSum struct {
		NumBytes   int64 `json:"num_bytes"`
		NumObjects int64 `json:"num_objects"`
	} `json:"stat_sum"`
}

func (pg PGStat) PoolID() int {
	id, _ := strconv.Atoi(strings.SplitN(pg.PGID, ".", 2)[0])
	return id
}

type Meta struct {
	CollectedAt   string   `json:"collected_at"`
	Cluster       string   `json:"cluster"`
	Synthetic     bool     `json:"synthetic"`
	OSDMapToolArg []string `json:"osdmaptool_args"`
	Backfills     int      `json:"osd_max_backfills"` // действующий на OSD, см. ceph config show
	MClockProfile string   `json:"osd_mclock_profile"`
	MClock        *MClock  `json:"mclock,omitempty"`
	MaxPGPerOSD   int      `json:"mon_max_pg_per_osd,omitempty"`
	// Ceph не выводит в out OSD целого отказавшего бакета этого типа и выше
	DownOutSubtreeLimit string  `json:"mon_osd_down_out_subtree_limit,omitempty"`
	MinInRatio          float64 `json:"mon_osd_min_in_ratio,omitempty"`
}

type Snapshot struct {
	Dir           string
	Meta          Meta
	Dump          OSDDump
	Tree          map[int]TreeNode
	ByName        map[string]TreeNode
	Parent        map[int]int
	DF            map[int]DFNode
	PGs           []PGStat
	Pools         map[int]Pool
	WriteBytesSec float64
	Placement     *Placement
	baseMap       Mapping // раскладка снимка, считается один раз
	Calibration   *Calibration
}

func readJSON(dir, name string, v any) error {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func LoadSnapshot(dir string) (*Snapshot, error) {
	s := &Snapshot{Dir: dir, Tree: map[int]TreeNode{}, ByName: map[string]TreeNode{}, Parent: map[int]int{}, DF: map[int]DFNode{}, Pools: map[int]Pool{}}
	if _, err := os.Stat(filepath.Join(dir, fOSDMap)); err != nil {
		return nil, fmt.Errorf("нет %s в снимке: %w", fOSDMap, err)
	}
	_ = readJSON(dir, fMeta, &s.Meta)
	if err := readJSON(dir, fOSDDump, &s.Dump); err != nil {
		return nil, err
	}
	if s.Dump.FullRatio == 0 {
		s.Dump.FullRatio = 0.95
	}
	if s.Dump.BackfillFullRatio == 0 {
		s.Dump.BackfillFullRatio = 0.90
	}
	if s.Dump.NearFullRatio == 0 {
		s.Dump.NearFullRatio = 0.85
	}

	var tree struct {
		Nodes []TreeNode `json:"nodes"`
	}
	if err := readJSON(dir, fOSDTree, &tree); err != nil {
		return nil, err
	}
	for _, n := range tree.Nodes {
		s.Tree[n.ID] = n
		s.ByName[n.Name] = n
		for _, c := range n.Children {
			s.Parent[c] = n.ID
		}
	}

	var df struct {
		Nodes []DFNode `json:"nodes"`
	}
	if err := readJSON(dir, fOSDDF, &df); err != nil {
		return nil, err
	}
	for _, n := range df.Nodes {
		s.DF[n.ID] = n
	}

	// `ceph pg ls -f json` до Quincy отдаёт массив, начиная с Quincy - объект с pg_stats.
	raw, err := os.ReadFile(filepath.Join(dir, fPGLs))
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		PGStats []PGStat `json:"pg_stats"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.PGStats != nil {
		s.PGs = wrapped.PGStats
	} else if err := json.Unmarshal(raw, &s.PGs); err != nil {
		return nil, fmt.Errorf("%s: %w", fPGLs, err)
	}

	var status struct {
		PGMap struct {
			WriteBytesSec float64 `json:"write_bytes_sec"`
		} `json:"pgmap"`
	}
	if readJSON(dir, fStatus, &status) == nil {
		s.WriteBytesSec = status.PGMap.WriteBytesSec
	}

	profiles := map[string]map[string]string{}
	_ = readJSON(dir, fECProfiles, &profiles)
	for _, p := range s.Dump.Pools {
		if p.EC() {
			if k, err := strconv.Atoi(profiles[p.ECProfile]["k"]); err == nil {
				p.K = k
			} else {
				p.K = p.Size - 2
				fmt.Fprintf(os.Stderr, "предупреждение: нет профиля EC %q для пула %s, считаю k=%d\n", p.ECProfile, p.Name, p.K)
			}
		}
		s.Pools[p.ID] = p
	}
	if s.Placement, err = LoadPlacement(dir); err != nil {
		return nil, fmt.Errorf("раскладка: %w (снимок старой версии? пересоберите через cephsim collect)", err)
	}
	return s, nil
}

// Ancestor возвращает имя предка OSD нужного типа (host, rack, ...).
func (s *Snapshot) Ancestor(osd int, typ string) string {
	id := osd
	for {
		p, ok := s.Parent[id]
		if !ok {
			return "-"
		}
		if s.Tree[p].Type == typ {
			return s.Tree[p].Name
		}
		id = p
	}
}

// ResolveOSDs раскрывает имена из CRUSH-дерева (osd.N, хост, стойка) в список OSD.
func (s *Snapshot) ResolveOSDs(names []string) ([]int, error) {
	seen := map[int]bool{}
	var out []int
	var walk func(id int)
	walk = func(id int) {
		if id >= 0 {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
			return
		}
		for _, c := range s.Tree[id].Children {
			walk(c)
		}
	}
	for _, n := range names {
		node, ok := s.ByName[n]
		if !ok {
			return nil, fmt.Errorf("в CRUSH-дереве нет %q", n)
		}
		walk(node.ID)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("под %v нет ни одного OSD", names)
	}
	return out, nil
}

func (s *Snapshot) Class(osd int) string {
	if c := s.DF[osd].DeviceClass; c != "" {
		return c
	}
	if c := s.Tree[osd].DeviceClass; c != "" {
		return c
	}
	return "hdd"
}

// baseMapping - раскладка PG снимка без изменений. Кешируется: сводный отчёт
// считает десятки сценариев на одном снимке.
func (s *Snapshot) baseMapping() Mapping {
	if s.baseMap == nil {
		s.baseMap = s.Placement.MapAll(s.Pools)
	}
	return s.baseMap
}
