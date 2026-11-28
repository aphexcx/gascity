// Package agent is a city's side of the hosted mail relay. It keeps the
// city's encryption key, sends sealed mail, and holds one signed read open at
// the relay, filing each mail it receives through the city's own mail API and
// then telling the recipient through the gc program. It makes outbound calls
// only; the one thing it listens on is a Unix socket for gc's health probe.
package agent

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
)

// Setting names, shared by the settings file, the flags and the environment.
const (
	SettingConfig            = "config"
	SettingCity              = "city"
	SettingRelayURL          = "relay-url"
	SettingSigningKey        = "signing-key"
	SettingEncryptionKeysDir = "encryption-keys-dir"
	SettingPeersDir          = "peers-dir"
	SettingAPIURL            = "api-url"
	SettingGCCity            = "gc-city"
	SettingGC                = "gc"
	SettingStateDir          = "state-dir"
	SettingSocket            = "socket"
	SettingWait              = "wait"
	SettingRequestLimit      = "request-limit"
)

// EnvPrefix starts the environment variable of every setting.
const EnvPrefix = "GC_MAIL_RELAY_"

// The variables gc sets for a service process it supervises.
const (
	envServiceStateRoot = "GC_SERVICE_STATE_ROOT"
	envServiceSecrets   = "GC_SERVICE_SECRETS_DIR"
	envServiceSocket    = "GC_SERVICE_SOCKET"
)

const (
	defaultWait         = 25 * time.Second
	defaultRequestLimit = 35 * time.Second
	defaultGC           = "gc"
)

// Settings configures the agent. Paths are used as given.
type Settings struct {
	City              string        // the city's own id on the relay
	RelayURL          string        // the relay's base address
	SigningKey        string        // the city's Ed25519 signing key file, read and never written
	EncryptionKeysDir string        // the folder of the city's X25519 keys and their statements
	PeersDir          string        // the folder of pinned peer signing keys, one <id>.pub each
	APIURL            string        // the city's own API
	GCCity            string        // the city's name on its own API
	GC                string        // the gc program
	StateDir          string        // the folder of the agent's own durable state
	Socket            string        // the Unix socket gc probes for health; empty for none
	Wait              time.Duration // how long the relay may hold one read
	RequestLimit      time.Duration // the longest any one relay request may take
}

// SettingUsage describes each setting, in the order the commands list them.
var SettingUsage = []struct{ Name, Usage string }{
	{SettingConfig, "settings file (TOML; keys are the setting names)"},
	{SettingCity, "this city's id on the relay"},
	{SettingRelayURL, "relay address (https, or http to a loopback host)"},
	{SettingSigningKey, "this city's signing key file (mode 0600)"},
	{SettingEncryptionKeysDir, "folder of this city's encryption keys (default: $" + envServiceSecrets + ")"},
	{SettingPeersDir, "folder of pinned peer signing keys, one <city>.pub each"},
	{SettingAPIURL, "this city's API address (https, or http to a loopback host)"},
	{SettingGCCity, "this city's name on its API"},
	{SettingGC, "the gc program (default: gc on PATH)"},
	{SettingStateDir, "folder of the agent's state (default: $" + envServiceStateRoot + ")"},
	{SettingSocket, "Unix socket for the health path (default: $" + envServiceSocket + ")"},
	{SettingWait, "how long the relay may hold one read (default 25s)"},
	{SettingRequestLimit, "longest one relay request may take (default 35s)"},
}

// EnvName returns the environment variable that carries a setting.
func EnvName(setting string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(setting, "-", "_"))
}

// DefaultSettings returns the defaults. The folders and socket a gc service
// is handed are the defaults for state, keys and health.
func DefaultSettings(getenv func(string) string) Settings {
	return Settings{
		EncryptionKeysDir: getenv(envServiceSecrets),
		GC:                defaultGC,
		StateDir:          getenv(envServiceStateRoot),
		Socket:            getenv(envServiceSocket),
		Wait:              defaultWait,
		RequestLimit:      defaultRequestLimit,
	}
}

