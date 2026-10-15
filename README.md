# vws-network

The VWS network control service. It stores the desired state of VPCs, subnets, security groups and elastic IPs per project, and finishes anything that touches the dataplane as an operation.

## Run

```sh
go run ./cmd/vws-network -db /tmp/vws-network.db -pool 10.250.0.0/24
```

`-listen` defaults to `127.0.0.1:8080`.

When Kube-OVN is selected with `-kubeconfig` (or the in-cluster ServiceAccount) and the API server cannot be reached at start, the service does not come up and exits with code 1. The error is `kube-ovn start checks need the API server, so vws-network does not start: ...`. The reason is that everything that must happen before serving uses the API server:

- Without underlay flags, it checks that `-pool` is inside the `cidrBlock` of the external Subnet the operator created. Coming up without that check could hand out elastic IPs Kube-OVN cannot route (#52).
- When an address is attached with `spec.association` (instance, device), it reads that interface's IP from the VMI in `-kube-vm-namespace` and creates an OvnFip, and deletes it when the association is removed. The operation stays pending and retries until the VMI gets an IP (`docs/rbac.md`). When the VMI's IP changes, the VMI watch moves the stored Private and the OvnFip to the new IP.
- The VM MTU of VPC subnets is `-vm-mtu` (default 9001, 68–9001). It goes down as the Subnet `spec.mtu` and the DHCP options. The 1500 external path is in `docs/mtu.md`.
- With an underlay and `-kube-direct-networks`, it creates their ProviderNetwork, Vlan, Subnet and NAD. Coming up without them would make the API show direct subnets that do not exist.

Before, only `/readyz` failed after start. Now the deployment just retries by restarting. A wrong value (`domain.Invalid`, `domain.Conflict`) returns that error as is. Details are in `docs/underlay.md`.

## Test

```sh
go test ./...
```

## Behaviour

Within one project, VPC CIDRs cannot overlap. A subnet CIDR must be inside its VPC and cannot overlap other subnets of the same VPC. Overlap with other projects is fine.

Mutating RPCs need an `idempotency_key`. The same project, key and request return the original operation. The same key with a different request is refused. A request that fails validation leaves neither an operation nor a key.

When an operation is `SUCCEEDED`, the fake dataplane matches that resource's desired state. When the process comes back up, operations left `RUNNING` go back to `PENDING` and converge from there.

`third_party/vws-proto` is `main` `a1c6084` of `github.com/popododo0720/vws-proto`. The replace in `go.mod` points at that path. Once vws-proto is public, the replace is removed and a tagged version is required.

`third_party/vws-lib` is commit `5b88ea9` of `github.com/popododo0720/vws-lib` (the English-docs branch; same Go code as `v0.2.0` `fe9cd29`). It was copied with `scripts/vendor-lib.sh vendor <commit>` and is used through a replace. CI checks with `VWS_LIB_OFFLINE=1 scripts/vendor-lib.sh check` that the copy has not changed. Do not edit the copy.

## Observability

`main` calls `obs.Setup` once and shuts it down at exit. Logs are slog JSON. Metrics are `vws.rpc.server.*`, `vws.reconcile.total` and `vws.reconcile.duration` (dataplane ensure calls, `controller`=`network.<kind>`, `result`=`ok`, `conflict` or `error`), `vws.audit.events` (mutating RPCs), `vws.panics`, `vws.errors`, `vws.queue.depth` (`queue`=`network.operations`, the number of unfinished operations) and `vws.build.info`. Metrics are exported over OTLP; there is no `/metrics` pull endpoint (vws-lib has no Prometheus reader yet). Labels never carry resource ids or request ids.

Environment variables this service reads:

| Variable | Meaning |
| --- | --- |
| `OTEL_SERVICE_NAME`, `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG`, `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER`, `OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_SDK_DISABLED` | Standard OpenTelemetry settings. With `OTEL_EXPORTER_OTLP_ENDPOINT` empty, nothing is exported. |
| `SENTRY_DSN`, `SENTRY_ENVIRONMENT`, `SENTRY_RELEASE`, `SENTRY_SAMPLE_RATE` | Sends to Sentry only when `SENTRY_DSN` is set. |
| `VWS_LOG_LEVEL` | `debug`, `info` (default), `warn`, `error`. |
| `VWS_LOG_FORMAT` | `json` (default) or `text`. |
| `VWS_HEALTH_ADDR` | Address for `/healthz` and `/readyz`. Empty means not opened. `/readyz` calls `(*Service).Ready` and fails after SIGTERM. |

gRPC health is described in `docs/health.md`.

No deployment manifests are kept here.
