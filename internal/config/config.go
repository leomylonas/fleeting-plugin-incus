package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"github.com/lxc/incus/v6/shared/api"
)

const (
	// ConnectionUnix uses Incus' local Unix socket. This is the usual choice
	// when GitLab Runner runs on an Incus cluster member.
	ConnectionUnix = "unix"
	// ConnectionHTTPS connects to a remote Incus API endpoint with client TLS.
	ConnectionHTTPS = "https"

	// These strings intentionally match Incus API instance type values.
	InstanceContainer = "container"
	InstanceVM        = "virtual-machine"

	// Defaults match a typical Linux SSH worker image.
	DefaultPoolConfigKey = "fleeting.pool"
	DefaultSSHPort       = 22
	DefaultOS            = "linux"
	DefaultArch          = "amd64"
)

var poolKeyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	// Fleeting config is decoded from JSON. Accept Go duration strings like
	// "30s" or "5m", and also raw duration numbers for test/code callers.
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if s == "" {
			*d = 0
			return nil
		}

		parsed, err := time.ParseDuration(s)
		if err != nil {
			return err
		}

		*d = Duration(parsed)
		return nil
	}

	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}

	*d = Duration(n)
	return nil
}

func (d Duration) Std() time.Duration {
	return time.Duration(d)
}

type Config struct {
	// Connection fields describe how the plugin reaches Incus. Fleeting
	// populates this flat struct from `[runners.autoscaler.plugin_config]`.
	ConnectionType     string   `json:"connection_type"`
	Endpoint           string   `json:"endpoint"`
	SocketPath         string   `json:"socket_path"`
	Project            string   `json:"project"`
	TLSClientCert      string   `json:"tls_client_cert"`
	TLSClientKey       string   `json:"tls_client_key"`
	TLSServerCert      string   `json:"tls_server_cert"`
	TLSCA              string   `json:"tls_ca"`
	InsecureSkipVerify bool     `json:"insecure_skip_verify"`
	ConnectTimeout     Duration `json:"connect_timeout"`
	OperationTimeout   Duration `json:"operation_timeout"`

	// Pool identity is used for discovery and destructive-operation safety.
	// Incus has no tag API for instances, so PoolConfigKey is normalized to a
	// user.* instance config key and paired with NamePrefix.
	NamePrefix    string `json:"name_prefix"`
	PoolID        string `json:"pool_id"`
	PoolConfigKey string `json:"pool_config_key"`
	MaxSize       int    `json:"max_size"`

	// Instance fields become the Incus create payload. InstanceConfig is named
	// this way to avoid colliding with the embedded Config type in provider code,
	// but its public JSON/TOML key remains `config`.
	InstanceType   string                       `json:"instance_type"`
	Privileged     bool                         `json:"privileged"`
	Profiles       []string                     `json:"profiles"`
	InstanceConfig map[string]string            `json:"config"`
	Devices        map[string]map[string]string `json:"devices"`
	Target         string                       `json:"target"`
	StoragePool    string                       `json:"storage_pool"`
	RootDiskSize   string                       `json:"root_disk_size"`

        Image api.InstanceSource `json:"image"`

	// SSH public keys are optional. If omitted, the selected image/template must
	// already contain credentials usable by SSHUsername.
	SSHUsername      string `json:"ssh_username"`
	SSHPublicKey     string `json:"ssh_public_key"`
	SSHPublicKeyPath string `json:"ssh_public_key_path"`

	// Connector/address fields control what ConnectInfo returns to GitLab Runner
	// after the Incus instance has started.
	Network          string           `json:"network"`
	OS               string           `json:"os"`
	Arch             string           `json:"arch"`
	NetworkInterface string           `json:"network_interface"`
	AddressFamily    string           `json:"address_family"`
	Connector        ConnectorOptions `json:"connector"`
}

/* The old "TemplateSource" can be emulated with
 *      type: copy
 *      source: [[the name]]
 *      project: [[same]]
 *      instance_only: [[same]]
 */

type ConnectorOptions struct {
	// Keepalive and Timeout override Fleeting's connector defaults for SSH.
	Keepalive Duration `json:"keepalive"`
	Timeout   Duration `json:"timeout"`
}

type Normalized struct {
	// Normalized wraps raw config with values derived during Init. Provider code
	// uses this form so defaults and validation are applied once.
	Config
	NormalizedPoolConfigKey string
	PublicKey               string
	ConnectorConfig         provider.ConnectorConfig
}

