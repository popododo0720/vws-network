# 외부망 underlay 연동

vws-network가 엘라스틱 IP와 SNAT에 쓰는 Kube-OVN 외부 Subnet을 노드 NIC에 직결된 underlay로 직접 만든다. 근거 코드는 `internal/network/dataplane_kubeovn_underlay.go`다.

## 플래그

| 플래그 | 뜻 | 검사 |
| --- | --- | --- |
| `-pool` | 엘라스틱 IP 대역. **필수, 기본값 없음** | 없거나 RFC 5737 문서용 대역이면 시작 거절(종료 코드 2) |
| `-kube-external-subnet` | 외부 Subnet 이름. 배포 쪽은 고정 `vws-external`(vws-deploy#125) | underlay를 켜면 필수 |
| `-kube-provider-network` | 만들 ProviderNetwork 이름(12자 이하). 배포 쪽은 `vws-pn<N>`, N은 underlay NIC 이름 정렬 순서 | underlay를 켜면 필수 |
| `-kube-underlay-interface` | ProviderNetwork의 `defaultInterface`(예: `ens3`) | 필수 |
| `-kube-underlay-vlan` | 0은 flat, 1-4094는 태그 VLAN | 범위 밖이면 거절 |
| `-kube-underlay-cidr` | 외부망 CIDR | `-pool`을 품어야 한다 |
| `-kube-underlay-gateway` | 외부망 게이트웨이 | CIDR 안, `-pool` 밖 |
| `-kube-underlay-exclude` | Kube-OVN이 주면 안 되는 주소. 쉼표로 나눈 `a` 또는 `a..b`. 배포 쪽은 `external.excludeIPs` | 각 범위는 CIDR 안 오름차순, `-pool`과 겹치면 거절. 게이트웨이는 항상 들어간다 |

underlay 플래그를 하나도 주지 않으면 지금처럼 운영자가 만든 외부 Subnet을 읽기만 한다.

## 만드는 객체 (시작 때, 이 순서)

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

모든 Vpc는 `spec.enableExternal: true`, `spec.extraExternalSubnets: [<-kube-external-subnet>]`, 기본 라우트 `staticRoutes: [{cidr: 0.0.0.0/0, nextHopIP: <외부 게이트웨이>, policy: policyDst}]`로 외부 Subnet에 붙는다. 외부 게이트웨이는 underlay를 켜면 `-kube-underlay-gateway`, 아니면 운영자가 만든 외부 Subnet의 `spec.gateway`다(없으면 `domain.Invalid`). 이 셋이 있어야 OvnSnatRule이 동작했다(vws-test 42, 43). 게이트웨이 노드를 정하는 `kube-system/ovn-external-gw-config` ConfigMap은 이 서비스가 건드리지 않는다. 배포 쪽이 만든다(vws-deploy#127).

- 같은 이름의 객체가 id annotation 없이 있거나 우리가 쓰는 spec 필드가 다르면 `domain.Conflict`로 시작을 거절한다. 살아 있는 물리망을 덮어쓰지 않는다. 운영자가 미리 만든 외부 Subnet을 계속 쓰려면 underlay 플래그를 주지 않는다.
- 서비스는 이 객체를 지우지 않는다. 지우는 것은 운영자 몫이다.
- 배포 RBAC에 `provider-networks`, `vlans`의 `get`, `create`와 `subnets`의 `create`(이미 있음)가 필요하다. vws-deploy#149(PR #152)가 더한다.
- `excludeIps`에 EIP 풀을 넣지 않는다. Kube-OVN v1.16.12가 excludeIps 안의 주소를 OvnEip `v4Ip`로 받는지 실측한 뒤 정한다(vws-deploy#125 "Subnet의 excludeIps"). 그 전에는 Kube-OVN이 VPC lrp 주소를 풀 안에서 고를 수 있다.
- 이 변경 전에 만든 Vpc는 `enableExternal`, `extraExternalSubnets`, 기본 라우트가 없어서 다시 ensure하면 `domain.Conflict`가 난다. 덮어쓰지 않는다. 복구는 `docs/kube-ovn-conflict-recovery.md`를 따른다.

## 노드망 인터페이스 (vws-deploy#125)

노드망 설계와 플래그 계약은 vws-deploy#125 "vws-network 인터페이스 계약" 표가 진실이다. 이 문서의 플래그 이름은 그 표와 같다. 요점은 다음과 같다.

1. 배포 쪽(`vws render`)이 VWSCluster `spec.network.nodes.external`(nic, vlan, cidr, gateway, excludeIPs, eipPool)을 위 플래그로 바꾼다. 이 서비스는 호스트 네트워크를 설정하지 않는다.
2. NIC 이름은 모든 노드에서 같다. `customInterfaces`는 v1에 없다.
3. underlay NIC는 OVS 브리지로 넘어간다. 그 NIC에 호스트 망은 untagged 하나만 둘 수 있다(#125 규칙 4). 1장 NIC에 호스트 VLAN을 두면 underlay를 쓸 수 없다.
4. Kube-OVN 기본 네트워크는 geneve 그대로이고 underlay는 외부 Subnet(과 나중의 direct Subnet)에만 쓴다.

## 운영자가 만든 외부 Subnet (underlay 플래그 없음, #52)

underlay 플래그가 없으면 `Open`이 시작 때 `-kube-external-subnet`을 Get해서 `spec.cidrBlock` 안에 `-pool`이 있는지 본다. Subnet이 없거나 cidrBlock이 IPv4 prefix가 아니거나 풀이 밖이면 `domain.Invalid`로 시작을 거절한다. API 서버에 닿지 못하면 시작이 실패한다(전에는 시작한 뒤 ready만 실패했다). 이유와 오류 문구는 README "실행"에 있다.

## direct 네트워크 (`-kube-direct-networks`, #52, vws-deploy#125)

한 줄 JSON 배열이다. 배포 쪽이 VWSCluster `spec.network.nodes.direct`에서 그린다.

```json
[{"name":"prod","providerNetwork":"vws-pn0","interface":"ens3","vlan":200,"cidr":"192.168.200.0/24","gateway":"192.168.200.1","exclude":["192.168.200.2..192.168.200.20"]}]
```

모르는 키는 시작을 거절한다(종료 코드 2). `Open`이 검사하는 것:

- `name`은 `^[a-z][a-z0-9-]{0,14}$`이고 겹치지 않는다.
- `providerNetwork`, `interface`, `vlan`, `cidr`, `gateway`, `exclude`는 underlay 플래그와 같은 규칙을 탄다.
- ProviderNetwork 하나는 NIC 하나다. 외부망과 같은 NIC면 같은 ProviderNetwork를 써야 하고(공유), 다른 이름이면 거절한다.
- (NIC, VLAN) 쌍은 망 하나만 싣는다. 그래서 NIC 하나에 untagged 망은 하나뿐이고, 외부망과 direct는 VLAN이 달라야 한다(#125 규칙 4).
- CIDR은 외부망, 다른 direct, `-pool`과 겹치지 않는다.

시작 때 외부망 다음에 망마다 이 순서로 `ensure`한다. 다르면 `domain.Conflict`로 거절한다.

```yaml
kind: ProviderNetwork   # 외부망과 공유하면 같은 객체, spec 같음
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

NAD는 #125대로 VM 네임스페이스에 둔다. VM은 자기 네임스페이스의 NAD만 붙일 수 있기 때문이다. 그 네임스페이스는 `-kube-direct-nad-namespace`로 받고 기본값이 없다. `-kube-direct-networks`가 있는데 비어 있으면 `domain.Invalid`로 시작을 거절한다. #125가 이름을 정하지 않았으므로 배포 쪽이 정해서 넘긴다. 테넌트 subnet NAD는 그대로 `-kube-nad-namespace`다.

API에서는 모든 프로젝트에 VPC `vpc-direct`의 Subnet `subnet-direct-<name>`(라벨 `vws:network-type=direct`)으로 보인다. Get과 List(vpc 비우거나 `vpc-direct`)에 나오고, 이름의 region은 요청한 region이다. 읽기 전용이다.

- `vpc-direct`나 `subnet-direct-*`에 대한 CreateSubnet, UpdateSubnet, DeleteSubnet, CreateVPC는 `domain.Forbidden`(gRPC `PermissionDenied`, `UnauthorizedOperation`)이다. direct가 없어도 이 이름은 예약이다.
- `vpc-direct`에 AllocateElasticIP, 그리고 EIP의 VPC 서브넷에는 없고 direct CIDR에 있는 사설 주소에 AssociateElasticIP는 `domain.Invalid`(`InvalidParameterCombination`)다.
- 서비스는 direct 객체를 지우지 않는다. 플래그에서 뺀 망의 객체 정리는 운영자 몫이다.
- 배포 RBAC: 외부망 underlay와 같은 `provider-networks`, `vlans`의 `get`, `create`, `subnets`의 `create`에 더해 VM 네임스페이스의 NAD `get`, `create` Role이 필요하다(`docs/rbac.md`). ready는 direct 객체를 읽지 않는다.

## 미검증

- 실제 Kube-OVN에서 이 Subnet과 `extraExternalSubnets`가 OvnEip/OvnFip/OvnSnatRule의 외부 대역으로 동작하는지, `ovn-external-gw-config` 없이도 되는지.
- Kube-OVN이 외부 Subnet에서 스스로 고르는 주소(VPC lrp 등)가 `-pool`과 겹치지 않는지.
- 단일 NIC 노드에서 ProviderNetwork를 만들 때 노드 연결이 끊기지 않는지.
- 같은 ProviderNetwork 위 다른 VLAN의 direct Subnet에 VM이 붙어 물리 게이트웨이에 닿는지, 외부망 EIP 트래픽과 섞이지 않는지.
