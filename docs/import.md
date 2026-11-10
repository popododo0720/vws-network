# Import a SQLite store into the Kubernetes store

`vws-network import-sqlite` copies the desired state of a `-db` file into the
ConfigMaps of `-store-namespace` once, before the SQLite store is removed
(vws-network#130). Stop every service on the file first. Stop older destination binaries before
upgrading; they do not participate in lifecycle admission. The import holds no
Lease.

```sh
vws-network import-sqlite -from /var/lib/vws/network.db -store-namespace vws-system -dry-run
vws-network import-sqlite -from /var/lib/vws/network.db -store-namespace vws-system
vws-network import-sqlite -from /var/lib/vws/network.db -store-namespace vws-system -verify
```

- It reads the file's projects, operations (with their idempotency keys,
  fingerprints, attempts and errors) and pool addresses, and writes what the
  store lacks through the same commits the service makes: one per resource,
  so a large project never has to fit one object, then one per operation in
  the file's order.
- When the store holds any record differently, or holds a project, operation,
  key or address the file does not, it prints each `differs:` line, writes
  no resource records and exits 1. The lifecycle marker remains `importing` so
  services cannot start on a partial or rejected import. So does a resource id two projects of the file hold, an
  address another project holds in the store, and a document too large for
  the store to change again: each is found before the first resource write.
- `-dry-run` prints a `would create:` line per object and writes nothing.
- `-verify` writes nothing and exits 1 unless the store holds exactly the
  file's projects, operations, idempotency keys and addresses.
- `-dry-run` and `-verify` read the store with lists only. A change a crash
  left unfinished is reported as a difference, not finished; a run without
  either flag finishes it first.
- The namespace ConfigMap `vws-network-lifecycle` admits either the first
  service start (`serving`) or import (`importing`) atomically, before repair.
  Import refuses a `serving` store. Services refuse an `importing` store.
  Failed imports stay `importing`; rerun with the same offline source snapshot.
  Its digest prevents different snapshots from being combined on restart.
- Completion sets the marker to `imported`. Every later write import is a
  zero-write no-op, even after services have changed the imported records.
  It does not migrate the source or repair the destination. Use `-verify`,
  not a second write import, to check equality. Never delete or edit the marker
  to bypass admission on a namespace that has served.
- A restart during import writes only missing records. A crash between taking
  a claim and the head swap leaves the claim for up to
  10 minutes (the store's grace period); a rerun inside that window stops
  with `project version moved` or `pool address taken` and succeeds after it.
- A write run migrates a file of an older schema in place first, as a start
  on it would. `-dry-run` and `-verify` refuse such a file and leave it as it
  is.
- Flag errors exit 2. Credentials come from `-kubeconfig`, else the in-cluster
  ServiceAccount. Only the `configmaps` row of the store Role in
  `docs/rbac.md` is used.
