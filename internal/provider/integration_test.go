package provider

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/leomylonas/fleeting-plugin-incus/internal/config"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	fleeting "gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"gopkg.in/yaml.v2"
)

const integrationEnv = "INCUS_INTEGRATION"

type integrationConfig struct {
	ConnectionType     string
	Endpoint           string
	SocketPath         string
	ProjectPrefix      string
	TLSClientCert      string
	TLSClientKey       string
	TLSServerCert      string
	TLSCA              string
	InsecureSkipVerify bool

	ImageAlias   string
	ImageProject string
	InstanceType string
	Profiles     []string
	UplinkNetwork string
	SSHUsername  string
	WaitTimeout  time.Duration
}

type incusClientConfig struct {
	DefaultRemote string                       `yaml:"default-remote"`
	Remotes       map[string]incusClientRemote `yaml:"remotes"`
}

type incusClientRemote struct {
	Addr     string `yaml:"addr"`
	Protocol string `yaml:"protocol"`
	Project  string `yaml:"project"`
}

func TestIntegrationImageLifecycle(t *testing.T) {
	it := loadIntegrationConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), it.WaitTimeout+2*time.Minute)
	defer cancel()

	root := connectIntegrationClient(t, ctx, it)
	project := createIntegrationProject(t, root, it)
	network := createIntegrationNetwork(t, root, it)
	defer func() {
		cleanupIntegrationProject(t, root, project)
		cleanupIntegrationNetwork(t, root, network)
	}()

	group := &InstanceGroup{Config: providerConfigForIntegration(it, project, network, "img")}
	info, err := group.Init(ctx, hclog.NewNullLogger(), fleeting.Settings{})
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer func() {
		if err := group.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()
	if info.ID == "" || info.MaxSize != 2 {
		t.Fatalf("ProviderInfo = %#v", info)
	}

	succeeded, err := group.Increase(ctx, 1)
	if err != nil {
		t.Fatalf("Increase() error = %v", err)
	}
	if succeeded != 1 {
		t.Fatalf("Increase() succeeded = %d, want 1", succeeded)
	}

	name := waitForManagedInstance(t, ctx, group, it.WaitTimeout)
	waitForConnectInfo(t, ctx, group, name, it.WaitTimeout)

	deleted, err := group.Decrease(ctx, []string{name})
	if err != nil {
		t.Fatalf("Decrease() error = %v", err)
	}
	if len(deleted) != 1 || deleted[0] != name {
		t.Fatalf("Decrease() deleted = %#v, want [%s]", deleted, name)
	}
	assertInstanceMissing(t, root.UseProject(project), name)
}

