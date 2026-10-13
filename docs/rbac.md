# Kube-OVN 데이터플레인 RBAC

vws-network가 Kube-OVN 데이터플레인에서 쓰는 권한 전부다. 코드(`internal/network/dataplane_kubeovn*.go`의 `k.c.Get/List/Create/Update/Delete`)를 읽고 적었고, `TestEnvtestDocumentedRBACIsEnough`가 이 표만 가진 사용자로 모든 경로를 돌린다. 표를 바꾸면 그 테스트의 `clusterRules`/`nadRules`도 같이 바꾼다.

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

모든 delete는 먼저 get으로 id annotation과 UID를 읽는다(`removeObject`). 그래서 delete가 있는 resource에는 get이 반드시 있다.

## NAD 네임스페이스 Role (`-kube-nad-namespace`)

| apiGroup | resource | verbs | 쓰는 곳 |
| --- | --- | --- | --- |
| k8s.cni.cncf.io | network-attachment-definitions | get, create, delete | `ensureNAD`, `removeNAD` |

## subnets list와 vpc-nat-gateways (vws-test 45b, 45c)

전 릴리스의 `-kube-snat`은 VPC마다 VpcNatGateway를 두고, 그 서브넷을 지울 때 같은 VPC의 남은 서브넷을 list해서 게이트웨이를 옮겼다. 그래서 subnets `list`가 없으면 DeleteSubnet이 재시도만 했다(45b, 45c). `-kube-snat`이 OVN 네이티브 SNAT(OvnEip + OvnSnatRule)만 쓰게 되면서 두 권한 모두 필요 없다. `TestEnvtestDeleteSubnetWithoutSubnetList`가 list 없는 이 표의 권한으로 SNAT 서브넷을 지운다. 이전 릴리스가 만든 VpcNatGateway는 운영자가 지운다(`docs/kube-ovn-conflict-recovery.md`). 차트에 이미 더한 subnets `list`와 vpc-nat-gateways는 빼도 된다.
