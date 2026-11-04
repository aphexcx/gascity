package exec

import (
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
