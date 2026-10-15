# External network underlay

vws-network itself creates the Kube-OVN external Subnet it uses for elastic IPs and SNAT, as an underlay attached directly to a node NIC. The code is `internal/network/dataplane_kubeovn_underlay.go`.

## Flags

| Flag | Meaning | Check |
| --- | --- | --- |
| `-pool` | Elastic IP range. **Required, no default** | Missing or an RFC 5737 documentation range refuses start (exit code 2) |
| `-kube-external-subnet` | External Subnet name. The deploy side always uses `vws-external` (vws-deploy#125) | Required with the underlay on |
| `-kube-provider-network` | Name of the ProviderNetwork to create (at most 12 characters). The deploy side uses `vws-pn<N>`, N being the sort position of the underlay NIC name | Required with the underlay on |
| `-kube-underlay-interface` | The ProviderNetwork's `defaultInterface` (for example `ens3`) | Required |
| `-kube-underlay-vlan` | 0 is flat, 1-4094 is a tagged VLAN | Refused when out of range |
| `-kube-underlay-cidr` | External network CIDR | Must contain `-pool` |
| `-kube-underlay-gateway` | External network gateway | Inside the CIDR, outside `-pool` |
| `-kube-underlay-exclude` | Addresses Kube-OVN must not hand out: comma-separated `a` or `a..b`. The deploy side passes `external.excludeIPs` | Each range is inside the CIDR and ascending; overlap with `-pool` is refused. The gateway is always included |

With no underlay flag at all, it only reads the external Subnet the operator created, as before.

## Objects created (at start, in this order)

```yaml
apiVersion: kubeovn.io/v1
kind: ProviderNetwork
metadata: {name: <provider>, annotations: {vws.popododo0720.io/id: <provider>}}
spec: {defaultInterface: <interface>}
---
apiVersion: kubeovn.io/v1
kind: Vlan
metadata: {name: <provider>-vlan<id>, annotations: {vws.popododo0720.io/id: <provider>-vlan<id>}}
spec: {id: <id>, provider: <provider>}
---
apiVersion: kubeovn.io/v1
kind: Subnet
metadata: {name: <external-subnet>, annotations: {vws.popododo0720.io/id: <external-subnet>}}
spec: {protocol: IPv4, cidrBlock: <cidr>, gateway: <gateway>, vlan: <provider>-vlan<id>, excludeIps: [<gateway>, <exclude>...]}
```

Every Vpc attaches to the external Subnet with `spec.enableExternal: true`, `spec.extraExternalSubnets: [<-kube-external-subnet>]` and the default route `staticRoutes: [{cidr: 0.0.0.0/0, nextHopIP: <external gateway>, policy: policyDst}]`. The external gateway is `-kube-underlay-gateway` with the underlay on, otherwise the `spec.gateway` of the operator's external Subnet (`domain.Invalid` without one). OvnSnatRule worked only with all three (vws-test 42, 43). This service does not touch the `kube-system/ovn-external-gw-config` ConfigMap that chooses the gateway nodes; the deploy side creates it (vws-deploy#127).

- If an object with the same name exists without the id annotation, or a spec field we write differs, start is refused with `domain.Conflict`. A live physical network is never overwritten. To keep using an external Subnet the operator made in advance, do not pass underlay flags.
- The service never deletes these objects. Deleting them is the operator's job.
- The deploy RBAC needs `get` and `create` on `provider-networks` and `vlans`, and `create` on `subnets` (already there). vws-deploy#149 (PR #152) adds them.
- The EIP pool is not put in `excludeIps`. That is decided after measuring whether Kube-OVN v1.16.12 accepts an address inside excludeIps as an OvnEip `v4Ip` (vws-deploy#125 "Subnet excludeIps"). Until then, Kube-OVN may pick a VPC lrp address inside the pool.
- Vpcs created before this change lack `enableExternal`, `extraExternalSubnets` and the default route, so ensuring them again gives `domain.Conflict`. They are not overwritten. Recover with `docs/kube-ovn-conflict-recovery.md`.

## Node network interface (vws-deploy#125)

The "vws-network interface contract" table in vws-deploy#125 is the source of truth for the node network design and the flag contract. The flag names in this document match that table. In short:

1. The deploy side (`vws render`) turns VWSCluster `spec.network.nodes.external` (nic, vlan, cidr, gateway, excludeIPs, eipPool) into the flags above. This service does not configure host networking.
2. NIC names are the same on every node. `customInterfaces` is not in v1.
3. The underlay NIC is handed to an OVS bridge. That NIC can carry only one untagged host network (#125 rule 4). With a host VLAN on a single NIC, the underlay cannot be used.
4. The Kube-OVN default network stays geneve; the underlay is used only for the external Subnet (and later the direct Subnets).

## External Subnet made by the operator (no underlay flags, #52)

Without underlay flags, `Open` gets `-kube-external-subnet` at start and checks that `-pool` is inside its `spec.cidrBlock`. If the Subnet is missing, its cidrBlock is not an IPv4 prefix, or the pool is outside, start is refused with `domain.Invalid`. If the API server cannot be reached, start fails (before, only ready failed after start). The reason and the error text are in README "Run".

## Direct networks (`-kube-direct-networks`, #52, vws-deploy#125)

A one-line JSON array. The deploy side renders it from VWSCluster `spec.network.nodes.direct`.

```json
[{"name":"prod","providerNetwork":"vws-pn0","interface":"ens3","vlan":200,"cidr":"192.168.200.0/24","gateway":"192.168.200.1","exclude":["192.168.200.2..192.168.200.20"]}]
```

An unknown key refuses start (exit code 2). `Open` checks:

- `name` matches `^[a-z][a-z0-9-]{0,14}$` and is unique.
- `providerNetwork`, `interface`, `vlan`, `cidr`, `gateway` and `exclude` follow the same rules as the underlay flags.
- One ProviderNetwork is one NIC. On the same NIC as the external network it must use the same ProviderNetwork (shared); a different name is refused.
- A (NIC, VLAN) pair carries one network only. So a NIC has only one untagged network, and the external network and direct networks need different VLANs (#125 rule 4).
- A CIDR does not overlap the external network, other direct networks or `-pool`.

At start, after the external network, each network is ensured in this order; a difference is refused with `domain.Conflict`.

```yaml
kind: ProviderNetwork   # shared with the external network: the same object, the same spec
metadata: {name: <providerNetwork>}
spec: {defaultInterface: <interface>}
---
kind: Vlan
metadata: {name: <providerNetwork>-vlan<vlan>}
spec: {id: <vlan>, provider: <providerNetwork>}
---
kind: Subnet
metadata: {name: vws-direct-<name>}
spec: {protocol: IPv4, cidrBlock: <cidr>, gateway: <gateway>, vlan: <providerNetwork>-vlan<vlan>, excludeIps: [<gateway>, <exclude>...], provider: vws-direct-<name>.<vm-ns>.ovn}
---
kind: NetworkAttachmentDefinition
metadata: {name: vws-direct-<name>, namespace: <-kube-direct-nad-namespace>}
spec: {config: '{"type":"kube-ovn","provider":"vws-direct-<name>.<vm-ns>.ovn",...}'}
```

As #125 says, the NAD lives in the VM namespace, because a VM can attach only NADs from its own namespace. That namespace comes from `-kube-direct-nad-namespace`, which has no default. If `-kube-direct-networks` is set and it is empty, start is refused with `domain.Invalid`. #125 did not name it, so the deploy side picks it and passes it in. Tenant subnet NADs still use `-kube-nad-namespace`.

In the API they appear in every project as Subnet `subnet-direct-<name>` of VPC `vpc-direct` (label `vws:network-type=direct`). They show up in Get and List (vpc empty or `vpc-direct`), and the region in the name is the requested region. They are read-only.

- CreateSubnet, UpdateSubnet, DeleteSubnet and CreateVPC on `vpc-direct` or `subnet-direct-*` are `domain.Forbidden` (gRPC `PermissionDenied`, `UnauthorizedOperation`). These names are reserved even with no direct networks.
- AllocateElasticIP in `vpc-direct`, and AssociateElasticIP to a private address that is not in a subnet of the EIP's VPC but is in a direct CIDR, are `domain.Invalid` (`InvalidParameterCombination`).
- The service never deletes direct objects. Cleaning up objects of a network removed from the flag is the operator's job.
- Deploy RBAC: besides the same `get` and `create` on `provider-networks` and `vlans` and `create` on `subnets` as the external underlay, a Role with NAD `get` and `create` in the VM namespace is needed (`docs/rbac.md`). ready does not read direct objects.

## Not verified

- Whether, on real Kube-OVN, this Subnet and `extraExternalSubnets` work as the external range for OvnEip/OvnFip/OvnSnatRule, and whether that works without `ovn-external-gw-config`.
- Whether the addresses Kube-OVN picks itself in the external Subnet (VPC lrp and so on) stay clear of `-pool`.
- Whether creating a ProviderNetwork on a single-NIC node keeps the node connected.
- Whether a VM attached to a direct Subnet on another VLAN of the same ProviderNetwork reaches the physical gateway without mixing with external network EIP traffic.
