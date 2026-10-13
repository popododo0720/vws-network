# vws-network

VWS 네트워크 제어 서비스다. 프로젝트 단위로 VPC, 서브넷, 보안 그룹, 엘라스틱 IP의 원하는 상태를 저장하고, 데이터플레인에 닿는 일은 연산으로 끝낸다.

## 실행

```sh
go run ./cmd/vws-network -db /tmp/vws-network.db -pool 10.250.0.0/24
```

`-listen`의 기본값은 `127.0.0.1:8080`이다.

## 테스트

```sh
go test ./...
```

## 동작

같은 프로젝트 안에서 VPC CIDR은 서로 겹칠 수 없다. 서브넷 CIDR은 그 VPC 안에 있어야 하고, 같은 VPC의 다른 서브넷과 겹칠 수 없다. 다른 프로젝트와는 겹쳐도 된다.

변경 RPC는 `idempotency_key`가 필요하다. 같은 프로젝트, 같은 키, 같은 요청은 원래 연산을 돌려준다. 키가 같은데 요청이 다르면 거절한다. 검증에 실패한 요청은 연산도 키도 남기지 않는다.

연산이 `SUCCEEDED`가 되면 가짜 데이터플레인이 그 리소스의 원하는 상태와 같다. 프로세스가 다시 뜨면 `RUNNING`으로 남아 있던 연산을 `PENDING`으로 되돌리고 이어서 수렴시킨다.

`third_party/vws-proto`는 `github.com/popododo0720/vws-proto`의 `main` `a1c6084`이다. `go.mod`의 replace가 그 경로를 가리킨다. vws-proto가 공개되면 replace를 제거하고 태그 버전을 require한다.

`third_party/vws-lib`는 `github.com/popododo0720/vws-lib`의 `v0.1.1`(`007a04d`)이다. `scripts/vendor-lib.sh vendor v0.1.1`으로 복사했고 replace로 쓴다. CI는 `VWS_LIB_OFFLINE=1 scripts/vendor-lib.sh check`로 사본이 바뀌지 않았는지 본다. 사본은 고치지 않는다.

## 관측성

`main`이 `obs.Setup`을 한 번 부르고 끝날 때 shutdown한다. 로그는 slog JSON이다. 메트릭은 `vws.rpc.server.*`, `vws.reconcile.total`·`vws.reconcile.duration`(데이터플레인 ensure 호출, `controller`=`network.<종류>`, `result`=`ok`·`conflict`·`error`), `vws.audit.events`(변경 RPC), `vws.panics`, `vws.errors`다. 레이블에 리소스 id나 요청 id는 넣지 않는다.

이 서비스가 읽는 환경 변수:

| 변수 | 뜻 |
| --- | --- |
| `OTEL_SERVICE_NAME`, `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG`, `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER`, `OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_SDK_DISABLED` | 표준 OpenTelemetry 설정. `OTEL_EXPORTER_OTLP_ENDPOINT`가 비면 아무것도 내보내지 않는다. |
| `SENTRY_DSN`, `SENTRY_ENVIRONMENT`, `SENTRY_RELEASE`, `SENTRY_SAMPLE_RATE` | `SENTRY_DSN`이 있을 때만 Sentry로 보낸다. |
| `VWS_LOG_LEVEL` | `debug`, `info`(기본), `warn`, `error`. |
| `VWS_LOG_FORMAT` | `json`(기본) 또는 `text`. |
| `VWS_HEALTH_ADDR` | `/healthz`, `/readyz` 주소. 비면 열지 않는다. `/readyz`는 `(*Service).Ready`를 부르고, SIGTERM 뒤에는 실패한다. |

gRPC health는 `docs/health.md`에 있다.

배포용 매니페스트는 두지 않는다.
