# Kube-OVN dataplane RBAC

All the permissions vws-network uses in the Kube-OVN dataplane. They were written by reading the code (`k.c.Get/List/Create/Update/Delete` in `internal/network/dataplane_kubeovn*.go`), and `TestEnvtestDocumentedRBACIsEnough` runs every path as a user that has only this table (nodes included). When the table changes, change that test's `clusterRules`/`nadRules` with it.

## ClusterRole

| apiGroup | resource | verbs | Used by |
| --- | --- | --- | --- |
| kubeovn.io | vpcs | get, create, delete | `ensureVPC`, `deleteVPC` |
| kubeovn.io | subnets | get, create, delete | `ensureSubnet`, `deleteSubnet`, `ready` (get of the external Subnet), `ensureUnderlay`. list is not used |
| kubeovn.io | security-groups | get, create, update, delete | `ensureGroup`, `ensureRule`/`deleteRule` (update), `deleteGroup` |
| kubeovn.io | ovn-eips | get, create, delete | `ensureElasticIP`, `deleteElasticIP`, SNAT addresses |
| kubeovn.io | ovn-fips | get, **list**, create, delete | `ensureNAT`, `deleteNAT`. list lets `deleteElasticIP` see whether an OvnFip using that address is left |
| kubeovn.io | ovn-snat-rules | get, create, delete | `ensureSNAT`, `deleteSNAT` |
| kubeovn.io | provider-networks, vlans | get, create | `ensureUnderlay` (at start, never deleted), `ready` (provider-networks get) |
| "" (core) | nodes | get, patch | `(*kubeOVN).labelGatewayNodes`, only with `-kube-gateway-nodes` (below) |

Every delete first reads the id annotation and UID with a get (`removeObject`), so every resource with delete also has get.

## Direct NAD Role (`-kube-direct-nad-namespace`, the VM namespace)

| apiGroup | resource | verbs | Used by |
| --- | --- | --- | --- |
| k8s.cni.cncf.io | network-attachment-definitions | get, create | `ensureDirect` (`vws-direct-<name>` at start, never deleted) |

Needed only when `-kube-direct-networks` is given.

## VMI read Role (`-kube-vm-namespace`, the VM namespace)

| apiGroup | resource | verbs | Used by |
| --- | --- | --- | --- |
| kubevirt.io | virtualmachineinstances | get, list, watch | `(*kubeOVN).interfaceAddress`. When an address is attached with `spec.association` (instance, device), it reads the `ipAddress` of the entry in `status.interfaces[]` of VMI `<instance id>` whose `interfaceName` (else `name`) is the device, and creates the OvnFip. If no name matches, `pickVMIInterface` picks the only interface, or (nic0 with several) the pod network interface (#95). list/watch are used by `(*Service).vmiLoop`, which watches the VMIs in the VM namespace and, when the IP of an associated interface changes, moves the stored Private and the OvnFip to the new IP (#86) |

- If the VMI is missing or has no IP yet, the operation does not fail and keeps retrying (`errInterfaceNotReady`). Without permission (Forbidden) it retries like an ordinary error and then fails.
- Removing or changing the association deletes that address's OvnFip.
- If the VMI IP is briefly empty (during a restart), nothing changes; when a new IP is reported, a new OvnFip is created and the old one deleted. Unassociated VMIs are ignored. When the watch breaks it lists again after 1 second, and otherwise every 5 minutes.

## Gateway node label (`-kube-gateway-nodes`)

- With `-kube-gateway-nodes`, while at least one VPC exists, it gets those nodes every `-kube-gateway-recheck` (default 5s) and, if `ovn.kubernetes.io/external-gw` is not `true`, adds `true` with a merge patch. A kube-ovn-controller restart sets this label to `false`, which breaks SNAT. Pinning it with the `ovn-external-gw-config` ConfigMap breaks the underlay, so this approach is used.
- Nodes not on the list are neither read nor changed. The label is never removed. list and watch are not needed.
- nodes are cluster-scoped, so a ClusterRole is needed. RBAC can narrow it by node name (`resourceNames`). vws-platform changes the chart.

## Tenant subnet NAD Role (`-kube-nad-namespace`)

Direct NADs are not here but under the `-kube-direct-nad-namespace` Role above.

| apiGroup | resource | verbs | Used by |
| --- | --- | --- | --- |
| k8s.cni.cncf.io | network-attachment-definitions | get, create, delete | `ensureNAD`, `removeNAD` |

## subnets list and vpc-nat-gateways (vws-test 45b, 45c)

In the previous release, `-kube-snat` kept a VpcNatGateway per VPC and, when deleting its subnet, listed the remaining subnets of the same VPC to move the gateway. So without subnets `list`, DeleteSubnet only retried (45b, 45c). Now that `-kube-snat` uses only OVN-native SNAT (OvnEip + OvnSnatRule), neither permission is needed. `TestEnvtestDeleteSubnetWithoutSubnetList` deletes a SNAT subnet with this table's permissions, without list. VpcNatGateways made by the previous release are deleted by the operator (`docs/kube-ovn-conflict-recovery.md`). subnets `list` and vpc-nat-gateways already added to the chart can be removed.
