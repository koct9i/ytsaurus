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

func NormalizeConfig(c *Config, backend ConfigBackend) (*Config, error) {
	if c == nil {
		return nil, fmt.Errorf("config is nil")
	}

	fromFile, err := loadYTConfigFromFile()
	if err != nil {
		return nil, err
	}

	resolved := *c

	proxyFromFile := readString(fromFile, "proxy", "url")
	httpProxyRoleFromFile := readString(fromFile, "proxy", "http_proxy_role")
	rpcProxyRoleFromFile := readString(fromFile, "proxy", "rpc_proxy_role")
	networkNameFromFile := readString(fromFile, "proxy", "network_name")
	proxyDiscoveryURLFromFile := readString(fromFile, "proxy", "proxy_discovery_url")
	enableProxyDiscoveryFromFile := readBoolPtr(fromFile, "proxy", "enable_proxy_discovery")
	preferHTTPSFromFile := readBoolPtr(fromFile, "proxy", "prefer_https")
	tvmOnlyFromFile := readBoolPtr(fromFile, "proxy", "tvm_only")
	tokenFromFile := readString(fromFile, "token")
	tokenPathFromFile := readString(fromFile, "token_path")

	if resolved.Proxy == "" {
		resolved.Proxy = firstNonEmpty(os.Getenv("YT_PROXY"), proxyFromFile)
	}
	if resolved.ProxyRole == "" {
		resolved.ProxyRole = readProxyRoleForBackend(backend, httpProxyRoleFromFile, rpcProxyRoleFromFile)
	}
	if resolved.NetworkName == "" {
		resolved.NetworkName = networkNameFromFile
	}
	if resolved.HostsPath == "" {
		resolved.HostsPath = firstNonEmpty(os.Getenv("YT_HOSTS"), proxyDiscoveryURLFromFile)
	}
	if resolved.Token == "" {
		resolved.Token = firstNonEmpty(os.Getenv("YT_TOKEN"), tokenFromFile)
	}
	if resolved.TokenPath == "" {
		tokenPathFromEnv := firstNonEmpty(os.Getenv("YT_TOKEN_FILE"), os.Getenv("YT_TOKEN_PATH"))
		resolved.TokenPath = firstNonEmpty(tokenPathFromEnv, tokenPathFromFile)
	}
	if !resolved.ReadTokenFromFile && resolved.Token == "" && resolved.TokenPath != "" {
		resolved.ReadTokenFromFile = true
	}

	enableProxyDiscovery, err := readEnableProxyDiscovery(enableProxyDiscoveryFromFile)
	if err != nil {
		return nil, err
	}
	if enableProxyDiscovery != nil && !*enableProxyDiscovery {
		resolved.DisableProxyDiscovery = true
	}

	preferHTTPS, err := readPreferHTTPS(preferHTTPSFromFile)
	if err != nil {
		return nil, err
	}
	if preferHTTPS {
		resolved.UseTLS = true
	}

	tvmOnly, err := readTVMOnly(tvmOnlyFromFile)
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
		return false, true, fmt.Errorf("failed to parse %s as int bool: %w", key, err)
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

func loadYTConfigFromFile() (map[string]any, error) {
	configPath, ok := resolveYTConfigPath()
	if !ok {
		return nil, nil
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read YT config from %s: %w", configPath, err)
	}

	configFormat := strings.ToLower(firstNonEmpty(os.Getenv("YT_CONFIG_FORMAT"), "yson"))
	var parsed map[string]any
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
		return nil, fmt.Errorf("incorrect config_format %q", configFormat)
	}

	return extractProfileConfig(parsed)
}

func resolveYTConfigPath() (string, bool) {
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

func extractProfileConfig(parsed map[string]any) (map[string]any, error) {
	configVersion, ok := readInt(parsed["config_version"])
	if !ok {
		return parsed, nil
	}

	if configVersion != 2 {
		return nil, fmt.Errorf("unknown config version %d", configVersion)
	}

	profilesAny, ok := parsed["profiles"]
	if !ok {
		return nil, fmt.Errorf("missing profiles key in YT config")
	}
	profiles, ok := profilesAny.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("profiles should be object")
	}

	profileName := os.Getenv("YT_CONFIG_PROFILE")
	if profileName == "" {
		defaultProfile, _ := parsed["default_profile"].(string)
		profileName = defaultProfile
		if profileName == "" {
			return nil, fmt.Errorf("profile has not been set and there is no default profile in the config")
		}
	}

	profileAny, ok := profiles[profileName]
	if !ok {
		return nil, fmt.Errorf("unknown profile %q", profileName)
	}

	profile, ok := profileAny.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("profile %q should be object", profileName)
	}

	return profile, nil
}

func readInt(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int8:
		return int64(v), true
	case int16:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case uint:
		return int64(v), true
	case uint8:
		return int64(v), true
	case uint16:
		return int64(v), true
	case uint32:
		return int64(v), true
	case uint64:
		return int64(v), true
	case float64:
		return int64(v), true
	case float32:
		return int64(v), true
	default:
		return 0, false
	}
}

func readString(config map[string]any, path ...string) string {
	value := readPath(config, path...)
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func readBoolPtr(config map[string]any, path ...string) *bool {
	value := readPath(config, path...)
	if value == nil {
		return nil
	}

	switch b := value.(type) {
	case bool:
		return &b
	case int:
		v := b != 0
		return &v
	case int8:
		v := b != 0
		return &v
	case int16:
		v := b != 0
		return &v
	case int32:
		v := b != 0
		return &v
	case int64:
		v := b != 0
		return &v
	case uint:
		v := b != 0
		return &v
	case uint8:
		v := b != 0
		return &v
	case uint16:
		v := b != 0
		return &v
	case uint32:
		v := b != 0
		return &v
	case uint64:
		v := b != 0
		return &v
	case float32:
		v := b != 0
		return &v
	case float64:
		v := b != 0
		return &v
	default:
		return nil
	}
}

func readPath(config map[string]any, path ...string) any {
	if config == nil {
		return nil
	}

	var value any = config
	for _, key := range path {
		m, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value, ok = m[key]
		if !ok {
			return nil
		}
	}
	return value
}
