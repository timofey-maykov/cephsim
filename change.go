package main

import (
	"fmt"
	"math/bits"
	"sort"
	"strconv"
	"strings"

	"github.com/timofey-maykov/cephsim/crush"
)

// HostSpec - новый хост для -action expand.
type HostSpec struct {
	Name  string      `json:"name"`
	Loc   []crush.Loc `json:"location"` // от хоста вверх: host, rack, ..., root
	OSDs  int         `json:"osds"`
	Size  int64       `json:"osd_size_bytes"`
	Class string      `json:"class"`
	// CRUSH-вес OSD; 0 - размер в TiB, как при osd_crush_initial_weight=-1.
	Weight float64 `json:"crush_weight,omitempty"`
}

// parseHosts: host=new-01,rack=rack2,osds=20,size=16T,class=hdd[,count=3][,weight=14.55].
// При count>1 имена host-01, host-02, ... от заданного префикса.
func parseHosts(spec string, types map[string]int) ([]HostSpec, error) {
	h := HostSpec{Class: "hdd"}
	count := 1
	var name string
	var loc []crush.Loc
	for _, kv := range strings.Split(spec, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			return nil, fmt.Errorf("-add: ожидаю ключ=значение, получил %q", kv)
		}
		switch k {
		case "host":
			name = v
		case "osds":
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("-add: osds=%q", v)
			}
			h.OSDs = n
		case "size":
			b, err := parseSize(v)
			if err != nil {
				return nil, fmt.Errorf("-add: size=%q: %w", v, err)
			}
			h.Size = b
		case "class":
			h.Class = v
		case "weight":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f <= 0 {
				return nil, fmt.Errorf("-add: weight=%q", v)
			}
			h.Weight = f
		case "count":
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("-add: count=%q", v)
			}
			count = n
		default:
			if _, ok := types[k]; !ok {
				return nil, fmt.Errorf("-add: в CRUSH нет типа %q", k)
			}
			loc = append(loc, crush.Loc{Type: k, Name: v})
		}
	}
	if name == "" || h.OSDs == 0 || h.Size == 0 {
		return nil, fmt.Errorf("-add: нужны host=, osds= и size=")
	}
	// Уровни снизу вверх, как insert_item проходит type_map.
	sort.SliceStable(loc, func(i, j int) bool { return types[loc[i].Type] < types[loc[j].Type] })
	var out []HostSpec
	for i := 1; i <= count; i++ {
		x := h
		x.Name = name
		if count > 1 {
			x.Name = fmt.Sprintf("%s-%02d", name, i)
		}
		x.Loc = append([]crush.Loc{{Type: "host", Name: x.Name}}, loc...)
		out = append(out, x)
	}
	return out, nil
}

func parseSize(v string) (int64, error) {
	units := []struct {
		suf string
		mul float64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"TB", 1e12}, {"GB", 1e9}, {"T", 1e12}, {"G", 1e9}}
	for _, u := range units {
		if n, ok := strings.CutSuffix(v, u.suf); ok {
			f, err := strconv.ParseFloat(n, 64)
			if err != nil || f <= 0 {
				return 0, fmt.Errorf("неверный размер")
			}
			return int64(f * u.mul), nil
		}
	}
	return 0, fmt.Errorf("нужна единица: T, TB, TiB, G, GB, GiB")
}

// parseRuleSpec: replicated,fd=rack,class=hdd[,root=default] | erasure,... | msr,fd=host,per=2.
func parseRuleSpec(spec string, pool Pool) (crush.RuleSpec, error) {
	f := strings.Split(spec, ",")
	sp := crush.RuleSpec{Mode: strings.TrimSpace(f[0]), Root: "default", FailureDomain: "host", Width: pool.Size}
	for _, kv := range f[1:] {
		k, v, _ := strings.Cut(strings.TrimSpace(kv), "=")
		switch k {
		case "fd", "failure-domain":
			sp.FailureDomain = v
		case "class":
			sp.Class = v
		case "root":
			sp.Root = v
		case "per", "osds-per-fd":
			n, err := strconv.Atoi(v)
			if err != nil {
				return sp, fmt.Errorf("-new-rule: per=%q", v)
			}
			sp.OSDsPerDomain = n
		case "name":
			sp.Name = v
		default:
			return sp, fmt.Errorf("-new-rule: неизвестный ключ %q", k)
		}
	}
	switch {
	case sp.Mode == "replicated" && pool.EC(), sp.Mode != "replicated" && !pool.EC():
		return sp, fmt.Errorf("-new-rule: правило %s не подходит пулу %s (%s)", sp.Mode, pool.Name, pool.Kind())
	}
	if sp.Name == "" {
		sp.Name = fmt.Sprintf("%s_%s", pool.Name, sp.FailureDomain)
		if sp.Class != "" {
			sp.Name += "_" + sp.Class
		}
	}
	return sp, nil
}

