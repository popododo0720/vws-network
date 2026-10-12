# 에이전트 규칙

새 변경은 가장 가까운 예시를 복사한다. 리소스 규칙은 `internal/domain`의 `State` 메서드에만 둔다. 저장은 `repository.commit` 한 경로만 쓴다. gRPC 핸들러는 값을 옮기고 `network.Service` 메서드 하나만 호출한다.

## 규칙표

| 규칙 | 강제 |
| --- | --- |
| VPC CIDR은 정규 IPv4이고 길이는 /16에서 /28이다. `0.0.0.0/8`은 거절한다. 한 프로젝트 안에서는 겹치지 않는다. | `domain.CheckVPCCIDR`, `(*State).CreateVPC`, `TestCheckVPCCIDR`, `TestVPCOverlapAndSubnetContainment`, `TestServiceAgainstStores/overlap_writes_nothing` |
| VPC를 다시 지워도 빈 보안 그룹을 만들지 않는다. | `(*State).DeleteVPC`, `TestDeleteVPCWithoutDefaultGroup`, `TestRetryVPCDeleteDoesNotCreateGhostGroup` |
| 서브넷 CIDR은 VPC 안에 있고, 같은 VPC의 서브넷과 겹치지 않으며, /28보다 길지 않다. | `domain.CheckSubnetCIDR`, `(*State).CreateSubnet`, 위 테스트 |
| 규칙의 피어 CIDR은 VPC CIDR 검사를 타지 않는다. `0.0.0.0/0`을 허용한다. | `domain.CheckRule`, `TestPeerCIDRIsNotVPCCIDR` |
| 의존 리소스가 있으면 삭제를 거절한다. 이미 없는 대상의 삭제는 성공한다. | `(*State).DeleteVPC`와 같은 메서드, `testConvergeAndDelete`, `testIdempotency` |
| 엘라스틱 IP는 설정된 풀에서 한 번만 할당되고, 그 프로젝트의 VPC CIDR 밖에 있다. | `domain.NextAddress`, `(*State).AllocateElasticIP`, `TestRepositoryContract`, `testElasticIP` |
| 엘라스틱 IP는 만들 때 VPC에 묶이고 바뀌지 않는다. 업데이트에서 빈 VPC는 저장된 값을 지운다고 보지 않는다. | `(*Service).UpdateElasticIP`, `testElasticIP`, `TestUpdateElasticIPKeepsVPC`, `TestAddressVPCAndAssociation` |
| 인스턴스 연결(instance, device name)은 한 NIC에 주소 하나이고, 연결된 주소는 해제할 수 없다. | `(*State).SetElasticIPAssociation`, `(*State).ReleaseElasticIP`, `TestElasticIPInstanceAssociation`, `TestElasticIPAssociationSurvivesEncoding` |
| 변경은 멱등 키로 한 번만 적용된다. 거절된 요청은 키를 남기지 않는다. | `(*Service).mutate`, `testIdempotency`, `testOverlapWritesNothing` |
| 원하는 상태와 그 연산은 한 번에 커밋된다. | `repository.commit`, `TestRepositoryContract` |
| 재시작은 `RUNNING` 연산을 `PENDING`으로 되돌리고 같은 경로로 수렴한다. | `repository.requeue`, `TestRestartRequeuesRunningOperation`, `TestSQLiteSurvivesReopen` |
| 실패한 VPC의 삭제 중인 서브넷, 보안 그룹, 엘라스틱 IP는 계속 지운다. | `planSubnet`, `planGroup`, `planEIP`, `TestPlanDeletesChildOfFailedVPC`, `TestServiceAgainstStores/delete_children_of_failed_vpc` |
| 철회 중인 규칙은 데이터플레인 삭제에 실패해도 다시 철회된다. | `(*State).RevokeRules`, `TestPlanRevokeAndSettleClearsMark`, `TestServiceAgainstStores/revoke_retries_after_failure` |
| 피어 보안 그룹은 그 그룹을 가리키는 규칙보다 먼저 보장된다. | `(*State).ensureRules`, `TestPlanEnsuresPeerBeforeRule`, `TestPeerGroupEnsuredBeforeRule` |
| 클레임이 정산되지 않으면 그 연산을 `PENDING`으로 되돌린다. | `(*Service).step`, `TestStepReleasesClaimWhenLoadFails` |
| 틀린 클레임 토큰이나 이미 끝난 연산은 상태를 쓰지 않는다. | `repository.commit`, `TestSettleTokenGuardsState` |
| SQLite 스키마와 `user_version`은 한 트랜잭션이고, `busy_timeout`은 연결에 붙는다. | `openSQLite`, `TestSQLiteSchemaIsOneVersion` |
| 멱등 지문은 요청의 JSON 이름으로 계산한다. | 요청 구조체의 `json` 태그, `TestRequestJSONNames` |
| VPC, 서브넷, 보안 그룹의 description과 VPC, 서브넷, 보안 그룹, 주소의 labels는 저장되고 조회되며 `update_mask`로 바뀐다. 라벨은 64개 이하이고 키는 `[a-z][a-z0-9_-]{0,62}`이며 값은 63바이트 이하의 제어 문자가 없는 UTF-8이다. description은 1024바이트 이하다. 프로젝트 id는 리소스 id 문법이라 `a`와 계정 번호를 잇는 형식도 받는다. | `domain.CheckLabels`, `domain.CheckDescription`, `(*State).CreateVPC`, `(*State).ApplyVPCText`, `TestLabelsRoundTrip`, `TestSQLiteMigratesV1`, `TestLabelsSurviveReopen`, `TestLabelsIdempotent`, `TestAccountProjectStoresLabels` |
| 데이터플레인 구현은 `fakeDataplane`과 `kubeOVN`이고, 다른 패키지에서 호출할 수 없다. | 비공개 `dataplane`, `var _ dataplane = (*fakeDataplane)(nil)`, `var _ dataplane = (*kubeOVN)(nil)` |
| Kube-OVN 객체는 클러스터 범위이고 이름이 리소스 id다. AWS id는 annotation에만 두며 CRD를 만들지 않는다. 생성은 덮어쓰지 않고, 없는 객체의 삭제는 성공한다. | `kubeOVN.ensure`, `kubeOVN.remove`, `annotationID`, `TestKubeOVNVPCAndSubnet`, `TestKubeOVNDeleteIsIdempotent` |
| 규칙 추가와 삭제는 SecurityGroup의 ingress 또는 egress 목록 하나의 Update이고 같은 규칙은 한 번만 들어간다. 지원하지 못하는 규칙(tcp, udp 포트 0)은 `domain.Invalid`로 거절한다. | `ovnRule`, `(*kubeOVN).editRules`, `TestOvnRule`, `TestOvnRuleRejectsUnsupported`, `TestKubeOVNRulesAreIdempotent` |
| 엘라스틱 IP는 `OvnEip`, 연결은 `OvnFip`이다. 외부 대역 이름은 `kubeOVN.externalSubnet` 한 곳에서만 읽고, OvnFip 이름은 `fipName` 한 곳에서만 만든다. OvnFip이 가리키는 `OvnEip`은 지우지 않는다. | `(*kubeOVN).ensureElasticIP`, `(*kubeOVN).deleteElasticIP`, `fipName`, `TestKubeOVNElasticIPLifecycle`, `TestKubeOVNElasticIPNeedsExternalSubnet` |
| 서브넷마다 Multus `NetworkAttachmentDefinition`이 하나이고 서브넷과 함께 만들고 지운다. NAD 이름은 `nadName` 한 곳에서만 만든다. | `(*kubeOVN).ensureNAD`, `nadName`, `TestKubeOVNSubnetOwnsOneNAD`, `TestKubeOVNSubnetNeedsNADNamespace` |
| 데이터플레인이 입력 오류(`domain.Invalid`)를 돌려주면 재시도하지 않고 연산을 실패시킨다. | `(*Service).failOrRetry`, `TestInvalidDataplaneErrorFailsWithoutRetry` |
| RPC를 추가하면 핸들러를 비워 둘 수 없다. | `TestHandlersOverrideUnimplemented` |
| VPC, 서브넷, 보안 그룹의 생성, 수정, 삭제 연산 타입 문자열은 RPC 이름과 같다. | `domain.OpCreateVPC`, `domain.OpUpdateVPC`, `domain.OpDeleteVPC`와 같은 상수, `TestOperationTypesAreRPCNames` |
| 도메인 오류 종류는 모두 gRPC 코드로 바뀐다. | `toStatus`, `TestAllErrKindsMapped` |
| proto는 `third_party/vws-proto`로 두고 `go.mod` replace가 그 모듈을 가리킨다. | `TestVWSProtoIsReplaced` |
| 배포 매니페스트와 상표 고지를 넣지 않는다. | `TestNoDeployment`, `TestNoBrandNotice` |
| 포맷, 테스트, 린트가 통과해야 한다. | `.github/workflows/ci.yml`의 `gofmt`, `go test ./...`, `golangci-lint` |
| `main`에 직접 푸시하지 않고, 커밋은 원자적으로 나눈다. | 아래 절차. 브랜치 보호는 저장소 설정이다. |
| 설계와 반복되는 실수는 pstack으로 다룬다. | 아래 pstack 절차. 판단이라 린트로 막지 않는다. |

