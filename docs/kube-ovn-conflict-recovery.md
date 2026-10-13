# Kube-OVN 충돌 복구 절차

Kube-OVN 데이터플레인이 이름이 같은 기존 객체를 만나 `domain.Conflict`를 돌려줬을 때의 운영 절차다. 아래 내용은 `internal/network/dataplane_kubeovn.go`의 `ensureObject`, `remove`, `removeObject`, `deleteSubnet`과 `internal/network/service.go`의 `failOrRetry`, `internal/domain/state.go`의 `Fail`, `Plan`을 읽고 적었다. 실제 Kube-OVN 클러스터에서 따라 해 본 절차가 아니다. 미검증이다.

## 언제 충돌이 나는가

객체 이름은 리소스 id이고 id는 `vpc-`, `subnet-` 같은 접두사 뒤에 무작위 8바이트 16진수가 붙는다. 같은 이름의 객체를 사람이 미리 만들었을 때만 충돌한다. 대상은 Vpc, Subnet, SecurityGroup, OvnEip, OvnSnatRule, NetworkAttachmentDefinition이다.

`ensureObject`는 먼저 생성을 시도하고, 이미 있으면 그 객체를 읽어 아래 순서로 검사한다.

1. 삭제 중(`deletionTimestamp`가 있음)이면 일반 오류를 돌려주고 연산은 재시도된다. 충돌이 아니다.
2. annotation `vws.popododo0720.io/id`가 없거나 id와 다르면 `domain.Conflict`다.
3. 우리가 쓰는 spec 필드 중 하나라도 원하는 값과 다르면 `domain.Conflict`다. 보안 그룹의 규칙 목록과 NAT 게이트웨이의 호스트 서브넷은 비교하지 않는다.

재부팅이나 재시작은 충돌의 원인이 아니다. 재시작은 `RUNNING` 연산을 `PENDING`으로 되돌리고(`repository.requeue`), 다시 도는 ensure는 우리가 만든 객체(id annotation과 spec이 같음)를 정상으로 본다. 충돌은 이름이 같은 다른 객체가 있거나 사람이 우리 객체의 annotation이나 spec을 바꿨을 때만 난다.

## 충돌이 나면 코드가 하는 일

`failOrRetry`는 `domain.Invalid`와 `domain.Conflict`를 재시도하지 않고 연산을 바로 `FAILED`로 끝낸다. 오류 메시지는 객체 종류, 이름, 기대한 값과 실제 값을 담는다. gRPC로는 `AlreadyExists`(`ALREADY_EXISTS`)로 보인다.

리소스 상태는 연산 종류에 따라 갈린다.

- 만들던 중(`PENDING`)이던 리소스는 `FAILED`가 된다. `Plan`은 `FAILED` 리소스에 ensure를 만들지 않고 `FAILED` 부모 아래의 자식도 막는다. 같은 id로 다시 수렴하는 길이 없다.
- 이미 있던 리소스의 수정 중 충돌이면 리소스는 그대로이고 연산만 `FAILED`다.

## 복구

### 수정 연산이 충돌한 경우

1. 오류 메시지가 가리키는 객체를 `kubectl get <kind> <이름> -o yaml`로 본다.
2. annotation이 빠졌으면 `kubectl annotate <kind> <이름> vws.popododo0720.io/id=<이름>`으로 되돌린다. spec이 달라졌으면 메시지의 기대 값으로 되돌린다.
3. 같은 수정을 새 `idempotency_key`로 다시 보낸다. 같은 키는 원래 연산을 그대로 돌려주므로 효과가 없다.

### 생성 연산이 충돌해 리소스가 `FAILED`인 경우

코드가 지원하는 복구는 `FAILED` 리소스를 삭제하고 새 id로 다시 만드는 것뿐이다.

`remove`와 `removeObject`는 객체를 읽어 annotation `vws.popododo0720.io/id`가 리소스 id와 같을 때만 지운다. annotation이 없거나 다르면 `domain.Conflict`로 거절하고 아무것도 지우지 않는다. 지울 때는 읽은 객체의 UID를 삭제 조건으로 걸어서, 읽은 뒤 같은 이름으로 바뀐 객체는 지우지 않는다. 객체가 없으면 성공이다. 그래서 `FAILED` 리소스의 삭제는 이름이 같은 사람 객체를 지우지 않는다. 대신 그 삭제 연산이 `Conflict`로 `FAILED`가 된다.

삭제 연산이 실패해도 리소스는 `DELETING`에 남는다. `FailKind`는 `PENDING` 리소스만 `FAILED`로 바꾸기 때문이다. 서브넷 삭제는 SNAT 규칙, SNAT 주소, 그 서브넷이 둔 NAT 게이트웨이, Subnet, NAD 순으로 지우므로, 뒤 단계에서 충돌하면 앞 단계에서 지운 우리 객체는 이미 없다.

1. 오류 메시지가 가리키는 객체를 `kubectl get <kind> <이름> -o yaml`로 본다.
2. 그 객체가 필요하면 내용을 백업하고 다른 이름으로 다시 만든다. 객체 이름은 바꿀 수 없다. 그다음 원래 이름의 객체를 `kubectl delete`로 지운다. 필요 없으면 바로 지운다. 코드는 이 객체를 지우지 않으므로 사람이 지워야 한다.
3. 같은 삭제를 새 `idempotency_key`로 다시 보낸다. 객체가 없으므로 삭제가 끝난다. 의존 리소스(서브넷, 주소 등)가 있으면 삭제가 먼저 거절된다.
4. 삭제가 끝나면 새 `CreateXxx` 요청을 보내 새 id를 받는다.

## 코드가 지원하지 않는 것

- 자동 adopt는 없다. annotation이 없는 객체를 우리 것으로 가져오지 않는다. 사람이 annotation을 붙여도 이미 `FAILED`가 된 생성은 되살아나지 않는다.
- `FAILED` 리소스를 다시 수렴시키는 재시도 RPC는 없다.
- 충돌한 객체를 우리 spec으로 덮어쓰지 않는다. 생성은 덮어쓰지 않고 기존 객체는 읽기만 한다.
- id annotation이 없거나 다른 객체를 대신 지우거나 옮기는 기능은 없다. 사람이 `kubectl`로 정리해야 한다.

## 이전 릴리스가 만든 VpcNatGateway

`-kube-snat`은 이제 OVN 네이티브 SNAT(OvnEip + OvnSnatRule)만 쓴다. 이전 릴리스는 VPC마다 iptables `VpcNatGateway` `nat-<vpc id>`를 만들었고, 그 파드는 kube-system에 외부 Subnet 이름의 NAD가 없어 뜨지 못했다(vws-test 40, 41). 서비스는 그 객체를 지우지 않는다. 운영자가 id annotation(`vws.popododo0720.io/id: nat-<vpc id>`)을 확인하고 `kubectl delete vpc-nat-gateway nat-<vpc id>`로 지운다.
