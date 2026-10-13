package network

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go sqlite driver

	"github.com/popododo0720/vws-lib/obs"

	"github.com/popododo0720/vws-network/internal/domain"
)

// schemaVersion 2 rewrites project documents so description and labels survive.
// Version 1 is the previous document. Opening it migrates in one transaction
// with the user_version bump.
const schemaVersion = 2

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
		return nil, fmt.Errorf("openSQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	r := &sqliteRepo{db: db, mu: make(chan struct{}, 1)}
	if err := r.lock(ctx); err != nil {
		return nil, failClose(err, db)
	}
	defer r.unlock()
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return nil, failClose(err, db)
	}
	if version == 0 {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return nil, failClose(err, db)
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
			return nil, failClose(rollbackOn(err, tx), db)
		}
		if _, err := tx.ExecContext(ctx, `PRAGMA user_version = `+strconv.Itoa(schemaVersion)); err != nil {
			return nil, failClose(rollbackOn(err, tx), db)
		}
		if err := tx.Commit(); err != nil {
			return nil, failClose(err, db)
		}
	} else if version < schemaVersion {
		if err := migrateSQLite(ctx, db, version); err != nil {
			return nil, failClose(err, db)
		}
	} else if version != schemaVersion {
		return nil, failClose(errors.New("network: unsupported sqlite schema version"), db)
	}
	return r, nil
}

// migrateSQLite rewrites stored documents and sets user_version in one transaction.
func migrateSQLite(ctx context.Context, db *sql.DB, version int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrateSQLite: %w", err)
	}
	defer rollback(ctx, tx)
	for version < schemaVersion {
		switch version {
		case 1:
			if err := rewriteProjectDocuments(ctx, tx); err != nil {
				return err
			}
		default:
			return errors.New("network: unsupported sqlite schema version")
		}
		version++
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version = `+strconv.Itoa(schemaVersion)); err != nil {
		return fmt.Errorf("migrateSQLite: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrateSQLite: %w", err)
	}
	return nil
}

func rewriteProjectDocuments(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, state FROM projects`)
	if err != nil {
		return fmt.Errorf("rewriteProjectDocuments: %w", err)
	}
	type row struct {
		id    string
		state []byte
	}
	var saved []row
	for rows.Next() {
		var item row
		if err := rows.Scan(&item.id, &item.state); err != nil {
			return failClose(err, rows)
		}
		saved = append(saved, item)
	}
	if err := rows.Err(); err != nil {
		return failClose(err, rows)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("rewriteProjectDocuments: %w", err)
	}
	for _, item := range saved {
		st, err := domain.UnmarshalState(item.state)
		if err != nil {
			return fmt.Errorf("network: project %s: %w", item.id, err)
		}
		blob, err := domain.MarshalState(st)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET state = ? WHERE id = ?`, blob, item.id); err != nil {
			return fmt.Errorf("rewriteProjectDocuments: %w", err)
		}
	}
	return nil
}

func (r *sqliteRepo) lock(ctx context.Context) error {
	select {
	case r.mu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("sqliteRepo.lock: %w", ctx.Err())
	}
}

func (r *sqliteRepo) unlock() { <-r.mu }

// failClose returns err joined with the error from closing c, so a failed
// close on an error path is reported instead of dropped.
func failClose(err error, c io.Closer) error {
	if cerr := c.Close(); cerr != nil {
		return errors.Join(err, cerr)
	}
	return err
}

// rollbackOn rolls tx back after err and joins a rollback failure to err.
func rollbackOn(err error, tx *sql.Tx) error {
	if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
		return errors.Join(err, rerr)
	}
	return err
}

// rollback is the deferred rollback of a transaction that may have committed.
// sql.ErrTxDone is the normal result after Commit. Any other failure is logged
// because the caller is already returning.
func rollback(ctx context.Context, tx *sql.Tx) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		slog.Default().LogAttrs(ctx, slog.LevelError, "sqlite rollback", slog.String(obs.FieldErr, err.Error()))
	}
}

// closeLogged closes a read-only cursor whose error cannot change the result.
func closeLogged(ctx context.Context, c io.Closer) {
	if err := c.Close(); err != nil {
		slog.Default().LogAttrs(ctx, slog.LevelError, "sqlite close", slog.String(obs.FieldErr, err.Error()))
	}
}

func (r *sqliteRepo) ping(ctx context.Context) error {
	var one int
	if err := r.db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("ping sqlite: %w", err)
	}
	return nil
}

func (r *sqliteRepo) close() error {
	if err := r.db.Close(); err != nil {
		return fmt.Errorf("close sqlite: %w", err)
	}
	return nil
}

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
		return domain.State{}, 0, fmt.Errorf("load: %w", err)
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
		return nil, fmt.Errorf("addressesInUse: %w", err)
	}
	defer closeLogged(ctx, rows)
	out := map[netip.Addr]bool{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("addressesInUse: %w", err)
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return nil, fmt.Errorf("addressesInUse: %w", err)
		}
		out[addr] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("addressesInUse: %w", err)
	}
	return out, nil
}

// pending takes no lock: it is one read for a metric and must not wait behind
// a worker's transaction.
func (r *sqliteRepo) pending(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE state IN (?, ?)`, int(domain.OpPending), int(domain.OpRunning)).Scan(&n); err != nil {
		return 0, fmt.Errorf("pending: %w", err)
	}
	return n, nil
}

