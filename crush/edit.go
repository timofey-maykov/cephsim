package crush

import (
	"fmt"
	"sort"
	"strings"
)

// Изменения карты так, как их делает CrushWrapper: id новых бакетов, теневые
// деревья классов (name~class) и новые правила должны совпасть с тем, что
// получится в кластере, иначе CRUSH разложит PG по-другому.

func (m *Map) reindex() {
	m.parents = map[int][]int{}
	for i, b := range m.Buckets {
		if b == nil {
			continue
		}
		for _, it := range b.Items {
			m.parents[it] = append(m.parents[it], i)
		}
	}
}

// BucketByName - бакет по имени, включая теневые (host~hdd).
func (m *Map) BucketByName(name string) *Bucket {
	for _, b := range m.Buckets {
		if b != nil && b.Name == name {
			return b
		}
	}
	return nil
}

func (m *Map) TypeName(id int) string {
	for n, t := range m.Types {
		if t == id {
			return n
		}
	}
	return fmt.Sprint(id)
}

func shadow(name string) bool { return strings.Contains(name, "~") }

// freeBucketID - CrushWrapper::get_new_bucket_id: первый свободный индекс,
// не занятый и не числящийся за прежними теневыми бакетами.
func (m *Map) freeBucketID(used map[int]bool) int {
	for i := 0; ; i++ {
		if (i >= len(m.Buckets) || m.Buckets[i] == nil) && !used[-1-i] {
			return -1 - i
		}
	}
}

func (m *Map) putBucket(b *Bucket) {
	idx := -1 - b.ID
	for len(m.Buckets) <= idx {
		m.Buckets = append(m.Buckets, nil)
	}
	m.Buckets[idx] = b
}

// Loc - уровень расположения: тип CRUSH и имя бакета, от нижнего к верхнему.
type Loc struct{ Type, Name string }

// InsertDevice - CrushWrapper::insert_item: создаёт недостающие бакеты снизу
// вверх, кладёт OSD с весом weight (16.16) и перестраивает теневые деревья.
func (m *Map) InsertDevice(osd int, weight uint32, class string, loc []Loc) error {
	cur := osd
	attached := false
	for _, l := range loc {
		t, ok := m.Types[l.Type]
		if !ok {
			return fmt.Errorf("в CRUSH нет типа %q", l.Type)
		}
		if b := m.BucketByName(l.Name); b != nil {
			if b.Type != t {
				return fmt.Errorf("%s имеет тип %s, а не %s", l.Name, m.TypeName(b.Type), l.Type)
			}
			b.Items = append(b.Items, cur)
			b.Weights = append(b.Weights, 0)
			m.extendWeightSets(-1-b.ID, 0)
			attached = true
			break
		}
		nb := &Bucket{ID: m.freeBucketID(nil), Name: l.Name, Type: t, Alg: "straw2", Items: []int{cur}, Weights: []uint32{0}}
		m.putBucket(nb)
		cur = nb.ID
	}
	if !attached {
		return fmt.Errorf("OSD %d: ни один бакет из %v не существует, новый корень не поддерживается", osd, loc)
	}
	m.MaxDevices = max(m.MaxDevices, osd+1)
	if class != "" {
		m.DevClass[osd] = class
	}
	m.reindex()
	m.SetItemWeight(osd, weight)
	m.RebuildShadows()
	return nil
}

// Новый элемент в бакете с weight set: добавляем позицию с весом w.
func (m *Map) extendWeightSets(bi int, w uint32) {
	for _, args := range m.ChooseArgs {
		if a := args[bi]; a != nil {
			for i := range a.WeightSet {
				a.WeightSet[i] = append(a.WeightSet[i], w)
			}
			if len(a.IDs) > 0 {
				a.IDs = append(a.IDs, m.Buckets[bi].Items[len(m.Buckets[bi].Items)-1])
			}
		}
	}
}

