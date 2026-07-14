package userrouting

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultCPAPort            = 8317
	defaultModelCacheTTL      = 5 * time.Second
	defaultModelLookupTimeout = 3 * time.Second
)

// PrefixMap accepts either a YAML mapping or a scalar containing a JSON object.
type PrefixMap map[string]string

func (p *PrefixMap) UnmarshalYAML(node *yaml.Node) error {
	if p == nil {
		return errors.New("prefix map receiver is nil")
	}
	var values map[string]string
	switch node.Kind {
	case yaml.MappingNode:
		if err := node.Decode(&values); err != nil {
			return fmt.Errorf("decode prefix_map mapping: %w", err)
		}
	case yaml.ScalarNode:
		var raw string
		if err := node.Decode(&raw); err != nil {
			return fmt.Errorf("decode prefix_map JSON string: %w", err)
		}
		if strings.TrimSpace(raw) == "" {
			values = map[string]string{}
		} else if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return fmt.Errorf("decode prefix_map JSON string: %w", err)
		}
	default:
		return fmt.Errorf("prefix_map must be a mapping or a JSON object string")
	}

	normalized := make(PrefixMap, len(values)+1)
	for key, prefix := range values {
		key = strings.TrimSpace(key)
		if key == "" {
			return errors.New("prefix_map contains an empty API key")
		}
		normalized[key] = normalizePrefix(prefix)
	}
	if _, ok := normalized["default"]; !ok {
		normalized["default"] = ""
	}
	*p = normalized
	return nil
}

func normalizePrefix(prefix string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}

type pluginConfig struct {
	Enabled                     bool                `yaml:"enabled"`
	CPAConfigPath               string              `yaml:"cpa_config_path"`
	PrefixMap                   PrefixMap           `yaml:"prefix_map"`
	QuotaFallback               quotaFallbackConfig `yaml:"quota_fallback"`
	StrictKeyValidation         bool                `yaml:"strict_key_validation"`
	ModelsURL                   string              `yaml:"models_url"`
	ModelCacheTTL               string              `yaml:"model_cache_ttl"`
	ModelLookupTimeout          string              `yaml:"model_lookup_timeout"`
	ModelsTLSInsecureSkipVerify bool                `yaml:"models_tls_insecure_skip_verify"`
	LogRouting                  bool                `yaml:"log_routing"`
}

// quotaFallbackConfig defines ordered model-prefix substitutions used only after
// Codex reports a usage_limit_reached quota error.
type quotaFallbackConfig struct {
	Enabled               bool                `yaml:"enabled"`
	FallbackOnOtherErrors bool                `yaml:"fallback_on_other_errors"`
	Prefixes              map[string][]string `yaml:"prefixes"`
}

type runtimeConfig struct {
	Enabled                     bool
	PrefixMap                   PrefixMap
	QuotaFallback               quotaFallbackConfig
	StrictKeyValidation         bool
	ModelsURL                   string
	ModelCacheTTL               time.Duration
	ModelLookupTimeout          time.Duration
	ModelsTLSInsecureSkipVerify bool
	LogRouting                  bool
	CPAConfig                   *CPAConfigReader
}

type ConfigureOptions struct {
	Args []string
	CWD  string
}