func TestIntegrationTemplateLifecycle(t *testing.T) {
	it := loadIntegrationConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), it.WaitTimeout+2*time.Minute)
	defer cancel()

	root := connectIntegrationClient(t, ctx, it)
	project := createIntegrationProject(t, root, it)
	network := createIntegrationNetwork(t, root, it)
	defer func() {
		cleanupIntegrationProject(t, root, project)
		cleanupIntegrationNetwork(t, root, network)
	}()
	projectClient := root.UseProject(project)

	templateName := "template-" + uniqueSuffix()
	createStoppedImageInstance(t, ctx, projectClient, it, network, templateName, map[string]string{
		"user.integration.template": "true",
	})

	group := &InstanceGroup{Config: providerConfigForIntegration(it, project, network, "tmpl")}
	group.Image = api.InstanceSource {
                Type: "copy",
                Source: templateName,
                InstanceOnly: true,
            }
	if _, err := group.Init(ctx, hclog.NewNullLogger(), fleeting.Settings{}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer func() {
		if err := group.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	succeeded, err := group.Increase(ctx, 1)
	if err != nil {
		t.Fatalf("Increase() error = %v", err)
	}
	if succeeded != 1 {
		t.Fatalf("Increase() succeeded = %d, want 1", succeeded)
	}

	name := waitForManagedInstance(t, ctx, group, it.WaitTimeout)
	waitForConnectInfo(t, ctx, group, name, it.WaitTimeout)

	deleted, err := group.Decrease(ctx, []string{name})
	if err != nil {
		t.Fatalf("Decrease() error = %v", err)
	}
	if len(deleted) != 1 || deleted[0] != name {
		t.Fatalf("Decrease() deleted = %#v, want [%s]", deleted, name)
	}
}

func TestIntegrationRefusesUnmanagedDecrease(t *testing.T) {
	it := loadIntegrationConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), it.WaitTimeout+2*time.Minute)
	defer cancel()

	root := connectIntegrationClient(t, ctx, it)
	project := createIntegrationProject(t, root, it)
	network := createIntegrationNetwork(t, root, it)
	defer func() {
		cleanupIntegrationProject(t, root, project)
		cleanupIntegrationNetwork(t, root, network)
	}()
	projectClient := root.UseProject(project)

	group := &InstanceGroup{Config: providerConfigForIntegration(it, project, network, "safe")}
	if _, err := group.Init(ctx, hclog.NewNullLogger(), fleeting.Settings{}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer func() {
		if err := group.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	unmanagedName := group.NamePrefix + "unmanaged-" + uniqueSuffix()
	createStoppedImageInstance(t, ctx, projectClient, it, network, unmanagedName, map[string]string{
		group.cfg.NormalizedPoolConfigKey: "different-pool",
	})

	deleted, err := group.Decrease(ctx, []string{unmanagedName})
	if err == nil {
		t.Fatal("Decrease() error = nil, want unmanaged-instance error")
	}
	if len(deleted) != 0 {
		t.Fatalf("Decrease() deleted = %#v, want none", deleted)
	}
	if _, _, err := projectClient.GetInstance(unmanagedName); err != nil {
		t.Fatalf("unmanaged instance should still exist: %v", err)
	}
}

func loadIntegrationConfig(t *testing.T) integrationConfig {
	t.Helper()
	if os.Getenv(integrationEnv) != "1" {
		t.Skipf("set %s=1 to run Incus integration tests", integrationEnv)
	}

	imageAlias := os.Getenv("INCUS_INTEGRATION_IMAGE_ALIAS")
	if imageAlias == "" {
		t.Skip("INCUS_INTEGRATION_IMAGE_ALIAS is required for Incus integration tests")
	}

	waitTimeout := 2 * time.Minute
	if raw := os.Getenv("INCUS_INTEGRATION_WAIT_TIMEOUT"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("parse INCUS_INTEGRATION_WAIT_TIMEOUT: %v", err)
		}
		waitTimeout = parsed
	}

	it, found, err := loadIncusClientIntegrationConfig()
	if err != nil {
		t.Fatalf("load Incus client config: %v", err)
	}
	if !found && os.Getenv("INCUS_INTEGRATION_CONNECTION_TYPE") == "" {
		t.Fatalf("INCUS_INTEGRATION_CONNECTION_TYPE is required when %s cannot be read", incusClientConfigPath())
	}

	it.ConnectionType = getenvDefault("INCUS_INTEGRATION_CONNECTION_TYPE", it.ConnectionType)
	it.Endpoint = getenvDefault("INCUS_INTEGRATION_ENDPOINT", it.Endpoint)
	it.SocketPath = getenvDefault("INCUS_INTEGRATION_SOCKET_PATH", it.SocketPath)
	it.ProjectPrefix = getenvDefault("INCUS_INTEGRATION_PROJECT_PREFIX", "fleeting-plugin-incus-it")
	it.TLSClientCert = getenvDefault("INCUS_INTEGRATION_TLS_CLIENT_CERT", it.TLSClientCert)
	it.TLSClientKey = getenvDefault("INCUS_INTEGRATION_TLS_CLIENT_KEY", it.TLSClientKey)
	it.TLSServerCert = os.Getenv("INCUS_INTEGRATION_TLS_SERVER_CERT")
	it.TLSCA = os.Getenv("INCUS_INTEGRATION_TLS_CA")
	it.InsecureSkipVerify = os.Getenv("INCUS_INTEGRATION_INSECURE_SKIP_VERIFY") == "1"
	it.ImageAlias = imageAlias
	it.ImageProject = getenvDefault("INCUS_INTEGRATION_IMAGE_PROJECT", api.ProjectDefaultName)
	it.InstanceType = getenvDefault("INCUS_INTEGRATION_INSTANCE_TYPE", config.InstanceContainer)
	it.Profiles = splitList(getenvDefault("INCUS_INTEGRATION_PROFILES", "default"))
	it.UplinkNetwork = getenvDefault("INCUS_INTEGRATION_UPLINK_NETWORK", "uplink")
	it.SSHUsername = getenvDefault("INCUS_INTEGRATION_SSH_USERNAME", "root")
	it.WaitTimeout = waitTimeout

	if it.ConnectionType == config.ConnectionHTTPS {
		it.TLSClientCert, it.TLSClientKey = config.DefaultIncusClientCertAndKey(it.TLSClientCert, it.TLSClientKey)
	}
	validateIntegrationConnectionConfig(t, it)
	return it
}

