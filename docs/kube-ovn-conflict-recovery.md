# Kube-OVN 충돌 복구 절차

Kube-OVN 데이터플레인이 이름이 같은 기존 객체를 만나 `domain.Conflict`를 돌려줬을 때의 운영 절차다. 아래 내용은 `internal/network/dataplane_kubeovn.go`의 `ensureObject`, `remove`와 `internal/network/service.go`의 `failOrRetry`, `internal/domain/state.go`의 `Fail`, `Plan`을 읽고 적었다. 실제 Kube-OVN 클러스터에서 따라 해 본 절차가 아니다. 미검증이다.

## 언제 충돌이 나는가

객체 이름은 리소스 id이고 id는 `vpc-`, `subnet-` 같은 접두사 뒤에 무작위 8바이트 16진수가 붙는다. 같은 이름의 객체를 사람이 미리 만들었을 때만 충돌한다. 대상은 Vpc, Subnet, SecurityGroup, OvnEip, OvnSnatRule, VpcNatGateway, NetworkAttachmentDefinition이다.

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

코드가 지원하는 복구는 `FAILED` 리소스를 삭제하고 새 id로 다시 만드는 것뿐이다. 그 전에 알아야 할 것이 있다.

`remove`는 이름으로 객체를 지우고 annotation을 확인하지 않는다. `FAILED` 리소스의 삭제는 계획에 `ActDelete*`를 만들고, 그 호출이 이름이 같은 사람 객체를 지운다. 사람 객체를 남겨야 하면 이 삭제를 보내면 안 된다.

1. 이름이 같은 객체가 필요 없으면, 삭제를 보내 `FAILED` 리소스와 그 이름의 객체를 함께 정리한다. 의존 리소스(서브넷, 주소 등)가 있으면 삭제가 먼저 거절된다.
2. 그 객체가 필요하면 먼저 내용을 `kubectl get -o yaml`로 백업하고 다른 이름으로 다시 만든 뒤에 삭제를 보낸다. 객체 이름은 바꿀 수 없다.
3. 삭제가 끝나면 새 `CreateXxx` 요청을 보내 새 id를 받는다.

## 코드가 지원하지 않는 것

- 자동 adopt는 없다. annotation이 없는 객체를 우리 것으로 가져오지 않는다. 사람이 annotation을 붙여도 이미 `FAILED`가 된 생성은 되살아나지 않는다.
- `FAILED` 리소스를 다시 수렴시키는 재시도 RPC는 없다.
- 충돌한 객체를 우리 spec으로 덮어쓰지 않는다. 생성은 덮어쓰지 않고 기존 객체는 읽기만 한다.
- 사람 객체를 지키려고 삭제를 막는 검사는 없다. 위의 `remove` 동작을 따른다.
