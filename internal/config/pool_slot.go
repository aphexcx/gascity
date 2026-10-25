package config

import (
	"strconv"
	"strings"
)

// PoolSlotInstance returns an independent runtime configuration for a pool slot.
// Slot numbers are one-based; invalid slots return an empty Agent.
// Singleton pools retain their configured identity;
// namepools use their configured names before falling back to numeric names.
// Capacity is enforced by the caller, so existing slots survive a cap decrease.
func (a Agent) PoolSlotInstance(slot int) Agent {
	if slot < 1 {
		return Agent{}
	}
	instance := a.Clone()
	instance.PoolName = a.QualifiedName()
	if !a.SupportsInstanceExpansion() || a.UsesCanonicalSingletonPoolIdentity() {
		return instance
	}
	if slot >= 1 && slot <= len(a.NamepoolNames) {
		instance.Name = a.NamepoolNames[slot-1]
	} else {
		instance.Name = a.Name + "-" + strconv.Itoa(slot)
	}
	return instance
}

// PoolSlotForIdentity returns the positive slot that produces a qualified
// instance identity, or zero if the identity is not a member of this template.
// Like PoolSlotInstance, it does not enforce the current capacity limit.
func (a Agent) PoolSlotForIdentity(identity string) int {
	for i, name := range a.NamepoolNames {
		if a.QualifiedInstanceName(name) == identity {
			return i + 1
		}
	}
	slot := NumericPoolSlot(a.QualifiedName(), identity)
	if slot > 0 {
		instance := a.PoolSlotInstance(slot)
		if instance.QualifiedName() == identity {
			return slot
		}
	}
	return 0
}

// NumericPoolSlot parses the canonical positive numeric suffix of a qualified
// pool member. Zero means no match, including an empty pool, signs, padding,
// overflow, and identities belonging to another pool.
func NumericPoolSlot(pool, identity string) int {
	if pool == "" {
		return 0
	}
	suffix, ok := strings.CutPrefix(identity, pool+"-")
	if !ok {
		return 0
	}
	slot, err := strconv.Atoi(suffix)
	if err != nil || slot < 1 || strconv.Itoa(slot) != suffix {
		return 0
	}
	return slot
}
