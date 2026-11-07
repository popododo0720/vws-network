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
	dst := &kubeRepo{c: c, ns: store.Namespace, now: time.Now}
	if mode == ImportWrite {
		if dst, err = newKubeRepo(ctx, c, store.Namespace); err != nil {
			return errors.Join(err, src.close())
		}
	}
	return errors.Join(importStore(ctx, src, dst, mode, out), src.close())
}

// importStep is one commit of the import: one resource document of a
// project, an empty project, or one operation.
type importStep struct {
	project domain.ProjectID
	ref     *resRef
	doc     string
	job     *job
}

func (s importStep) objects() ([]string, error) {
	switch {
	case s.job != nil:
		return []string{fmt.Sprintf("project %s operation %s key %q", s.project, s.job.op.ID, s.job.op.Key)}, nil
	case s.ref == nil:
		return []string{fmt.Sprintf("project %s", s.project)}, nil
	}
	out := []string{fmt.Sprintf("project %s %s %s", s.project, s.ref.Kind, s.ref.ID)}
	one, err := domain.UnmarshalState([]byte(s.doc))
	if err != nil {
		return nil, fmt.Errorf("project %s %s %s: %w", s.project, s.ref.Kind, s.ref.ID, err)
	}
	for a := range addressesOf(one) {
		out = append(out, fmt.Sprintf("project %s address %s", s.project, a))
	}
	return out, nil
}

// storeView is what the Kubernetes store holds, read with lists only, so
// reading it never finishes a change or reclaims a claim.
type storeView struct {
	heads     map[domain.ProjectID]head
	states    map[domain.ProjectID]domain.State
	keys      map[opKey]string
	jobs      map[string]job
	addresses map[netip.Addr]bool
}

type opKey struct {
	project domain.ProjectID
	key     domain.IdempotencyKey
}

// readStore reads r. A head whose change is not applied yet is named in
// unsettled and its project is not read: only a write may finish it.
func readStore(ctx context.Context, r *kubeRepo) (storeView, []string, error) {
	v := storeView{
		heads: map[domain.ProjectID]head{}, states: map[domain.ProjectID]domain.State{},
		keys: map[opKey]string{}, jobs: map[string]job{}, addresses: map[netip.Addr]bool{},
	}
	var unsettled []string
	heads, err := r.list(ctx, storeHead, nil)
	if err != nil {
		return v, nil, err
	}
	for i := range heads {
		h, err := decodeHead(&heads[i])
		if err != nil {
			return v, nil, err
		}
		p := domain.ProjectID(heads[i].Data["project"])
		v.heads[p] = h
		if h.change != nil && !h.applied {
			unsettled = append(unsettled, fmt.Sprintf(
				"project %s: the store has an unfinished change; a run without -dry-run or -verify finishes it", p))
			continue
		}
		st, _, ok, err := r.snapshot(ctx, p, h.seq)
		if err != nil {
			return v, nil, err
		}
		if !ok {
			return v, nil, &domain.Error{Kind: domain.Busy, Msg: fmt.Sprintf("project %s changed while it was read", p)}
		}
		v.states[p] = st
	}
	keys, err := r.list(ctx, storeKey, nil)
	if err != nil {
		return v, nil, err
	}
	for _, cm := range keys {
		v.keys[opKey{domain.ProjectID(cm.Data["project"]), domain.IdempotencyKey(cm.Data["key"])}] = cm.Data["op"]
	}
	ops, err := r.list(ctx, storeOp, nil)
	if err != nil {
		return v, nil, err
	}
	for i := range ops {
		j, err := decodeJob(&ops[i])
		if err != nil {
			return v, nil, err
		}
		v.jobs[j.op.ID] = j
	}
	addrs, err := r.list(ctx, storeAddress, nil)
	if err != nil {
		return v, nil, err
	}
	for _, cm := range addrs {
		a, err := netip.ParseAddr(cm.Data["value"])
		if err != nil {
			return v, nil, fmt.Errorf("network: address claim %s is corrupt", cm.Name)
		}
		v.addresses[a] = true
	}
	return v, unsettled, nil
}

// importPlan is what the store lacks (steps) and what it holds differently
// or in addition (diffs). count is how many records of the file were compared.
type importPlan struct {
	steps []importStep
	diffs []string
	count struct{ projects, operations, addresses int }
}

