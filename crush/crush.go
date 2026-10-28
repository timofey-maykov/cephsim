// Package crush - порт CRUSH из ceph/src/crush/mapper.c для straw2-бакетов,
// обычных и MSR-правил. Нужен, чтобы считать раскладку с reweight группы OSD
// (osdmaptool --mark-out принимает только один OSD) и с изменённой картой.
package crush

import (
	"encoding/json"
	"fmt"
	"math/bits"
	"os"
	"strconv"
)

const (
	ItemNone  = 0x7fffffff
	itemUndef = 0x7ffffffe
)

type Bucket struct {
	ID      int
	Name    string
	Type    int
	Alg     string
	Items   []int
	Weights []uint32
}

type ChooseArg struct {
	IDs       []int
	WeightSet [][]uint32 // [position][item]
}

type step struct {
	op         string
	arg1, arg2 int
}

type Rule struct {
	ID    int
	Name  string
	Type  int
	steps []step
}

type Tunables struct {
	ChooseLocalTries         int `json:"choose_local_tries"`
	ChooseLocalFallbackTries int `json:"choose_local_fallback_tries"`
	ChooseTotalTries         int `json:"choose_total_tries"`
	ChooseleafDescendOnce    int `json:"chooseleaf_descend_once"`
	ChooseleafVaryR          int `json:"chooseleaf_vary_r"`
	ChooseleafStable         int `json:"chooseleaf_stable"`
	MSRDescents              int `json:"msr_descents"`
	MSRCollisionTries        int `json:"msr_collision_tries"`
}

type Map struct {
	Buckets    []*Bucket // индекс -1-id
	Rules      map[int]*Rule
	Tunables   Tunables
	MaxDevices int
	ChooseArgs map[int]map[int]*ChooseArg // ключ: id пула или -1; дальше индекс бакета
	Types      map[string]int
	DevClass   map[int]string // класс устройства
	parents    map[int][]int  // item -> индексы бакетов, где он лежит
}

func (m *Map) bucket(id int) *Bucket {
	i := -1 - id
	if i < 0 || i >= len(m.Buckets) {
		return nil
	}
	return m.Buckets[i]
}

// Формат `ceph osd crush dump -f json` и `crushtool --dump`.
type dumpJSON struct {
	Devices []struct {
		ID    int    `json:"id"`
		Class string `json:"class"`
	} `json:"devices"`
	Types []struct {
		ID   int    `json:"type_id"`
		Name string `json:"name"`
	} `json:"types"`
	Buckets []struct {
		ID     int    `json:"id"`
		Name   string `json:"name"`
		TypeID int    `json:"type_id"`
		Alg    string `json:"alg"`
		Hash   string `json:"hash"`
		Items  []struct {
			ID     int    `json:"id"`
			Weight uint32 `json:"weight"`
		} `json:"items"`
	} `json:"buckets"`
	Rules []struct {
		ID    int    `json:"rule_id"`
		Name  string `json:"rule_name"`
		Type  int    `json:"type"`
		Steps []struct {
			Op   string `json:"op"`
			Item int    `json:"item"`
			Num  int    `json:"num"`
			Type string `json:"type"`
		} `json:"steps"`
	} `json:"rules"`
	Tunables   Tunables `json:"tunables"`
	ChooseArgs map[string][]struct {
		BucketID  int         `json:"bucket_id"`
		WeightSet [][]float64 `json:"weight_set"`
		IDs       []int       `json:"ids"`
	} `json:"choose_args"`
}

