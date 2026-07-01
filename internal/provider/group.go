package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hashicorp/go-hclog"
	"github.com/leomylonas/fleeting-plugin-incus/internal/config"
	"github.com/leomylonas/fleeting-plugin-incus/internal/incusclient"
	"github.com/lxc/incus/v6/shared/api"
	fleeting "gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

type InstanceGroup struct {
	// Config is embedded so the Fleeting plugin loader can populate these fields
	// directly from `[runners.autoscaler.plugin_config]` JSON/TOML data. The
	// fields themselves live in internal/config so validation is kept separate
	// from Incus lifecycle code.
	config.Config

	// mu protects cfg/client/settings because GitLab Runner may call provider
	// methods from different goroutines. The helper method snapshot() returns a
	// copy of the references so network calls happen without holding the lock.
	mu sync.RWMutex
	// logger is the structured logger supplied by Fleeting. It is stored for
	// future provider logging without threading it through every helper.
	logger hclog.Logger
	// cfg is the validated, defaulted version of Config. Runtime methods use cfg
	// instead of Config so every path sees the same normalized values.
	cfg config.Normalized
	// client is the live Incus API client scoped to the configured project/target.
	client incusclient.Client
	// settings is the raw Fleeting connector settings passed to Init. cfg also
	// contains the merged connector config used by ConnectInfo.
	settings fleeting.Settings
}

func (g *InstanceGroup) Init(ctx context.Context, logger hclog.Logger, settings fleeting.Settings) (fleeting.ProviderInfo, error) {

	// Fleeting calls Init exactly once when GitLab Runner starts the plugin.
	// This is the right place to validate user config, merge Fleeting connector
	// defaults, and establish the Incus API client.
	cfg, err := config.Normalize(g.Config, settings)
	if err != nil {
		return fleeting.ProviderInfo{}, err
	}

	// If a public key is provided, the plugin owns cloud-init.user-data because
	// it must inject that key consistently. If no public key is configured, the
	// image/template is expected to already contain usable SSH credentials, and
	// user-provided cloud-init config is allowed.
	if cfg.PublicKey != "" {
		if _, ok := cfg.InstanceConfig["cloud-init.user-data"]; ok {
			return fleeting.ProviderInfo{}, errors.New("config.cloud-init.user-data cannot be set when the plugin injects ssh_public_key")
		}
	}

	// The Incus client stores the context passed during construction and uses it
	// for later requests. Fleeting's Init context is RPC-scoped, so incusclient
	// deliberately switches to a background context internally rather than
	// retaining this Init context across the provider lifetime.
	client, err := incusclient.New(ctx, cfg)
	if err != nil {
		return fleeting.ProviderInfo{}, fmt.Errorf("connect to incus: %w", err)
	}

	g.mu.Lock()
	// Store normalized config and the live Incus client behind a mutex. Fleeting
	// can call lifecycle methods concurrently, so readers use snapshot().
	g.logger = logger
	g.cfg = cfg
	g.client = client
	g.settings = settings

	g.mu.Unlock()

	// ProviderInfo tells Fleeting what this plugin instance represents. MaxSize
	// is reported here so Fleeting can account for provider capacity, while
	// Capabilities advertises optional Fleeting features this provider accepts.
	return fleeting.ProviderInfo{
		ID:           cfg.PoolID,
		MaxSize:      cfg.MaxSize,
		Version:      Version.String(),
		BuildInfo:    Version.BuildInfo(),
		Capabilities: []fleeting.Capability{fleeting.CapabilitySuspendResume},
	}, nil
}

func (g *InstanceGroup) Update(ctx context.Context, update func(instance string, state fleeting.State)) error {
	// Update is Fleeting's reconciliation hook. We list all Incus instances of
	// the configured type, filter down to instances owned by this pool, then
	// report each current state back through the callback supplied by Fleeting.
	instances, err := g.managedInstances(ctx)
	if err != nil {
            return err
	}

	cfg, client := g.snapshot()
	for _, inst := range instances {
		// Fleeting tracks instances by string ID. Incus instance names are stable
		// IDs in this provider, so the name is passed through directly.
		state := StateFromIncus(inst.StatusCode)
		if state == fleeting.StateRunning {
			instanceState, _, err := client.GetInstanceState(inst.Name)
			if err != nil {
				return fmt.Errorf("get state for %s: %w", inst.Name, err)
			}
			if SelectAddress(instanceState, cfg.NetworkInterface, cfg.AddressFamily) == "" {
				// A started instance without a usable address is not ready for
				// Runner yet. Keep it in creating so taskscaler waits instead of
				// immediately failing ConnectInfo and recycling it.
				state = fleeting.StateCreating
			}
		}
		update(inst.Name, state)
	}

	return nil
}

