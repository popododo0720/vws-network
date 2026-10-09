package network

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go sqlite driver

	"github.com/popododo0720/vws-network/internal/domain"
)

const schemaVersion = 1

const opSelect = `SELECT seq, id, project, type, resource, lane, state, err_kind, err_msg, key, fingerprint, attempts, not_before, token FROM operations`

type sqliteRepo struct {
	db *sql.DB
	mu chan struct{}
}

func openSQLite(ctx context.Context, path string) (*sqliteRepo, error) {
	// Pragmas live on the DSN so a replaced connection keeps the busy timeout.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	r := &sqliteRepo{db: db, mu: make(chan struct{}, 1)}
	if err := r.lock(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	defer r.unlock()
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		_ = db.Close()
		return nil, err
	}
	if version == 0 {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
CREATE TABLE projects (
  id TEXT PRIMARY KEY,
  version INTEGER NOT NULL,
  state BLOB NOT NULL
);
CREATE TABLE operations (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT UNIQUE NOT NULL,
  project TEXT NOT NULL,
  type TEXT NOT NULL,
  resource TEXT NOT NULL,
  lane TEXT NOT NULL,
  state INTEGER NOT NULL,
  err_kind INTEGER NOT NULL DEFAULT 0,
  err_msg TEXT NOT NULL DEFAULT '',
  key TEXT NOT NULL,
  fingerprint BLOB NOT NULL,
  attempts INTEGER NOT NULL,
  not_before INTEGER NOT NULL,
  token TEXT NOT NULL DEFAULT '',
  UNIQUE (project, key)
);
CREATE TABLE addresses (
  addr TEXT PRIMARY KEY,
  project TEXT NOT NULL
);
`); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `PRAGMA user_version = 1`); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			_ = db.Close()
			return nil, err
		}
	} else if version != schemaVersion {
		_ = db.Close()
		return nil, errors.New("network: unsupported sqlite schema version")
	}
	return r, nil
}

func (r *sqliteRepo) lock(ctx context.Context) error {
	select {
	case r.mu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *sqliteRepo) unlock() { <-r.mu }

func (r *sqliteRepo) close() error { return r.db.Close() }

func (r *sqliteRepo) load(ctx context.Context, p domain.ProjectID) (domain.State, int64, error) {
	if err := r.lock(ctx); err != nil {
		return domain.State{}, 0, err
	}
	defer r.unlock()
	var ver int64
	var blob []byte
	err := r.db.QueryRowContext(ctx, `SELECT version, state FROM projects WHERE id = ?`, string(p)).Scan(&ver, &blob)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.State{}, 0, nil
	}
	if err != nil {
		return domain.State{}, 0, err
	}
	st, err := domain.UnmarshalState(blob)
	if err != nil {
		return domain.State{}, 0, err
	}
	return st, ver, nil
}

func (r *sqliteRepo) addressesInUse(ctx context.Context) (map[netip.Addr]bool, error) {
	if err := r.lock(ctx); err != nil {
		return nil, err
	}
	defer r.unlock()
	rows, err := r.db.QueryContext(ctx, `SELECT addr FROM addresses`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[netip.Addr]bool{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return nil, err
		}
		out[addr] = true
	}
	return out, rows.Err()
}

func (r *sqliteRepo) key(ctx context.Context, p domain.ProjectID, k domain.IdempotencyKey) (job, bool, error) {
	if err := r.lock(ctx); err != nil {
		return job{}, false, err
	}
	defer r.unlock()
	row := r.db.QueryRowContext(ctx, opSelect+` WHERE project = ? AND key = ?`, string(p), string(k))
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return job{}, false, nil
	}
	if err != nil {
		return job{}, false, err
	}
	return j, true, nil
}

func (r *sqliteRepo) operation(ctx context.Context, p domain.ProjectID, id string) (domain.Operation, error) {
	if err := r.lock(ctx); err != nil {
		return domain.Operation{}, err
	}
	defer r.unlock()
	row := r.db.QueryRowContext(ctx, opSelect+` WHERE project = ? AND id = ?`, string(p), id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Operation{}, &domain.Error{Kind: domain.NotFound, Msg: "operation not found"}
	}
	if err != nil {
		return domain.Operation{}, err
	}
	return j.op, nil
}

func (r *sqliteRepo) commit(ctx context.Context, c commit) error {
	if err := r.lock(ctx); err != nil {
		return err
	}
	defer r.unlock()
	if c.enqueue != nil && c.settle != nil {
		return errLostClaim
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if c.settle != nil {
		terminal, err := settleAllowed(ctx, tx, *c.settle)
		if err != nil {
			return err
		}
		if terminal {
			return nil
		}
	}
	if c.state != nil {
		if err := writeState(ctx, tx, c); err != nil {
			return err
		}
	}
	if c.enqueue != nil {
		if err := insertJob(ctx, tx, *c.enqueue); err != nil {
			return err
		}
	}
	if c.settle != nil {
		if err := settleJob(ctx, tx, *c.settle); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// settleAllowed reports whether the claim may write. A token mismatch is
// errLostClaim. A job that is already terminal is a no-op and must not write state.
func settleAllowed(ctx context.Context, tx *sql.Tx, j job) (bool, error) {
	var state int
	var token string
	err := tx.QueryRowContext(ctx, `SELECT state, token FROM operations WHERE id = ?`, j.op.ID).Scan(&state, &token)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && token != j.token) {
		return false, errLostClaim
	}
	if err != nil {
		return false, err
	}
	done := domain.OpState(state) == domain.OpSucceeded || domain.OpState(state) == domain.OpFailed
	return done, nil
}

func writeState(ctx context.Context, tx *sql.Tx, c commit) error {
	var ver int64
	err := tx.QueryRowContext(ctx, `SELECT version FROM projects WHERE id = ?`, string(c.project)).Scan(&ver)
	if errors.Is(err, sql.ErrNoRows) {
		ver = 0
	} else if err != nil {
		return err
	}
	if ver != c.base {
		return errStale
	}
	for addr := range addressesOf(*c.state) {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT project FROM addresses WHERE addr = ?`, addr.String()).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if domain.ProjectID(owner) != c.project {
			return errAddressTaken
		}
	}
	blob, err := domain.MarshalState(*c.state)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO projects (id, version, state) VALUES (?, ?, ?)