func Load(path string) (*Map, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d dumpJSON
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	m := &Map{Rules: map[int]*Rule{}, Tunables: d.Tunables, ChooseArgs: map[int]map[int]*ChooseArg{}, Types: map[string]int{}, DevClass: map[int]string{}, parents: map[int][]int{}}
	types := m.Types
	for _, t := range d.Types {
		types[t.Name] = t.ID
	}
	for _, dv := range d.Devices {
		m.MaxDevices = max(m.MaxDevices, dv.ID+1)
		if dv.Class != "" {
			m.DevClass[dv.ID] = dv.Class
		}
	}
	for _, bk := range d.Buckets {
		if bk.Alg != "straw2" {
			return nil, fmt.Errorf("бакет %s: алгоритм %s не поддерживается, только straw2", bk.Name, bk.Alg)
		}
		if bk.Hash != "" && bk.Hash != "rjenkins1" {
			return nil, fmt.Errorf("бакет %s: хеш %s не поддерживается", bk.Name, bk.Hash)
		}
		nb := &Bucket{ID: bk.ID, Name: bk.Name, Type: bk.TypeID, Alg: bk.Alg}
		for _, it := range bk.Items {
			nb.Items = append(nb.Items, it.ID)
			nb.Weights = append(nb.Weights, it.Weight)
		}
		idx := -1 - bk.ID
		for len(m.Buckets) <= idx {
			m.Buckets = append(m.Buckets, nil)
		}
		m.Buckets[idx] = nb
		for _, it := range nb.Items {
			m.parents[it] = append(m.parents[it], idx)
		}
	}
	for _, r := range d.Rules {
		nr := &Rule{ID: r.ID, Name: r.Name, Type: r.Type}
		for _, s := range r.Steps {
			st := step{op: s.Op}
			switch s.Op {
			case "take":
				st.arg1 = s.Item
			case "choose_firstn", "chooseleaf_firstn", "choose_indep", "chooseleaf_indep", "choosemsr":
				t, ok := types[s.Type]
				if !ok {
					return nil, fmt.Errorf("правило %s: неизвестный тип %q", r.Name, s.Type)
				}
				st.arg1, st.arg2 = s.Num, t
			case "emit", "noop":
			default:
				st.arg1 = s.Num
			}
			nr.steps = append(nr.steps, st)
		}
		m.Rules[r.ID] = nr
	}
	for key, list := range d.ChooseArgs {
		id, err := strconv.Atoi(key)
		if err != nil {
			continue
		}
		args := map[int]*ChooseArg{}
		for _, a := range list {
			ca := &ChooseArg{IDs: a.IDs}
			for _, pos := range a.WeightSet {
				ws := make([]uint32, len(pos))
				for i, f := range pos {
					ws[i] = uint32(f*0x10000 + 0.5)
				}
				ca.WeightSet = append(ca.WeightSet, ws)
			}
			args[-1-a.BucketID] = ca
		}
		m.ChooseArgs[id] = args
	}
	return m, nil
}

// Clone копирует веса, чтобы менять их в сценарии, не трогая исходную карту.
func (m *Map) Clone() *Map {
	c := *m
	c.Buckets = make([]*Bucket, len(m.Buckets))
	for i, b := range m.Buckets {
		if b != nil {
			nb := *b
			nb.Items = append([]int(nil), b.Items...)
			nb.Weights = append([]uint32(nil), b.Weights...)
			c.Buckets[i] = &nb
		}
	}
	c.Rules = map[int]*Rule{}
	for k, r := range m.Rules {
		c.Rules[k] = r
	}
	c.DevClass = map[int]string{}
	for k, v := range m.DevClass {
		c.DevClass[k] = v
	}
	c.reindex()
	c.ChooseArgs = map[int]map[int]*ChooseArg{}
	for k, args := range m.ChooseArgs {
		na := map[int]*ChooseArg{}
		for bi, a := range args {
			ca := &ChooseArg{IDs: a.IDs}
			for _, ws := range a.WeightSet {
				ca.WeightSet = append(ca.WeightSet, append([]uint32(nil), ws...))
			}
			na[bi] = ca
		}
		c.ChooseArgs[k] = na
	}
	return &c
}

