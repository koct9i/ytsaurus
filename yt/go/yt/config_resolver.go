package yt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.ytsaurus.tech/yt/go/yson"
)

type ConfigBackend string

const (
	ConfigBackendHTTP ConfigBackend = "http"
	ConfigBackendRPC  ConfigBackend = "rpc"
)

type ytConfigProxy struct {
	URL                  string `yson:"url" json:"url"`
	HTTPProxyRole        string `yson:"http_proxy_role" json:"http_proxy_role"`
	RPCProxyRole         string `yson:"rpc_proxy_role" json:"rpc_proxy_role"`
	NetworkName          string `yson:"network_name" json:"network_name"`
	ProxyDiscoveryURL    string `yson:"proxy_discovery_url" json:"proxy_discovery_url"`
	EnableProxyDiscovery *bool  `yson:"enable_proxy_discovery" json:"enable_proxy_discovery"`
	PreferHTTPS          *bool  `yson:"prefer_https" json:"prefer_https"`
	TVMOnly              *bool  `yson:"tvm_only" json:"tvm_only"`
}

type ytConfigProfile struct {
	Proxy     ytConfigProxy `yson:"proxy" json:"proxy"`
	Token     string        `yson:"token" json:"token"`
	TokenPath string        `yson:"token_path" json:"token_path"`
}

type ytConfigFile struct {
	Proxy          ytConfigProxy              `yson:"proxy" json:"proxy"`
	Token          string                     `yson:"token" json:"token"`
	TokenPath      string                     `yson:"token_path" json:"token_path"`
	ConfigVersion  *int64                     `yson:"config_version" json:"config_version"`
	DefaultProfile string                     `yson:"default_profile" json:"default_profile"`
	Profiles       map[string]ytConfigProfile `yson:"profiles" json:"profiles"`
}

func NormalizeConfig(c *Config, backend ConfigBackend) (*Config, error) {
	if c == nil {
		return nil, fmt.Errorf("config is nil")
	}

	fromFile, err := loadYTConfigFromFile()
	if err != nil {
		return nil, err
	}

	resolved := *c

	if resolved.Proxy == "" {
		resolved.Proxy = firstNonEmpty(os.Getenv("YT_PROXY"), fromFile.Proxy.URL)
	}
	if resolved.ProxyRole == "" {
		resolved.ProxyRole = readProxyRoleForBackend(backend, fromFile.Proxy.HTTPProxyRole, fromFile.Proxy.RPCProxyRole)
	}
	if resolved.NetworkName == "" {
		resolved.NetworkName = fromFile.Proxy.NetworkName
	}
	if resolved.HostsPath == "" {
		resolved.HostsPath = firstNonEmpty(os.Getenv("YT_HOSTS"), fromFile.Proxy.ProxyDiscoveryURL)
	}
	if resolved.Token == "" {
		resolved.Token = firstNonEmpty(os.Getenv("YT_TOKEN"), fromFile.Token)
	}
	if resolved.TokenPath == "" {
		resolved.TokenPath = firstNonEmpty(lookupTokenFileFromEnv(), fromFile.TokenPath)
	}
	if !resolved.ReadTokenFromFile && resolved.Token == "" && resolved.TokenPath != "" {
		resolved.ReadTokenFromFile = true
	}

	enableProxyDiscovery, err := readEnableProxyDiscovery(fromFile.Proxy.EnableProxyDiscovery)
	if err != nil {
		return nil, err
	}
	if enableProxyDiscovery != nil && !*enableProxyDiscovery {
		resolved.DisableProxyDiscovery = true
	}

	preferHTTPS, err := readPreferHTTPS(fromFile.Proxy.PreferHTTPS)
	if err != nil {
		return nil, err
	}
	if preferHTTPS {
		resolved.UseTLS = true
	}

	tvmOnly, err := readTVMOnly(fromFile.Proxy.TVMOnly)
	if err != nil {
		return nil, err
	}
	if tvmOnly {
		resolved.UseTVMOnlyEndpoint = true
	}

	return &resolved, nil
}

func readProxyRoleForBackend(backend ConfigBackend, httpProxyRoleFromFile string, rpcProxyRoleFromFile string) string {
	if backend == ConfigBackendRPC {
		return firstNonEmpty(os.Getenv("YT_RPC_PROXY_ROLE"), os.Getenv("YT_HTTP_PROXY_ROLE"), rpcProxyRoleFromFile, httpProxyRoleFromFile)
	}

	return firstNonEmpty(os.Getenv("YT_HTTP_PROXY_ROLE"), os.Getenv("YT_RPC_PROXY_ROLE"), httpProxyRoleFromFile, rpcProxyRoleFromFile)
}