func (g *InstanceGroup) Increase(ctx context.Context, n int) (int, error) {
	// Increase is intentionally written as partial-success logic. Fleeting can
	// retry failed capacity later, while any instances that were successfully
	// requested should be counted immediately.
	if n <= 0 {
		return 0, nil
	}

	cfg, client := g.snapshot()

	// Read the current pool size before creating anything. This count is based
	// on Incus state, not local memory, so restarts and other runner managers see
	// the same source of truth.
	instances, err := g.managedInstances(ctx)
	if err != nil {
		return 0, err
	}

	allowed := n
	if cfg.MaxSize > 0 {
		// max_size is a provider-side safety cap in addition to GitLab Runner's
		// autoscaler limits. A value of 0 means "do not cap in the plugin".
		remaining := cfg.MaxSize - len(instances)
		if remaining <= 0 {
			return 0, nil
		}
		if remaining < allowed {
			allowed = remaining
		}
	}

	var errs []error
	succeeded := 0
	for i := 0; i < allowed; i++ {
		// Respect cancellation between each create request. This keeps shutdowns
		// responsive if Runner stops while a scale-out batch is in progress.
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}

		name, err := g.newName()
		if err != nil {
			// A random-name failure is unlikely, but it should not hide any
			// instances created earlier in this same Increase call.
			errs = append(errs, err)
			continue
		}

                g.logger.Info("Requesting creation of new instance",
                    "name", name)
		req := g.createRequest(name)
		// CreateInstance returns an Incus operation. Waiting for it means the
		// instance creation/start request has completed before we report success.
		op, err := client.CreateInstance(req)
		if err != nil {
			errs = append(errs, fmt.Errorf("create %s: %w", name, err))
			continue
		}

		waitCtx, cancel := context.WithTimeout(ctx, cfg.OperationTimeout.Std())
		// Use a timeout for Incus operations as well as connection setup. If Incus
		// accepts an operation but never completes it, Fleeting gets a retryable
		// error instead of waiting forever.
		err = op.WaitContext(waitCtx)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("wait create %s: %w", name, err))
			continue
		}

		succeeded++
	}

	// Return both the number that succeeded and a joined error for failures.
	// Fleeting's contract allows partial success; callers should not discard
	// successfully requested capacity just because later items failed.
	return succeeded, errors.Join(errs...)
}

func (g *InstanceGroup) Decrease(ctx context.Context, instances []string) ([]string, error) {
	// Decrease is the destructive path, so every instance is re-read from Incus
	// and checked for ownership immediately before stop/delete. This prevents a
	// bad or stale Fleeting input from deleting unrelated instances.
	cfg, client := g.snapshot()
	var succeeded []string
	var errs []error

	for _, name := range instances {
		// Fetch the instance fresh before deleting it. Ownership may have changed
		// since Fleeting last saw this name in an Update call.
		inst, _, err := client.GetInstance(name)
		if err != nil {
			errs = append(errs, fmt.Errorf("get %s: %w", name, err))
			continue
		}
		if !g.isManaged(*inst) {
			errs = append(errs, fmt.Errorf("refusing to delete unmanaged instance %s", name))
			continue
		}

		if inst.StatusCode != api.Stopped {
			// Incus requires stopped instances for deletion in normal cases. Force
			// stop is acceptable here because Fleeting instances are disposable
			// CI workers and Runner asked us to remove them.
			op, err := client.UpdateInstanceState(name, api.InstanceStatePut{
				Action:  "stop",
				Timeout: 30,
				Force:   true,
			}, "")
			if err != nil {
				errs = append(errs, fmt.Errorf("stop %s: %w", name, err))
				continue
			}
			waitCtx, cancel := context.WithTimeout(ctx, cfg.OperationTimeout.Std())
			// Wait for the stop before deleting so the next DeleteInstance call is
			// operating on a state Incus normally accepts.
			err = op.WaitContext(waitCtx)
			cancel()
			if err != nil {
				errs = append(errs, fmt.Errorf("wait stop %s: %w", name, err))
				continue
			}
		}

		op, err := client.DeleteInstance(name)
		if err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", name, err))
			continue
		}
		waitCtx, cancel := context.WithTimeout(ctx, cfg.OperationTimeout.Std())
		// Delete is also an asynchronous Incus operation. Waiting here means a
		// returned success really indicates the delete completed.
		err = op.WaitContext(waitCtx)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("wait delete %s: %w", name, err))
			continue
		}

		succeeded = append(succeeded, name)
	}

	// As with Increase, deletions can partially succeed. Returning succeeded
	// names prevents Fleeting from retrying deletes that already completed.
	return succeeded, errors.Join(errs...)
}

