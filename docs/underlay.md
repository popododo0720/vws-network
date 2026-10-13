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

아직 없는 것(vws-network#52): `-kube-direct-networks`(direct Subnet/Vlan, 같은 NIC면 external과 ProviderNetwork 공유), underlay 없이 운영자가 만든 외부 Subnet의 cidrBlock 안에 풀이 있는지 시작 때 확인하는 일, direct Subnet 노출.

## 미검증

- 실제 Kube-OVN에서 이 Subnet과 `extraExternalSubnets`가 OvnEip/OvnFip/OvnSnatRule의 외부 대역으로 동작하는지, `ovn-external-gw-config` 없이도 되는지.
- Kube-OVN이 외부 Subnet에서 스스로 고르는 주소(VPC lrp 등)가 `-pool`과 겹치지 않는지.
- 단일 NIC 노드에서 ProviderNetwork를 만들 때 노드 연결이 끊기지 않는지.