func (r *sqliteRepo) projectIDs(ctx context.Context) ([]domain.ProjectID, error) {
	if err := r.lock(ctx); err != nil {
		return nil, err
	}
	defer r.unlock()
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM projects ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("projectIDs: %w", err)
	}
	defer closeLogged(ctx, rows)
	var out []domain.ProjectID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("projectIDs: %w", err)
		}
		out = append(out, domain.ProjectID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projectIDs: %w", err)
	}
	return out, nil
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
		return fmt.Errorf("commit: %w", err)
	}
	defer rollback(ctx, tx)
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
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
		return false, fmt.Errorf("settleAllowed: %w", err)
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
		return fmt.Errorf("writeState: %w", err)
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
			return fmt.Errorf("writeState: %w", err)
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
		return fmt.Errorf("writeState: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM addresses WHERE project = ?`, string(c.project)); err != nil {
		return fmt.Errorf("writeState: %w", err)
	}
	for addr := range addressesOf(*c.state) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO addresses (addr, project) VALUES (?, ?)`, addr.String(), string(c.project)); err != nil {
			return fmt.Errorf("writeState: %w", err)
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
		return fmt.Errorf("insertJob: %w", err)
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
	if err != nil {
		return fmt.Errorf("insertJob: %w", err)
	}
	return nil
}

func settleJob(ctx context.Context, tx *sql.Tx, j job) error {
	var state int
	var token string
	err := tx.QueryRowContext(ctx, `SELECT state, token FROM operations WHERE id = ?`, j.op.ID).Scan(&state, &token)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && token != j.token) {
		return errLostClaim
	}
	if err != nil {
		return fmt.Errorf("settleJob: %w", err)
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
	if err != nil {
		return fmt.Errorf("settleJob: %w", err)
	}
	return nil
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
		return job{}, false, fmt.Errorf("claim: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return job{}, false, fmt.Errorf("claim: %w", err)
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
		return nil, fmt.Errorf("nonTerminal: %w", err)
	}
	defer closeLogged(ctx, rows)
	var jobs []job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("nonTerminal: %w", err)
	}
	return jobs, nil
}

func (r *sqliteRepo) requeue(ctx context.Context) error {
	if err := r.lock(ctx); err != nil {
		return err
	}
	defer r.unlock()
	_, err := r.db.ExecContext(ctx, `UPDATE operations SET state = ?, token = '' WHERE state = ?`,
		int(domain.OpPending), int(domain.OpRunning))
	if err != nil {
		return fmt.Errorf("requeue: %w", err)
	}
	return nil
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
