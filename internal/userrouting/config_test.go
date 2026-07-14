package userrouting

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrefixMapAcceptsMappingAndJSONString(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "mapping",
			raw:  "prefix_map:\n  key-1: team-a\n  default: /fallback///\n",
		},
		{
			name: "json string",
			raw:  "prefix_map: '{\"key-1\":\"team-a/\",\"default\":\"fallback/\"}'\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeCPAConfig(t, "key-1")
			cfg, err := decodeRuntimeConfig([]byte("cpa_config_path: "+quotedYAML(path)+"\n"+tt.raw), ConfigureOptions{CWD: t.TempDir()})
			if err != nil {
				t.Fatalf("decodeRuntimeConfig() error = %v", err)
			}
			if got := cfg.PrefixMap["key-1"]; got != "team-a/" {
				t.Fatalf("key prefix = %q, want team-a/", got)
			}
			if got := cfg.PrefixMap["default"]; got != "fallback/" {
				t.Fatalf("default prefix = %q, want fallback/", got)
			}
		})
	}
}

func TestDecodeRuntimeConfigRejectsUnknownMappedKey(t *testing.T) {
	path := writeCPAConfig(t, "key-1")
	raw := "cpa_config_path: " + quotedYAML(path) + "\nprefix_map:\n  missing-key: team/\n"
	_, err := decodeRuntimeConfig([]byte(raw), ConfigureOptions{})
	if err == nil {
		t.Fatal("decodeRuntimeConfig() error = nil, want unknown key error")
	}
}

func TestResolveCPAConfigPathUsesCPAArgument(t *testing.T) {
	cwd := t.TempDir()
	got, err := resolveCPAConfigPath("", ConfigureOptions{
		CWD:  cwd,
		Args: []string{"cliproxyapi", "-config", "custom.yaml"},
	})
	if err != nil {
		t.Fatalf("resolveCPAConfigPath() error = %v", err)
	}
	want := filepath.Join(cwd, "custom.yaml")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func writeCPAConfig(t *testing.T, keys ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := []byte("host: 127.0.0.1\nport: 8317\napi-keys:\n")
	for _, key := range keys {
		raw = append(raw, []byte("  - "+key+"\n")...)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write CPA config: %v", err)
	}
	return path
}

func quotedYAML(value string) string {
	return "'" + value + "'"
}
