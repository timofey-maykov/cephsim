package crush

import (
	"fmt"
	"testing"
)

func dumpBuckets(m *Map) map[int]string {
	out := map[int]string{}
	for _, b := range m.Buckets {
		if b != nil {
			out[b.ID] = fmt.Sprint(b.Name, b.Type, b.Items, b.Weights)
		}
	}
	return out
}

// Перестройка теневых деревьев на неизменённой карте должна дать ту же карту.
func TestRebuildShadowsIdentity(t *testing.T) {
	for _, p := range []string{"../testdata/crush-classes.json", "../testdata/snap-synthetic/crush.json"} {
		m, err := Load(p)
		if err != nil {
			t.Skip(err)
		}
		want := dumpBuckets(m)
		c := m.Clone()
		c.RebuildShadows()
		got := dumpBuckets(c)
		if len(got) != len(want) {
			t.Fatalf("%s: бакетов %d, ожидалось %d", p, len(got), len(want))
		}
		for id, w := range want {
			if got[id] != w {
				t.Fatalf("%s: бакет %d\n  было  %s\n  стало %s", p, id, w, got[id])
			}
		}
	}
}
