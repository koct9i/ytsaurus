package yt

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClusterURL(t *testing.T) {
	type config struct {
		proxy                   string
		defaultSuffix           string
		useTLS                  bool
		useTVM                  bool
		ytProxyUrlAliasingValue string
	}

	tvmHTTPPort := fmt.Sprint(TVMOnlyHTTPProxyPort)
	tvmHTTPSPort := fmt.Sprint(TVMOnlyHTTPSProxyPort)

	for _, test := range []struct {
		name        string
		config      config
		expectedURL string
	}{
		{
			"localhost",
			config{proxy: "localhost"},
			"http://localhost",
		},
		{
			"localhost w port",
			config{proxy: "localhost:23924"},
			"http://localhost:23924",
		},
		{
			"short name w port",
			config{proxy: "hostname:123"},
			"http://hostname:123",
		},
		{
			"short_host_w_scheme",
			config{proxy: "https://cluster1"},
			"https://cluster1",
		},
		{
			"cluster name",
			config{proxy: "cluster1"},
			"http://cluster1.yt.yandex.net",
		},
		{
			"proxy_fqdn",
			config{proxy: "sas4-5340-proxy-cluster1.man-pre.yp-c.yandex.net:80"},
			"http://sas4-5340-proxy-cluster1.man-pre.yp-c.yandex.net:80",
		},
		{
			"tvm_only",
			config{proxy: "cluster1", useTVM: true},
			"http://tvm.cluster1.yt.yandex.net:" + tvmHTTPPort,
		},
		{
			"tvm_only_https",
			config{proxy: "https://cluster1", useTVM: true},
			"https://tvm.cluster1:" + tvmHTTPSPort,
		},
		{
			"tvm_only_http",
			config{proxy: "http://cluster1", useTVM: true},
			"http://tvm.cluster1:" + tvmHTTPPort,
		},
		{
			"default_suffix",
			config{proxy: "cluster1", defaultSuffix: ".imaginary.yt.yandex.net"},
			"http://cluster1.imaginary.yt.yandex.net",
		},
		{
			"cluster_name config https",
			config{proxy: "cluster1", useTLS: true},
			"https://cluster1.yt.yandex.net",
		},
		{
			"localhost",
			config{proxy: "localhost", useTLS: true},
			"https://localhost",
		},
		{
			"localhost override",
			config{proxy: "http://localhost", useTLS: true},
			"http://localhost",
		},
		{
			"cluster_name url priority over config 1",
			config{proxy: "http://cluster1.yt.domain.net", useTLS: true},
			"http://cluster1.yt.domain.net",
		},
		{
			"cluster_name url priority over config 2",
			config{proxy: "https://cluster1.yt.domain.net", useTLS: false},
			"https://cluster1.yt.domain.net",
		},
		{
			"proxy_fqdn config https",
			config{proxy: "sas4-5340-proxy-cluster1.man-pre.yp-c.domain.net:80", useTLS: true},
			"https://sas4-5340-proxy-cluster1.man-pre.yp-c.domain.net:80",
		},
		{
			"tvm_only config https",
			config{proxy: "cluster1", useTVM: true, useTLS: true},
			"https://tvm.cluster1.yt.yandex.net:" + tvmHTTPSPort,
		},
		{
			"default_suffix config https",
			config{proxy: "cluster1", defaultSuffix: ".imaginary.yt.cluster.net", useTLS: true},
			"https://cluster1.imaginary.yt.cluster.net",
		},
		{
			"ipv4",
			config{proxy: "127.0.0.1"},
			"http://127.0.0.1",
		},
		{
			"ipv4 with port",
			config{proxy: "127.0.0.1:23924"},
			"http://127.0.0.1:23924",
		},
		{
			"ipv4 with scheme and port",
			config{proxy: "https://127.0.0.1:23924"},
			"https://127.0.0.1:23924",
		},
		{
			"ipv6",
			config{proxy: "[::1]"},
			"http://[::1]",
		},
		{
			"ipv4-mapped",
			config{proxy: "[::ffff:127.0.0.1]"},
			"http://[::ffff:127.0.0.1]",
		},
		{
			"ipv6 with port",
			config{proxy: "[::1]:23924"},
			"http://[::1]:23924",
		},
		{
			"ipv6 with scheme and port",
			config{proxy: "https://[::1]:23924"},
			"https://[::1]:23924",
		},
		{
			"alias found",
			config{
				proxy:                   "cluster3",
				ytProxyUrlAliasingValue: `{"cluster2"="http://127.0.0.1:30799";"cluster3"="http://127.0.0.1:30399";}`,
			},
			"http://127.0.0.1:30399",
		},
		{
			"alias not found",
			config{
				proxy:                   "cluster4",
				ytProxyUrlAliasingValue: `{"cluster2"="http://127.0.0.1:30799";"cluster3"="http://127.0.0.1:30399";}`,
			},
			"http://cluster4.yt.yandex.net",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.config.defaultSuffix != "" {
				t.Skip("default_suffix is not supported yet")
			}
			if test.config.ytProxyUrlAliasingValue != "" {
				require.NoError(t, os.Setenv("YT_PROXY_URL_ALIASING_CONFIG", test.config.ytProxyUrlAliasingValue))
				defer func() {
					require.NoError(t, os.Unsetenv("YT_PROXY_URL_ALIASING_CONFIG"))
				}()
			}

			conf := Config{
				Proxy:              test.config.proxy,
				UseTLS:             test.config.useTLS,
				UseTVMOnlyEndpoint: test.config.useTVM,
			}

			clusterURL, err := conf.GetClusterURL()
			require.NoError(t, err)

			url := clusterURL.Scheme + "://" + clusterURL.Address
			require.Equal(t, test.expectedURL, url)
		})
	}
}