func decodeRuntimeConfig(raw []byte, opts ConfigureOptions) (runtimeConfig, error) {
	cfg := pluginConfig{
		Enabled:             true,
		PrefixMap:           PrefixMap{"default": ""},
		StrictKeyValidation: true,
		ModelCacheTTL:       defaultModelCacheTTL.String(),
		ModelLookupTimeout:  defaultModelLookupTimeout.String(),
		LogRouting:          true,
	}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return runtimeConfig{}, fmt.Errorf("decode plugin config: %w", err)
		}
	}
	if cfg.PrefixMap == nil {
		cfg.PrefixMap = PrefixMap{"default": ""}
	}
	if _, ok := cfg.PrefixMap["default"]; !ok {
		cfg.PrefixMap["default"] = ""
	}
	quotaFallback, err := normalizeQuotaFallback(cfg.QuotaFallback)
	if err != nil {
		return runtimeConfig{}, err
	}

	cacheTTL, err := parseNonNegativeDuration(cfg.ModelCacheTTL, defaultModelCacheTTL, "model_cache_ttl")
	if err != nil {
		return runtimeConfig{}, err
	}
	lookupTimeout, err := parsePositiveDuration(cfg.ModelLookupTimeout, defaultModelLookupTimeout, "model_lookup_timeout")
	if err != nil {
		return runtimeConfig{}, err
	}

	configPath, err := resolveCPAConfigPath(cfg.CPAConfigPath, opts)
	if err != nil {
		return runtimeConfig{}, err
	}
	reader := NewCPAConfigReader(configPath)
	snapshot, err := reader.Snapshot()
	if err != nil {
		return runtimeConfig{}, err
	}
	if cfg.StrictKeyValidation {
		if err := validateMappedKeys(cfg.PrefixMap, snapshot.APIKeys); err != nil {
			return runtimeConfig{}, err
		}
	}

	modelsURL := strings.TrimSpace(cfg.ModelsURL)
	if modelsURL == "" {
		modelsURL, err = deriveModelsURL(snapshot)
		if err != nil {
			return runtimeConfig{}, err
		}
	} else {
		modelsURL, err = normalizeModelsURL(modelsURL)
		if err != nil {
			return runtimeConfig{}, err
		}
	}

	return runtimeConfig{
		Enabled:                     cfg.Enabled,
		PrefixMap:                   clonePrefixMap(cfg.PrefixMap),
		QuotaFallback:               quotaFallback,
		StrictKeyValidation:         cfg.StrictKeyValidation,
		ModelsURL:                   modelsURL,
		ModelCacheTTL:               cacheTTL,
		ModelLookupTimeout:          lookupTimeout,
		ModelsTLSInsecureSkipVerify: cfg.ModelsTLSInsecureSkipVerify,
		LogRouting:                  cfg.LogRouting,
		CPAConfig:                   reader,
	}, nil
}

func normalizeQuotaFallback(input quotaFallbackConfig) (quotaFallbackConfig, error) {
	out := quotaFallbackConfig{
		Enabled:               input.Enabled,
		FallbackOnOtherErrors: input.FallbackOnOtherErrors,
		Prefixes:              make(map[string][]string),
	}
	for rawSource, rawTargets := range input.Prefixes {
		source := normalizePrefixName(rawSource)
		if source == "" {
			return quotaFallbackConfig{}, errors.New("quota_fallback.prefixes contains an empty source prefix")
		}
		seen := make(map[string]struct{}, len(rawTargets))
		for _, rawTarget := range rawTargets {
			target := normalizePrefixName(rawTarget)
			if target == "" {
				return quotaFallbackConfig{}, fmt.Errorf("quota_fallback.prefixes.%s contains an empty target prefix", source)
			}
			if target == source {
				continue
			}
			if _, exists := seen[target]; exists {
				continue
			}
			seen[target] = struct{}{}
			out.Prefixes[source] = append(out.Prefixes[source], target)
		}
	}
	return out, nil
}

func normalizePrefixName(prefix string) string {
	return strings.Trim(strings.TrimSpace(prefix), "/")
}

func parseNonNegativeDuration(raw string, fallback time.Duration, field string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s must be a non-negative duration", field)
	}
	return d, nil
}

func parsePositiveDuration(raw string, fallback time.Duration, field string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", field)
	}
	return d, nil
}

func resolveCPAConfigPath(explicit string, opts ConfigureOptions) (string, error) {
	path := strings.TrimSpace(explicit)
	if path == "" {
		path = configPathFromArgs(opts.Args)
	}
	if path == "" {
		path = strings.TrimSpace(os.Getenv("CPA_CONFIG_PATH"))
	}
	if path == "" {
		path = "config.yaml"
	}
	cwd := strings.TrimSpace(opts.CWD)
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve CPA config path: %w", err)
	}
	return filepath.Clean(abs), nil
}

