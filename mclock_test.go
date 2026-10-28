package main

import (
	"math"
	"testing"
	"time"
)

func TestClientCapProfiles(t *testing.T) {
	for name, want := range map[string]float64{"high_client_ops": .6, "high_recovery_ops": .3, "balanced": .5} {
		if got := mclockProfiles[name].clientCap(); math.Abs(got-want) > 1e-9 {
			t.Fatalf("%s: %v", name, got)
		}
	}
	// Резервы не на всю ёмкость: свободная часть делится по весам 2:1.
	if got := (MClockProfile{ClientRes: .2, ClientWgt: 2, RecRes: .2, RecWgt: 1}).clientCap(); math.Abs(got-.6) > 1e-9 {
		t.Fatalf("custom: %v", got)
	}
}

// HDD с мелкими объектами: операция в mClock стоит 150 MiB/315 ≈ 500 KB,
// поэтому 1 млн объектов по 32 KiB упирается в ёмкость, а не в байты.
func TestMClockLimitsSmallObjectsOnHDD(t *testing.T) {
	m := &MClock{Scheduler: "mclock_scheduler", Profile: "high_client_ops",
		CapIOPS: map[string]float64{"hdd": 315}, CapBW: map[string]float64{"hdd": 157286400}}
	c := &Core{OSDProj: map[int]*OSDProj{0: {ID: 0, Class: "hdd", In: 32 << 30, InObj: 1 << 20}}}
	sc := Scenario{Backfills: 5, SlotMBps: map[string]float64{"hdd": 40}, MClock: m, Client: map[string]ClientLoad{}}
	idle, who := recoveryTime(c, sc)
	if who.Limit != "mclock" {
		t.Fatalf("limit %q", who.Limit)
	}
	// Клиенты хотят 80%, профиль даёт им 60%: recovery остаётся 40%, время x2.5.
	sc.Client["hdd"] = ClientLoad{Ops: 0, Bytes: 0.8 * 157286400}
	busy, _ := recoveryTime(c, sc)
	if r := float64(busy) / float64(idle); math.Abs(r-2.5) > 0.01 {
		t.Fatalf("busy/idle = %.2f (%v / %v)", r, busy, idle)
	}
	if v := m.view("hdd", sc.Client["hdd"]); !v.Starved || math.Abs(v.RecShare-.4) > 1e-9 {
		t.Fatalf("%+v", v)
	}
	if idle < 25*time.Minute {
		t.Fatalf("idle %v: ожидаю около получаса на 1 млн объектов", idle)
	}
}