func planImport(ctx context.Context, src *sqliteRepo, view storeView) (importPlan, error) {
	var plan importPlan
	ids, err := src.projectIDs(ctx)
	if err != nil {
		return plan, err
	}
	inFile := map[domain.ProjectID]bool{}
	fileAddrs := map[netip.Addr]domain.ProjectID{}
	for _, p := range ids {
		inFile[p] = true
		st, _, err := src.load(ctx, p)
		if err != nil {
			return plan, err
		}
		for a := range addressesOf(st) {
			fileAddrs[a] = p
		}
		if _, ok := view.heads[p]; !ok && len(resourcesOf(st)) == 0 {
			plan.steps = append(plan.steps, importStep{project: p})
			continue
		}
		steps, diffs, err := resourcePlan(p, st, view.states[p])
		if err != nil {
			return plan, err
		}
		plan.steps = append(plan.steps, steps...)
		plan.diffs = append(plan.diffs, diffs...)
	}
	for p := range view.heads {
		if !inFile[p] {
			plan.diffs = append(plan.diffs, fmt.Sprintf("project %s: only in the store", p))
		}
	}
	plan.count.projects = len(ids)
	if err := planAddresses(ctx, src, view, fileAddrs, &plan); err != nil {
		return plan, err
	}
	if err := planJobs(ctx, src, view, &plan); err != nil {
		return plan, err
	}
	slices.Sort(plan.diffs)
	return plan, nil
}

// resourcePlan is one step per resource of want the store lacks, so each
// commit holds one document, and a diff per resource held differently.
func resourcePlan(p domain.ProjectID, want, got domain.State) ([]importStep, []string, error) {
	w, err := documents(want)
	if err != nil {
		return nil, nil, err
	}
	g, err := documents(got)
	if err != nil {
		return nil, nil, err
	}
	var steps []importStep
	var diffs []string
	for ref, doc := range w {
		switch cur, ok := g[ref]; {
		case !ok:
			steps = append(steps, importStep{project: p, ref: &ref, doc: doc})
		case cur != doc:
			diffs = append(diffs, fmt.Sprintf("project %s %s %s: store has %s, file has %s", p, ref.Kind, ref.ID, cur, doc))
		}
	}
	for ref := range g {
		if _, ok := w[ref]; !ok {
			diffs = append(diffs, fmt.Sprintf("project %s %s %s: only in the store", p, ref.Kind, ref.ID))
		}
	}
	slices.SortFunc(steps, func(a, b importStep) int {
		return strings.Compare(a.ref.Kind+"/"+a.ref.ID, b.ref.Kind+"/"+b.ref.ID)
	})
	return steps, diffs, nil
}

// planAddresses checks the address index of the file both ways: each address
// is held by one project document of the file, the store holds it unless this
// import writes it, and the store holds no other.
func planAddresses(
	ctx context.Context, src *sqliteRepo, view storeView, holder map[netip.Addr]domain.ProjectID, plan *importPlan,
) error {
	used, err := src.addressesInUse(ctx)
	if err != nil {
		return err
	}
	for a := range used {
		p, ok := holder[a]
		switch {
		case !ok:
			plan.diffs = append(plan.diffs, fmt.Sprintf("address %s: no project of the file holds it", a))
		case !view.addresses[a] && !importsAddress(plan.steps, a):
			plan.diffs = append(plan.diffs, fmt.Sprintf("project %s address %s: missing in the store", p, a))
		}
	}
	for a := range view.addresses {
		if !used[a] {
			plan.diffs = append(plan.diffs, fmt.Sprintf("address %s: only in the store", a))
		}
	}
	plan.count.addresses = len(used)
	return nil
}

func importsAddress(steps []importStep, a netip.Addr) bool {
	for _, s := range steps {
		if s.ref == nil || s.ref.Kind != kindEIP {
			continue
		}
		if one, err := domain.UnmarshalState([]byte(s.doc)); err == nil && addressesOf(one)[a] {
			return true
		}
	}
	return false
}

