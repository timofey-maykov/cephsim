package main

import (
	"fmt"
	"math"
	"math/bits"
	"path/filepath"

	"github.com/timofey-maykov/cephsim/crush"
)

const fCrush = "crush.json"

// Placement - то, что OSDMap делает поверх CRUSH: pps пула, reweight, up/exists,
// pg_upmap и pg_upmap_items (OSDMap::_pg_to_up_acting_osds без pg_temp).
type Placement struct {
	Crush      *crush.Map
	Weight     []uint32 // osd_weight, 0x10000 = in
	Up, Exists []bool
	Upmap      map[string][]int
	UpmapItems map[string][][2]int
}

type osdDumpExtra struct {
	MaxOSD int `json:"max_osd"`
	OSDs   []struct {
		OSD    int     `json:"osd"`
		Up     int     `json:"up"`
		In     int     `json:"in"`
		Weight float64 `json:"weight"`
	} `json:"osds"`
	PGUpmap []struct {
		PGID string `json:"pgid"`
		OSDs []int  `json:"osds"`
	} `json:"pg_upmap"`
	PGUpmapItems []struct {
		PGID     string `json:"pgid"`
		Mappings []struct {
			From int `json:"from"`
			To   int `json:"to"`
		} `json:"mappings"`
	} `json:"pg_upmap_items"`
}

func LoadPlacement(dir string) (*Placement, error) {
	cm, err := crush.Load(filepath.Join(dir, fCrush))
	if err != nil {
		return nil, err
	}
	if cm.Tunables.ChooseLocalFallbackTries > 0 {
		return nil, fmt.Errorf("legacy tunables (choose_local_fallback_tries > 0) не поддерживаются")
	}
	var d osdDumpExtra
	if err := readJSON(dir, fOSDDump, &d); err != nil {
		return nil, err
	}
	n := max(d.MaxOSD, cm.MaxDevices)
	p := &Placement{Crush: cm, Weight: make([]uint32, n), Up: make([]bool, n), Exists: make([]bool, n),
		Upmap: map[string][]int{}, UpmapItems: map[string][][2]int{}}
	for _, o := range d.OSDs {
		if o.OSD < 0 || o.OSD >= n {
			continue
		}
		p.Exists[o.OSD] = true
		p.Up[o.OSD] = o.Up == 1
		// В JSON reweight - float, в osdmap - 16.16.
		p.Weight[o.OSD] = uint32(math.Round(o.Weight * 0x10000))
	}
	for _, u := range d.PGUpmap {
		p.Upmap[u.PGID] = u.OSDs
	}
	for _, u := range d.PGUpmapItems {
		for _, mm := range u.Mappings {
			p.UpmapItems[u.PGID] = append(p.UpmapItems[u.PGID], [2]int{mm.From, mm.To})
		}
	}
	return p, nil
}

func (p *Placement) Clone() *Placement {
	c := *p
	c.Crush = p.Crush.Clone()
	c.Weight = append([]uint32(nil), p.Weight...)
	c.Up = append([]bool(nil), p.Up...)
	c.Exists = append([]bool(nil), p.Exists...)
	return &c
}

// MarkOut - out и down группы OSD, как после отказа и mon_osd_down_out_interval.
func (p *Placement) MarkOut(osds []int, down bool) {
	for _, o := range osds {
		if o < len(p.Weight) {
			p.Weight[o] = 0
			if down {
				p.Up[o] = false
			}
		}
	}
}

// CrushZero - CRUSH-вес 0, как `ceph osd crush reweight osd.N 0` при drain.
func (p *Placement) CrushZero(osds []int) {
	for _, o := range osds {
		p.Crush.SetItemWeight(o, 0)
	}
}

func stableMod(x, b, mask uint32) uint32 {
	if x&mask < b {
		return x & mask
	}
	return x & (mask >> 1)
}

func (p *Placement) PPS(pool Pool, ps uint32) uint32 {
	pgp := uint32(pool.PGPNum)
	if pgp == 0 {
		pgp = uint32(pool.PGNum)
	}
	mask := uint32(1)<<bits.Len32(pgp-1) - 1
	m := stableMod(ps, pgp, mask)
	if pool.Flags&1 != 0 { // FLAG_HASHPSPOOL
		return crush.Hash32_2(m, uint32(pool.ID))
	}
	return m + uint32(pool.ID)
}

func (p *Placement) okOSD(o int) bool { return o >= 0 && o < len(p.Exists) && p.Exists[o] }

// UpSet - up-набор PG.
func (p *Placement) UpSet(pool Pool, ps uint32) []int {
	pgid := fmt.Sprintf("%d.%x", pool.ID, ps)
	raw := p.Crush.DoRule(pool.CrushRule, int(p.PPS(pool, ps)), pool.Size, p.Weight, p.Crush.ArgsFor(pool.ID))

	// _remove_nonexistent_osds
	if !pool.EC() {
		k := raw[:0]
		for _, o := range raw {
			if p.okOSD(o) {
				k = append(k, o)
			}
		}
		raw = k
	} else {
		for i, o := range raw {
			if o != crush.ItemNone && !p.okOSD(o) {
				raw[i] = crush.ItemNone
			}
		}
	}

	// _apply_upmap
	outOSD := func(o int) bool { return o != crush.ItemNone && o >= 0 && o < len(p.Weight) && p.Weight[o] == 0 }
	if u, ok := p.Upmap[pgid]; ok {
		valid := true
		for _, o := range u {
			if outOSD(o) {
				valid = false
			}
		}
		if valid {
			raw = append([]int(nil), u...)
		}
	}
	for _, it := range p.UpmapItems[pgid] {
		from, to := it[0], it[1]
		exists, pos := false, -1
		for i, o := range raw {
			if o == to {
				exists = true
				break
			}
			if o == from && pos < 0 && !outOSD(to) {
				pos = i
			}
		}
		if !exists && pos >= 0 {
			raw[pos] = to
		}
	}

	// _raw_to_up_osds
	up := make([]int, 0, len(raw))
	for _, o := range raw {
		isUp := p.okOSD(o) && p.Up[o]
		switch {
		case isUp:
			up = append(up, o)
		case pool.EC():
			up = append(up, crush.ItemNone)
		}
	}
	return up
}

// MapAll - up-наборы всех PG всех пулов.
func (p *Placement) MapAll(pools map[int]Pool) Mapping {
	res := Mapping{}
	for _, pool := range pools {
		for ps := 0; ps < pool.PGNum; ps++ {
			res[fmt.Sprintf("%d.%x", pool.ID, ps)] = p.UpSet(pool, uint32(ps))
		}
	}
	return res
}