// change - снимок после изменения и что меняется: раскладка, пулы, выключаемые OSD.
type change struct {
	s       *Snapshot
	after   *Placement
	pools   map[int]Pool
	down    map[int]bool // выключаемые (maintenance, failure) или выводимые (drain) OSD
	degrade bool         // есть потеря копий: считаем доступность по acting-set
	added   []int
	note    string
}

func (s *Snapshot) poolByName(name string) (Pool, error) {
	for _, p := range s.Pools {
		if p.Name == name || fmt.Sprint(p.ID) == name {
			return p, nil
		}
	}
	return Pool{}, fmt.Errorf("нет пула %q", name)
}

func prepare(s *Snapshot, sc Scenario, osds []int) (*change, error) {
	ch := &change{s: s, pools: s.Pools, down: map[int]bool{}}
	for _, o := range osds {
		ch.down[o] = true
	}
	if s.Placement == nil {
		// Снимок без раскладки (тесты доступности): считаем только потерю копий.
		ch.degrade = degrading(sc.Action)
		return ch, nil
	}
	ch.after = s.Placement.Clone()
	switch sc.Action {
	case ActMaintenance, ActFailure:
		ch.degrade = true
		ch.after.MarkOut(osds, true)
	case ActDrain:
		ch.after.CrushZero(osds)
	case ActExpand:
		return ch, ch.expand(sc.Add)
	case ActRule:
		pool, err := s.poolByName(sc.Pool)
		if err != nil {
			return nil, err
		}
		id := -1
		if sc.NewRule != nil {
			if id, err = ch.after.Crush.AddRule(*sc.NewRule); err != nil {
				return nil, err
			}
		} else if r, ok := ch.after.Crush.RuleByName(sc.RuleName); ok {
			id = r
			// replicated - тип 1 (msr_firstn 4), erasure - 3 (msr_indep 5)
			if t := ch.after.Crush.Rules[r].Type; pool.EC() != (t == 3 || t == crush.RuleTypeMSRIndep) {
				return nil, fmt.Errorf("правило %s (тип %d) не подходит пулу %s (%s)", sc.RuleName, t, pool.Name, pool.Kind())
			}
		} else {
			return nil, fmt.Errorf("нет CRUSH-правила %q", sc.RuleName)
		}
		if id == pool.CrushRule {
			return nil, fmt.Errorf("пул %s уже на правиле %s", pool.Name, sc.RuleName)
		}
		ch.note = fmt.Sprintf("пул %s: правило %s → %s (%s)", pool.Name, s.Placement.Crush.Rules[pool.CrushRule].Name,
			ch.after.Crush.Rules[id].Name, ch.after.Crush.Describe(id))
		pool.CrushRule = id
		ch.setPool(pool)
	case ActPGNum:
		pool, err := s.poolByName(sc.Pool)
		if err != nil {
			return nil, err
		}
		if sc.PGNum <= 0 || sc.PGNum == pool.PGNum {
			return nil, fmt.Errorf("-pg-num: нужно значение, отличное от текущего %d", pool.PGNum)
		}
		ch.note = fmt.Sprintf("пул %s: pg_num %d → %d", pool.Name, pool.PGNum, sc.PGNum)
		pool.PGNum, pool.PGPNum = sc.PGNum, sc.PGNum
		ch.setPool(pool)
	}
	return ch, nil
}