// planJobs compares the operations and idempotency keys both ways.
func planJobs(ctx context.Context, src *sqliteRepo, view storeView, plan *importPlan) error {
	jobs, err := src.jobs(ctx)
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	keys := map[opKey]bool{}
	for i := range jobs {
		j := jobs[i]
		ids[j.op.ID] = true
		k := opKey{j.op.Project, j.op.Key}
		keys[k] = true
		id, ok := view.keys[k]
		if !ok {
			if _, taken := view.jobs[j.op.ID]; taken {
				plan.diffs = append(plan.diffs, fmt.Sprintf("operation %s: in the store without key %q", j.op.ID, j.op.Key))
				continue
			}
			plan.steps = append(plan.steps, importStep{project: j.op.Project, job: &j})
			continue
		}
		got, ok := view.jobs[id]
		if !ok || !sameJob(j, got) {
			plan.diffs = append(plan.diffs, fmt.Sprintf("project %s key %q: store has operation %+v, file has %+v",
				j.op.Project, j.op.Key, docOf(got), docOf(j)))
		}
	}
	for id, j := range view.jobs {
		if !ids[id] {
			plan.diffs = append(plan.diffs, fmt.Sprintf("project %s operation %s: only in the store", j.op.Project, id))
		}
	}
	for k := range view.keys {
		if !keys[k] {
			plan.diffs = append(plan.diffs, fmt.Sprintf("project %s key %q: only in the store", k.project, k.key))
		}
	}
	plan.count.operations = len(jobs)
	return nil
}

// sameJob compares every stored field but the sequence, which each store
// numbers itself, and the claim token, which no import keeps.
func sameJob(a, b job) bool {
	da, db := docOf(a), docOf(b)
	da.Seq, db.Seq, da.Token, db.Token = 0, 0, "", ""
	return da == db
}

func importStore(ctx context.Context, src *sqliteRepo, dst *kubeRepo, mode ImportMode, out io.Writer) error {
	view, unsettled, err := readStore(ctx, dst)
	if err != nil {
		return err
	}
	plan, err := planImport(ctx, src, view)
	if err != nil {
		return err
	}
	plan.diffs = append(unsettled, plan.diffs...)
	var w strings.Builder
	for _, d := range plan.diffs {
		fmt.Fprintf(&w, "differs: %s\n", d)
	}
	verb := map[ImportMode]string{ImportWrite: "create", ImportDryRun: "would create", ImportVerify: "missing"}[mode]
	write := mode == ImportWrite && len(plan.diffs) == 0
	if mode == ImportWrite && !write {
		verb = "not written"
	}
	writes := 0
	for _, s := range plan.steps {
		objs, err := s.objects()
		if err != nil {
			return errors.Join(flush(out, &w), err)
		}
		if write {
			if err := commitStep(ctx, dst, &view, s); err != nil {
				return errors.Join(flush(out, &w), fmt.Errorf("import %s: %w", objs[0], err))
			}
			writes++
		}
		for _, o := range objs {
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

// commitStep writes s on top of the state view holds for its project, so a
// project document grows one resource per commit.
func commitStep(ctx context.Context, dst *kubeRepo, view *storeView, s importStep) error {
	if s.job != nil {
		return dst.commit(ctx, commit{project: s.project, enqueue: s.job})
	}
	next := domain.State{
		VPCs: map[domain.VPCID]domain.VPC{}, Subnets: map[domain.SubnetID]domain.Subnet{},
		Groups: map[domain.SecurityGroupID]domain.SecurityGroup{}, ElasticIPs: map[domain.ElasticIPID]domain.ElasticIP{},
	}
	if cur, ok := view.states[s.project]; ok {
		merge(&next, cur)
	}
	if s.ref != nil {
		one, err := domain.UnmarshalState([]byte(s.doc))
		if err != nil {
			return fmt.Errorf("decode %s %s: %w", s.ref.Kind, s.ref.ID, err)
		}
		merge(&next, one)
	}
	h := view.heads[s.project]
	if err := dst.commit(ctx, commit{project: s.project, base: h.version, state: &next}); err != nil {
		return err
	}
	h.version++
	view.heads[s.project], view.states[s.project] = h, next
	return nil
}

func flush(out io.Writer, w *strings.Builder) error {
	if _, err := io.WriteString(out, w.String()); err != nil {
		return fmt.Errorf("write import report: %w", err)
	}
	w.Reset()
	return nil
}
