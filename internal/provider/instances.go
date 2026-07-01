package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/leomylonas/fleeting-plugin-incus/internal/cloudinit"
	"github.com/leomylonas/fleeting-plugin-incus/internal/config"
	"github.com/leomylonas/fleeting-plugin-incus/internal/incusclient"
	"github.com/lxc/incus/v6/shared/api"
)

func (g *InstanceGroup) managedInstances(ctx context.Context) ([]api.Instance, error) {
	// Incus does not have a first-class tag API for instances. Ownership is
	// therefore encoded as a normal instance config key under user.*, and we also
	// require the configured name prefix. Both must match.
	cfg, client := g.snapshot()

	// Request only the configured Incus instance type. That keeps container and
	// VM pools from seeing each other even if they share a project.
	instances, err := client.GetInstances(api.InstanceType(cfg.InstanceType))
	if err != nil {
		return nil, err
	}

	managed := make([]api.Instance, 0, len(instances))
	for _, inst := range instances {
		// Listing can return a lot of instances in a busy project. Check the
		// context during filtering so Runner shutdown does not wait for a full
		// scan if cancellation has already been requested.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if g.isManaged(inst) {
			managed = append(managed, inst)
		}
	}

	return managed, nil
}

func (g *InstanceGroup) isManaged(inst api.Instance) bool {
	// The double check is deliberate. The prefix makes human inspection easy and
	// avoids scanning unrelated names; the user.* config key is the authoritative
	// pool identity that prevents prefix collisions from becoming destructive.
	cfg, _ := g.snapshot()

	// Both checks are required:
	// 1. the prefix marks the naming scheme this provider owns;
	// 2. the user.* config value marks the exact pool identity.
	return strings.HasPrefix(inst.Name, cfg.NamePrefix) &&
		inst.Config[cfg.NormalizedPoolConfigKey] == cfg.PoolID
}

func (g *InstanceGroup) createRequest(name string) api.InstancesPost {
	// Build the Incus create payload. Start with user-provided instance config,
	// add the managed-pool identity, and optionally add cloud-init data when the
	// plugin is responsible for injecting an SSH public key.
	cfg, _ := g.snapshot()
	instanceConfig := map[string]string{}
	for k, v := range cfg.InstanceConfig {
		// Copy the map instead of mutating cfg.InstanceConfig. cfg is shared
		// between calls, so mutation here would leak per-instance fields into
		// future creates.
		instanceConfig[k] = v
	}

	// Every created instance gets the pool marker. Later Update/Decrease calls
	// depend on this value to distinguish managed instances from unrelated ones.
	instanceConfig[cfg.NormalizedPoolConfigKey] = cfg.PoolID
	if cfg.Privileged {
		// Incus uses security.privileged to request an un-namespaced container.
		// Keep this as a top-level plugin option so Runner configs do not need to
		// know the Incus-specific instance config key.
		instanceConfig["security.privileged"] = "true"
	}
	if cfg.PublicKey != "" {
		// Only set cloud-init.user-data when the plugin is injecting a key. When
		// PublicKey is empty, the image/template is expected to be preconfigured
		// and any user-provided cloud-init config should be left untouched.
		instanceConfig["cloud-init.user-data"] = cloudinit.SSHUserData(cfg.ConnectorConfig.Username, cfg.PublicKey)
	}

	devices := map[string]map[string]string{}
	for deviceName, device := range cfg.Devices {
		copiedDevice := map[string]string{}
		for key, value := range device {
			copiedDevice[key] = value
		}
		devices[deviceName] = copiedDevice
	}
	if cfg.Network != "" {
		// Allow a simple network name in plugin config without forcing users to
		// spell out a full NIC device stanza when the defaults are sufficient.
		deviceName := cfg.NetworkInterface
		if deviceName == "" {
			deviceName = "eth0"
		}
		if _, exists := devices[deviceName]; !exists {
			devices[deviceName] = map[string]string{
				"type":    "nic",
				"network": cfg.Network,
				"name":    deviceName,
			}
		}
	}
	applyRootDiskOverrides(devices, cfg.StoragePool, cfg.RootDiskSize)

	// InstancesPost is the Incus API request body for creating either a container
	// or VM. Start=true asks Incus to boot/start the instance immediately after
	// creating it.
	req := api.InstancesPost{
		Name: name,
		Type: api.InstanceType(cfg.InstanceType),
		InstancePut: api.InstancePut{
			Config:   api.ConfigMap(instanceConfig),
			Devices:  api.DevicesMap(devices),
			Profiles: append([]string(nil), cfg.Profiles...),
		},
		Start: true,
                Source: cfg.Image,
	}
	return req
}

func applyRootDiskOverrides(devices map[string]map[string]string, storagePool string, rootDiskSize string) {
	// Incus applies per-instance disk devices over profile devices. When users
	// configure storage_pool or root_disk_size, add just enough of a root disk
	// device to override those fields while preserving any explicit device
	// options already present.
	if storagePool == "" && rootDiskSize == "" {
		return
	}

	deviceName := rootDiskDeviceName(devices)
	device := devices[deviceName]
	if device == nil {
		device = map[string]string{}
		devices[deviceName] = device
	}
	if device["type"] == "" {
		device["type"] = "disk"
	}
	if device["path"] == "" {
		device["path"] = "/"
	}
	if storagePool != "" {
		device["pool"] = storagePool
	}
	if rootDiskSize != "" {
		device["size"] = rootDiskSize
	}
}

func rootDiskDeviceName(devices map[string]map[string]string) string {
	for deviceName, device := range devices {
		if device["type"] == "disk" && device["path"] == "/" {
			return deviceName
		}
	}
	if _, exists := devices["root"]; !exists {
		return "root"
	}
	for i := 1; ; i++ {
		deviceName := fmt.Sprintf("root%d", i)
		if _, exists := devices[deviceName]; !exists {
			return deviceName
		}
	}
}

func (g *InstanceGroup) snapshot() (config.Normalized, incusclient.Client) {
	// Copy the current config/client references under a read lock. The returned
	// values are immutable enough for method-local use and avoid holding a mutex
	// while network calls are in flight.
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.cfg, g.client
}

func (g *InstanceGroup) newName() (string, error) {
	// Incus instance names must be unique. A cryptographically random suffix is
	// simple, process-safe, and avoids maintaining shared counters.
	cfg, _ := g.snapshot()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	// Twelve hex characters gives enough space for practical uniqueness while
	// keeping instance names readable in `incus list`.
	return cfg.NamePrefix + hex.EncodeToString(b[:]), nil
}
