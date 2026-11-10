package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// Profile is one saved server + credential pair.
type Profile struct {
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
}

// ConfigFile is ~/.config/weave/config.json (or %AppData%\weave\config.json).
// The legacy format {"URL":..,"Token":..} is read as the "default" profile.
type ConfigFile struct {
	Current  string             `json:"current,omitempty"`
	Profiles map[string]Profile `json:"profiles,omitempty"`
}

func configDir() string {
	if d := os.Getenv("WEAVE_CONFIG_DIR"); d != "" {
		return d
	}
	d, err := os.UserConfigDir()
	if err != nil || d == "" {
		h, _ := os.UserHomeDir()
		d = filepath.Join(h, ".config")
	}
	return filepath.Join(d, "weave")
}

func configPath() string { return filepath.Join(configDir(), "config.json") }

func loadConfigFile() ConfigFile {
	var cf ConfigFile
	b, err := os.ReadFile(configPath())
	if err != nil {
		return ConfigFile{Profiles: map[string]Profile{}}
	}
	_ = json.Unmarshal(b, &cf)
	if cf.Profiles == nil {
		var legacy Profile
		if json.Unmarshal(b, &legacy) == nil && (legacy.URL != "" || legacy.Token != "") {
			cf = ConfigFile{Current: "default", Profiles: map[string]Profile{"default": legacy}}
		}
	}
	if cf.Profiles == nil {
		cf.Profiles = map[string]Profile{}
	}
	return cf
}

func saveConfigFile(cf ConfigFile) error {
	b, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	if err := ensurePrivateDir(configDir()); err != nil {
		return err
	}
	return atomicWrite(configPath(), append(b, '\n'), 0o600)
}

func (cf ConfigFile) names() []string {
	var n []string
	for k := range cf.Profiles {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// Resolved is the effective connection: flags > env > profile > default.
type Resolved struct {
	URL, Token, Profile string
}

func resolveConfig(flagURL, flagToken, flagProfile string) Resolved {
	cf := loadConfigFile()
	name := flagProfile
	if name == "" {
		name = os.Getenv("WEAVE_PROFILE")
	}
	if name == "" {
		name = cf.Current
	}
	if name == "" {
		name = "default"
	}
	p := cf.Profiles[name]
	r := Resolved{URL: p.URL, Token: p.Token, Profile: name}
	if v := os.Getenv("WEAVE_URL"); v != "" {
		r.URL = v
	}
	if v := os.Getenv("WEAVE_TOKEN"); v != "" {
		r.Token = v
	}
	if flagURL != "" {
		r.URL = flagURL
	}
	if flagToken != "" {
		r.Token = flagToken
	}
	if r.URL == "" {
		r.URL = "http://localhost:8080"
	}
	return r
}
