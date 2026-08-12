package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	setDefaultEnv(t)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("Load() = %#v, want %#v", cfg, Default())
	}
}

func TestLoadYAMLThenEnvironment(t *testing.T) {
	path := writeConfig(t, `
server:
  addr: ":9001"
  read_timeout: 1s
  write_timeout: 2s
  idle_timeout: 3s
  shutdown_timeout: 4s
log:
  level: warn
`)
	t.Setenv("LITEROUTER_SERVER_ADDR", ":8318")
	t.Setenv("LITEROUTER_SERVER_READ_TIMEOUT", "5s")
	t.Setenv("LITEROUTER_LOG_LEVEL", "debug")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.Addr != ":8318" || cfg.Server.ReadTimeout != 5*time.Second {
		t.Fatalf("environment overrides not applied: %#v", cfg.Server)
	}
	if cfg.Server.WriteTimeout != 2*time.Second || cfg.Log.Level != "debug" {
		t.Fatalf("YAML values not retained: %#v", cfg)
	}
	if cfg.Storage.Path != defaultStoragePath {
		t.Fatalf("Storage.Path = %q, want %q", cfg.Storage.Path, defaultStoragePath)
	}
	if cfg.OAuth.RefreshInterval != defaultRefreshInterval {
		t.Fatalf("OAuth.RefreshInterval = %v, want %v", cfg.OAuth.RefreshInterval, defaultRefreshInterval)
	}
}

func TestLoadRouterPlanModel(t *testing.T) {
	path := writeConfig(t, "router:\n  plan_model: \"  yaml-strong  \"\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	// Padding must be stripped: the gateway treats a blank plan model as "disabled",
	// and " " would otherwise be routed to as a model name.
	if cfg.Router.PlanModel != "yaml-strong" {
		t.Fatalf("Router.PlanModel = %q, want %q", cfg.Router.PlanModel, "yaml-strong")
	}

	t.Setenv("LITEROUTER_ROUTER_PLAN_MODEL", "env-strong")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Router.PlanModel != "env-strong" {
		t.Fatalf("Router.PlanModel = %q, want %q", cfg.Router.PlanModel, "env-strong")
	}
}

func TestLoadCompactModelAndContextMode(t *testing.T) {
	path := writeConfig(t, "router:\n  compact_model: \"  yaml-fast  \"\ncontext:\n  mode: \"Aggressive\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Router.CompactModel != "yaml-fast" {
		t.Fatalf("Router.CompactModel = %q, want trimmed yaml value", cfg.Router.CompactModel)
	}
	if cfg.Context.Mode != "aggressive" {
		t.Fatalf("Context.Mode = %q, want lowercased aggressive", cfg.Context.Mode)
	}

	t.Setenv("LITEROUTER_ROUTER_COMPACT_MODEL", "env-fast")
	t.Setenv("LITEROUTER_CONTEXT_MODE", "safe")
	t.Setenv("LITEROUTER_CONTEXT_SUMMARIZE_MODEL", "env-summarizer")
	t.Setenv("LITEROUTER_CONTEXT_KEEP_RECENT_TURNS", "4")
	t.Setenv("LITEROUTER_CONTEXT_SOFT_RATIO", "0.7")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Router.CompactModel != "env-fast" || cfg.Context.Mode != "safe" ||
		cfg.Context.SummarizeModel != "env-summarizer" || cfg.Context.KeepRecentTurns != 4 ||
		cfg.Context.SoftRatio != 0.7 {
		t.Fatalf("env overrides not applied: %+v", cfg.Context)
	}

	t.Setenv("LITEROUTER_CONTEXT_MODE", "bogus")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "context.mode") {
		t.Fatalf("invalid mode error = %v", err)
	}
	t.Setenv("LITEROUTER_CONTEXT_MODE", "safe")
	t.Setenv("LITEROUTER_CONTEXT_SOFT_RATIO", "0.95")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "context ratios") {
		t.Fatalf("bad ratio error = %v", err)
	}
}

func TestLoadRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		content string
		envName string
		envVal  string
		wantErr string
	}{
		{
			name:    "unknown YAML field",
			content: "unknown: true\n",
			wantErr: "field unknown not found",
		},
		{
			name:    "multiple YAML documents",
			content: "server: {}\n---\nlog: {}\n",
			wantErr: "multiple YAML documents",
		},
		{
			name:    "invalid environment duration",
			envName: "LITEROUTER_SERVER_READ_TIMEOUT",
			envVal:  "later",
			wantErr: "LITEROUTER_SERVER_READ_TIMEOUT",
		},
		{
			name:    "invalid OAuth refresh interval",
			envName: "LITEROUTER_OAUTH_REFRESH_INTERVAL",
			envVal:  "later",
			wantErr: "LITEROUTER_OAUTH_REFRESH_INTERVAL",
		},
		{
			name:    "invalid xAI prompt cache flag",
			envName: "LITEROUTER_CACHE_XAI_PROMPT_CACHE_KEY",
			envVal:  "maybe",
			wantErr: "LITEROUTER_CACHE_XAI_PROMPT_CACHE_KEY",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := ""
			if tt.content != "" {
				path = writeConfig(t, tt.content)
			}
			if tt.envName != "" {
				t.Setenv(tt.envName, tt.envVal)
			}

			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"address", func(cfg *Config) { cfg.Server.Addr = "8317" }, "host:port"},
		{"port", func(cfg *Config) { cfg.Server.Addr = ":0" }, "between 1 and 65535"},
		{"timeout", func(cfg *Config) { cfg.Server.IdleTimeout = 0 }, "server.idle_timeout"},
		{"negative write timeout", func(cfg *Config) { cfg.Server.WriteTimeout = -1 }, "server.write_timeout"},
		{"OAuth refresh interval", func(cfg *Config) { cfg.OAuth.RefreshInterval = 0 }, "oauth.refresh_interval"},
		{"storage path", func(cfg *Config) { cfg.Storage.Path = " " }, "storage.path"},
		{"negative usage retention", func(cfg *Config) { cfg.Storage.UsageRetention = -time.Second }, "storage.usage_retention"},
		{"router strategy", func(cfg *Config) { cfg.Router.Strategy = "random" }, "router.strategy"},
		{"empty alias chain", func(cfg *Config) { cfg.Router.ModelAliases["alias"] = nil }, "router.model_aliases"},
		{"cache entries", func(cfg *Config) { cfg.Cache.ResponseMaxEntries = -1 }, "cache.response_max_entries"},
		{"cache prompt bytes", func(cfg *Config) { cfg.Cache.PromptCacheMinBytes = -1 }, "cache.prompt_cache_min_bytes"},
		{"compression mode", func(cfg *Config) { cfg.Cache.CompressionMode = "lossy" }, "cache.compression_mode"},
		{"provider URL", func(cfg *Config) { cfg.Providers.OpenAI.BaseURL = "http://example.com" }, "providers.openai.base_url"},
		{"log level", func(cfg *Config) { cfg.Log.Level = "verbose" }, "log.level"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func setDefaultEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LITEROUTER_SERVER_ADDR", defaultAddr)
	t.Setenv("LITEROUTER_SERVER_READ_TIMEOUT", defaultReadTimeout.String())
	t.Setenv("LITEROUTER_SERVER_WRITE_TIMEOUT", defaultWriteTimeout.String())
	t.Setenv("LITEROUTER_SERVER_IDLE_TIMEOUT", defaultIdleTimeout.String())
	t.Setenv("LITEROUTER_SERVER_SHUTDOWN_TIMEOUT", defaultShutdownTimeout.String())
	t.Setenv("LITEROUTER_OAUTH_REFRESH_INTERVAL", defaultRefreshInterval.String())
	t.Setenv("LITEROUTER_STORAGE_USAGE_RETENTION", defaultUsageRetention.String())
	t.Setenv("LITEROUTER_ROUTER_STRATEGY", defaultRouterStrategy)
	t.Setenv("LITEROUTER_CACHE_RESPONSE_TTL", defaultResponseCacheTTL.String())
	t.Setenv("LITEROUTER_CONTEXT_SUMMARIZE_TIMEOUT", defaultSummarizeTimeout.String())
	t.Setenv("LITEROUTER_CACHE_RESPONSE_MAX_ENTRIES", fmt.Sprint(defaultResponseCacheEntries))
	t.Setenv("LITEROUTER_CACHE_COMPRESSION_MODE", defaultCompressionMode)
	t.Setenv("LITEROUTER_CACHE_PROMPT_MIN_BYTES", fmt.Sprint(defaultPromptCacheMinBytes))
	t.Setenv("LITEROUTER_CACHE_XAI_PROMPT_CACHE_KEY", "false")
	t.Setenv("LITEROUTER_OPENAI_BASE_URL", defaultOpenAIBaseURL)
	t.Setenv("LITEROUTER_XAI_BASE_URL", defaultXAIBaseURL)
	t.Setenv("LITEROUTER_LOG_LEVEL", defaultLogLevel)
}

func TestLoadBuildImagePromptFromEnv(t *testing.T) {
	path := writeConfig(t, "router:\n  image_model: vision\n  text_only_models:\n    - text\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Router.BuildImagePrompt {
		t.Fatal("BuildImagePrompt defaults on")
	}

	t.Setenv("LITEROUTER_ROUTER_BUILD_IMAGE_PROMPT", "true")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Router.BuildImagePrompt {
		t.Fatal("LITEROUTER_ROUTER_BUILD_IMAGE_PROMPT=true not applied")
	}

	t.Setenv("LITEROUTER_ROUTER_BUILD_IMAGE_PROMPT", "not-a-bool")
	if _, err := Load(path); err == nil {
		t.Fatal("invalid LITEROUTER_ROUTER_BUILD_IMAGE_PROMPT accepted")
	}
}
