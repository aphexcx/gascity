package config

import (
	"reflect"
	"testing"
)

func TestPoolSlotInstanceRejectsNonpositiveSlot(t *testing.T) {
	for _, slot := range []int{0, -1} {
		if got := (Agent{Name: "worker"}).PoolSlotInstance(slot); !reflect.DeepEqual(got, Agent{}) {
			t.Errorf("PoolSlotInstance(%d) = %+v, want empty instance", slot, got)
		}
	}
}

func TestPoolSlotInstance(t *testing.T) {
	capacity := 3
	base := Agent{Name: "worker", Dir: "demo", BindingName: "pack", MaxActiveSessions: &capacity, Env: map[string]string{"key": "original"}}
	for _, tc := range []struct {
		name  string
		names []string
		slot  int
		want  string
	}{
		{"numeric", nil, 2, "demo/pack.worker-2"},
		{"namepool", []string{"alpha", "beta"}, 2, "demo/pack.beta"},
		{"overflow", []string{"alpha"}, 2, "demo/pack.worker-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := base.Clone()
			a.NamepoolNames = tc.names
			instance := a.PoolSlotInstance(tc.slot)
			if instance.QualifiedName() != tc.want || instance.PoolName != a.QualifiedName() {
				t.Fatalf("instance = %q, pool = %q", instance.QualifiedName(), instance.PoolName)
			}
			if slot := a.PoolSlotForIdentity(instance.QualifiedName()); slot != tc.slot {
				t.Fatalf("slot = %d, want %d", slot, tc.slot)
			}
			instance.Env["key"] = "changed"
			*instance.MaxActiveSessions = 4
			if a.Env["key"] != "original" || *a.MaxActiveSessions != 3 {
				t.Fatal("slot instance aliases template")
			}
		})
	}
	singleton := 1
	base.MaxActiveSessions = &singleton
	if got := base.PoolSlotInstance(1); got.QualifiedName() != base.QualifiedName() {
		t.Fatalf("singleton = %q, want %q", got.QualifiedName(), base.QualifiedName())
	}
}

func TestNumericPoolSlot(t *testing.T) {
	for _, tc := range []struct {
		identity string
		want     int
	}{
		{"demo/pack.worker-2", 2},
		{"demo/pack.worker-02", 0},
		{"demo/pack.worker-+2", 0},
		{"demo/pack.worker-0", 0},
		{"demo/pack.worker--1", 0},
		{"demo/pack.worker-abc", 0},
		{"demo/pack.worker-999999999999999999999999", 0},
		{"other/pack.worker-2", 0},
	} {
		if got := NumericPoolSlot("demo/pack.worker", tc.identity); got != tc.want {
			t.Errorf("NumericPoolSlot(%q) = %d, want %d", tc.identity, got, tc.want)
		}
	}
	if got := NumericPoolSlot("", "-2"); got != 0 {
		t.Fatalf("empty pool = %d", got)
	}
}