// ResolveSettings layers the defaults, then the settings file, then the
// environment, then the flags given. The settings file is named by the
// config flag or its environment variable.
func ResolveSettings(getenv func(string) string, flags map[string]string) (Settings, error) {
	s := DefaultSettings(getenv)
	file := flags[SettingConfig]
	if file == "" {
		file = getenv(EnvName(SettingConfig))
	}
	if file != "" {
		values, err := readSettingsFile(file)
		if err != nil {
			return Settings{}, err
		}
		if err := s.apply(values, "settings file "+file); err != nil {
			return Settings{}, err
		}
	}
	env := make(map[string]string)
	for _, u := range SettingUsage {
		if v := getenv(EnvName(u.Name)); v != "" && u.Name != SettingConfig {
			env[u.Name] = v
		}
	}
	if err := s.apply(env, "environment"); err != nil {
		return Settings{}, err
	}
	given := make(map[string]string, len(flags))
	for name, v := range flags {
		if name != SettingConfig {
			given[name] = v
		}
	}
	if err := s.apply(given, "flags"); err != nil {
		return Settings{}, err
	}
	return s, nil
}

func readSettingsFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading settings file: %w", err)
	}
	values := make(map[string]string)
	if err := toml.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("settings file %s: %w", path, err)
	}
	return values, nil
}

func (s *Settings) apply(values map[string]string, source string) error {
	for name, v := range values {
		if err := s.set(name, v); err != nil {
			return fmt.Errorf("%s: %w", source, err)
		}
	}
	return nil
}

func (s *Settings) set(name, value string) error {
	var err error
	switch name {
	case SettingCity:
		s.City = value
	case SettingRelayURL:
		s.RelayURL = value
	case SettingSigningKey:
		s.SigningKey = value
	case SettingEncryptionKeysDir:
		s.EncryptionKeysDir = value
	case SettingPeersDir:
		s.PeersDir = value
	case SettingAPIURL:
		s.APIURL = value
	case SettingGCCity:
		s.GCCity = value
	case SettingGC:
		s.GC = value
	case SettingStateDir:
		s.StateDir = value
	case SettingSocket:
		s.Socket = value
	case SettingWait:
		s.Wait, err = time.ParseDuration(value)
	case SettingRequestLimit:
		s.RequestLimit, err = time.ParseDuration(value)
	default:
		return fmt.Errorf("unknown setting %q", name)
	}
	if err != nil {
		// The value is not quoted: it may be a credential typed in by mistake.
		return fmt.Errorf("setting %s is not a duration", name)
	}
	return nil
}

func (s Settings) checkKeygen() error {
	return firstError(s.checkCity(), required(SettingSigningKey, s.SigningKey), required(SettingEncryptionKeysDir, s.EncryptionKeysDir))
}

func (s Settings) checkSend() error {
	return firstError(s.checkCity(), required(SettingSigningKey, s.SigningKey), required(SettingPeersDir, s.PeersDir),
		required(SettingStateDir, s.StateDir), checkAddress(SettingRelayURL, s.RelayURL, false), s.checkTimes())
}

func (s Settings) checkRun() error {
	return firstError(s.checkKeygen(), s.checkSend(), checkAddress(SettingAPIURL, s.APIURL, true),
		required(SettingGCCity, s.GCCity), required(SettingGC, s.GC))
}

func (s Settings) checkCity() error {
	if !envelope.ValidCityID(s.City) {
		return fmt.Errorf("setting %s is not a relay city id (1 to 40 of a-z, 0-9 and -, not starting with -)", SettingCity)
	}
	return nil
}

func (s Settings) checkTimes() error {
	if s.Wait <= 0 || s.RequestLimit <= s.Wait {
		return fmt.Errorf("setting %s must be positive and shorter than %s", SettingWait, SettingRequestLimit)
	}
	return nil
}

func required(name, value string) error {
	if value == "" {
		return fmt.Errorf("setting %s is required", name)
	}
	return nil
}

// checkAddress accepts https, or http to a loopback host, since mail crosses
// this hop. A relay address carries no path: grants bind the path from /v1.
// A refusal names the setting and never quotes its value, which may hold a
// credential typed in by mistake.
func checkAddress(name, raw string, pathAllowed bool) error {
	if raw == "" {
		return fmt.Errorf("setting %s is required", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("setting %s is not a valid address", name)
	}
	switch {
	case u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "":
		return fmt.Errorf("setting %s must be a plain scheme://host[:port] address", name)
	case !pathAllowed && u.Path != "" && u.Path != "/":
		return fmt.Errorf("setting %s must not carry a path", name)
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && loopback(u.Hostname()):
		return nil
	}
	return fmt.Errorf("setting %s must use https, or http to a loopback host", name)
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
