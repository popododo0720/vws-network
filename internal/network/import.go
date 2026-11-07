package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/popododo0720/vws-network/internal/domain"
)

// ImportMode is what ImportSQLite does with what it found.
type ImportMode int

const (
	// ImportWrite writes what the store lacks, and nothing when anything differs.
	ImportWrite ImportMode = iota
	// ImportDryRun prints what ImportWrite would write and writes nothing.
	ImportDryRun
	// ImportVerify fails unless the store already holds every record of the file.
	ImportVerify
)

// ErrImportDiffers is returned when the store holds a record the file holds
// differently, or, for ImportVerify, lacks one.
var ErrImportDiffers = errors.New("network: the kubernetes store differs from the sqlite file")

// ImportSQLite copies the projects, operations, idempotency keys and pool
// addresses of the SQLite file from into the Kubernetes store, and prints one
// line per object to out. Run it while no service uses either store. A file of
// an older schema is migrated in place first, as a start on it would.
func ImportSQLite(ctx context.Context, from string, store StoreConfig, mode ImportMode, out io.Writer) error {
	if from == "" {
		return &domain.Error{Kind: domain.Invalid, Msg: "the sqlite file is required"}
	}
	if _, err := os.Stat(from); err != nil {
		return fmt.Errorf("sqlite file: %w", err)
	}
	if err := checkStore(Config{Store: store}); err != nil {
		return err
	}
	c, _, err := storeClient(ctx, store)
	if err != nil {
		return err
	}
	src, err := openSQLite(ctx, from)
	if err != nil {
		return err
	}
	var dst repository = &kubeRepo{c: c, ns: store.Namespace, now: time.Now}
	if mode == ImportWrite {
		if dst, err = newKubeRepo(ctx, c, store.Namespace); err != nil {
			return errors.Join(err, src.close())
		}
	}
	return errors.Join(importStore(ctx, src, dst, mode, out), src.close())
}

// importStep is one commit of the import: a project document or an operation.
type importStep struct {
	project domain.ProjectID
	state   *domain.State
	job     *job
}

func (s importStep) objects() []string {
	if s.job != nil {
		return []string{fmt.Sprintf("project %s operation %s key %q", s.project, s.job.op.ID, s.job.op.Key)}
	}
	var out []string
	for ref := range resourcesOf(*s.state) {
		out = append(out, fmt.Sprintf("project %s %s %s", s.project, ref.Kind, ref.ID))
	}
	for a := range addressesOf(*s.state) {
		out = append(out, fmt.Sprintf("project %s address %s", s.project, a))
	}
	if out == nil {
		out = []string{fmt.Sprintf("project %s", s.project)}
	}
	slices.Sort(out)
	return out
}

// importPlan is what the store lacks (steps) and what it holds differently
// (diffs). count is how many records of the file were compared.
type importPlan struct {
	steps []importStep
	diffs []string
	count struct{ projects, operations, addresses int }
}

func planImport(ctx context.Context, src *sqliteRepo, dst repository) (importPlan, error) {
	var plan importPlan
	ids, err := src.projectIDs(ctx)
	if err != nil {
		return plan, err
	}
	holder := map[netip.Addr]domain.ProjectID{}
	written := map[domain.ProjectID]bool{}
	for _, p := range ids {
		st, _, err := src.load(ctx, p)
		if err != nil {
			return plan, err
		}
		for a := range addressesOf(st) {
			holder[a] = p
		}
		cur, ver, err := dst.load(ctx, p)
		if err != nil {
			return plan, err
		}
		if ver == 0 {
			plan.steps = append(plan.steps, importStep{project: p, state: &st})
			written[p] = true
			continue
		}
		d, err := documentDiffs(p, st, cur)
		if err != nil {
			return plan, err
		}
		plan.diffs = append(plan.diffs, d...)
	}
	plan.count.projects = len(ids)
	if err := planAddresses(ctx, src, dst, holder, written, &plan); err != nil {
		return plan, err
	}
	jobs, err := src.jobs(ctx)
	if err != nil {
		return plan, err
	}
	for i := range jobs {
		j := jobs[i]
		got, ok, err := dst.key(ctx, j.op.Project, j.op.Key)
		if err != nil {
			return plan, err
		}
		if !ok {
			plan.steps = append(plan.steps, importStep{project: j.op.Project, job: &j})
			continue
		}
		if !sameJob(j, got) {
			plan.diffs = append(plan.diffs, fmt.Sprintf("project %s key %q: store has operation %+v, file has %+v",
				j.op.Project, j.op.Key, docOf(got), docOf(j)))
		}
	}
	plan.count.operations = len(jobs)
	return plan, nil
}

