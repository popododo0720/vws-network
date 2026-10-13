# Kube-OVN 데이터플레인 RBAC

vws-network가 Kube-OVN 데이터플레인에서 쓰는 권한 전부다. 코드(`internal/network/dataplane_kubeovn*.go`의 `k.c.Get/List/Create/Update/Delete`)를 읽고 적었고, `TestEnvtestDocumentedRBACIsEnough`가 이 표만 가진 사용자로 모든 경로를 돌린다. 표를 바꾸면 그 테스트의 `clusterRules`/`nadRules`도 같이 바꾼다.

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

모든 delete는 먼저 get으로 id annotation과 UID를 읽는다(`removeObject`). 그래서 delete가 있는 resource에는 get이 반드시 있다.

## direct NAD Role (`-kube-direct-nad-namespace`, VM 네임스페이스)

| apiGroup | resource | verbs | 쓰는 곳 |
| --- | --- | --- | --- |
| k8s.cni.cncf.io | network-attachment-definitions | get, create | `ensureDirect`(시작 때 `vws-direct-<name>`, 지우지 않음) |

`-kube-direct-networks`를 줄 때만 필요하다.

## NAD 네임스페이스 Role (`-kube-nad-namespace`)

| apiGroup | resource | verbs | 쓰는 곳 |
| --- | --- | --- | --- |
| k8s.cni.cncf.io | network-attachment-definitions | get, create, delete | `ensureNAD`, `removeNAD` |

## subnets list가 없을 때 (vws-test 45b, 45c)

vws-deploy 차트의 network `rbac`에는 subnets `list`가 없다(2026-10-13 main 기준). `-kube-snat`으로 서브넷을 만든 뒤 DeleteSubnet을 부르면 규칙과 주소는 지워지지만 `firstOtherSubnet`의 list가 Forbidden이라 연산이 재시도만 하고 Subnet이 남는다(45b). 임시로 subnets list ClusterRole을 붙이자 끝났다(45c). `TestEnvtestDeleteSubnetNeedsSubnetList`가 이 실패를 재현한다. 차트에 subnets `list`를 더해야 한다.

`-kube-snat`이 VpcNatGateway를 쓰지 않게 되면(OVN 네이티브 SNAT 수정) list와 vpc-nat-gateways 권한은 필요 없어진다. 그 수정이 머지되면 이 표도 줄인다.
