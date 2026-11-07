# Import a SQLite store into the Kubernetes store

`vws-network import-sqlite` copies the desired state of a `-db` file into the
ConfigMaps of `-store-namespace` once, before the SQLite store is removed
(vws-network#130). Stop every service on the file first; the import holds no
Lease.

```sh
vws-network import-sqlite -from /var/lib/vws/network.db -store-namespace vws-system -dry-run
vws-network import-sqlite -from /var/lib/vws/network.db -store-namespace vws-system
vws-network import-sqlite -from /var/lib/vws/network.db -store-namespace vws-system -verify
```

- It reads the file's projects, operations (with their idempotency keys,
  fingerprints, attempts and errors) and pool addresses, and writes what the
  store lacks through the same commits the service makes: one per project
  document, then one per operation in the file's order.
- When the store holds any of them differently, it prints each `differs:` line,
  writes nothing and exits 1.
- `-dry-run` prints a `would create:` line per object and writes nothing.
- `-verify` writes nothing and exits 1 unless every project, operation,
  idempotency key and address of the file is in the store unchanged.
- It is idempotent. A second run, or a run after a crash, writes only what is
  still missing, and the last line ends in `writes: 0` once nothing is.
- A file of an older schema is migrated in place first, as a start on it would.
- Flag errors exit 2. Credentials come from `-kubeconfig`, else the in-cluster
  ServiceAccount. Only the `configmaps` row of the store Role in
  `docs/rbac.md` is used.