func Normalize(in Config, settings provider.Settings) (Normalized, error) {
	// Work on a copy so validation/defaulting does not mutate the struct that
	// Fleeting populated.
	cfg := in
	if cfg.ConnectionType == "" {
		cfg.ConnectionType = ConnectionUnix
	}
	if cfg.PoolConfigKey == "" {
		cfg.PoolConfigKey = DefaultPoolConfigKey
	}
	if cfg.InstanceType == "" {
		cfg.InstanceType = InstanceContainer
	}
	if cfg.OS == "" {
		cfg.OS = DefaultOS
	}
	if cfg.Arch == "" {
		cfg.Arch = DefaultArch
	}
	if cfg.AddressFamily == "" {
		cfg.AddressFamily = "inet"
	}
	if cfg.OperationTimeout == 0 {
		cfg.OperationTimeout = Duration(5 * time.Minute)
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = Duration(30 * time.Second)
	}
	if cfg.ConnectionType == ConnectionHTTPS {
		cfg.TLSClientCert, cfg.TLSClientKey = DefaultIncusClientCertAndKey(cfg.TLSClientCert, cfg.TLSClientKey)
	}

	// Validate structural config before reading files or connecting to Incus.
	if err := validate(cfg); err != nil {
		return Normalized{}, err
	}

	// Public key config is optional. Empty means "do not inject cloud-init; the
	// image/template already knows how Runner will SSH in".
	publicKey := strings.TrimSpace(cfg.SSHPublicKey)
	if cfg.SSHPublicKeyPath != "" {
		b, err := os.ReadFile(cfg.SSHPublicKeyPath)
		if err != nil {
			return Normalized{}, fmt.Errorf("read ssh_public_key_path: %w", err)
		}
		publicKey = strings.TrimSpace(string(b))
	}

	// Merge plugin-level SSH settings with Fleeting's connector settings. The
	// resulting ConnectorConfig is returned later by ConnectInfo.
	connector := settings.ConnectorConfig
	connector.OS = firstNonEmpty(connector.OS, cfg.OS)
	connector.Arch = firstNonEmpty(connector.Arch, cfg.Arch)
	if connector.Protocol == "" {
		connector.Protocol = provider.ProtocolSSH
	}
	if connector.ProtocolPort == 0 {
		connector.ProtocolPort = DefaultSSHPort
	}
	connector.Username = firstNonEmpty(connector.Username, cfg.SSHUsername)
	connector.UseStaticCredentials = true
	if cfg.Connector.Keepalive != 0 {
		connector.Keepalive = cfg.Connector.Keepalive.Std()
	}
	if cfg.Connector.Timeout != 0 {
		connector.Timeout = cfg.Connector.Timeout.Std()
	}
	if err := connector.Protocol.Valid(); err != nil {
		return Normalized{}, err
	}
	if connector.Protocol != provider.ProtocolSSH {
		return Normalized{}, errors.New("only ssh connector protocol is supported")
	}
	if connector.Username == "" {
		return Normalized{}, errors.New("ssh_username or autoscaler connector username is required")
	}

	return Normalized{
		// Incus requires custom instance metadata to live under the user.*
		// namespace. Users configure the shorter suffix so they cannot
		// accidentally provide a key outside the user namespace.
		Config:                  cfg,
		NormalizedPoolConfigKey: "user." + cfg.PoolConfigKey,
		PublicKey:               publicKey,
		ConnectorConfig:         connector,
	}, nil
}

func validate(cfg Config) error {
	// Fail early during Init so bad config never creates Incus resources.
	switch cfg.ConnectionType {
	case ConnectionUnix:
	case ConnectionHTTPS:
		if cfg.Endpoint == "" {
			return errors.New("endpoint is required when connection_type is https")
		}
		if cfg.TLSClientCert == "" || cfg.TLSClientKey == "" {
			return errors.New("tls_client_cert and tls_client_key are required when connection_type is https")
		}
	default:
		return fmt.Errorf("connection_type must be %q or %q", ConnectionUnix, ConnectionHTTPS)
	}

	if cfg.NamePrefix == "" {
		return errors.New("name_prefix is required")
	}
	if cfg.PoolID == "" {
		return errors.New("pool_id is required")
	}
	if cfg.MaxSize < 0 {
		return errors.New("max_size cannot be negative")
	}
	if strings.HasPrefix(cfg.PoolConfigKey, "user.") {
		return errors.New("pool_config_key must not include the user. prefix")
	}
	// Keep accepted key syntax intentionally simple. Incus accepts arbitrary
	// user.* keys, but common identifier characters avoid whitespace/escaping
	// mistakes in config files and examples.
	if !poolKeyRE.MatchString(cfg.PoolConfigKey) {
		return errors.New("pool_config_key must contain only letters, numbers, dots, underscores, and dashes")
	}

        if cfg.Image.Type != "image" && cfg.Image.Type != "copy" {
            return errors.New("image.type is required (must be either 'image' or 'copy')")
        }

	switch cfg.InstanceType {
	case InstanceContainer, InstanceVM:
	default:
		return fmt.Errorf("instance_type must be %q or %q", InstanceContainer, InstanceVM)
	}
	if cfg.Privileged && cfg.InstanceType != InstanceContainer {
		return errors.New("privileged is only supported for container instances")
	}

	if cfg.SSHPublicKey != "" && cfg.SSHPublicKeyPath != "" {
		return errors.New("configure only one of ssh_public_key or ssh_public_key_path")
	}

	return nil
}

func firstNonEmpty(values ...string) string {
	// Small helper used when plugin config should fill an empty Fleeting setting
	// without hiding a value the caller already supplied.
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