func (ch *change) setPool(p Pool) {
	np := map[int]Pool{}
	for k, v := range ch.pools {
		np[k] = v
	}
	np[p.ID] = p
	ch.pools = np
}

// expand добавляет хосты как `ceph orch` + create-or-move: OSD получают
// наименьшие свободные id и CRUSH-вес, равный размеру в TiB.
func (ch *change) expand(hosts []HostSpec) error {
	s := *ch.s
	s.Tree, s.ByName, s.Parent, s.DF = copyMap(s.Tree), copyMap(s.ByName), copyMap(s.Parent), copyMap(s.DF)
	ch.s = &s
	p := ch.after
	next := 0
	nodeID := func(name string) int {
		if b := p.Crush.BucketByName(name); b != nil {
			return b.ID
		}
		return 0
	}
	for _, h := range hosts {
		if _, ok := s.ByName[h.Name]; ok || nodeID(h.Name) != 0 {
			return fmt.Errorf("хост %s уже есть в CRUSH", h.Name)
		}
		tib := h.Weight
		if tib == 0 {
			tib = float64(h.Size) / (1 << 40)
		}
		w := uint32(tib * 0x10000)
		for i := 0; i < h.OSDs; i++ {
			for next < len(p.Exists) && p.Exists[next] {
				next++
			}
			id := next
			for len(p.Exists) <= id {
				p.Exists, p.Up, p.Weight = append(p.Exists, false), append(p.Up, false), append(p.Weight, 0)
			}
			p.Exists[id], p.Up[id], p.Weight[id] = true, true, 0x10000
			if err := p.Crush.InsertDevice(id, w, h.Class, h.Loc); err != nil {
				return err
			}
			ch.added = append(ch.added, id)
			s.DF[id] = DFNode{ID: id, Name: osdName(id), DeviceClass: h.Class, KB: h.Size / 1024, CrushWeight: float64(w) / 0x10000, Reweight: 1}
			s.Tree[id] = TreeNode{ID: id, Name: osdName(id), Type: "osd", DeviceClass: h.Class}
			s.ByName[osdName(id)] = s.Tree[id]
			// Дерево для Ancestor: новые бакеты и их родители.
			child := id
			for _, l := range h.Loc {
				bid := nodeID(l.Name)
				n, ok := s.ByName[l.Name]
				if !ok {
					n = TreeNode{ID: bid, Name: l.Name, Type: l.Type}
				}
				if !containsInt(n.Children, child) {
					n.Children = append(n.Children, child)
				}
				s.Tree[n.ID], s.ByName[n.Name] = n, n
				s.Parent[child] = n.ID
				if ok {
					break
				}
				child = n.ID
			}
		}
	}
	var size int64
	for _, h := range hosts {
		size += h.Size * int64(h.OSDs)
	}
	ch.note = fmt.Sprintf("+%d хост(ов), %d OSD, %s сырой ёмкости", len(hosts), len(ch.added), bytesH(size))
	return nil
}

func copyMap[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func containsInt(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// splitShares: при смене pg_num объект с хешем h попадает в старую PG
// stable_mod(h, old) и новую stable_mod(h, new). Распределение зависит только
// от младших бит хеша, поэтому доли считаются точно перебором 2^bits значений.
// Результат: [старая ps][новая ps] -> доля объектов старой PG.
func splitShares(oldN, newN int) map[uint32]map[uint32]float64 {
	b := bits.Len(uint(max(oldN, newN) - 1))
	om := uint32(1)<<bits.Len(uint(oldN-1)) - 1
	nm := uint32(1)<<bits.Len(uint(newN-1)) - 1
	cnt := map[uint32]map[uint32]int{}
	tot := map[uint32]int{}
	for v := uint32(0); v < 1<<b; v++ {
		o, n := stableMod(v, uint32(oldN), om), stableMod(v, uint32(newN), nm)
		if cnt[o] == nil {
			cnt[o] = map[uint32]int{}
		}
		cnt[o][n]++
		tot[o]++
	}
	out := map[uint32]map[uint32]float64{}
	for o, m := range cnt {
		out[o] = map[uint32]float64{}
		for n, c := range m {
			out[o][n] = float64(c) / float64(tot[o])
		}
	}
	return out
}
