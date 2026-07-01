# fleeting-plugin-incus

`fleeting-plugin-incus` is a GitLab Fleeting provider plugin that provisions ephemeral GitLab Runner capacity on an Incus cluster.

It supports Incus containers and virtual machines, local Unix socket or remote HTTPS/TLS connections, image-based launches or local template copies, and SSH static-key access.

## Status

This project is an initial production-oriented implementation. Use a dedicated Incus project and a unique pool identity for early deployments.

## Build

Install Go, then build the plugin:

```sh
go build -o bin/fleeting-plugin-incus ./cmd/fleeting-plugin-incus
```

For a versioned build:

```sh
go build \
  -ldflags "-X main.version=0.1.0 -X main.revision=$(git rev-parse --short HEAD) -X main.reference=$(git rev-parse --abbrev-ref HEAD) -X main.builtAt=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o bin/fleeting-plugin-incus \
  ./cmd/fleeting-plugin-incus
```

Place the binary on the GitLab Runner manager host `PATH`, or install it through the Fleeting plugin path used by your Runner installation.

## Configuration

Configure this under `[runners.autoscaler.plugin_config]`.

### Connection

| Field | Required | Description |
| --- | --- | --- |
| `connection_type` | no | `unix` or `https`; defaults to `unix`. |
| `endpoint` | for `connection_type = "https"` | Incus HTTPS endpoint, for example `https://incus.example.com:8443`. |
| `socket_path` | no | Unix socket path; empty uses Incus client defaults. |
| `project` | no | Incus project to use. |
| `tls_client_cert`, `tls_client_key` | no | Client certificate and key paths, or inline PEM contents; for HTTPS they default to `~/.config/incus/client.crt` and `~/.config/incus/client.key` when both are omitted. |
| `tls_server_cert`, `tls_ca` | no | Server certificate or CA path, or inline PEM contents. |
| `insecure_skip_verify` | no | For HTTPS connections, disables TLS certificate and hostname verification. Useful for self-signed certificates or hostname mismatches, but should only be used for explicitly trusted endpoints. When set, `tls_server_cert` is ignored for the TLS handshake so that the skip actually takes effect. Prefer `tls_server_cert` or `tls_ca` when you can validate the server properly. |
| `connect_timeout` | no | Timeout for each Incus operation at connection time, as a Go duration string (e.g. `"30s"`). Defaults to `30s`. |
| `operation_timeout` | no | Timeout for Incus instance operations such as create and start, as a Go duration string (e.g. `"5m"`). Defaults to `5m`. |

### Pool identity

| Field | Required | Description |
| --- | --- | --- |
| `name_prefix` | yes | Required prefix for every managed instance name. |
| `pool_id` | yes | Pool identity value written into Incus instance config. |
| `pool_config_key` | no | Incus user config key without `user.`; defaults to `fleeting.pool`. |
| `max_size` | no | Maximum managed instances; `0` means no plugin-side cap. |

`pool_config_key` is normalized before API calls. For example, `pool_config_key = "fleeting.pool"` is stored and matched as `user.fleeting.pool`.

### Instance

| Field | Required | Description |
| --- | --- | --- |
| `instance_type` | no | `container` or `virtual-machine`; defaults to `container`. |
| `privileged` | no | For `instance_type = "container"`, requests a privileged Incus container by setting `security.privileged=true` on created instances. |
| `profiles` | no | Incus profiles to apply. |
| `config` | no | Extra Incus instance config. Do not set `cloud-init.user-data` when `ssh_public_key` or `ssh_public_key_path` is configured. |
| `devices` | no | Extra Incus devices. |
| `storage_pool` | no | Incus storage pool override for the created instance root disk. |
| `root_disk_size` | no | Root disk size override, for example `30GiB`. |
| `target` | no | Incus cluster member to place the instance on. |

### Networking and address selection

| Field | Required | Description |
| --- | --- | --- |
| `network` | if profiles/devices do not define a usable NIC | Incus managed network name to attach as a NIC on created instances. If omitted, networking falls back to the selected profiles and explicit `devices`. |
| `network_interface` | no | Name of the NIC inside the instance to prefer when selecting the address returned to GitLab Runner (e.g. `eth0`). Also used as the device name when `network` injects a NIC and no explicit device with that name exists in `devices`. When unset, the plugin prefers the first Incus-managed NIC (identified by a non-empty host-side interface name). Set this explicitly on instances that run additional software—such as Docker—that creates internal bridge interfaces, to ensure Runner always receives a reachable address rather than an address on an unreachable internal bridge. |
| `address_family` | no | `inet` (IPv4) or `inet6` (IPv6); defaults to `inet`. |
| `os` | no | OS hint returned to Fleeting connector; defaults to `linux`. |
| `arch` | no | Architecture hint returned to Fleeting connector; defaults to `amd64`. |

