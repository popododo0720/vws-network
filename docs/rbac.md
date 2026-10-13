# Kube-OVN 데이터플레인 RBAC

vws-network가 Kube-OVN 데이터플레인에서 쓰는 권한 전부다. 코드(`internal/network/dataplane_kubeovn*.go`의 `k.c.Get/List/Create/Update/Delete`)를 읽고 적었고, `TestEnvtestDocumentedRBACIsEnough`가 이 표(nodes 포함)만 가진 사용자로 모든 경로를 돌린다. 표를 바꾸면 그 테스트의 `clusterRules`/`nadRules`도 같이 바꾼다.

## ClusterRole

| apiGroup | resource | verbs | 쓰는 곳 |
| --- | --- | --- | --- |
| kubeovn.io | vpcs | get, create, delete | `ensureVPC`, `deleteVPC` |
| kubeovn.io | subnets | get, create, delete | `ensureSubnet`, `deleteSubnet`, `ready`(외부 Subnet get), `ensureUnderlay`. list는 쓰지 않는다 |
| kubeovn.io | security-groups | get, create, update, delete | `ensureGroup`, `ensureRule`/`deleteRule`(update), `deleteGroup` |
| kubeovn.io | ovn-eips | get, create, delete | `ensureElasticIP`, `deleteElasticIP`, SNAT 주소 |
| kubeovn.io | ovn-fips | get, **list**, create, delete | `ensureNAT`, `deleteNAT`. list는 `deleteElasticIP`가 그 주소를 쓰는 OvnFip이 남았는지 본다 |
| kubeovn.io | ovn-snat-rules | get, create, delete | `ensureSNAT`, `deleteSNAT` |
| kubeovn.io | provider-networks, vlans | get, create | `ensureUnderlay`(시작 때, 지우지 않음), `ready`(provider-networks get) |
| "" (core) | nodes | get, patch | `(*kubeOVN).labelGatewayNodes`, `-kube-gateway-nodes`일 때만(아래) |

모든 delete는 먼저 get으로 id annotation과 UID를 읽는다(`removeObject`). 그래서 delete가 있는 resource에는 get이 반드시 있다.

## direct NAD Role (`-kube-direct-nad-namespace`, VM 네임스페이스)

| apiGroup | resource | verbs | 쓰는 곳 |
| --- | --- | --- | --- |
| k8s.cni.cncf.io | network-attachment-definitions | get, create | `ensureDirect`(시작 때 `vws-direct-<name>`, 지우지 않음) |

`-kube-direct-networks`를 줄 때만 필요하다.

## VMI 읽기 Role (`-kube-vm-namespace`, VM 네임스페이스)

| apiGroup | resource | verbs | 쓰는 곳 |
| --- | --- | --- | --- |
| kubevirt.io | virtualmachineinstances | get, list, watch | `(*kubeOVN).interfaceAddress`. 주소가 `spec.association`(instance, device)으로 붙으면 VMI `<instance id>`의 `status.interfaces[]`에서 `interfaceName`(없으면 `name`)이 device인 항목의 `ipAddress`를 읽어 OvnFip을 만든다. 지금은 get만 쓰고 list/watch는 나중 watch용이다 |

- VMI가 없거나 IP가 아직 없으면 연산을 실패시키지 않고 계속 재시도한다(`errInterfaceNotReady`). 권한이 없으면(Forbidden) 보통 오류처럼 재시도하다 실패한다.
- 연결을 풀거나 바꾸면 그 주소의 OvnFip을 지운다.

## 게이트웨이 노드 라벨 (`-kube-gateway-nodes`)

- `-kube-gateway-nodes`를 주면 VPC가 하나라도 있는 동안 `-kube-gateway-recheck`(기본 5s)마다 그 노드들을 get하고, `ovn.kubernetes.io/external-gw`가 `true`가 아니면 merge patch로 `true`를 붙인다. kube-ovn-controller가 재시작하면 이 라벨을 `false`로 바꿔 SNAT이 깨진다. `ovn-external-gw-config` ConfigMap으로 고정하면 underlay가 깨져서 이 방식을 쓴다.
- 목록에 없는 노드는 읽지도 바꾸지도 않는다. 라벨을 지우지 않는다. list와 watch는 필요 없다.
- nodes는 클러스터 범위라 ClusterRole이 필요하다. RBAC은 노드 이름으로 좁힐 수 있다(`resourceNames`). 차트는 vws-platform이 고친다.

## 테넌트 subnet NAD Role (`-kube-nad-namespace`)

direct NAD는 여기가 아니라 위의 `-kube-direct-nad-namespace` Role이다.

| apiGroup | resource | verbs | 쓰는 곳 |
| --- | --- | --- | --- |
| k8s.cni.cncf.io | network-attachment-definitions | get, create, delete | `ensureNAD`, `removeNAD` |

## subnets list와 vpc-nat-gateways (vws-test 45b, 45c)

전 릴리스의 `-kube-snat`은 VPC마다 VpcNatGateway를 두고, 그 서브넷을 지울 때 같은 VPC의 남은 서브넷을 list해서 게이트웨이를 옮겼다. 그래서 subnets `list`가 없으면 DeleteSubnet이 재시도만 했다(45b, 45c). `-kube-snat`이 OVN 네이티브 SNAT(OvnEip + OvnSnatRule)만 쓰게 되면서 두 권한 모두 필요 없다. `TestEnvtestDeleteSubnetWithoutSubnetList`가 list 없는 이 표의 권한으로 SNAT 서브넷을 지운다. 이전 릴리스가 만든 VpcNatGateway는 운영자가 지운다(`docs/kube-ovn-conflict-recovery.md`). 차트에 이미 더한 subnets `list`와 vpc-nat-gateways는 빼도 된다.
