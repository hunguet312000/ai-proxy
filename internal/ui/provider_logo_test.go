package ui

import (
	"image"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"literouter/internal/storage"
)

func TestProviderLogoCoversEveryRoutedProvider(t *testing.T) {
	// Every provider the gateway can attribute usage to needs its own mark. The bug
	// this guards against was silent: unknown ids fell through to OpenAI's logo, so
	// Antigravity and Cursor traffic looked like OpenAI traffic on the dashboard.
	for provider, want := range map[string]string{
		"codex": "codex", "cx": "codex",
		// `openai` is its own upstream, not Codex: they were split once the dashboard
		// started filing Gemini traffic under the Codex heading.
		"openai": "openai",
		"claude": "claude", "anthropic": "claude",
		"xai": "xai", "grok": "xai",
		"antigravity": "antigravity", "gemini": "antigravity",
	} {
		if got := providerLogo(provider); got != want {
			t.Errorf("providerLogo(%q) = %q, want %q", provider, got, want)
		}
	}
}

func TestProviderLogoDoesNotBorrowAVendorMarkForCustomUpstreams(t *testing.T) {
	for _, provider := range []string{"custom:fpt-ai", "custom:anything", "", "who-knows"} {
		if got := providerLogo(provider); got == "codex" || got == "claude" || got == "xai" {
			t.Errorf("providerLogo(%q) = %q, want a neutral mark", provider, got)
		}
	}
}

func TestProviderLogoAssetsExist(t *testing.T) {
	// A name with no file behind it renders as a broken image, which is how the drift
	// would come back unnoticed.
	seen := map[string]bool{}
	for _, provider := range []string{"codex", "claude", "xai", "antigravity", "custom:x", "unknown"} {
		seen[providerLogo(provider)] = true
	}
	for name := range seen {
		if _, err := os.Stat(filepath.Join("assets", "providers", name+".png")); err != nil {
			t.Errorf("logo %q has no asset: %v", name, err)
		}
	}
}

func TestProviderLabelNamesCustomUpstreams(t *testing.T) {
	if got := providerLabel("custom:fpt-ai"); got != "fpt-ai (custom)" {
		t.Errorf("providerLabel(custom:fpt-ai) = %q, want it marked as custom", got)
	}
}

func TestProviderAssetsShareOneCanvas(t *testing.T) {
	// Marks come from a dozen sources and are framed side by side. A file on a
	// different canvas lands at a different optical size in every chip, which is the
	// raggedness tools/normalize-provider-logos.py exists to remove — this keeps a new
	// asset from reintroducing it unnoticed.
	entries, err := os.ReadDir(filepath.Join("assets", "providers"))
	if err != nil {
		t.Fatalf("read assets: %v", err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".png" {
			continue
		}
		file, err := os.Open(filepath.Join("assets", "providers", entry.Name()))
		if err != nil {
			t.Fatalf("open %s: %v", entry.Name(), err)
		}
		config, _, err := image.DecodeConfig(file)
		_ = file.Close()
		if err != nil {
			t.Errorf("%s does not decode: %v", entry.Name(), err)
			continue
		}
		if config.Width != 128 || config.Height != 128 {
			t.Errorf("%s is %dx%d, want 128x128 — run tools/normalize-provider-logos.py",
				entry.Name(), config.Width, config.Height)
		}
	}
}

func TestUnknownProviderNeverBorrowsTheOpenAIMark(t *testing.T) {
	// Both fallbacks used to be wrong in different ways: one handed every unknown
	// provider OpenAI's logo, the other named a file that does not exist. On the routing
	// map that put the OpenAI swirl on a custom upstream.
	for _, id := range []string{"custom:fpt-ai", "fpt-ai", "something-new"} {
		info := providerInfoByID(id)
		if info.Icon == "openai" || info.Icon == "codex" {
			t.Errorf("providerInfoByID(%q).Icon = %q, want a neutral mark", id, info.Icon)
		}
		if _, err := os.Stat(filepath.Join("assets", "providers", info.Icon+".png")); err != nil {
			t.Errorf("providerInfoByID(%q).Icon = %q has no asset: %v", id, info.Icon, err)
		}
	}
}

func TestCustomProviderDoesNotWearAVendorLogo(t *testing.T) {
	// Speaking the OpenAI protocol is not the same as being OpenAI, and the routing map
	// is exactly where that confusion costs something.
	infos := customProviderInfos([]storage.CustomProvider{
		{Prefix: "fpt-ai", Name: "FPT AI", BaseURL: "https://example.invalid"},
	})
	if len(infos) != 1 {
		t.Fatalf("infos = %d, want 1", len(infos))
	}
	if infos[0].Icon == "openai" || infos[0].Icon == "codex" || infos[0].Icon == "anthropic" {
		t.Errorf("custom provider icon = %q, want a neutral mark", infos[0].Icon)
	}
	if !strings.Contains(infos[0].Description, "OpenAI compatible") {
		t.Errorf("description = %q, want the protocol still stated in words", infos[0].Description)
	}
}
