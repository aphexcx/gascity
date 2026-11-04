package exec

import (
	"fmt"
	"os"
	"testing"
)

func TestShutdownBindingRechecksOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, city, id string
		owned, allow   bool
	}{
		{name: "metadata free", allow: true},
		{name: "owned", owned: true, city: "/city", id: "original", allow: true},
		{name: "new city only", city: "/city"},
		{name: "new id only", id: "original"},
		{name: "new owner", city: "/foreign", id: "replacement"},
		{name: "same owner published", city: "/city", id: "original"},
		{name: "owned city changed", owned: true, city: "/foreign", id: "original"},
		{name: "owned id changed", owned: true, city: "/city", id: "replacement"},
		{name: "owned metadata removed", owned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := writeScript(t, t.TempDir(), `
case "$1" in
get-meta) if [ -f "$0.$3" ]; then cat "$0.$3"; fi ;;
set-meta) cat > "$0.$3" ;;
stop|interrupt|send-keys) printf '%s\n' "$1" >> "$0.calls" ;;
*) exit 2 ;;
esac
`)
			p := NewProvider(script)
			setOwner := func(city, id string) {
				t.Helper()
				for key, value := range map[string]string{"GC_CITY_PATH": city, "GC_SESSION_ID": id} {
					if err := p.SetMeta("worker", key, value); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.owned {
				setOwner("/city", "original")
			}
			bound, err := p.BindForShutdown("worker", "/city", "original")
			if err != nil {
				t.Fatal(err)
			}
			setOwner(tc.city, tc.id)
			for name, mutate := range map[string]func() error{
				"stop":      func() error { return bound.Stop("worker") },
				"interrupt": func() error { return bound.Interrupt("worker") },
				"send keys": func() error { return bound.SendKeys("worker", "C-c") },
			} {
				if err := mutate(); (err == nil) != tc.allow {
					t.Errorf("%s error = %v, allow=%v", name, err, tc.allow)
				}
			}
			calls, err := os.ReadFile(script + ".calls")
			if tc.allow {
				if err != nil || len(calls) == 0 {
					t.Fatalf("no shutdown operations: %q, %v", calls, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("ownership change reached script: %q, %v", calls, err)
			}
		})
	}
}

func TestShutdownBindingRefusesPartialOwnership(t *testing.T) {
	p := NewProvider(writeScript(t, t.TempDir(), `
case "$1:$3" in
get-meta:GC_CITY_PATH) echo /city ;;
get-meta:GC_SESSION_ID) ;;
*) exit 2 ;;
esac
`))
	if _, err := p.BindForShutdown("worker", "/city", "original"); err == nil {
		t.Fatal("partial metadata accepted as a metadata-free runtime")
	}
}

func TestShutdownBindingRejectsOwnershipReadErrors(t *testing.T) {
	for _, code := range []int{1, 2} {
		for _, key := range []string{"GC_CITY_PATH", "GC_SESSION_ID"} {
			for _, afterBinding := range []bool{false, true} {
				t.Run(fmt.Sprintf("exit=%d/key=%s/after_binding=%v", code, key, afterBinding), func(t *testing.T) {
					script := writeScript(t, t.TempDir(), fmt.Sprintf(`
case "$1" in
get-meta) if [ -f "$0.reject-$3" ]; then exit %d; fi ;;
stop|interrupt|send-keys) echo "$1" >> "$0.calls" ;;
*) exit 2 ;;
esac
`, code))
					rejectRead := func() {
						t.Helper()
						if err := os.WriteFile(script+".reject-"+key, nil, 0o600); err != nil {
							t.Fatal(err)
						}
					}
					if !afterBinding {
						rejectRead()
					}
					bound, err := NewProvider(script).BindForShutdown("worker", "/city", "original")
					if !afterBinding {
						if err == nil {
							t.Fatal("failed metadata read authorized initial binding")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					rejectRead()
					for name, mutate := range map[string]func() error{
						"stop":      func() error { return bound.Stop("worker") },
						"interrupt": func() error { return bound.Interrupt("worker") },
						"send keys": func() error { return bound.SendKeys("worker", "C-c") },
					} {
						if err := mutate(); err == nil {
							t.Errorf("%s accepted a failed metadata read", name)
						}
					}
					if calls, err := os.ReadFile(script + ".calls"); !os.IsNotExist(err) {
						t.Fatalf("unreadable ownership reached script: %q, %v", calls, err)
					}
				})
			}
		}
	}
}