### SSH

| Field | Required | Description |
| --- | --- | --- |
| `ssh_username` | yes | User GitLab Runner should SSH as. |
| `ssh_public_key` or `ssh_public_key_path` | no | Public key injected through `cloud-init.user-data`. Omit both when the image/template already has usable SSH credentials. |

### Connector tuning

These fields nest under `[runners.autoscaler.plugin_config.connector]`.

| Field | Description |
| --- | --- |
| `keepalive` | SSH keepalive interval as a Go duration string; overrides Fleeting's default. |
| `timeout` | SSH connection timeout as a Go duration string; overrides Fleeting's default. |

### Image source

Images can be either created from an image server or copied from existing
instances.


```toml
[runners.autoscaler.plugin_config.image]
alias = "ubuntu/24.04/cloud"
server = "https://images.linuxcontainers.org/"
protocol = "simplestreams"
type = "image"

```

The fields are exactly the same as in the incus api
[`instanceSource`](https://pkg.go.dev/github.com/lxc/incus@v0.6.0/shared/api#InstanceSource)
type.
Note that fuzzy matching for image aliases has been removed. Your alias
must be an exact match among the existing aliases.

| Field | Description |
| --- | --- |
| `type` | Either "image" or "copy" (required)
| `alias` | Incus image alias.
| `fingerprint` | Exact image fingerprint. More reproducible than `alias` when builds must not drift. |
| `server` | Remote server, if any.
| `protocol` | Protocol used by the remote server, if any.
| `secret` | Secret used by the remote server, if any.
| `properties` | Image metadata key/value map; can be used for filtering.
| `project` | Incus project to look up the image in. Defaults to the plugin's configured `project`. |

In order to copy an existing instance as a template, you can use this
example configuration instead of the above:

```toml
[runners.autoscaler.plugin_config.image]
type = "copy"
source = "gitlab-runner-template"
instance_only = true
```

| Field | Description |
| --- | --- |
| `source` | Name of the source instance or snapshot to copy. |
| `project` | Incus project containing the template. Defaults to the plugin's configured `project`. |
| `instance_only` | When `true`, snapshots from the source are not copied. |

## GitLab Runner Example

```toml
[[runners]]
  name = "incus-autoscaler"
  url = "https://gitlab.com"
  token = "REDACTED"
  executor = "instance"

  [runners.autoscaler]
    plugin = "fleeting-plugin-incus"
    capacity_per_instance = 1
    max_use_count = 1
    max_instances = 10

  [runners.autoscaler.plugin_config]
    connection_type = "unix"
    project = "gitlab-runners"
    name_prefix = "gl-ci-"
    pool_id = "default"
    pool_config_key = "fleeting.pool"
    max_size = 10
    instance_type = "virtual-machine"
    profiles = ["default", "gitlab-runner"]
    storage_pool = "fast"
    root_disk_size = "30GiB"
    network = "gitlab-runners"
    ssh_username = "runner"
    ssh_public_key_path = "/etc/gitlab-runner/runner.pub"

    [runners.autoscaler.plugin_config.image]
      alias = "ubuntu/24.04/cloud"

  [runners.autoscaler.connector_config]
    protocol = "ssh"
    username = "runner"
    key_path = "/etc/gitlab-runner/runner"
    use_external_addr = false
```

Configure SSH access under `[runners.autoscaler.connector_config]`. `key_path` is the private key path on the GitLab Runner manager host; it should match the public key already baked into the image/template or injected with `ssh_public_key` or `ssh_public_key_path`. Fleeting does not automatically fall back to SSH client defaults such as `~/.ssh/id_rsa`.

## Incus Requirements

- If `ssh_public_key` or `ssh_public_key_path` is configured, use images/templates that support `cloud-init.user-data`; the plugin uses cloud-init only to inject that key.
- If your image/template already contains the SSH user and authorized key, omit both SSH public key fields and point `[runners.autoscaler.connector_config].key_path` at the matching private key on the Runner host. In that mode, cloud-init support is not required by the plugin.
- If you provide your own `config.cloud-init.user-data` while omitting the plugin SSH public key fields, cloud-init support is required by your own instance config rather than by the plugin.
- Set `privileged = true` only for container workloads that require privileged Incus containers. This option is rejected for virtual machines.
- If you connect over HTTPS to an endpoint with a self-signed certificate or hostname mismatch, set `insecure_skip_verify = true`. Do not also set `tls_server_cert` in that case: the Incus client library only applies `InsecureSkipVerify` when no pinned server certificate is present, so a combined configuration silently falls back to cert-pinning and hostname verification still fails. Prefer `tls_server_cert` or `tls_ca` when you can validate the server properly.
- Ensure the selected profiles/devices provide network connectivity to the Runner manager. If they do not define a usable NIC, configure `network` explicitly.
- If your instances run additional software that creates internal bridge interfaces (for example Docker's `docker0`), set `network_interface` to the name of the Incus-managed NIC (e.g. `eth0`). Without it, the plugin prefers Incus-managed NICs automatically, but an explicit setting is more robust.
- Use a dedicated Incus project where possible.
- Do not reuse the same `name_prefix` and `pool_id` across unrelated pools.

## Tests

```sh
go test ./...
go vet ./...
```

## Integration Tests

Integration tests are skipped unless `INCUS_INTEGRATION=1` is set. They create a temporary Incus project, launch real instances, test image and template provisioning, verify unmanaged instances are not deleted, and force-delete the temporary project during cleanup.

When `INCUS_INTEGRATION_CONNECTION_TYPE` is not set, the test harness tries to read the normal Incus client config at `~/.config/incus/config.yml` and derive the connection from that file's `default-remote`. Set `INCUS_INTEGRATION_REMOTE` only when you want to use a different named remote from the same config file. For HTTPS remotes, it also uses the conventional client cert/key and cached server cert path when present. Environment variables still override values discovered from that file. If no Incus client config exists, connection environment variables are required.

Minimum local run:

```sh
INCUS_INTEGRATION=1 \
INCUS_INTEGRATION_IMAGE_ALIAS="ubuntu/24.04/cloud" \
go test ./internal/provider -run Integration -count=1
```

Useful environment variables:

| Variable | Default | Description |
| --- | --- | --- |
| `INCUS_INTEGRATION_CONNECTION_TYPE` | Incus client config | `unix` or `https`. Required if no local Incus client config exists. |
| `INCUS_INTEGRATION_CONFIG_PATH` | `~/.config/incus/config.yml` | Incus client config file to inspect when connection type is unset. |
| `INCUS_INTEGRATION_REMOTE` | config file's `default-remote` | Optional remote-name override for the Incus client config. |
| `INCUS_INTEGRATION_SOCKET_PATH` | Incus default | Unix socket path for local Incus. |
| `INCUS_INTEGRATION_ENDPOINT` | empty | Remote Incus HTTPS endpoint. |
| `INCUS_INTEGRATION_TLS_CLIENT_CERT` | `~/.config/incus/client.crt` | Client cert path or inline PEM for HTTPS. |
| `INCUS_INTEGRATION_TLS_CLIENT_KEY` | `~/.config/incus/client.key` | Client key path or inline PEM for HTTPS. |
| `INCUS_INTEGRATION_TLS_CA` | empty | CA path or inline PEM for HTTPS. |
| `INCUS_INTEGRATION_TLS_SERVER_CERT` | empty | Server certificate path or inline PEM for HTTPS. |
| `INCUS_INTEGRATION_IMAGE_ALIAS` | required | Local Incus image alias used to create test instances/templates. |
| `INCUS_INTEGRATION_IMAGE_PROJECT` | `default` | Project where the image alias exists. |
| `INCUS_INTEGRATION_INSTANCE_TYPE` | `container` | `container` or `virtual-machine`. |
| `INCUS_INTEGRATION_PROFILES` | `default` | Comma-separated profiles to apply. |
| `INCUS_INTEGRATION_UPLINK_NETWORK` | `uplink` | Existing managed uplink network used as the parent for a temporary OVN test network with DHCP/NAT enabled. |
| `INCUS_INTEGRATION_SSH_USERNAME` | `root` | SSH username returned by ConnectInfo. |
| `INCUS_INTEGRATION_WAIT_TIMEOUT` | `2m` | How long tests wait for running/address state. |