func loadIncusClientIntegrationConfig() (integrationConfig, bool, error) {
	path := incusClientConfigPath()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return integrationConfig{}, false, nil
		}
		return integrationConfig{}, false, err
	}

	var cfg incusClientConfig
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return integrationConfig{}, false, err
	}

	remoteName := getenvDefault("INCUS_INTEGRATION_REMOTE", cfg.DefaultRemote)
	if remoteName == "" {
		return integrationConfig{}, false, fmt.Errorf("%s has no default-remote", path)
	}
	remote, ok := cfg.Remotes[remoteName]
	if !ok {
		return integrationConfig{}, false, fmt.Errorf("%s does not define remote %q", path, remoteName)
	}

	it := integrationConfig{}
	addr := strings.TrimSpace(remote.Addr)
	switch {
	case addr == "", addr == "unix://", remote.Protocol == "unix":
		it.ConnectionType = config.ConnectionUnix
	case strings.HasPrefix(addr, "unix://"):
		it.ConnectionType = config.ConnectionUnix
		it.SocketPath = strings.TrimPrefix(addr, "unix://")
	case strings.HasPrefix(addr, "https://"):
		it.ConnectionType = config.ConnectionHTTPS
		it.Endpoint = addr
		it.TLSClientCert, it.TLSClientKey = config.DefaultIncusClientCertAndKey("", "")
		if path := config.DefaultIncusServerCertPath(remoteName); fileExists(path) {
			it.TLSServerCert = path
		}
	default:
		it.ConnectionType = config.ConnectionHTTPS
		it.Endpoint = addr
		it.TLSClientCert, it.TLSClientKey = config.DefaultIncusClientCertAndKey("", "")
		if path := config.DefaultIncusServerCertPath(remoteName); fileExists(path) {
			it.TLSServerCert = path
		}
	}

	return it, true, nil
}

func incusClientConfigPath() string {
	return getenvDefault("INCUS_INTEGRATION_CONFIG_PATH", config.DefaultIncusClientConfigPath())
}

func validateIntegrationConnectionConfig(t *testing.T, it integrationConfig) {
	t.Helper()
	switch it.ConnectionType {
	case config.ConnectionUnix:
	case config.ConnectionHTTPS:
		if it.Endpoint == "" {
			t.Fatal("INCUS_INTEGRATION_ENDPOINT is required for https integration tests")
		}
	default:
		t.Fatalf("INCUS_INTEGRATION_CONNECTION_TYPE must be %q or %q, got %q", config.ConnectionUnix, config.ConnectionHTTPS, it.ConnectionType)
	}
}

