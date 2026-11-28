package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSettingsDefaults(t *testing.T) {
	s := DefaultSettings(func(string) string { return "" })
	if s.Wait != 25*time.Second || s.RequestLimit != 35*time.Second || s.GC != "gc" {
		t.Fatalf("defaults %+v", s)
	}
	if s.StateDir != "" || s.EncryptionKeysDir != "" || s.Socket != "" {
		t.Fatalf("service defaults without a service: %+v", s)
	}
}

func TestSettingsLayersFileThenEnvironmentThenFlags(t *testing.T) {
	file := filepath.Join(t.TempDir(), "agent.toml")
	content := `city = "from-file"
relay-url = "https://relay.example"
wait = "10s"
gc = "/file/gc"
`
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"GC_MAIL_RELAY_CONFIG":    file,
		"GC_MAIL_RELAY_CITY":      "from-env",
		"GC_MAIL_RELAY_WAIT":      "12s",
		"GC_SERVICE_STATE_ROOT":   "/service/state",
		"GC_MAIL_RELAY_STATE_DIR": "/env/state",
	}
	s, err := ResolveSettings(func(k string) string { return env[k] }, map[string]string{"city": "from-flag"})
	if err != nil {
		t.Fatal(err)
	}
	if s.City != "from-flag" || s.Wait != 12*time.Second || s.RelayURL != "https://relay.example" || s.GC != "/file/gc" || s.StateDir != "/env/state" {
		t.Fatalf("layered settings %+v", s)
	}
}

func TestSettingsFileRefusesUnknownKeys(t *testing.T) {
	file := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(file, []byte(`relay = "https://relay.example"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSettings(func(string) string { return "" }, map[string]string{"config": file}); err == nil || !strings.Contains(err.Error(), "relay") {
		t.Fatalf("unknown setting: %v", err)
	}
}

func TestSettingsCheckAddresses(t *testing.T) {
	for _, tc := range []struct {
		relay, api string
		ok         bool
	}{
		{"https://relay.example", "http://127.0.0.1:8372", true},
		{"http://127.0.0.1:9000", "http://localhost:8372", true},
		{"http://[::1]:9000", "https://city.example", true},
		{"http://relay.example", "http://127.0.0.1:8372", false},
		{"https://relay.example", "http://city.example", false},
		{"https://relay.example/base", "http://127.0.0.1:8372", false},
		{"https://relay.example?x=1", "http://127.0.0.1:8372", false},
		{"ftp://relay.example", "http://127.0.0.1:8372", false},
	} {
		c := newCityFixture(t, "alpha")
		c.settings.RelayURL, c.settings.APIURL = tc.relay, tc.api
		if err := c.settings.checkRun(); (err == nil) != tc.ok {
			t.Errorf("relay %q api %q: %v", tc.relay, tc.api, err)
		}
	}
}

func TestSettingsCheckTimes(t *testing.T) {
	c := newCityFixture(t, "alpha")
	c.settings.Wait, c.settings.RequestLimit = 40*time.Second, 35*time.Second
	if err := c.settings.checkRun(); err == nil {
		t.Fatal("accepted a wait longer than the request limit")
	}
	c.settings.Wait = 0
	if err := c.settings.checkRun(); err == nil {
		t.Fatal("accepted a zero wait")
	}
}

func TestSettingsCheckCityID(t *testing.T) {
	c := newCityFixture(t, "alpha")
	c.settings.City = "Alpha"
	if err := c.settings.checkKeygen(); err == nil {
		t.Fatal("accepted a city id the relay cannot carry")
	}
}
