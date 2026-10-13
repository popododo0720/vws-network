# 외부망 underlay 연동

vws-network가 엘라스틱 IP와 SNAT에 쓰는 Kube-OVN 외부 Subnet을 노드 NIC에 직결된 underlay로 직접 만든다. 근거 코드는 `internal/network/dataplane_kubeovn_underlay.go`다.

## 플래그

| 플래그 | 뜻 | 검사 |
| --- | --- | --- |
| `-pool` | 엘라스틱 IP 대역. **필수, 기본값 없음** | 없거나 RFC 5737 문서용 대역이면 시작 거절(종료 코드 2) |
| `-kube-external-subnet` | 외부 Subnet 이름 | underlay를 켜면 필수 |
| `-kube-provider-network` | 만들 ProviderNetwork 이름(12자 이하) | underlay를 켜면 필수 |
| `-kube-underlay-interface` | ProviderNetwork의 `defaultInterface`(예: `ens3`) | 필수 |
| `-kube-underlay-vlan` | 0은 flat, 1-4094는 태그 VLAN | 범위 밖이면 거절 |
| `-kube-underlay-cidr` | 외부망 CIDR | `-pool`을 품어야 한다 |
| `-kube-underlay-gateway` | 외부망 게이트웨이 | CIDR 안, `-pool` 밖 |

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
spec: {protocol: IPv4, cidrBlock: <cidr>, gateway: <gateway>, vlan: <provider>-vlan<id>}
```

- 같은 이름의 객체가 id annotation 없이 있거나 우리가 쓰는 spec 필드가 다르면 `domain.Conflict`로 시작을 거절한다. 살아 있는 물리망을 덮어쓰지 않는다. 운영자가 미리 만든 외부 Subnet을 계속 쓰려면 underlay 플래그를 주지 않는다.
- 서비스는 이 객체를 지우지 않는다. 지우는 것은 운영자 몫이다.
- 배포 RBAC에 `provider-networks`, `vlans`의 `get`, `create`와 `subnets`의 `create`(이미 있음)가 필요하다. vws-deploy `charts/vws/values.yaml`의 network `rbac`에는 아직 `provider-networks`, `vlans`가 없다.

## 노드망 인터페이스에 대한 가정

popododo0720/vws-deploy의 공개 이슈와 문서(2026-10-13 기준)에서 노드망이나 underlay 설계 이슈를 찾지 못했다. 찾아본 곳은 이슈 제목 검색(`ProviderNetwork`, `underlay`, `vlan`, `node network`), `docs/service-config.md`, `charts/vws/values.yaml`, `platform/kube-ovn.values.yaml`(`NETWORK_TYPE: geneve`)이다. 그래서 다음을 가정했다.

1. 노드망(NIC, VLAN, CIDR, 게이트웨이)은 배포 쪽이 정하고 차트 `services.network.flags`로 위 플래그를 그대로 넘긴다. 이 서비스는 노드를 설정하지 않는다.
2. 모든 노드에서 외부 NIC 이름이 같다. 노드마다 다르면 ProviderNetwork `customInterfaces`가 필요하고, 지금은 지원하지 않는다.
3. 외부 NIC가 관리망 NIC와 같을 수 있다(vws-test는 `ens3` 하나). Kube-OVN은 그 NIC를 OVS 브리지로 옮기므로 노드 IP가 브리지로 넘어간다.
4. Kube-OVN 기본 네트워크는 geneve 그대로이고 underlay는 외부 Subnet에만 쓴다.

vws-platform의 노드망 설계 이슈가 생기면 플래그 이름과 값 출처를 그쪽에 맞춘다.

## 미검증

- 실제 Kube-OVN에서 이 Subnet이 OvnEip/OvnFip/OvnSnatRule의 외부 대역으로 동작하는지. Kube-OVN 버전에 따라 `ovn-external-gw-config` ConfigMap이나 VPC `enableExternal`이 더 필요할 수 있다.
- Kube-OVN이 외부 Subnet에서 스스로 고르는 주소(VPC 게이트웨이 등)가 `-pool`과 겹치지 않는지. 지금은 `excludeIps`를 쓰지 않는다.
- 단일 NIC 노드에서 ProviderNetwork를 만들 때 노드 연결이 끊기지 않는지.
