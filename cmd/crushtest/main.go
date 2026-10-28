// crushtest печатает раскладку в формате `crushtool --test --show-mappings`,
// чтобы сверить Go-порт CRUSH с crushtool на любых правилах, reweight и правках карты.
package main

import (
	"flag"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/timofey-maykov/cephsim/crush"
)

type list []string

func (l *list) String() string     { return strings.Join(*l, " ") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	path := flag.String("crush", "", "crush.json")
	rule := flag.Int("rule", 0, "id правила")
	numRep := flag.Int("num-rep", 3, "размер")
	maxX := flag.Int("max-x", 1023, "x от 0 до max-x")
	weights := flag.String("weight", "", "reweight: id=w,id=w (w от 0 до 1)")
	zero := flag.String("crush-zero", "", "CRUSH-вес 0 для OSD: id,id (как crush reweight при drain)")
	var adds list
	flag.Var(&adds, "add", "добавить OSD: id:вес:класс:host=h,rack=r (повторяется, как crushtool --add-item)")
	newRule := flag.String("new-rule", "", "создать правило name:replicated|erasure|msr:root:тип:класс[:osd_на_домен:ширина] и считать по нему")
	flag.Parse()
	m, err := crush.Load(*path)
	if err != nil {
		log.Fatal(err)
	}
	for _, a := range adds {
		f := strings.SplitN(a, ":", 4)
		id, _ := strconv.Atoi(f[0])
		w, _ := strconv.ParseFloat(f[1], 64)
		var loc []crush.Loc
		for _, kv := range strings.Split(f[3], ",") {
			k, v, _ := strings.Cut(kv, "=")
			loc = append(loc, crush.Loc{Type: k, Name: v})
		}
		if err := m.InsertDevice(id, uint32(w*0x10000), f[2], loc); err != nil {
			log.Fatal(err)
		}
	}
	if *newRule != "" {
		f := strings.Split(*newRule, ":")
		sp := crush.RuleSpec{Name: f[0], Mode: f[1], Root: f[2], FailureDomain: f[3], Class: f[4]}
		if len(f) > 6 {
			sp.OSDsPerDomain, _ = strconv.Atoi(f[5])
			sp.Width, _ = strconv.Atoi(f[6])
		}
		if *rule, err = m.AddRule(sp); err != nil {
			log.Fatal(err)
		}
	}
	for _, f := range strings.Split(*zero, ",") {
		if id, err := strconv.Atoi(f); err == nil {
			m.SetItemWeight(id, 0)
		}
	}
	w := make([]uint32, m.MaxDevices)
	for i := range w {
		w[i] = 0x10000
	}
	for _, kv := range strings.Split(*weights, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			id, _ := strconv.Atoi(k)
			f, _ := strconv.ParseFloat(v, 64)
			w[id] = uint32(f * 0x10000)
		}
	}
	for x := 0; x <= *maxX; x++ {
		out := m.DoRule(*rule, x, *numRep, w, nil)
		s := make([]string, len(out))
		for i, o := range out {
			s[i] = strconv.Itoa(o)
		}
		fmt.Printf("CRUSH rule %d x %d [%s]\n", *rule, x, strings.Join(s, ","))
	}
}
