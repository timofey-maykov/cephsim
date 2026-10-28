package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestClusterReport(t *testing.T) {
	s := synth(t)
	sc := baseScenario(ActMaintenance)
	cr, err := BuildCluster(s, sc, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// 12 хостов в 3 стойках, пул rep 3 с failure domain host: потерю хоста кластер
	// переживает, а стойки - нет, потому что две копии PG могут лежать в одной стойке.
	hosts, racks := 0, 0
	for _, d := range cr.Domains {
		switch d.Type {
		case "host":
			hosts++
			if d.FailInactive+d.FailIncomplete > 0 || d.MaintLost > 0 {
				t.Fatalf("потеря одного хоста не должна делать PG недоступными: %+v", d)
			}
		case "rack":
			racks++
			if d.FailInactive == 0 {
				t.Fatalf("при правиле по host потеря стойки должна давать inactive: %+v", d)
			}
		}
	}
	if hosts != 12 || racks != 3 {
		t.Fatalf("доменов: host %d, rack %d", hosts, racks)
	}
	if cr.Rolling == nil || len(cr.Rolling.Batches) != 12 || cr.Verdict != LevelBad {
		t.Fatalf("rolling %v, вердикт %v", cr.Rolling, cr.Verdict)
	}
	var b bytes.Buffer
	if err := WriteClusterHTML(&b, clusterData(s, cr)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Коротко", "Ответы на частые вопросы", "Карта отказов", "Подробно по каждому", "План обслуживания всех хостов", "Пулы", "Словарь"} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("нет раздела %q", want)
		}
	}
}