// planAddresses checks the address index of the file: each address is held by
// one project document, and the store holds it unless this import writes it.
func planAddresses(
	ctx context.Context, src *sqliteRepo, dst repository,
	holder map[netip.Addr]domain.ProjectID, written map[domain.ProjectID]bool, plan *importPlan,
) error {
	used, err := src.addressesInUse(ctx)
	if err != nil {
		return err
	}
	inStore, err := dst.addressesInUse(ctx)
	if err != nil {
		return err
	}
	for a := range used {
		p, ok := holder[a]
		switch {
		case !ok:
			plan.diffs = append(plan.diffs, fmt.Sprintf("address %s: no project of the file holds it", a))
		case !inStore[a] && !written[p]:
			plan.diffs = append(plan.diffs, fmt.Sprintf("project %s address %s: missing in the store", p, a))
		}
	}
	plan.count.addresses = len(used)
	return nil
}

// documentDiffs names every resource of p whose document differs.
func documentDiffs(p domain.ProjectID, want, got domain.State) ([]string, error) {
	w, err := documents(want)
	if err != nil {
		return nil, err
	}
	g, err := documents(got)
	if err != nil {
		return nil, err
	}
	var out []string
	for ref, doc := range w {
		switch cur, ok := g[ref]; {
		case !ok:
			out = append(out, fmt.Sprintf("project %s %s %s: missing in the store", p, ref.Kind, ref.ID))
		case cur != doc:
			out = append(out, fmt.Sprintf("project %s %s %s: store has %s, file has %s", p, ref.Kind, ref.ID, cur, doc))
		}
	}
	for ref := range g {
		if _, ok := w[ref]; !ok {
			out = append(out, fmt.Sprintf("project %s %s %s: only in the store", p, ref.Kind, ref.ID))
		}
	}
	slices.Sort(out)
	return out, nil
}

// sameJob compares every stored field but the sequence, which each store
// numbers itself, and the claim token, which no import keeps.
func sameJob(a, b job) bool {
	da, db := docOf(a), docOf(b)
	da.Seq, db.Seq, da.Token, db.Token = 0, 0, "", ""
	return da == db
}

func importStore(ctx context.Context, src *sqliteRepo, dst repository, mode ImportMode, out io.Writer) error {
	plan, err := planImport(ctx, src, dst)
	if err != nil {
		return err
	}
	var w strings.Builder
	for _, d := range plan.diffs {
		fmt.Fprintf(&w, "differs: %s\n", d)
	}
	verb := map[ImportMode]string{ImportWrite: "create", ImportDryRun: "would create", ImportVerify: "missing"}[mode]
	if mode == ImportWrite && len(plan.diffs) > 0 {
		verb = "not written"
	}
	writes := 0
	for _, s := range plan.steps {
		if mode == ImportWrite && len(plan.diffs) == 0 {
			c := commit{project: s.project, state: s.state, enqueue: s.job}
			if err := dst.commit(ctx, c); err != nil {
				return errors.Join(flush(out, &w), fmt.Errorf("import %s: %w", s.objects()[0], err))
			}
			writes++
		}
		for _, o := range s.objects() {
			fmt.Fprintf(&w, "%s: %s\n", verb, o)
		}
	}
	fmt.Fprintf(&w, "projects: %d, operations: %d, addresses: %d, differences: %d, missing: %d, writes: %d\n",
		plan.count.projects, plan.count.operations, plan.count.addresses, len(plan.diffs), len(plan.steps), writes)
	if err := flush(out, &w); err != nil {
		return err
	}
	if len(plan.diffs) > 0 || (mode == ImportVerify && len(plan.steps) > 0) {
		return ErrImportDiffers
	}
	return nil
}

func flush(out io.Writer, w *strings.Builder) error {
	if _, err := io.WriteString(out, w.String()); err != nil {
		return fmt.Errorf("write import report: %w", err)
	}
	w.Reset()
	return nil
}
