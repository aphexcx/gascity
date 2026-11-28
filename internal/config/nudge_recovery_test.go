package config

import (
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestSessionOwnPasteRecoveryConfiguration(t *testing.T) {
	var cfg City
	if cfg.Session.NudgeOwnPasteAgeDuration() != 15*time.Second || cfg.Session.NudgeReservationAgeDuration() != 10*time.Minute || !cfg.Session.NudgeOpaquePasteRecoveryEnabled() {
		t.Fatal("unexpected recovery defaults")
	}
	_, err := toml.Decode("[session]\nnudge_own_paste_age = '30s'\nnudge_reservation_age = '20m'\nnudge_opaque_paste_recovery = false\n", &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Session.NudgeOwnPasteAgeDuration() != 30*time.Second || cfg.Session.NudgeReservationAgeDuration() != 20*time.Minute || cfg.Session.NudgeOpaquePasteRecoveryEnabled() {
		t.Fatalf("override ignored: %+v", cfg.Session)
	}
	for _, invalid := range []string{"bad", "0s", "-1s"} {
		cfg.Session.NudgeOwnPasteAge = invalid
		if len(ValidateDurations(&cfg, "city.toml")) == 0 {
			t.Fatalf("invalid recovery age %q accepted", invalid)
		}
		cfg.Session.NudgeOwnPasteAge = "15s"
		cfg.Session.NudgeReservationAge = invalid
		if len(ValidateDurations(&cfg, "city.toml")) == 0 {
			t.Fatalf("invalid reservation age %q accepted", invalid)
		}
		cfg.Session.NudgeReservationAge = "10m"
	}
}

func TestSessionNudgeUnseenPasteAgeConfiguration(t *testing.T) {
	var cfg City
	if got := cfg.Session.NudgeUnseenPasteAgeDuration(); got != 5*time.Minute {
		t.Fatalf("default unseen paste age = %s, want 5m", got)
	}
	if _, err := toml.Decode("[session]\nnudge_unseen_paste_age = '90s'\n", &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Session.NudgeUnseenPasteAgeDuration(); got != 90*time.Second {
		t.Fatalf("override ignored: %s", got)
	}
	if warnings := ValidateDurations(&cfg, "city.toml"); len(warnings) != 0 {
		t.Fatalf("valid unseen paste age rejected: %v", warnings)
	}
	for _, invalid := range []string{"bad", "0s", "-1s"} {
		cfg.Session.NudgeUnseenPasteAge = invalid
		if len(ValidateDurations(&cfg, "city.toml")) == 0 {
			t.Fatalf("invalid unseen paste age %q accepted", invalid)
		}
		if got := cfg.Session.NudgeUnseenPasteAgeDuration(); got != 5*time.Minute {
			t.Fatalf("invalid unseen paste age %q read as %s, want the 5m the runtime applies", invalid, got)
		}
	}
}