func connectIntegrationClient(t *testing.T, ctx context.Context, it integrationConfig) incus.InstanceServer {
	t.Helper()
	tlsClientCert := resolveIntegrationPEM(t, "INCUS_INTEGRATION_TLS_CLIENT_CERT", it.TLSClientCert)
	tlsClientKey := resolveIntegrationPEM(t, "INCUS_INTEGRATION_TLS_CLIENT_KEY", it.TLSClientKey)
	tlsServerCert := resolveIntegrationPEM(t, "INCUS_INTEGRATION_TLS_SERVER_CERT", it.TLSServerCert)
	tlsCA := resolveIntegrationPEM(t, "INCUS_INTEGRATION_TLS_CA", it.TLSCA)

	args := &incus.ConnectionArgs{
		UserAgent:          "fleeting-plugin-incus-integration-test",
		TLSClientCert:      tlsClientCert,
		TLSClientKey:       tlsClientKey,
		TLSServerCert:      tlsServerCert,
		TLSCA:              tlsCA,
		InsecureSkipVerify: it.InsecureSkipVerify,
	}

	var client incus.InstanceServer
	var err error
	switch it.ConnectionType {
	case config.ConnectionUnix:
		client, err = incus.ConnectIncusUnixWithContext(ctx, it.SocketPath, args)
	case config.ConnectionHTTPS:
		client, err = incus.ConnectIncusWithContext(ctx, it.Endpoint, args)
	default:
		t.Fatalf("unsupported INCUS_INTEGRATION_CONNECTION_TYPE %q", it.ConnectionType)
	}
	if err != nil {
		t.Fatalf("connect to Incus: %v", err)
	}
	return client
}

func resolveIntegrationPEM(t *testing.T, name string, value string) string {
	t.Helper()
	resolved, err := config.ResolvePEMValue(value)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	return resolved
}

func createIntegrationProject(t *testing.T, root incus.InstanceServer, it integrationConfig) string {
	t.Helper()
	project := it.ProjectPrefix + "-" + uniqueSuffix()
	err := root.CreateProject(api.ProjectsPost{
		Name: project,
		ProjectPut: api.ProjectPut{Config: map[string]string{
			"features.images":          "false",
			"features.networks":        "false",
			"features.profiles":        "false",
			"features.storage.volumes": "false",
		}},
	})
	if err != nil {
		t.Fatalf("create integration project %q: %v", project, err)
	}
	return project
}

func cleanupIntegrationProject(t *testing.T, root incus.InstanceServer, project string) {
	t.Helper()
	if project == "" {
		return
	}
	if err := root.DeleteProjectForce(project); err != nil {
		if fallbackErr := root.DeleteProject(project); fallbackErr != nil && !api.StatusErrorCheck(fallbackErr, http.StatusNotFound) {
			t.Fatalf("delete integration project %q: force=%v fallback=%v", project, err, fallbackErr)
		}
	}
}

func createIntegrationNetwork(t *testing.T, root incus.InstanceServer, it integrationConfig) string {
	t.Helper()
	name := "fit-ovn-" + uniqueSuffix()
	err := root.CreateNetwork(api.NetworksPost{
		Name: name,
		Type: "ovn",
		NetworkPut: api.NetworkPut{Config: api.ConfigMap{
			"network":      it.UplinkNetwork,
			"ipv4.address": "auto",
			"ipv4.nat":     "true",
			"ipv6.address": "none",
		}},
	})
	if err != nil {
		t.Fatalf("create integration network %q: %v", name, err)
	}
	return name
}

func cleanupIntegrationNetwork(t *testing.T, root incus.InstanceServer, network string) {
	t.Helper()
	if network == "" {
		return
	}
	if err := root.DeleteNetwork(network); err != nil && !api.StatusErrorCheck(err, http.StatusNotFound) {
		t.Fatalf("delete integration network %q: %v", network, err)
	}
}