// SetItemWeight как `ceph osd crush reweight`: меняет вес элемента во всех
// бакетах (включая теневые деревья классов) и вес всех предков.
func (m *Map) SetItemWeight(item int, w uint32) {
	for _, bi := range m.parents[item] {
		b := m.Buckets[bi]
		for i, it := range b.Items {
			if it == item {
				diff := int64(w) - int64(b.Weights[i])
				b.Weights[i] = w
				m.adjustWeightSets(bi, i, diff)
				m.propagate(b.ID, diff)
			}
		}
	}
}

func (m *Map) propagate(id int, diff int64) {
	for _, bi := range m.parents[id] {
		b := m.Buckets[bi]
		for i, it := range b.Items {
			if it == id {
				b.Weights[i] = uint32(max(0, int64(b.Weights[i])+diff))
				m.adjustWeightSets(bi, i, diff)
				m.propagate(b.ID, diff)
			}
		}
	}
}

func (m *Map) adjustWeightSets(bi, pos int, diff int64) {
	for _, args := range m.ChooseArgs {
		if a := args[bi]; a != nil {
			for _, ws := range a.WeightSet {
				if pos < len(ws) {
					ws[pos] = uint32(max(0, int64(ws[pos])+diff))
				}
			}
		}
	}
}

// ArgsFor: choose_args пула, иначе общие (-1), как choose_args_get_with_fallback.
func (m *Map) ArgsFor(pool int) map[int]*ChooseArg {
	if a, ok := m.ChooseArgs[pool]; ok {
		return a
	}
	return m.ChooseArgs[-1]
}

func crushLn(xin uint32) uint64 {
	x := xin + 1
	iexpon := 15
	if x&0x18000 == 0 {
		b := bits.LeadingZeros32(x&0x1FFFF) - 16
		x <<= b
		iexpon = 15 - b
	}
	index1 := (x >> 8) << 1
	rh := rhLHTbl[index1-256]
	lh := rhLHTbl[index1+1-256]
	xl64 := (uint64(x) * rh) >> 48
	result := uint64(iexpon) << (12 + 32)
	lh += llTbl[xl64&0xff]
	lh >>= 48 - 12 - 32
	return result + lh
}

func straw2Choose(b *Bucket, x, r int, arg *ChooseArg, position int) int {
	weights, ids := b.Weights, b.Items
	if arg != nil {
		if len(arg.WeightSet) > 0 {
			weights = arg.WeightSet[min(position, len(arg.WeightSet)-1)]
		}
		if len(arg.IDs) > 0 {
			ids = arg.IDs
		}
	}
	high := 0
	var highDraw int64
	for i := range b.Items {
		var draw int64
		if weights[i] != 0 {
			u := hash32_3(uint32(x), uint32(ids[i]), uint32(r)) & 0xffff
			ln := int64(crushLn(u)) - 0x1000000000000
			draw = ln / int64(weights[i])
		} else {
			draw = -1 << 63
		}
		if i == 0 || draw > highDraw {
			high, highDraw = i, draw
		}
	}
	return b.Items[high]
}

func isOut(weight []uint32, item, x int) bool {
	if item >= len(weight) {
		return true
	}
	w := weight[item]
	if w >= 0x10000 {
		return false
	}
	if w == 0 {
		return true
	}
	return hash32_2(uint32(x), uint32(item))&0xffff >= w
}

type ctx struct {
	m      *Map
	weight []uint32
	x      int
	args   map[int]*ChooseArg
}

func (c *ctx) choose(in *Bucket, r, position int) int {
	var arg *ChooseArg
	if c.args != nil {
		arg = c.args[-1-in.ID]
	}
	return straw2Choose(in, c.x, r, arg, position)
}

func (c *ctx) itemType(item int) int {
	if item < 0 {
		return c.m.bucket(item).Type
	}
	return 0
}

