package yt

import (
	"context"
	"fmt"
	"os"
	"runtime"
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

func TestGetTokenOrRunCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows: shell commands differ")
	}

	ctx := context.Background()

	t.Run("explicit_token_wins", func(t *testing.T) {
		c := Config{
			Token:        "explicit-token",
			TokenCommand: []string{"sh", "-c", "echo cmd-token"},
		}
		token, err := c.GetTokenOrRunCommand(ctx)
		require.NoError(t, err)
		require.Equal(t, "explicit-token", token)
	})

	t.Run("yt_token_env_wins_over_command", func(t *testing.T) {
		t.Setenv("YT_TOKEN", "env-token")
		c := Config{
			TokenCommand: []string{"sh", "-c", "echo cmd-token"},
		}
		token, err := c.GetTokenOrRunCommand(ctx)
		require.NoError(t, err)
		require.Equal(t, "env-token", token)
	})

	t.Run("token_command_argv", func(t *testing.T) {
		c := Config{
			TokenCommand: []string{"sh", "-c", "echo mytoken"},
		}
		token, err := c.GetTokenOrRunCommand(ctx)
		require.NoError(t, err)
		require.Equal(t, "mytoken", token)
	})

	t.Run("yt_token_command_env", func(t *testing.T) {
		t.Setenv("YT_TOKEN_COMMAND", "echo envtoken")
		c := Config{}
		token, err := c.GetTokenOrRunCommand(ctx)
		require.NoError(t, err)
		require.Equal(t, "envtoken", token)
	})

	t.Run("token_command_first_line_only", func(t *testing.T) {
		c := Config{
			TokenCommand: []string{"sh", "-c", "printf 'firstline\nsecondline\n'"},
		}
		token, err := c.GetTokenOrRunCommand(ctx)
		require.NoError(t, err)
		require.Equal(t, "firstline", token)
	})

	t.Run("token_command_strips_trailing_newline", func(t *testing.T) {
		c := Config{
			TokenCommand: []string{"sh", "-c", "printf 'mytoken\n'"},
		}
		token, err := c.GetTokenOrRunCommand(ctx)
		require.NoError(t, err)
		require.Equal(t, "mytoken", token)
	})

	t.Run("token_command_nonzero_exit_fails", func(t *testing.T) {
		c := Config{
			TokenCommand: []string{"sh", "-c", "echo secret-not-found >&2; exit 1"},
		}
		_, err := c.GetTokenOrRunCommand(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "token_command")
	})

	t.Run("token_command_empty_output_fails", func(t *testing.T) {
		c := Config{
			TokenCommand: []string{"sh", "-c", "true"},
		}
		_, err := c.GetTokenOrRunCommand(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "empty")
	})

	t.Run("token_command_does_not_leak_yt_token", func(t *testing.T) {
		// Verify filterTokenEnv removes YT_TOKEN from the child environment.
		env := []string{"PATH=/usr/bin", "YT_TOKEN=secret", "HOME=/home/user"}
		filtered := filterTokenEnv(env)
		require.Equal(t, []string{"PATH=/usr/bin", "HOME=/home/user"}, filtered)
	})

	t.Run("no_fallback_after_command_failure", func(t *testing.T) {
		// Create a token file with a valid token.
		tmpFile, err := os.CreateTemp(t.TempDir(), "yt-token-*")
		require.NoError(t, err)
		_, err = tmpFile.WriteString("file-token\n")
		require.NoError(t, err)
		require.NoError(t, tmpFile.Close())

		// Even though ReadTokenFromFile is set and a valid token file exists,
		// a failing token_command must not fall back to the file.
		c := Config{
			TokenCommand:      []string{"sh", "-c", "exit 1"},
			ReadTokenFromFile: true,
		}
		// Override the path resolution by setting the env var so it points to our tmpFile.
		t.Setenv("YT_TOKEN_FILE", tmpFile.Name())

		_, err = c.GetTokenOrRunCommand(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "token_command")
	})
}