func providerConfigForIntegration(it integrationConfig, project string, network string, poolSuffix string) config.Config {
	return config.Config{
		ConnectionType:     it.ConnectionType,
		Endpoint:           it.Endpoint,
		SocketPath:         it.SocketPath,
		Project:            project,
		TLSClientCert:      it.TLSClientCert,
		TLSClientKey:       it.TLSClientKey,
		TLSServerCert:      it.TLSServerCert,
		TLSCA:              it.TLSCA,
		InsecureSkipVerify: it.InsecureSkipVerify,
		OperationTimeout:   config.Duration(2 * time.Minute),
		ConnectTimeout:     config.Duration(30 * time.Second),
		NamePrefix:         "fit-" + poolSuffix + "-",
		PoolID:             "pool-" + poolSuffix + "-" + uniqueSuffix(),
		PoolConfigKey:      "fleeting.integration.pool",
		MaxSize:            2,
		InstanceType:       it.InstanceType,
		Profiles:           it.Profiles,
		Network:            network,
		NetworkInterface:   "eth0",
		Image: api.InstanceSource{
                        Type: "image",
			Alias:   it.ImageAlias,
			Project: it.ImageProject,
		},
		SSHUsername:   it.SSHUsername,
		AddressFamily: "inet",
	}
}

func createStoppedImageInstance(t *testing.T, ctx context.Context, client incus.InstanceServer, it integrationConfig, network string, name string, instanceConfig map[string]string) {
	t.Helper()
	req := api.InstancesPost{
		Name: name,
		Type: api.InstanceType(it.InstanceType),
		InstancePut: api.InstancePut{
			Config:   api.ConfigMap(instanceConfig),
			Devices: api.DevicesMap{
				"eth0": map[string]string{
					"type":    "nic",
					"network": network,
					"name":    "eth0",
				},
			},
			Profiles: it.Profiles,
		},
		Source: api.InstanceSource{
			Type:    "image",
			Alias:   it.ImageAlias,
			Project: it.ImageProject,
		},
		Start: false,
	}
	op, err := client.CreateInstance(req)
	if err != nil {
		t.Fatalf("create stopped instance %q: %v", name, err)
	}
	if err := op.WaitContext(ctx); err != nil {
		t.Fatalf("wait create stopped instance %q: %v", name, err)
	}
}

func waitForManagedInstance(t *testing.T, ctx context.Context, group *InstanceGroup, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var found string
		err := group.Update(ctx, func(instance string, state fleeting.State) {
			if found == "" && state == fleeting.StateRunning {
				found = instance
			}
		})
		if err != nil {
			t.Fatalf("Update() while waiting for managed instance: %v", err)
		}
		if found != "" {
			return found
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out after %s waiting for managed instance to become running", timeout)
	return ""
}

func waitForConnectInfo(t *testing.T, ctx context.Context, group *InstanceGroup, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		info, err := group.ConnectInfo(ctx, name)
		if err == nil {
			if info.InternalAddr == "" && info.ExternalAddr == "" {
				t.Fatalf("ConnectInfo(%q) returned no address: %#v", name, info)
			}
			if err := group.Heartbeat(ctx, name); err != nil {
				t.Fatalf("Heartbeat(%q) error = %v", name, err)
			}
			return
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out after %s waiting for ConnectInfo(%q): last error: %v", timeout, name, lastErr)
}

func assertInstanceMissing(t *testing.T, client incus.InstanceServer, name string) {
	t.Helper()
	_, _, err := client.GetInstance(name)
	if err == nil {
		t.Fatalf("instance %q still exists", name)
	}
	if !api.StatusErrorCheck(err, http.StatusNotFound) {
		t.Fatalf("GetInstance(%q) error = %v, want not found", name, err)
	}
}

func uniqueSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func getenvDefault(key string, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func splitList(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