## 브랜치와 원자적 커밋

`main`에 직접 커밋하거나 푸시하지 않는다. 작업 브랜치를 만들고, 그 브랜치에서만 푸시한다. 이 저장소의 클라우드 작업 브랜치 이름은 `cursor/<설명>-<번호>`이다.

커밋 하나는 한 가지 변경만 담는다. 도메인 규칙, 저장소와 서비스, gRPC처럼 따로 읽고 되돌릴 수 있는 단위로 나눈다. 포맷만 고친 커밋과 동작 변경을 섞지 않는다.

## pstack

호출 경계를 넘는 설계는 코드를 쓰기 전에 architect를 쓴다. 서로 다른 모양의 스케치를 두 개 만들고, 불변조건의 주인이 하나인 쪽을 고른다.

구현을 합치기 전에 interrogate를 쓴다. 같은 diff를 서로 다른 모델이 보고, 맞는 지적만 고친다.

같은 종류의 실수가 반복되면 correct를 쓴다. 고치는 순서는 구조, 타입, 실패 원인이 드러나는 린트, 테스트, 문서다. 규칙을 이 표에 추가할 때 강제 칸이 비어 있으면 안 된다. 문장만 있고 깨지는 검사가 없으면 그 규칙은 아직 없는 것이다.

## 작업 방식

모든 코딩과 개발 작업은 pstack poteto-mode 스타일로 한다. 답은 짧고 단순하게 쓰고, 코드는 가장 작은 변경으로 하고, 실제로 돌려 본 것만 보고한다.

서브에이전트는 effort를 high로 쓴다.

이 프로젝트(VWS)의 작업에 Cursor 클라우드 에이전트를 쓰지 않는다. 사용자가 직접 시킬 때만 예외다.

이 절은 판단이라 린트로 막지 않는다.