func (c *ctx) firstn(bucket *Bucket, numrep, typ int, out []int, outpos, outSize, tries, recurseTries, localRetries, localFallbackRetries int, recurseToLeaf bool, varyR, stable int, out2 []int, parentR int) int {
	count := outSize
	rep := outpos
	if stable != 0 {
		rep = 0
	}
	for ; rep < numrep && count > 0; rep++ {
		ftotal := 0
		skipRep := false
		item := 0
		for {
			retryDescent := false
			in := bucket
			flocal := 0
			for {
				collide, retryBucket, reject := false, false, false
				r := rep + parentR + ftotal
				itemtype := 0
				if len(in.Items) == 0 {
					reject = true
				} else {
					item = c.choose(in, r, outpos)
					if item >= c.m.MaxDevices {
						skipRep = true
						break
					}
					if item < 0 && c.m.bucket(item) == nil {
						skipRep = true
						break
					}
					itemtype = c.itemType(item)
					if itemtype != typ {
						if item >= 0 {
							skipRep = true
							break
						}
						in = c.m.bucket(item)
						continue
					}
					for i := 0; i < outpos; i++ {
						if out[i] == item {
							collide = true
							break
						}
					}
					if !collide && recurseToLeaf {
						if item < 0 {
							subR := 0
							if varyR > 0 {
								subR = r >> (varyR - 1)
							}
							n := outpos + 1
							if stable != 0 {
								n = 1
							}
							if c.firstn(c.m.bucket(item), n, 0, out2, outpos, count, recurseTries, 0, localRetries, localFallbackRetries, false, varyR, stable, nil, subR) <= outpos {
								reject = true
							}
						} else {
							out2[outpos] = item
						}
					}
					if !reject && !collide && itemtype == 0 {
						reject = isOut(c.weight, item, c.x)
					}
				}
				if reject || collide {
					ftotal++
					flocal++
					switch {
					case collide && flocal <= localRetries:
						retryBucket = true
					case localFallbackRetries > 0 && flocal <= len(in.Items)+localFallbackRetries:
						retryBucket = true
					case ftotal < tries:
						retryDescent = true
					default:
						skipRep = true
					}
				}
				if !retryBucket {
					break
				}
			}
			if !retryDescent {
				break
			}
		}
		if skipRep {
			continue
		}
		out[outpos] = item
		outpos++
		count--
	}
	return outpos
}

func (c *ctx) indep(bucket *Bucket, left, numrep, typ int, out []int, outpos, tries, recurseTries int, recurseToLeaf bool, out2 []int, parentR int) {
	endpos := outpos + left
	for rep := outpos; rep < endpos; rep++ {
		out[rep] = itemUndef
		if out2 != nil {
			out2[rep] = itemUndef
		}
	}
	for ftotal := 0; left > 0 && ftotal < tries; ftotal++ {
		for rep := outpos; rep < endpos; rep++ {
			if out[rep] != itemUndef {
				continue
			}
			in := bucket
			for {
				r := rep + parentR + numrep*ftotal
				if len(in.Items) == 0 {
					break
				}
				item := c.choose(in, r, outpos)
				none := func() {
					out[rep] = ItemNone
					if out2 != nil {
						out2[rep] = ItemNone
					}
					left--
				}
				if item >= c.m.MaxDevices || (item < 0 && c.m.bucket(item) == nil) {
					none()
					break
				}
				itemtype := c.itemType(item)
				if itemtype != typ {
					if item >= 0 {
						none()
						break
					}
					in = c.m.bucket(item)
					continue
				}
				collide := false
				for i := outpos; i < endpos; i++ {
					if out[i] == item {
						collide = true
						break
					}
				}
				if collide {
					break
				}
				if recurseToLeaf {
					if item < 0 {
						c.indep(c.m.bucket(item), 1, numrep, 0, out2, rep, recurseTries, 0, false, nil, r)
						if out2 != nil && out2[rep] == ItemNone {
							break
						}
					} else if out2 != nil {
						out2[rep] = item
					}
				}
				if itemtype == 0 && isOut(c.weight, item, c.x) {
					break
				}
				out[rep] = item
				left--
				break
			}
		}
	}
	for rep := outpos; rep < endpos; rep++ {
		if out[rep] == itemUndef {
			out[rep] = ItemNone
		}
		if out2 != nil && out2[rep] == itemUndef {
			out2[rep] = ItemNone
		}
	}
}

