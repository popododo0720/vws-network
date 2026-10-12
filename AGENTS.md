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
| Kube-OVN 객체는 클러스터 범위이고 이름이 리소스 id다. AWS id는 annotation에만 두며 CRD를 만들지 않는다. annotation 키는 모두 `vws.popododo0720.io/<이름>`이다. 생성은 덮어쓰지 않고, 없는 객체의 삭제는 성공한다. 이미 있는 객체는 id annotation과 우리가 쓰는 spec 필드가 같아야 하고, 다르면 `domain.Conflict`이며 재시도하지 않는다. 삭제 중인 객체는 재시도한다. 삭제는 id annotation이 없거나 다르면 `domain.Conflict`로 거절하고 UID 조건으로 지운다. | `kubeOVN.ensure`, `kubeOVN.ensureObject`, `kubeOVN.remove`, `kubeOVN.removeObject`, `annotationID`, `(*Service).failOrRetry`, `TestKubeOVNRemoveRequiresOwnerAnnotation`, `TestEnvtestRemoveKeepsForeignObject`, `TestKubeOVNVPCAndSubnet`, `TestKubeOVNDeleteIsIdempotent`, `TestAnnotationKeyScheme`, `TestNoAnnotationKeyOutsideScheme`, `TestEnsureRejectsForeignObject`, `TestEnsureKeepsExistingObjectUntouched`, `TestEnsureTerminatingObjectIsRetryable`, `TestEnsureGroupIgnoresRuleLists`, `TestEnsureGatewayIgnoresHostSubnet`, `TestConflictFromDataplaneFailsWithoutRetry` |
| 규칙 추가와 삭제는 SecurityGroup의 ingress 또는 egress 목록 하나의 Update이고 같은 규칙은 한 번만 들어간다. 지원하지 못하는 규칙(tcp, udp 포트 0)은 `domain.Invalid`로 거절한다. | `ovnRule`, `(*kubeOVN).editRules`, `TestOvnRule`, `TestOvnRuleRejectsUnsupported`, `TestKubeOVNRulesAreIdempotent` |
| 엘라스틱 IP는 `OvnEip`, 연결은 `OvnFip`이다. 외부 대역 이름은 `kubeOVN.externalSubnet` 한 곳에서만 읽고, OvnFip 이름은 `fipName` 한 곳에서만 만든다. OvnFip이 가리키는 `OvnEip`은 지우지 않는다. | `(*kubeOVN).ensureElasticIP`, `(*kubeOVN).deleteElasticIP`, `fipName`, `TestKubeOVNElasticIPLifecycle`, `TestKubeOVNElasticIPNeedsExternalSubnet` |
| 서브넷마다 Multus `NetworkAttachmentDefinition`이 하나이고 서브넷과 함께 만들고 지운다. NAD 이름은 `nadName` 한 곳에서만 만든다. | `(*kubeOVN).ensureNAD`, `nadName`, `TestKubeOVNSubnetOwnsOneNAD`, `TestKubeOVNSubnetNeedsNADNamespace` |
| kubeconfig가 있을 때만 `Open`이 `kubeOVN`을 쓰고, 없으면 `fakeDataplane`이다. kubeconfig 없이 Kube-OVN 옵션만 주거나, kubeconfig만 있고 외부 대역 이름이 없으면 `domain.Invalid`로 거절한다. 생성은 `newDataplane` 한 곳에서만 한다. | `newDataplane`, `KubeOVNConfig`, `TestNewDataplaneSelection`, `TestNewDataplaneRejectsBadConfig`, `TestOpenUsesKubeOVNOnlyWithKubeconfig` |
| `SNAT`이 켜지면 서브넷마다 `OvnEip`(type nat)과 `OvnSnatRule`이 있고, VPC의 `VpcNatGateway`는 하나이며 처음 SNAT을 만든 서브넷에 둔다. 서브넷을 지우면 규칙, 주소 순으로 지우고, 그 서브넷이 게이트웨이를 두었으면 게이트웨이를 지운 뒤 같은 VPC의 남은 서브넷 중 이름이 가장 앞선 것에 다시 만든다. 남은 서브넷이 없으면 게이트웨이도 없다. 게이트웨이의 id annotation이 없거나 다르면 `domain.Conflict`로 거절하고 지우지 않으며, 지울 때는 읽은 UID를 조건으로 건다. 상태는 클러스터 객체뿐이다. 외부 대역 없이는 켤 수 없다. 이름은 `natGatewayName`, `snatName`에서만 만든다. | `(*kubeOVN).ensureSNAT`, `(*kubeOVN).deleteSNAT`, `(*kubeOVN).rehomeGateway`, `natGatewayName`, `snatName`, `TestKubeOVNSubnetSNAT`, `TestKubeOVNDeleteSubnetSNAT`, `TestKubeOVNDeletingGatewayHostMovesGateway`, `TestKubeOVNDeleteHostRecreatesLostGateway`, `TestKubeOVNDeleteHostWaitsForTerminatingGateway`, `TestKubeOVNRehomeKeepsForeignGateway`, `TestEnvtestGatewayMovesWhenHostSubnetIsDeleted`, `TestKubeOVNSNATNeedsExternalSubnet`, `TestNewDataplaneRejectsBadConfig` |
| `kubeOVN`은 실제 API 서버로도 검증한다. 테스트는 `envtest` 빌드 태그 뒤에 있고 `KUBEBUILDER_ASSETS`가 없으면 건너뛴다. CI는 `go test ./...`와 별도의 `envtest` 단계(`setup-envtest` v0.25.2, Kubernetes 1.37.0 고정)에서 이를 실제로 돌린다. CRD는 `internal/network/testdata/crds`의 최소 픽스처이고 스키마를 검증하지 않는다. 서브넷 CIDR 포함 관계 검사는 dataplane이 아니라 서비스가 한다. | `startEnv`, `make envtest`, `TestEnvtestSubnetAndSNATLifecycle`, `TestEnvtestRulesAndElasticIP`, `TestEnvtestServiceThroughOpen` |
| 데이터플레인이 입력 오류(`domain.Invalid`)나 충돌(`domain.Conflict`)을 돌려주면 재시도하지 않고 연산을 실패시킨다. | `(*Service).failOrRetry`, `TestInvalidDataplaneErrorFailsWithoutRetry` |
| RPC를 추가하면 핸들러를 비워 둘 수 없다. | `TestHandlersOverrideUnimplemented` |
| VPC, 서브넷, 보안 그룹의 생성, 수정, 삭제 연산 타입 문자열은 RPC 이름과 같다. | `domain.OpCreateVPC`, `domain.OpUpdateVPC`, `domain.OpDeleteVPC`와 같은 상수, `TestOperationTypesAreRPCNames` |
| 도메인 오류 종류는 모두 gRPC 코드로 바뀐다. | `toStatus`, `TestAllErrKindsMapped` |
| proto는 `third_party/vws-proto`로 두고 `go.mod` replace가 그 모듈을 가리킨다. | `TestVWSProtoIsReplaced` |
| 배포 매니페스트와 상표 고지를 넣지 않는다. | `TestNoDeployment`, `TestNoBrandNotice` |
| 포맷, 테스트, 린트가 통과해야 한다. | `.github/workflows/ci.yml`의 `gofmt`, `go test ./...`, `envtest`, `golangci-lint` |
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

