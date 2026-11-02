package tmux

import (
	"context"
	"errors"
	"testing"
)

func TestProviderIsCityScoped(t *testing.T) {
	for _, tt := range []struct {
		city, socket string
		want         bool
	}{
		{"test", "test", true},
		{"test", "", false},
		{"test", "default", false},
		{"test", "shared", false},
		{"test", "other", false},
		{"default", "default", false},
		{"", "", false},
	} {
		t.Run(tt.city+"/"+tt.socket, func(t *testing.T) {
			p := &Provider{cfg: Config{SocketName: tt.socket}}
			if got := p.IsCityScoped(tt.city); got != tt.want {
				t.Fatalf("IsCityScoped(%q) for socket %q = %v, want %v", tt.city, tt.socket, got, tt.want)
			}
		})
	}
}

func TestProviderCityOwnershipEnvironment(t *testing.T) {
	for _, tt := range []struct {
		name, output string
		err          error
		wantCity     string
	}{
		{"absent", "-GC_CITY_PATH\n", nil, ""},
		{"foreign city", "GC_CITY_PATH=/cities/other\n", nil, "/cities/other"},
		{"timeout", "", context.DeadlineExceeded, ""},
		{"transport failure", "", errors.New("transport failure"), ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tm := NewTmuxWithConfig(Config{SocketName: "test"})
			tm.exec = &fakeExecutor{out: tt.output, err: tt.err}
			p := &Provider{tm: tm}
			env, err := p.GetAllEnvironment("test-worker")
			if !errors.Is(err, tt.err) {
				t.Fatalf("GetAllEnvironment() error = %v, want %v", err, tt.err)
			}
			if env["GC_CITY_PATH"] != tt.wantCity {
				t.Fatalf("city ownership = %q, want %q", env["GC_CITY_PATH"], tt.wantCity)
			}
		})
	}
}