// DoRule - crush_do_rule_no_retry. weight - reweight OSD в 16.16 (osd_weight из osdmap).
func (m *Map) DoRule(ruleno, x, resultMax int, weight []uint32, args map[int]*ChooseArg) []int {
	rule := m.Rules[ruleno]
	if rule == nil {
		return nil
	}
	if isMSR(rule.Type) {
		return m.doRuleMSR(rule, x, resultMax, weight, args)
	}
	c := &ctx{m: m, weight: weight, x: x, args: args}
	w, o := make([]int, resultMax), make([]int, resultMax)
	cbuf := make([]int, resultMax)
	wsize := 0
	var result []int

	t := m.Tunables
	chooseTries := t.ChooseTotalTries + 1
	leafTries := 0
	localRetries, fallback := t.ChooseLocalTries, t.ChooseLocalFallbackTries
	varyR, stable := t.ChooseleafVaryR, t.ChooseleafStable

	for _, s := range rule.steps {
		switch s.op {
		case "take":
			if (s.arg1 >= 0 && s.arg1 < m.MaxDevices) || m.bucket(s.arg1) != nil {
				w[0], wsize = s.arg1, 1
			}
		case "set_choose_tries":
			if s.arg1 > 0 {
				chooseTries = s.arg1
			}
		case "set_chooseleaf_tries":
			if s.arg1 > 0 {
				leafTries = s.arg1
			}
		case "set_choose_local_tries":
			if s.arg1 >= 0 {
				localRetries = s.arg1
			}
		case "set_choose_local_fallback_tries":
			if s.arg1 >= 0 {
				fallback = s.arg1
			}
		case "set_chooseleaf_vary_r":
			if s.arg1 >= 0 {
				varyR = s.arg1
			}
		case "set_chooseleaf_stable":
			if s.arg1 >= 0 {
				stable = s.arg1
			}
		case "choose_firstn", "chooseleaf_firstn", "choose_indep", "chooseleaf_indep":
			if wsize == 0 {
				break
			}
			firstn := s.op == "choose_firstn" || s.op == "chooseleaf_firstn"
			leaf := s.op == "chooseleaf_firstn" || s.op == "chooseleaf_indep"
			osize := 0
			for i := 0; i < wsize; i++ {
				numrep := s.arg1
				if numrep <= 0 {
					numrep += resultMax
					if numrep <= 0 {
						continue
					}
				}
				b := m.bucket(w[i])
				if w[i] >= 0 || b == nil {
					continue
				}
				if firstn {
					recurseTries := chooseTries
					if leafTries > 0 {
						recurseTries = leafTries
					} else if t.ChooseleafDescendOnce != 0 {
						recurseTries = 1
					}
					osize += c.firstn(b, numrep, s.arg2, o[osize:], 0, resultMax-osize, chooseTries, recurseTries, localRetries, fallback, leaf, varyR, stable, cbuf[osize:], 0)
				} else {
					outSize := min(numrep, resultMax-osize)
					lt := leafTries
					if lt == 0 {
						lt = 1
					}
					c.indep(b, outSize, numrep, s.arg2, o[osize:], 0, chooseTries, lt, leaf, cbuf[osize:], 0)
					osize += outSize
				}
			}
			if leaf {
				copy(o, cbuf[:osize])
			}
			w, o = o, w
			wsize = osize
		case "emit":
			for i := 0; i < wsize && len(result) < resultMax; i++ {
				result = append(result, w[i])
			}
			wsize = 0
		}
	}
	return result
}
