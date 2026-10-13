# Kube-OVN 데이터플레인 RBAC

vws-network가 Kube-OVN 데이터플레인에서 쓰는 권한 전부다. 코드(`internal/network/dataplane_kubeovn*.go`의 `k.c.Get/List/Create/Update/Delete`)를 읽고 적었고, `TestEnvtestDocumentedRBACIsEnough`가 이 표(nodes 포함)만 가진 사용자로 모든 경로를 돌린다. 표를 바꾸면 그 테스트의 `clusterRules`/`nadRules`도 같이 바꾼다.

## ClusterRole

| apiGroup | resource | verbs | 쓰는 곳 |
| --- | --- | --- | --- |
| kubeovn.io | vpcs | get, create, delete | `ensureVPC`, `deleteVPC` |
| kubeovn.io | subnets | get, **list**, create, delete | `ensureSubnet`, `deleteSubnet`, `ready`(외부 Subnet get), `ensureUnderlay`. **list는 `-kube-snat`일 때 DeleteSubnet이 쓴다**: 지우는 서브넷이 VPC의 VpcNatGateway를 두고 있으면 `firstOtherSubnet`이 같은 VPC의 남은 서브넷을 list해서 게이트웨이를 옮긴다 |
| kubeovn.io | security-groups | get, create, update, delete | `ensureGroup`, `ensureRule`/`deleteRule`(update), `deleteGroup` |
| kubeovn.io | ovn-eips | get, create, delete | `ensureElasticIP`, `deleteElasticIP`, SNAT 주소 |
| kubeovn.io | ovn-fips | get, **list**, create, delete | `ensureNAT`, `deleteNAT`. list는 `deleteElasticIP`가 그 주소를 쓰는 OvnFip이 남았는지 본다 |
| kubeovn.io | ovn-snat-rules | get, create, delete | `ensureSNAT`, `deleteSNAT` |
| kubeovn.io | vpc-nat-gateways | get, create, delete | `ensureGateway`, `rehomeGateway` |
| kubeovn.io | provider-networks, vlans | get, create | `ensureUnderlay`(시작 때, 지우지 않음), `ready`(provider-networks get) |
| "" (core) | nodes | get, patch | `(*kubeOVN).labelGatewayNodes`, `-kube-gateway-nodes`일 때만(아래) |

모든 delete는 먼저 get으로 id annotation과 UID를 읽는다(`removeObject`). 그래서 delete가 있는 resource에는 get이 반드시 있다.

## 게이트웨이 노드 라벨 (`-kube-gateway-nodes`)

- `-kube-gateway-nodes`를 주면 VPC가 하나라도 있는 동안 `-kube-gateway-recheck`(기본 30s)마다 그 노드들을 get하고, `ovn.kubernetes.io/external-gw`가 `true`가 아니면 merge patch로 `true`를 붙인다. kube-ovn-controller가 재시작하면 이 라벨을 `false`로 바꿔 SNAT이 깨진다. `ovn-external-gw-config` ConfigMap으로 고정하면 underlay가 깨져서 이 방식을 쓴다.
- 목록에 없는 노드는 읽지도 바꾸지도 않는다. 라벨을 지우지 않는다. list와 watch는 필요 없다.
- nodes는 클러스터 범위라 ClusterRole이 필요하다. RBAC은 노드 이름으로 좁힐 수 있다(`resourceNames`). 차트는 vws-platform이 고친다.

## NAD 네임스페이스 Role (`-kube-nad-namespace`)

| apiGroup | resource | verbs | 쓰는 곳 |
| --- | --- | --- | --- |
| k8s.cni.cncf.io | network-attachment-definitions | get, create, delete | `ensureNAD`, `removeNAD` |

## subnets list가 없을 때 (vws-test 45b, 45c)

vws-deploy 차트의 network `rbac`에는 subnets `list`가 없다(2026-10-13 main 기준). `-kube-snat`으로 서브넷을 만든 뒤 DeleteSubnet을 부르면 규칙과 주소는 지워지지만 `firstOtherSubnet`의 list가 Forbidden이라 연산이 재시도만 하고 Subnet이 남는다(45b). 임시로 subnets list ClusterRole을 붙이자 끝났다(45c). `TestEnvtestDeleteSubnetNeedsSubnetList`가 이 실패를 재현한다. 차트에 subnets `list`를 더해야 한다.

`-kube-snat`이 VpcNatGateway를 쓰지 않게 되면(OVN 네이티브 SNAT 수정) list와 vpc-nat-gateways 권한은 필요 없어진다. 그 수정이 머지되면 이 표도 줄인다.