// classOrder: id классов в CRUSH не выгружаются, порядок восстанавливаем по
// теневым бакетам: класс, созданный раньше, получал id раньше.
func (m *Map) classOrder() []string {
	first := map[string]int{}
	for i, b := range m.Buckets {
		if b == nil || !shadow(b.Name) {
			continue
		}
		c := b.Name[strings.LastIndex(b.Name, "~")+1:]
		if _, ok := first[c]; !ok {
			first[c] = i
		}
	}
	for _, c := range m.DevClass {
		if _, ok := first[c]; !ok {
			first[c] = 1 << 30
		}
	}
	var out []string
	for c := range first {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if first[out[i]] != first[out[j]] {
			return first[out[i]] < first[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// RebuildShadows - rebuild_roots_with_classes: удалить теневые бакеты и
// построить заново обходом в глубину, сохраняя прежние id.
func (m *Map) RebuildShadows() {
	classes := m.classOrder()
	old := map[string]int{}
	used := map[int]bool{}
	oldArgs := map[int]map[int]*ChooseArg{}
	for i, b := range m.Buckets {
		if b != nil && shadow(b.Name) {
			old[b.Name] = b.ID
			used[b.ID] = true
			m.Buckets[i] = nil
			for k, args := range m.ChooseArgs {
				if a := args[i]; a != nil {
					if oldArgs[k] == nil {
						oldArgs[k] = map[int]*ChooseArg{}
					}
					oldArgs[k][i] = a
					delete(args, i)
				}
			}
		}
	}
	m.reindex()
	var roots []int
	for _, b := range m.Buckets {
		if b != nil && len(m.parents[b.ID]) == 0 {
			roots = append(roots, b.ID)
		}
	}
	sort.Ints(roots)
	// веса weight set теневых бакетов, по позициям: [ключ choose_args][id] -> веса
	cw := map[int]map[int][]uint32{}
	var clone func(orig int, class string) int
	clone = func(orig int, class string) int {
		ob := m.bucket(orig)
		name := ob.Name + "~" + class
		if b := m.BucketByName(name); b != nil {
			return b.ID
		}
		nb := &Bucket{Name: name, Type: ob.Type, Alg: ob.Alg}
		var pos []int
		for i, it := range ob.Items {
			if it >= 0 {
				if m.DevClass[it] != class {
					continue
				}
				nb.Items = append(nb.Items, it)
				nb.Weights = append(nb.Weights, ob.Weights[i])
			} else {
				c := clone(it, class)
				var w uint32
				for _, x := range m.bucket(c).Weights {
					w += x
				}
				nb.Items = append(nb.Items, c)
				nb.Weights = append(nb.Weights, w)
			}
			pos = append(pos, i)
		}
		if id, ok := old[name]; ok {
			nb.ID = id
		} else {
			nb.ID = m.freeBucketID(used)
		}
		m.putBucket(nb)
		for k, args := range m.ChooseArgs {
			o := args[-1-orig]
			if o == nil {
				continue
			}
			n := &ChooseArg{}
			if cw[k] == nil {
				cw[k] = map[int][]uint32{}
			}
			sum := make([]uint32, len(o.WeightSet))
			for s := range o.WeightSet {
				ws := make([]uint32, len(nb.Items))
				for i, it := range nb.Items {
					switch {
					case it >= 0:
						ws[i] = o.WeightSet[s][pos[i]]
					case cw[k][it] != nil:
						ws[i] = cw[k][it][s]
					}
					sum[s] += ws[i]
				}
				n.WeightSet = append(n.WeightSet, ws)
			}
			cw[k][nb.ID] = sum
			args[-1-nb.ID] = n
		}
		return nb.ID
	}
	for _, r := range roots {
		for _, c := range classes {
			clone(r, c)
		}
	}
	m.reindex()
}

// RuleByName - id правила по имени.
func (m *Map) RuleByName(name string) (int, bool) {
	for id, r := range m.Rules {
		if r.Name == name {
			return id, true
		}
	}
	return 0, false
}

// RuleSpec - новое правило, как его создают команды ceph.
type RuleSpec struct {
	Name          string
	Mode          string // replicated | erasure | msr
	Root, Class   string
	FailureDomain string
	OSDsPerDomain int // msr: OSD на один failure domain (crush-osds-per-failure-domain)
	Width         int // msr: k+m
}

// AddRule создаёт правило как `ceph osd crush rule create-replicated`,
// `create-erasure` или EC-профиль с crush-osds-per-failure-domain (MSR).
func (m *Map) AddRule(sp RuleSpec) (int, error) {
	rootName := sp.Root
	if sp.Class != "" {
		rootName += "~" + sp.Class
	}
	root := m.BucketByName(rootName)
	if root == nil {
		return 0, fmt.Errorf("в CRUSH нет %s", rootName)
	}
	fd, ok := m.Types[sp.FailureDomain]
	if !ok {
		return 0, fmt.Errorf("в CRUSH нет типа %q", sp.FailureDomain)
	}
	id := 0
	for m.Rules[id] != nil {
		id++
	}
	r := &Rule{ID: id, Name: sp.Name}
	take := step{op: "take", arg1: root.ID}
	switch sp.Mode {
	case "replicated":
		r.Type = 1
		op := "chooseleaf_firstn"
		if fd == 0 {
			op = "choose_firstn"
		}
		r.steps = []step{take, {op: op, arg1: 0, arg2: fd}, {op: "emit"}}
	case "erasure":
		r.Type = 3
		op := "chooseleaf_indep"
		if fd == 0 {
			op = "choose_indep"
		}
		r.steps = []step{{op: "set_chooseleaf_tries", arg1: 5}, {op: "set_choose_tries", arg1: 100}, take, {op: op, arg1: 0, arg2: fd}, {op: "emit"}}
	case "msr":
		if sp.OSDsPerDomain <= 0 || sp.Width <= 0 || sp.Width%sp.OSDsPerDomain != 0 {
			return 0, fmt.Errorf("msr: k+m (%d) должно делиться на OSD на домен (%d)", sp.Width, sp.OSDsPerDomain)
		}
		r.Type = RuleTypeMSRIndep
		r.steps = []step{take, {op: "choosemsr", arg1: sp.Width / sp.OSDsPerDomain, arg2: fd}, {op: "choosemsr", arg1: sp.OSDsPerDomain, arg2: 0}, {op: "emit"}}
	default:
		return 0, fmt.Errorf("режим правила %q: replicated, erasure или msr", sp.Mode)
	}
	m.Rules[id] = r
	return id, nil
}

// Describe - правило одной строкой, как в crushtool -d.
func (m *Map) Describe(id int) string {
	r := m.Rules[id]
	if r == nil {
		return "?"
	}
	var parts []string
	for _, s := range r.steps {
		switch s.op {
		case "take":
			n := fmt.Sprint(s.arg1)
			if b := m.bucket(s.arg1); b != nil {
				n = strings.Replace(b.Name, "~", " class ", 1)
			}
			parts = append(parts, "take "+n)
		case "emit":
			parts = append(parts, "emit")
		case "choose_firstn", "chooseleaf_firstn", "choose_indep", "chooseleaf_indep", "choosemsr":
			parts = append(parts, fmt.Sprintf("%s %d type %s", strings.ReplaceAll(s.op, "_", " "), s.arg1, m.TypeName(s.arg2)))
		default:
			parts = append(parts, fmt.Sprintf("%s %d", s.op, s.arg1))
		}
	}
	return strings.Join(parts, "; ")
}

// FailureDomain - тип, по которому правило разносит копии (последний choose с типом не osd).
func (m *Map) FailureDomain(id int) string {
	r := m.Rules[id]
	if r == nil {
		return ""
	}
	fd := ""
	for _, s := range r.steps {
		switch s.op {
		case "choose_firstn", "chooseleaf_firstn", "choose_indep", "chooseleaf_indep", "choosemsr":
			if s.arg2 != 0 || fd == "" {
				fd = m.TypeName(s.arg2)
			}
		}
	}
	return fd
}
