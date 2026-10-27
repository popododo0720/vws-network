# Upgrade fixtures

Golden network records written by a released build. `TestUpgradeFixtures` opens
each directory below with the current code and checks that the database reaches
the current schema, every stored field decodes, an idempotency key replays, and
the current Kube-OVN `ensure` accepts the objects that build wrote.

Add a directory per release. Never rewrite an existing one, and regenerate only
at a release bump, not for a normal change.

## b5d57ef

Written by the build at commit `b5d57ef9f23c410d09d5f07249e2e28ac4b73fad`, the
tip of `main` when the fixtures were added. This repository has no release tag
yet, so the build SHA is the version.

- `net.db` is the SQLite database after a clean shutdown with `user_version` 2.
  It holds one project (`a1001`) with a VPC, the default group, a subnet, a
  security group with ingress and egress rules and one revoking rule, and an
  elastic IP with an instance association and a detaching address. It also holds
  one succeeded, one failed and one pending operation, and the address index.
- `*.yaml` are the Kube-OVN and Multus objects that build writes for the
  project, with `uid`, `resourceVersion`, `creationTimestamp`, `generation`,
  `managedFields` and `status` stripped.

To write a new release's directory, replace the value with that release's tag or
build SHA:

```sh
UPGRADE_FIXTURE_VERSION=b5d57ef \
  go test -tags gen -run TestGenerateUpgradeFixtures ./internal/network
```

The normal `test` job runs `TestUpgradeFixtures`; the generator stays behind the
`gen` build tag.
