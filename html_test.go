package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// HTML-отчёт собирается для каждого сценария и содержит вердикт, графики и план.
func TestHTMLReports(t *testing.T) {
	s := synth(t)
	hs, _ := parseHosts("host=new,rack=rack2,osds=12,size=16T,weight=1", s.Placement.Crush.Types)
	cases := map[string]func(*Scenario){
		ActMaintenance: func(sc *Scenario) { sc.Targets, sc.NoOut = []string{"host-01"}, true },
		ActFailure:     func(sc *Scenario) { sc.Targets = []string{"rack1"} },
		ActDrain:       func(sc *Scenario) { sc.Targets = []string{"host-02"} },
		ActExpand:      func(sc *Scenario) { sc.Add = hs },
		ActPGNum:       func(sc *Scenario) { sc.Pool, sc.PGNum = "rbd", 8192 },
	}
	for action, set := range cases {
		sc := baseScenario(action)
		set(&sc)
		c, err := Analyze(s, sc, true)
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		r := Finalize(s, c, sc)
		r.Variants = Variants(s, c, sc)
		var b bytes.Buffer
		if err := WriteHTML(&b, reportData(s, r)); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		h := b.String()
		for _, want := range []string{"<!doctype html>", "class=\"card verdict", "Заполненность OSD", "<svg", "План", "Откуда данные"} {
			if !strings.Contains(h, want) {
				t.Fatalf("%s: в отчёте нет %q", action, want)
			}
		}
		if action != ActMaintenance && !strings.Contains(h, "Ход переезда") && !r.Stalled {
			t.Fatalf("%s: нет графика хода переезда", action)
		}
	}
	r, err := Rolling(s, baseScenario(ActRolling), 0, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := WriteHTML(&b, rollingData(s, r)); err != nil || !strings.Contains(b.String(), "Расписание") {
		t.Fatalf("rolling: %v", err)
	}
}
