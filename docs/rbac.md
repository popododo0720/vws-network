# vws-network RBAC

## 게이트웨이 노드 라벨 (`-kube-gateway-nodes`)

| apiGroup | resource | verbs | 쓰는 곳 |
| --- | --- | --- | --- |
| "" (core) | nodes | get, patch | `(*kubeOVN).labelGatewayNodes` |

- `-kube-gateway-nodes`를 주면 VPC가 하나라도 있는 동안 `-kube-gateway-recheck`(기본 30s)마다 그 노드들을 get하고, `ovn.kubernetes.io/external-gw`가 `true`가 아니면 merge patch로 `true`를 붙인다. kube-ovn-controller가 재시작하면 이 라벨을 `false`로 바꿔 SNAT이 깨진다. `ovn-external-gw-config` ConfigMap으로 고정하면 underlay가 깨져서 이 방식을 쓴다.
- 목록에 없는 노드는 읽지도 바꾸지도 않는다. 라벨을 지우지 않는다. list와 watch는 필요 없다.
- nodes는 클러스터 범위라 ClusterRole이 필요하다. RBAC은 노드 이름으로 좁힐 수 있다(`resourceNames`). 차트는 vws-platform이 고친다.