func configPathFromArgs(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		for _, prefix := range []string{"-config=", "--config="} {
			if strings.HasPrefix(arg, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(arg, prefix))
			}
		}
		if arg == "-config" || arg == "--config" {
			if i+1 < len(args) {
				return strings.TrimSpace(args[i+1])
			}
		}
	}
	return ""
}

type CPAConfigSnapshot struct {
	Host    string
	Port    int
	TLS     bool
	APIKeys map[string]struct{}
}

type rawCPAConfig struct {
	Host    string   `yaml:"host"`
	Port    int      `yaml:"port"`
	APIKeys []string `yaml:"api-keys"`
	TLS     struct {
		Enable bool `yaml:"enable"`
	} `yaml:"tls"`
}

type CPAConfigReader struct {
	path string

	mu      sync.Mutex
	modTime time.Time
	size    int64
	loaded  bool
	value   CPAConfigSnapshot
}

func NewCPAConfigReader(path string) *CPAConfigReader {
	return &CPAConfigReader{path: filepath.Clean(path)}
}

func (r *CPAConfigReader) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

func (r *CPAConfigReader) Snapshot() (CPAConfigSnapshot, error) {
	if r == nil || strings.TrimSpace(r.path) == "" {
		return CPAConfigSnapshot{}, errors.New("CPA config path is empty")
	}
	info, err := os.Stat(r.path)
	if err != nil {
		return CPAConfigSnapshot{}, fmt.Errorf("read CPA config %q: %w", r.path, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded && info.ModTime().Equal(r.modTime) && info.Size() == r.size {
		return cloneCPASnapshot(r.value), nil
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return CPAConfigSnapshot{}, fmt.Errorf("read CPA config %q: %w", r.path, err)
	}
	var decoded rawCPAConfig
	if err := yaml.Unmarshal(raw, &decoded); err != nil {
		return CPAConfigSnapshot{}, fmt.Errorf("decode CPA config %q: %w", r.path, err)
	}
	keys := make(map[string]struct{}, len(decoded.APIKeys))
	for _, key := range decoded.APIKeys {
		key = strings.TrimSpace(key)
		if key != "" {
			keys[key] = struct{}{}
		}
	}
	value := CPAConfigSnapshot{
		Host:    strings.TrimSpace(decoded.Host),
		Port:    decoded.Port,
		TLS:     decoded.TLS.Enable,
		APIKeys: keys,
	}
	r.modTime = info.ModTime()
	r.size = info.Size()
	r.loaded = true
	r.value = value
	return cloneCPASnapshot(value), nil
}

func validateMappedKeys(prefixes PrefixMap, keys map[string]struct{}) error {
	var unknown []string
	for key := range prefixes {
		if key == "default" {
			continue
		}
		if _, ok := keys[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("prefix_map contains API keys not present in CPA api-keys: %s", strings.Join(unknown, ", "))
}

func deriveModelsURL(snapshot CPAConfigSnapshot) (string, error) {
	host := strings.TrimSpace(snapshot.Host)
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	port := snapshot.Port
	if port <= 0 {
		port = defaultCPAPort
	}
	scheme := "http"
	if snapshot.TLS {
		scheme = "https"
	}
	return normalizeModelsURL((&url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort(host, fmt.Sprintf("%d", port)),
	}).String())
}

func normalizeModelsURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("models_url must be an absolute HTTP(S) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("models_url must use http or https")
	}
	path := strings.TrimRight(u.Path, "/")
	if path == "" {
		path = "/v1/models"
	}
	u.Path = path
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func clonePrefixMap(input PrefixMap) PrefixMap {
	out := make(PrefixMap, len(input))
	for key, prefix := range input {
		out[key] = prefix
	}
	return out
}

func cloneCPASnapshot(input CPAConfigSnapshot) CPAConfigSnapshot {
	out := input
	out.APIKeys = make(map[string]struct{}, len(input.APIKeys))
	for key := range input.APIKeys {
		out.APIKeys[key] = struct{}{}
	}
	return out
}