func TestNormalizeConfigFromFileAndEnv(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".yt")
	require.NoError(t, os.MkdirAll(configDir, 0o755))

	configPath := filepath.Join(configDir, "config")
	content := []byte(`{
		"proxy"={
			"url"="file-proxy";
			"http_proxy_role"="file-http-role";
			"rpc_proxy_role"="file-rpc-role";
			"network_name"="file-network";
			"proxy_discovery_url"="file-hosts";
			"enable_proxy_discovery"=%false;
			"prefer_https"=%true;
			"tvm_only"=%true;
		};
		"token"="file-token";
	}`)
	require.NoError(t, os.WriteFile(configPath, content, 0o644))

	t.Setenv("HOME", home)
	t.Setenv("YT_PROXY", "env-proxy")
	t.Setenv("YT_HTTP_PROXY_ROLE", "env-http-role")
	t.Setenv("YT_HOSTS", "env-hosts")
	t.Setenv("YT_USE_HOSTS", "1")
	t.Setenv("YT_TOKEN", "env-token")

	resolved, err := NormalizeConfig(&Config{}, ConfigBackendHTTP)
	require.NoError(t, err)

	require.Equal(t, "env-proxy", resolved.Proxy)
	require.Equal(t, "env-http-role", resolved.ProxyRole)
	require.Equal(t, "file-network", resolved.NetworkName)
	require.Equal(t, "env-hosts", resolved.HostsPath)
	require.Equal(t, "env-token", resolved.Token)
	require.False(t, resolved.DisableProxyDiscovery)
	require.True(t, resolved.UseTLS)
	require.True(t, resolved.UseTVMOnlyEndpoint)
}

func TestNormalizeConfigV2Profile(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".yt")
	require.NoError(t, os.MkdirAll(configDir, 0o755))

	configPath := filepath.Join(configDir, "config")
	content := []byte(`{
		"config_version"=2;
		"default_profile"="profile1";
		"profiles"={
			"profile1"={"proxy"={"url"="p1";};"token"="t1";};
			"profile2"={"proxy"={"url"="p2";};"token"="t2";};
		};
	}`)
	require.NoError(t, os.WriteFile(configPath, content, 0o644))

	t.Setenv("HOME", home)
	t.Setenv("YT_CONFIG_PROFILE", "profile2")

	resolved, err := NormalizeConfig(&Config{}, ConfigBackendHTTP)
	require.NoError(t, err)
	require.Equal(t, "p2", resolved.Proxy)
	require.Equal(t, "t2", resolved.Token)
}

func TestNormalizeConfigExplicitOverrides(t *testing.T) {
	t.Setenv("YT_PROXY", "env-proxy")
	t.Setenv("YT_HTTP_PROXY_ROLE", "env-http-role")
	t.Setenv("YT_TOKEN", "env-token")

	conf := &Config{
		Proxy:     "explicit-proxy",
		ProxyRole: "explicit-role",
		Token:     "explicit-token",
	}

	resolved, err := NormalizeConfig(conf, ConfigBackendHTTP)
	require.NoError(t, err)
	require.Equal(t, "explicit-proxy", resolved.Proxy)
	require.Equal(t, "explicit-role", resolved.ProxyRole)
	require.Equal(t, "explicit-token", resolved.Token)
}

func TestNormalizeConfigTokenPath(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("token-from-file\n"), 0o644))
	t.Setenv("YT_TOKEN_PATH", tokenFile)

	resolved, err := NormalizeConfig(&Config{}, ConfigBackendHTTP)
	require.NoError(t, err)
	require.True(t, resolved.ReadTokenFromFile)
	require.Equal(t, tokenFile, resolved.TokenPath)
	require.Equal(t, "token-from-file", resolved.GetToken())
}

func TestNormalizeConfigInvalidBool(t *testing.T) {
	t.Setenv("YT_USE_HOSTS", "invalid")

	_, err := NormalizeConfig(&Config{}, ConfigBackendHTTP)
	require.Error(t, err)
}
