package agent

import (
	"strings"
	"testing"
	"time"
)

// A refused setting is named, and its value is never printed: an operator
// may have typed a credential into it, and the error lands in a log.
func TestRefusedSettingIsNamedWithoutItsValue(t *testing.T) {
	const secret = "PROBE-SECRET"
	for _, tc := range []struct {
		name, setting string
		edit          func(s *Settings)
	}{
		{"relay address with a query", SettingRelayURL, func(s *Settings) { s.RelayURL = "https://relay.test/?token=" + secret }},
		{"relay address with a user", SettingRelayURL, func(s *Settings) { s.RelayURL = "https://user:" + secret + "@relay.test" }},
		{"relay address with a fragment", SettingRelayURL, func(s *Settings) { s.RelayURL = "https://relay.test/#" + secret }},
		{"relay address with a path", SettingRelayURL, func(s *Settings) { s.RelayURL = "https://relay.test/" + secret }},
		{"relay address over plain http", SettingRelayURL, func(s *Settings) { s.RelayURL = "http://" + secret + ".example" }},
		{"relay address that does not parse", SettingRelayURL, func(s *Settings) { s.RelayURL = "https://relay.test:" + secret }},
		{"api address with a query", SettingAPIURL, func(s *Settings) { s.APIURL = "http://127.0.0.1:8372/?token=" + secret }},
		{"city id", SettingCity, func(s *Settings) { s.City = "Alpha-" + secret }},
		{"wait longer than the request limit", SettingWait, func(s *Settings) { s.Wait, s.RequestLimit = 40*time.Second, 35*time.Second }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCityFixture(t, "alpha")
			tc.edit(&c.settings)
			err := c.settings.checkRun()
			if err == nil {
				t.Fatal("accepted")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "40s") {
				t.Fatalf("the error prints the value: %q", err.Error())
			}
			if !strings.Contains(err.Error(), tc.setting) {
				t.Fatalf("the error does not name %s: %q", tc.setting, err.Error())
			}
		})
	}
	for _, name := range []string{SettingWait, SettingRequestLimit} {
		_, err := ResolveSettings(func(string) string { return "" }, map[string]string{name: secret})
		if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), name) {
			t.Fatalf("setting %s to a non-duration: %v", name, err)
		}
	}
}
