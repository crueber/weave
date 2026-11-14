// Package config reads process configuration from the environment.
package config

import (
	"os"
	"strconv"
)

type Config struct {
	Addr, BaseURL, DataDir, SecretKey string
	AgentInstallURL                   string // release page shown in the column "Agent prompt"
	TrustProxy                        bool
	SessionHours                      int
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func Load() Config {
	h, _ := strconv.Atoi(env("WEAVE_SESSION_HOURS", "24"))
	return Config{
		Addr:            env("WEAVE_ADDR", ":8080"),
		BaseURL:         env("WEAVE_BASE_URL", "http://localhost:8080"),
		DataDir:         env("WEAVE_DATA_DIR", "./data"),
		SecretKey:       env("WEAVE_SECRET_KEY", ""),
		AgentInstallURL: env("WEAVE_AGENT_INSTALL_URL", "https://github.com/crueber/weave/releases/latest/download"),
		TrustProxy:      os.Getenv("WEAVE_TRUST_PROXY") == "true",
		SessionHours:    h,
	}
}