func (g *InstanceGroup) ConnectInfo(ctx context.Context, instance string) (fleeting.ConnectInfo, error) {
	// ConnectInfo tells GitLab Runner how to reach a worker. It does not create
	// credentials; it returns the SSH username/key policy determined in Init and
	// the current address discovered from Incus runtime state.
	cfg, client := g.snapshot()
	inst, _, err := client.GetInstance(instance)
	if err != nil {
		return fleeting.ConnectInfo{}, err
	}
	if !g.isManaged(*inst) {
		// Even non-destructive methods check ownership. This prevents accidental
		// credential/address disclosure for instances outside this pool.
		return fleeting.ConnectInfo{}, fmt.Errorf("instance %s is not managed by this pool", instance)
	}

	// Incus separates static instance config from runtime state. Network
	// addresses live in runtime state, so fetch it after confirming ownership.
	state, _, err := client.GetInstanceState(instance)
	if err != nil {
		return fleeting.ConnectInfo{}, err
	}
	addr := SelectAddress(state, cfg.NetworkInterface, cfg.AddressFamily)
	if addr == "" {
		return fleeting.ConnectInfo{}, fmt.Errorf("instance %s has no %s address", instance, cfg.AddressFamily)
	}

	info := fleeting.ConnectInfo{
		// Start with the connector config prepared during Init: SSH protocol,
		// username, port, timeout, and any key/password settings supplied by
		// Fleeting itself.
		ConnectorConfig: cfg.ConnectorConfig,
		ID:              instance,
	}
	if isPrivateAddress(addr) {
		// Fleeting distinguishes internal and external addresses. Incus clusters
		// commonly use RFC1918 addresses, so private/link-local addresses are
		// reported as internal.
		info.InternalAddr = addr
	} else {
		info.ExternalAddr = addr
	}

	return info, nil
}

func (g *InstanceGroup) Heartbeat(ctx context.Context, instance string) error {
	// Heartbeat is a lightweight health gate before Runner connects. It verifies
	// that the instance is still owned by this pool, is running, and has an
	// address that ConnectInfo can return.
	_, client := g.snapshot()
	inst, _, err := client.GetInstance(instance)
	if err != nil {
		return err
	}
	if !g.isManaged(*inst) {
		// An unmanaged instance is considered unhealthy from this provider's
		// perspective, even if it exists and is running in Incus.
		return fleeting.ErrInstanceUnhealthy
	}
	if inst.StatusCode != api.Running && inst.StatusCode != api.Started {
		return fmt.Errorf("%w: instance status is %s", fleeting.ErrInstanceUnhealthy, inst.Status)
	}

	state, _, err := client.GetInstanceState(instance)
	if err != nil {
		return err
	}
	cfg, _ := g.snapshot()
	if SelectAddress(state, cfg.NetworkInterface, cfg.AddressFamily) == "" {
		// Runner cannot use a worker that has not acquired a reachable address.
		// Returning ErrInstanceUnhealthy lets Fleeting retry later or recycle it.
		return fmt.Errorf("%w: no usable address", fleeting.ErrInstanceUnhealthy)
	}

	return nil
}

func (g *InstanceGroup) Suspend(ctx context.Context, instances []string) ([]string, error) {
	// Incus "freeze" maps naturally to Fleeting suspend: the instance remains
	// allocated but execution is paused.
	return g.changeState(ctx, instances, "freeze")
}

func (g *InstanceGroup) Resume(ctx context.Context, instances []string) ([]string, error) {
	// Incus "unfreeze" resumes an instance previously frozen by Suspend.
	return g.changeState(ctx, instances, "unfreeze")
}

func (g *InstanceGroup) Shutdown(ctx context.Context) error {
	// Shutdown is called when Runner is done with the plugin process. Disconnect
	// closes Incus event/HTTP resources held by the client.
	_, client := g.snapshot()
	if client != nil {
		client.Disconnect()
	}
	return nil
}

func (g *InstanceGroup) changeState(ctx context.Context, instances []string, action string) ([]string, error) {
	// Suspend/Resume are fire-and-forget in the Fleeting contract. We only need
	// to submit the Incus state-change operation successfully; later Update calls
	// observe the eventual state.
	_, client := g.snapshot()
	var succeeded []string
	var errs []error

	for _, name := range instances {
		// Re-check ownership here because Suspend/Resume can affect running
		// workloads. Never trust caller-provided instance names for state changes.
		inst, _, err := client.GetInstance(name)
		if err != nil {
			errs = append(errs, fmt.Errorf("get %s: %w", name, err))
			continue
		}
		if !g.isManaged(*inst) {
			errs = append(errs, fmt.Errorf("refusing to %s unmanaged instance %s", action, name))
			continue
		}

		op, err := client.UpdateInstanceState(name, api.InstanceStatePut{
			Action:  action,
			Timeout: 30,
		}, "")
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", action, name, err))
			continue
		}
		if op != nil {
			// We intentionally do not wait for freeze/unfreeze completion. The
			// Fleeting contract says these methods submit the transition and later
			// Update calls report the resulting state.
			succeeded = append(succeeded, name)
		}
	}

	return succeeded, errors.Join(errs...)
}
