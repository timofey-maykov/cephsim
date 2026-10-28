package main

import "math"

// Профили mClock из ceph/src/common/mclock_common.h. Резервы и лимиты - доли
// ёмкости OSD, лимит 0 - без лимита.
type MClockProfile struct {
	ClientRes float64 `json:"client_res"`
	ClientWgt float64 `json:"client_wgt"`
	ClientLim float64 `json:"client_lim"`
	RecRes    float64 `json:"recovery_res"`
	RecWgt    float64 `json:"recovery_wgt"`
	RecLim    float64 `json:"recovery_lim"`
}

var mclockProfiles = map[string]MClockProfile{
	"high_client_ops":   {ClientRes: .6, ClientWgt: 2, RecRes: .4, RecWgt: 1},
	"high_recovery_ops": {ClientRes: .3, ClientWgt: 1, RecRes: .7, RecWgt: 2},
	"balanced":          {ClientRes: .5, ClientWgt: 1, RecRes: .5, RecWgt: 1},
}

// MClock - настройки планировщика на OSD, снятые collect.
type MClock struct {
	Scheduler string             `json:"osd_op_queue"`
	Profile   string             `json:"profile"`
	Custom    *MClockProfile     `json:"custom,omitempty"`
	CapIOPS   map[string]float64 `json:"capacity_iops"`    // osd_mclock_max_capacity_iops_* по классам
	CapBW     map[string]float64 `json:"capacity_bytes_s"` // osd_mclock_max_sequential_bandwidth_* по классам
}

func (m *MClock) profile() (MClockProfile, bool) {
	if m == nil || m.Scheduler != "mclock_scheduler" {
		return MClockProfile{}, false
	}
	if m.Profile == "custom" {
		if m.Custom == nil {
			return MClockProfile{}, false
		}
		return *m.Custom, true
	}
	p, ok := mclockProfiles[m.Profile]
	return p, ok
}

// clientCap - наибольшая доля ёмкости, которую получат клиенты, пока recovery
// забирает всё, что ему дают: свой резерв плюс доля свободной части по весам.
func (p MClockProfile) clientCap() float64 {
	free := math.Max(0, 1-p.ClientRes-p.RecRes)
	c := p.ClientRes + free*p.ClientWgt/(p.ClientWgt+p.RecWgt)
	if p.ClientLim > 0 {
		c = math.Min(c, p.ClientLim)
	}
	return math.Min(1, c)
}

// ClientLoad - клиентская нагрузка на один OSD: операции (с subop реплик) и байты в секунду.
type ClientLoad struct {
	Ops   float64 `json:"ops"`
	Bytes float64 `json:"bytes"`
}

// ClientView - как клиенты и recovery делят ёмкость OSD класса.
type ClientView struct {
	Load       ClientLoad `json:"load"`
	CostPerIO  float64    `json:"cost_per_io_bytes"`
	Capacity   float64    `json:"capacity_bytes_s"`
	Demand     float64    `json:"demand_share"`      // клиенты хотят, доля ёмкости
	ClientCap  float64    `json:"client_cap_share"`  // профиль даёт им при recovery
	RecShare   float64    `json:"recovery_share"`    // остаётся recovery
	Starved    bool       `json:"clients_throttled"` // клиентам не хватит
	Configured bool       `json:"-"`
}

// view: в mClock операция стоит capacity_bw/capacity_iops байт плюс свой размер,
// ёмкость OSD - capacity_bw. Планировщик работает с запасом: что не взяли клиенты, уходит recovery.
func (m *MClock) view(class string, cl ClientLoad) ClientView {
	p, ok := m.profile()
	key := class
	if key != "hdd" {
		key = "ssd"
	}
	bw, iops := 0.0, 0.0
	if m != nil {
		bw, iops = m.CapBW[key], m.CapIOPS[key]
	}
	if !ok || bw <= 0 || iops <= 0 {
		return ClientView{Load: cl}
	}
	v := ClientView{Load: cl, CostPerIO: bw / iops, Capacity: bw, ClientCap: p.clientCap(), Configured: true}
	v.Demand = (cl.Ops*v.CostPerIO + cl.Bytes) / bw
	v.RecShare = 1 - math.Min(v.Demand, v.ClientCap)
	if p.RecLim > 0 {
		v.RecShare = math.Min(v.RecShare, p.RecLim)
	}
	v.Starved = v.Demand > v.ClientCap
	return v
}