ON CONFLICT(id) DO UPDATE SET version = excluded.version, state = excluded.state`,
		string(c.project), ver+1, blob); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM addresses WHERE project = ?`, string(c.project)); err != nil {
		return err
	}
	for addr := range addressesOf(*c.state) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO addresses (addr, project) VALUES (?, ?)`, addr.String(), string(c.project)); err != nil {
			return err
		}
	}
	return nil
}

func insertJob(ctx context.Context, tx *sql.Tx, j job) error {
	var existing string
	err := tx.QueryRowContext(ctx, `SELECT id FROM operations WHERE project = ? AND key = ?`, string(j.op.Project), string(j.op.Key)).Scan(&existing)
	if err == nil {
		return errKeyTaken
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	kind, msg := 0, ""
	if j.op.Err != nil {
		kind = int(j.op.Err.Kind)
		msg = j.op.Err.Msg
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO operations (id, project, type, resource, lane, state, err_kind, err_msg, key, fingerprint, attempts, not_before, token)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.op.ID, string(j.op.Project), string(j.op.Type), j.op.Resource, string(j.lane), int(j.op.State),
		kind, msg, string(j.op.Key), j.fingerprint[:], j.attempts, nano(j.notBefore), j.token)
	return err
}

func settleJob(ctx context.Context, tx *sql.Tx, j job) error {
	var state int
	var token string
	err := tx.QueryRowContext(ctx, `SELECT state, token FROM operations WHERE id = ?`, j.op.ID).Scan(&state, &token)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && token != j.token) {
		return errLostClaim
	}
	if err != nil {
		return err
	}
	if domain.OpState(state) == domain.OpSucceeded || domain.OpState(state) == domain.OpFailed {
		return nil
	}
	kind, msg := 0, ""
	if j.op.Err != nil {
		kind = int(j.op.Err.Kind)
		msg = j.op.Err.Msg
	}
	tokenOut := j.token
	if j.op.State == domain.OpPending {
		tokenOut = ""
	}
	_, err = tx.ExecContext(ctx, `
UPDATE operations SET state = ?, err_kind = ?, err_msg = ?, attempts = ?, not_before = ?, token = ?
WHERE id = ?`, int(j.op.State), kind, msg, j.attempts, nano(j.notBefore), tokenOut, j.op.ID)
	return err
}

func (r *sqliteRepo) claim(ctx context.Context, now time.Time, token string) (job, bool, error) {
	if err := r.lock(ctx); err != nil {
		return job{}, false, err
	}
	defer r.unlock()
	jobs, err := r.nonTerminal(ctx)
	if err != nil {
		return job{}, false, err
	}
	j, ok := pickRunnable(jobs, now)
	if !ok {
		return job{}, false, nil
	}
	res, err := r.db.ExecContext(ctx, `UPDATE operations SET state = ?, token = ? WHERE seq = ? AND state = ?`,
		int(domain.OpRunning), token, j.seq, int(domain.OpPending))
	if err != nil {
		return job{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return job{}, false, err
	}
	if n != 1 {
		return job{}, false, nil
	}
	j.op.State = domain.OpRunning
	j.token = token
	return j, true, nil
}

func (r *sqliteRepo) nonTerminal(ctx context.Context) ([]job, error) {
	rows, err := r.db.QueryContext(ctx, opSelect+` WHERE state IN (?, ?)`, int(domain.OpPending), int(domain.OpRunning))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (r *sqliteRepo) requeue(ctx context.Context) error {
	if err := r.lock(ctx); err != nil {
		return err
	}
	defer r.unlock()
	_, err := r.db.ExecContext(ctx, `UPDATE operations SET state = ?, token = '' WHERE state = ?`,
		int(domain.OpPending), int(domain.OpRunning))
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanJob(sc scanner) (job, error) {
	var (
		j                        job
		project, typ, resource   string
		lane, key, errMsg, token string
		state, errKind, attempts int
		fp                       []byte
		notBefore                int64
	)
	err := sc.Scan(&j.seq, &j.op.ID, &project, &typ, &resource, &lane, &state, &errKind, &errMsg, &key, &fp, &attempts, &notBefore, &token)
	if err != nil {
		return job{}, err
	}
	j.op.Project = domain.ProjectID(project)
	j.op.Type = domain.OperationType(typ)
	j.op.Resource = resource
	j.op.State = domain.OpState(state)
	j.op.Key = domain.IdempotencyKey(key)
	j.lane = domain.VPCID(lane)
	j.attempts = attempts
	j.notBefore = fromNano(notBefore)
	j.token = token
	copy(j.fingerprint[:], fp)
	if errKind != 0 {
		j.op.Err = &domain.Error{Kind: domain.ErrKind(errKind), Msg: errMsg}
	}
	return j, nil
}