## Insider-risk guardrails

봇도 내부자 위험이다. 아래는 예외 없는 규칙이다.

1. **검증 독립**: 중요 결과(머지, 보안, 데이터)는 작성한 봇/모델이 스스로 검증하지 않는다. 검증은 작성 봇이 아닌 다른 봇(또는 사람)이 한다. (프로세스 규칙, CI로 강제 불가)
2. **기록**: 모든 변경은 이슈 → PR(`Closes #N`) → 커밋으로 남긴다. 작업 로그는 PR 본문/이슈 코멘트에 사람이 읽을 수 있게 쓴다. force-push·히스토리 재작성 금지(사용자 승인 시 예외).
3. **실패 테스트**: 새 기능은 정상 경로만 테스트하지 않는다. 전원 차단/재시작, 멱등 재실행, 악성·경계 입력 케이스를 최소 1개씩 포함하거나 불가 사유를 PR에 적는다.
4. **최소 권한**: main 직접 푸시 금지. 모든 변경은 main에서 딴 독립 브랜치의 단일 PR. 스택 PR 금지. 로컬 머지 금지. 토큰은 해당 레포 범위만. RBAC은 서비스 코드가 실제 호출하는 리소스만, 가능하면 Role(네임스페이스 범위).
5. **감사 분리**: 작성 봇 ≠ 검증 봇 ≠ 머지 판단. 작성 봇은 자기 PR을 머지하지 않는다. 머지는 base==main 이고 head 커밋 CI가 전부 success일 때만.
6. **격리·비상정지**: 봇/에이전트 즉시 중단은 사용자 요청 또는 오케스트레이터가 해당 봇 세션 중단 + 토큰 폐기(`gh auth`/PAT revoke). 위험 작업은 샌드박스(박스/임시 클러스터)에서만. 운영 클러스터 접근 금지.
7. **사고 공개**: 규칙 위반·사고는 숨기지 않고 이슈에 `원인 / 영향 / 조치 / 재발방지`로 기록한다 (라벨 `incident`).

이 레포의 서버측 보호는 없다(private, 무료 플랜). 1, 5, 6은 프로세스 규칙이고, 2와 4의 일부는 `.github/workflows/repo-guard.yml`이 PR의 base와 `Closes #N`을 검사하고 `.github/workflows/direct-push-watch.yml`이 main에 PR 없이 들어온 커밋을 사후에 알린다. 차단이 아니라 탐지다. 가드 코드는 `scripts/guard-agents.sh`이고 `make guard`가 `selftest`와 `agents`를 돌린다.

이 레포의 추가 규칙이다.

- 보안 그룹, SNAT, 엘라스틱 IP, 서브넷 삭제처럼 네트워크 격리를 바꾸는 변경은 작성 봇이 아닌 다른 봇이나 사람이 검증한 뒤에만 머지한다.
- 실제 Kube-OVN 검증은 T2 랩 몫이고, 검증하지 못한 항목은 PR 본문 미검증 칸에 적는다.
- 이슈, PR 본문, 코드 어디에도 AWS 계정 ID 숫자를 쓰지 않는다.