func readEnableProxyDiscovery(enableProxyDiscoveryFromFile *bool) (*bool, error) {
	if value, ok, err := readEnvBool("YT_USE_HOSTS"); err != nil {
		return nil, err
	} else if ok {
		return &value, nil
	}

	return enableProxyDiscoveryFromFile, nil
}

func readPreferHTTPS(preferHTTPSFromFile *bool) (bool, error) {
	if value, ok, err := readEnvBool("YT_USE_TLS"); err != nil {
		return false, err
	} else if ok {
		return value, nil
	}

	return preferHTTPSFromFile != nil && *preferHTTPSFromFile, nil
}

func readTVMOnly(tvmOnlyFromFile *bool) (bool, error) {
	if value, ok, err := readEnvBool("YT_TVM_ONLY"); err != nil {
		return false, err
	} else if ok {
		return value, nil
	}

	return tvmOnlyFromFile != nil && *tvmOnlyFromFile, nil
}

func readEnvBool(key string) (value bool, ok bool, err error) {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return false, false, nil
	}

	intValue, err := strconv.Atoi(raw)
	if err != nil {
		return false, true, fmt.Errorf("failed to parse %s as boolean (expected 0 or 1): %w", key, err)
	}
	return intValue != 0, true, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func loadYTConfigFromFile() (*ytConfigProfile, error) {
	configPath, ok := resolveYTConfigPath()
	if !ok {
		return &ytConfigProfile{}, nil
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read YT config from %s: %w", configPath, err)
	}

	configFormat := strings.ToLower(firstNonEmpty(os.Getenv("YT_CONFIG_FORMAT"), "yson"))
	var parsed ytConfigFile
	switch configFormat {
	case "yson":
		if err = yson.Unmarshal(content, &parsed); err != nil {
			return nil, fmt.Errorf("failed to parse YT config from %s: %w", configPath, err)
		}
	case "json":
		if err = json.Unmarshal(content, &parsed); err != nil {
			return nil, fmt.Errorf("failed to parse YT config from %s: %w", configPath, err)
		}
	default:
		return nil, fmt.Errorf("unsupported config format %q (expected yson or json)", configFormat)
	}

	return extractProfileConfig(parsed)
}

func resolveYTConfigPath() (string, bool) {
	// Keep the same lookup order as python sdk:
	// 1) YT_CONFIG_PATH (if points to existing file),
	// 2) ~/.yt/config,
	// 3) /etc/ytclient.conf.
	currentPath := os.Getenv("YT_CONFIG_PATH")
	if currentPath != "" && isFile(currentPath) {
		return currentPath, true
	}

	configPath := "/etc/ytclient.conf"
	if homeDir, err := os.UserHomeDir(); err == nil && homeDir != "" {
		userConfigPath := filepath.Join(homeDir, ".yt", "config")
		if isFile(userConfigPath) {
			configPath = userConfigPath
		}
	}

	if !isReadable(configPath) {
		return "", false
	}

	return configPath, true
}

func isFile(path string) bool {
	stat, err := os.Stat(path)
	return err == nil && !stat.IsDir()
}

func isReadable(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = file.Close()
	return true
}

func extractProfileConfig(parsed ytConfigFile) (*ytConfigProfile, error) {
	if parsed.ConfigVersion == nil {
		profile := ytConfigProfile{
			Proxy:     parsed.Proxy,
			Token:     parsed.Token,
			TokenPath: parsed.TokenPath,
		}
		return &profile, nil
	}

	if *parsed.ConfigVersion != 2 {
		return nil, fmt.Errorf("unknown config version %d", *parsed.ConfigVersion)
	}
	if parsed.Profiles == nil {
		return nil, fmt.Errorf("missing profiles key in YT config")
	}

	profileName := os.Getenv("YT_CONFIG_PROFILE")
	if profileName == "" {
		profileName = parsed.DefaultProfile
		if profileName == "" {
			return nil, fmt.Errorf("profile has not been set and there is no default profile in the config")
		}
	}

	profile, ok := parsed.Profiles[profileName]
	if !ok {
		return nil, fmt.Errorf("unknown profile %q", profileName)
	}
	return &profile, nil
}
