package network

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// kubeRepo is the repository on plain ConfigMaps in one namespace. There is
// no CRD and no per-project blob.
//
//   - A project head holds the state version, a change counter and the last
//     change set. Every commit is one compare-and-swap of the head on its
//     resourceVersion, so commits to one project are serialized.
//   - Each VPC, subnet, security group and elastic IP is one ConfigMap holding
//     its document, the change that wrote it, and the document before that.
//   - Each operation is one ConfigMap. claim and requeue change it with CAS.
//   - An idempotency key, a pool address and a resource id are claim
//     ConfigMaps named after their subject, so object-name uniqueness keeps
//     each one single. A resource id claim keeps two projects from naming
//     the same cluster-scoped Kube-OVN object.
//
// A commit writes its change set into the head and then applies it to the
// other objects. A reader that finds an unapplied change set applies it
// first, so a crash between the two writes is finished by the next reader,
// or by repair at start. Applying is idempotent: each object records the
// change that last wrote it and is never moved back.
type kubeRepo struct {
	c  client.Client
	ns string
	// claimMu keeps two workers of this process from claiming two jobs of one
	// lane at once. CAS keeps the data safe; one leader runs the workers.
	claimMu sync.Mutex
	now     func() time.Time
}

const (
	labelStore   = annotationPrefix + "store"
	labelProject = annotationPrefix + "project"
	labelOpState = annotationPrefix + "op-state"
	labelKind    = annotationPrefix + "kind"
	labelID      = annotationPrefix + "id"

	storeHead     = "project"
	storeResource = "resource"
	storeOp       = "operation"
	storeKey      = "key"
	storeAddress  = "address"
	storeID       = "id"

	// kubeObjectLimit is the size limit of one written object (vws-deploy#478).
	kubeObjectLimit = 256 << 10
	// kubeGrace is how long a tombstone or a stray address claim is kept, so
	// a slow writer of an older change still finds it.
	kubeGrace = 10 * time.Minute
	// kubeCallTimeout bounds one repository call.
	kubeCallTimeout = 30 * time.Second
	kubeAttempts    = 8
)

var errObjectTooLarge = errors.New("network: store object exceeds the size limit")

func newKubeRepo(ctx context.Context, c client.Client, ns string) (*kubeRepo, error) {
	r := &kubeRepo{c: c, ns: ns, now: time.Now}
	if err := r.repair(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func transient(err error) bool {
	return apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err)
}

// retried runs one API call, retrying transient errors with the client-go
// default backoff. Conflicts and AlreadyExists are answers and return at once.
func retried(call func() error) error {
	return retry.OnError(retry.DefaultBackoff, transient, call) //nolint:wrapcheck // callers wrap it with the object
}

func hashName(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])[:40]
}

func storeName(store string, parts ...string) string {
	return "vws-net-" + store + "-" + hashName(parts...)
}

func headName(p domain.ProjectID) string { return storeName(storeHead, string(p)) }
func opName(id string) string            { return storeName(storeOp, id) }
func addrName(a netip.Addr) string       { return addrRef(a).name() }
func keyName(p domain.ProjectID, k domain.IdempotencyKey) string {
	return storeName(storeKey, string(p), string(k))
}

func resName(p domain.ProjectID, r resRef) string {
	return storeName(storeResource, string(p), r.Kind, r.ID)
}

// resPut is one resource document a change writes; an empty State deletes.
type resPut struct {
	resRef
	State string `json:"state,omitempty"`
}

// claimRef is a name one project holds alone in the namespace: a pool
// address, or a resource "<kind>/<id>". Its claim ConfigMap is named after it,
// so object-name uniqueness keeps it single.
type claimRef struct {
	Store string `json:"store"`
	Value string `json:"value"`
}

func (c claimRef) name() string { return storeName(c.Store, c.Value) }

func addrRef(a netip.Addr) claimRef { return claimRef{Store: storeAddress, Value: a.String()} }
func idRef(r resRef) claimRef       { return claimRef{Store: storeID, Value: r.Kind + "/" + r.ID} }

func (c claimRef) taken() error {
	if c.Store == storeAddress {
		return errAddressTaken
	}
	return errIDTaken
}

// claimsOf is every claim st holds.
func claimsOf(st domain.State) map[claimRef]bool {
	out := map[claimRef]bool{}
	for a := range addressesOf(st) {
		out[addrRef(a)] = true
	}
	for ref := range resourcesOf(st) {
		out[idRef(ref)] = true
	}
	return out
}

// claimRelease frees a claim the project no longer holds, if it is still the
// one taken by change Txn.
type claimRelease struct {
	claimRef
	Txn string `json:"txn"`
}

// changeSet is everything one commit writes besides the head.
type changeSet struct {
	ID       string         `json:"id"`
	Seq      int64          `json:"seq"`
	Puts     []resPut       `json:"puts,omitempty"`
	Op       *jobDoc        `json:"op,omitempty"`
	OpCreate bool           `json:"op_create,omitempty"`
	Release  []claimRelease `json:"release,omitempty"`
	// Take are the claims this change took. Applying it marks each claim
	// confirmed, so an unconfirmed old claim is known to be stray.
	Take []claimRef `json:"take,omitempty"`
}

type jobDoc struct {
	ID          string `json:"id"`
	Project     string `json:"project"`
	Type        string `json:"type"`
	Resource    string `json:"resource"`
	Lane        string `json:"lane"`
	State       int    `json:"state"`
	ErrKind     int    `json:"err_kind,omitempty"`
	ErrMsg      string `json:"err_msg,omitempty"`
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
	Attempts    int    `json:"attempts"`
	NotBefore   int64  `json:"not_before"`
	Token       string `json:"token,omitempty"`
	Seq         int64  `json:"seq"`
	// Rev is the change that last wrote the job; claim and requeue keep it.
	Rev int64 `json:"rev"`
}

func docOf(j job) jobDoc {
	d := jobDoc{
		ID: j.op.ID, Project: string(j.op.Project), Type: string(j.op.Type), Resource: j.op.Resource,
		Lane: string(j.lane), State: int(j.op.State), Key: string(j.op.Key),
		Fingerprint: hex.EncodeToString(j.fingerprint[:]), Attempts: j.attempts,
		NotBefore: nano(j.notBefore), Token: j.token, Seq: j.seq,
	}
	if j.op.Err != nil {
		d.ErrKind, d.ErrMsg = int(j.op.Err.Kind), j.op.Err.Msg
	}
	return d
}

func (d jobDoc) job() (job, error) {
	j := job{
		op: domain.Operation{
			ID: d.ID, Project: domain.ProjectID(d.Project), Type: domain.OperationType(d.Type),
			Resource: d.Resource, State: domain.OpState(d.State), Key: domain.IdempotencyKey(d.Key),
		},
		lane: domain.VPCID(d.Lane), seq: d.Seq, attempts: d.Attempts, notBefore: fromNano(d.NotBefore), token: d.Token,
	}
	fp, err := hex.DecodeString(d.Fingerprint)
	if err != nil || len(fp) != len(j.fingerprint) {
		return job{}, fmt.Errorf("network: operation %s fingerprint is corrupt", d.ID)
	}
	copy(j.fingerprint[:], fp)
	if d.ErrKind != 0 {
		j.op.Err = &domain.Error{Kind: domain.ErrKind(d.ErrKind), Msg: d.ErrMsg}
	}
	return j, nil
}

func terminal(s domain.OpState) bool { return s == domain.OpSucceeded || s == domain.OpFailed }

func opStateLabel(s domain.OpState) string {
	switch s {
	case domain.OpPending:
		return "pending"
	case domain.OpRunning:
		return "running"
	case domain.OpSucceeded:
		return "succeeded"
	case domain.OpFailed:
		return "failed"
	}
	return "unknown"
}

func (r *kubeRepo) configMap(name, store string, lbl labels.Set, data map[string]string) *corev1.ConfigMap {
	l := map[string]string{labelStore: store}
	for k, v := range lbl {
		l[k] = v
	}
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.ns, Labels: l}, Data: data}
}

func fitsLimit(cm *corev1.ConfigMap) error {
	b, err := json.Marshal(cm)
	if err != nil {
		return fmt.Errorf("encode configmap %s: %w", cm.Name, err)
	}
	if len(b) > kubeObjectLimit {
		return fmt.Errorf("%w: %s is %d bytes", errObjectTooLarge, cm.Name, len(b))
	}
	return nil
}

func (r *kubeRepo) create(ctx context.Context, cm *corev1.ConfigMap) error {
	if err := fitsLimit(cm); err != nil {
		return err
	}
	if err := retried(func() error { return r.c.Create(ctx, cm) }); err != nil {
		return fmt.Errorf("create configmap %s: %w", cm.Name, err)
	}
	return nil
}

func (r *kubeRepo) update(ctx context.Context, cm *corev1.ConfigMap) error {
	if err := fitsLimit(cm); err != nil {
		return err
	}
	if err := retried(func() error { return r.c.Update(ctx, cm) }); err != nil {
		return fmt.Errorf("update configmap %s: %w", cm.Name, err)
	}
	return nil
}

// get returns nil and no error when the object does not exist.
func (r *kubeRepo) get(ctx context.Context, name string) (*corev1.ConfigMap, error) {
	var cm corev1.ConfigMap
	err := retried(func() error { return r.c.Get(ctx, client.ObjectKey{Namespace: r.ns, Name: name}, &cm) })
	if apierrors.IsNotFound(err) {
		return nil, nil //nolint:nilnil // a missing object is an answer, not an error
	}
	if err != nil {
		return nil, fmt.Errorf("get configmap %s: %w", name, err)
	}
	return &cm, nil
}

// deleteAt deletes cm only if it has not changed since it was read.
func (r *kubeRepo) deleteAt(ctx context.Context, cm *corev1.ConfigMap) error {
	rv := cm.ResourceVersion
	err := retried(func() error { return r.c.Delete(ctx, cm, client.Preconditions{ResourceVersion: &rv}) })
	if err == nil || apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil
	}
	return fmt.Errorf("delete configmap %s: %w", cm.Name, err)
}

func (r *kubeRepo) list(
	ctx context.Context, store string, match labels.Set, extra ...labels.Requirement,
) ([]corev1.ConfigMap, error) {
	set := labels.Set{labelStore: store}
	for k, v := range match {
		set[k] = v
	}
	sel := labels.SelectorFromSet(set).Add(extra...)
	var out corev1.ConfigMapList
	err := retried(func() error {
		return r.c.List(ctx, &out, client.InNamespace(r.ns), client.MatchingLabelsSelector{Selector: sel})
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", store, err)
	}
	return out.Items, nil
}

func projectSet(p domain.ProjectID) labels.Set { return labels.Set{labelProject: hashName(string(p))} }

// resourceSet labels a resource document with its kind and id, so the
// document of an id is found with a label selector.
func resourceSet(p domain.ProjectID, ref resRef) labels.Set {
	return labels.Set{labelProject: hashName(string(p)), labelKind: ref.Kind, labelID: ref.ID}
}

func callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, kubeCallTimeout)
}

// head is the decoded project head.
type head struct {
	cm      *corev1.ConfigMap
	version int64
	seq     int64
	change  *changeSet
	applied bool
}

func decodeHead(cm *corev1.ConfigMap) (head, error) {
	h := head{cm: cm}
	if cm == nil {
		return h, nil
	}
	var err error
	if h.version, err = strconv.ParseInt(cm.Data["version"], 10, 64); err != nil {
		return head{}, fmt.Errorf("network: project head %s version is corrupt", cm.Name)
	}
	if h.seq, err = strconv.ParseInt(cm.Data["seq"], 10, 64); err != nil {
		return head{}, fmt.Errorf("network: project head %s seq is corrupt", cm.Name)
	}
	if raw := cm.Data["change"]; raw != "" {
		var cs changeSet
		if err := json.Unmarshal([]byte(raw), &cs); err != nil {
			return head{}, fmt.Errorf("network: project head %s change is corrupt", cm.Name)
		}
		h.change = &cs
	}
	h.applied = cm.Data["applied"] == "true"
	return h, nil
}

// settledHead reads the head of p and applies its change set if a writer
// stopped before finishing it.
func (r *kubeRepo) settledHead(ctx context.Context, p domain.ProjectID) (head, error) {
	for range kubeAttempts {
		cm, err := r.get(ctx, headName(p))
		if err != nil {
			return head{}, err
		}
		h, err := decodeHead(cm)
		if err != nil || h.change == nil || h.applied {
			return h, err
		}
		if err := r.apply(ctx, p, *h.change); err != nil {
			return head{}, err
		}
		cm.Data["applied"] = "true"
		err = r.update(ctx, cm)
		if err == nil {
			h.applied = true
			return h, nil
		}
		if !apierrors.IsConflict(err) {
			return head{}, err
		}
	}
	return head{}, errStale
}

// apply writes cs to the objects it names. It is idempotent and never moves
// an object back past a later change.
func (r *kubeRepo) apply(ctx context.Context, p domain.ProjectID, cs changeSet) error {
	for _, put := range cs.Puts {
		if err := r.applyResource(ctx, p, cs.Seq, put); err != nil {
			return err
		}
	}
	if cs.Op != nil {
		if err := r.applyOp(ctx, cs); err != nil {
			return err
		}
	}
	for _, ref := range cs.Take {
		if err := r.confirm(ctx, p, cs, ref); err != nil {
			return err
		}
	}
	for _, rel := range cs.Release {
		cm, err := r.get(ctx, rel.name())
		if err != nil {
			return err
		}
		if cm != nil && cm.Data["project"] == string(p) && cm.Data["txn"] == rel.Txn {
			if err := r.deleteAt(ctx, cm); err != nil {
				return err
			}
		}
	}
	return nil
}

// resourceMap is the largest object applying put over a resource that holds
// prev can write, so a commit can check its size before swapping the head.
func (r *kubeRepo) resourceMap(p domain.ProjectID, put resPut, prev string) *corev1.ConfigMap {
	most := strconv.FormatInt(math.MaxInt64, 10)
	return r.configMap(resName(p, put.resRef), storeResource, resourceSet(p, put.resRef), map[string]string{
		"project": string(p), "kind": put.Kind, "id": put.ID, "seq": most, "state": put.State,
		"prev_seq": most, "prev": prev, "deleted": most,
	})
}

// confirm marks the claim cs took on ref as committed.
func (r *kubeRepo) confirm(ctx context.Context, p domain.ProjectID, cs changeSet, ref claimRef) error {
	for range kubeAttempts {
		cm, err := r.get(ctx, ref.name())
		if err != nil || cm == nil || cm.Data["confirmed"] != "" {
			return err
		}
		if cm.Data["project"] != string(p) || cm.Data["txn"] != cs.ID {
			return nil
		}
		cm.Data["confirmed"] = strconv.FormatInt(cs.Seq, 10)
		if err := r.update(ctx, cm); !apierrors.IsConflict(err) {
			return err
		}
	}
	return errStale
}

func (r *kubeRepo) applyResource(ctx context.Context, p domain.ProjectID, seq int64, put resPut) error {
	name := resName(p, put.resRef)
	for range kubeAttempts {
		cm, err := r.get(ctx, name)
		if err != nil {
			return err
		}
		if cm == nil {
			if put.State == "" {
				return nil
			}
			cm = r.configMap(name, storeResource, resourceSet(p, put.resRef), map[string]string{
				"project": string(p), "kind": put.Kind, "id": put.ID,
				"seq": strconv.FormatInt(seq, 10), "state": put.State, "prev_seq": "0",
			})
			err = r.create(ctx, cm)
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			return err
		}
		cur, err := strconv.ParseInt(cm.Data["seq"], 10, 64)
		if err != nil {
			return fmt.Errorf("network: resource %s seq is corrupt", name)
		}
		if cur >= seq {
			return nil
		}
		cm.Data["prev_seq"], cm.Data["prev"] = cm.Data["seq"], cm.Data["state"]
		cm.Data["seq"], cm.Data["state"] = strconv.FormatInt(seq, 10), put.State
		delete(cm.Data, "deleted")
		if put.State == "" {
			cm.Data["deleted"] = strconv.FormatInt(r.now().UnixNano(), 10)
		}
		err = r.update(ctx, cm)
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return errStale
}

func (r *kubeRepo) applyOp(ctx context.Context, cs changeSet) error {
	want := *cs.Op
	want.Rev = cs.Seq
	raw, err := json.Marshal(want)
	if err != nil {
		return fmt.Errorf("encode operation %s: %w", want.ID, err)
	}
	if err := r.writeOp(ctx, cs, raw); err != nil {
		return err
	}
	if !cs.OpCreate {
		return nil
	}
	k := r.configMap(keyName(domain.ProjectID(want.Project), domain.IdempotencyKey(want.Key)), storeKey,
		labels.Set{labelProject: hashName(want.Project)},
		map[string]string{"project": want.Project, "key": want.Key, "op": want.ID})
	if err := r.create(ctx, k); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// writeOp creates or rewrites the operation of cs unless a later change
// already wrote it.
func (r *kubeRepo) writeOp(ctx context.Context, cs changeSet, raw []byte) error {
	name := opName(cs.Op.ID)
	lbl := labels.Set{labelProject: hashName(cs.Op.Project), labelOpState: opStateLabel(domain.OpState(cs.Op.State))}
	for range kubeAttempts {
		cm, err := r.get(ctx, name)
		if err != nil {
			return err
		}
		if cm == nil {
			if !cs.OpCreate {
				return nil
			}
			err = r.create(ctx, r.configMap(name, storeOp, lbl, map[string]string{"job": string(raw)}))
		} else {
			var cur jobDoc
			if err := json.Unmarshal([]byte(cm.Data["job"]), &cur); err != nil {
				return fmt.Errorf("network: operation %s is corrupt", cs.Op.ID)
			}
			if cur.Rev >= cs.Seq {
				return nil
			}
			cm.Data["job"] = string(raw)
			cm.Labels[labelOpState] = lbl[labelOpState]
			err = r.update(ctx, cm)
		}
		if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return errStale
}

// snapshot reads the resources of p as of change seq. A resource written by a
// later change answers with the document it held before.
func (r *kubeRepo) snapshot(
	ctx context.Context, p domain.ProjectID, seq int64,
) (domain.State, []corev1.ConfigMap, bool, error) {
	items, err := r.list(ctx, storeResource, projectSet(p))
	if err != nil {
		return domain.State{}, nil, false, err
	}
	st := domain.State{
		VPCs: map[domain.VPCID]domain.VPC{}, Subnets: map[domain.SubnetID]domain.Subnet{},
		Groups: map[domain.SecurityGroupID]domain.SecurityGroup{}, ElasticIPs: map[domain.ElasticIPID]domain.ElasticIP{},
	}
	for i := range items {
		cm := &items[i]
		if cm.Data["project"] != string(p) {
			continue
		}
		cur, err1 := strconv.ParseInt(cm.Data["seq"], 10, 64)
		prev, err2 := strconv.ParseInt(cm.Data["prev_seq"], 10, 64)
		if err1 != nil || err2 != nil {
			return domain.State{}, nil, false, fmt.Errorf("network: resource %s seq is corrupt", cm.Name)
		}
		doc := cm.Data["state"]
		if cur > seq {
			if prev > seq {
				return domain.State{}, nil, false, nil
			}
			doc = cm.Data["prev"]
		}
		if doc == "" {
			continue
		}
		one, err := domain.UnmarshalState([]byte(doc))
		if err != nil {
			return domain.State{}, nil, false, fmt.Errorf("network: resource %s: %w", cm.Name, err)
		}
		merge(&st, one)
	}
	return st, items, true, nil
}

func merge(dst *domain.State, src domain.State) {
	for k, v := range src.VPCs {
		dst.VPCs[k] = v
	}
	for k, v := range src.Subnets {
		dst.Subnets[k] = v
	}
	for k, v := range src.Groups {
		dst.Groups[k] = v
	}
	for k, v := range src.ElasticIPs {
		dst.ElasticIPs[k] = v
	}
}

// documents splits st into one encoded document per resource.
func documents(st domain.State) (map[resRef]string, error) {
	out := map[resRef]string{}
	add := func(ref resRef, one domain.State) error {
		b, err := domain.MarshalState(one)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", ref.Kind, ref.ID, err)
		}
		out[ref] = string(b)
		return nil
	}
	for id, v := range st.VPCs {
		if err := add(resRef{kindVPC, string(id)}, domain.State{VPCs: map[domain.VPCID]domain.VPC{id: v}}); err != nil {
			return nil, err
		}
	}
	for id, v := range st.Subnets {
		one := domain.State{Subnets: map[domain.SubnetID]domain.Subnet{id: v}}
		if err := add(resRef{kindSubnet, string(id)}, one); err != nil {
			return nil, err
		}
	}
	for id, v := range st.Groups {
		one := domain.State{Groups: map[domain.SecurityGroupID]domain.SecurityGroup{id: v}}
		if err := add(resRef{kindGroup, string(id)}, one); err != nil {
			return nil, err
		}
	}
	for id, v := range st.ElasticIPs {
		one := domain.State{ElasticIPs: map[domain.ElasticIPID]domain.ElasticIP{id: v}}
		if err := add(resRef{kindEIP, string(id)}, one); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r *kubeRepo) load(ctx context.Context, p domain.ProjectID) (domain.State, int64, error) {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	st, h, _, err := r.loadHead(ctx, p)
	return st, h.version, err
}

// loadHead reads a consistent state of p and the head it belongs to.
func (r *kubeRepo) loadHead(ctx context.Context, p domain.ProjectID) (domain.State, head, []corev1.ConfigMap, error) {
	for range kubeAttempts {
		h, err := r.settledHead(ctx, p)
		if err != nil {
			return domain.State{}, head{}, nil, err
		}
		if h.cm == nil {
			return domain.State{}, h, nil, nil
		}
		st, items, ok, err := r.snapshot(ctx, p, h.seq)
		if err != nil || ok {
			return st, h, items, err
		}
	}
	return domain.State{}, head{}, nil, &domain.Error{Kind: domain.Busy, Msg: "project changed concurrently"}
}

func (r *kubeRepo) addressesInUse(ctx context.Context) (map[netip.Addr]bool, error) {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	items, err := r.list(ctx, storeAddress, nil)
	if err != nil {
		return nil, err
	}
	out := make(map[netip.Addr]bool, len(items))
	for i := range items {
		cm := &items[i]
		a, err := netip.ParseAddr(cm.Data["value"])
		if err != nil {
			return nil, fmt.Errorf("network: address claim %s is corrupt", cm.Name)
		}
		if stray(cm, r.now()) {
			freed, err := r.reclaim(ctx, addrRef(a))
			if err != nil {
				return nil, err
			}
			if freed {
				continue
			}
		}
		out[a] = true
	}
	return out, nil
}

func (r *kubeRepo) projectIDs(ctx context.Context) ([]domain.ProjectID, error) {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	items, err := r.list(ctx, storeHead, nil)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ProjectID, 0, len(items))
	for _, cm := range items {
		out = append(out, domain.ProjectID(cm.Data["project"]))
	}
	return out, nil
}

func (r *kubeRepo) liveJobs(ctx context.Context, states ...string) ([]job, []corev1.ConfigMap, error) {
	req, err := labels.NewRequirement(labelOpState, selection.In, states)
	if err != nil {
		return nil, nil, fmt.Errorf("select operations: %w", err)
	}
	items, err := r.list(ctx, storeOp, nil, *req)
	if err != nil {
		return nil, nil, err
	}
	jobs := make([]job, 0, len(items))
	for _, cm := range items {
		j, err := decodeJob(&cm)
		if err != nil {
			return nil, nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, items, nil
}

func decodeJob(cm *corev1.ConfigMap) (job, error) {
	var d jobDoc
	if err := json.Unmarshal([]byte(cm.Data["job"]), &d); err != nil {
		return job{}, fmt.Errorf("network: operation %s is corrupt", cm.Name)
	}
	return d.job()
}

func (r *kubeRepo) pending(ctx context.Context) (int64, error) {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	jobs, _, err := r.liveJobs(ctx, "pending", "running")
	return int64(len(jobs)), err
}

func (r *kubeRepo) key(ctx context.Context, p domain.ProjectID, k domain.IdempotencyKey) (job, bool, error) {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	if _, err := r.settledHead(ctx, p); err != nil {
		return job{}, false, err
	}
	cm, err := r.get(ctx, keyName(p, k))
	if err != nil || cm == nil || cm.Data["project"] != string(p) || cm.Data["key"] != string(k) {
		return job{}, false, err
	}
	j, ok, err := r.job(ctx, cm.Data["op"])
	if err != nil || !ok || j.op.Project != p {
		return job{}, false, err
	}
	return j, true, nil
}

func (r *kubeRepo) job(ctx context.Context, id string) (job, bool, error) {
	cm, err := r.get(ctx, opName(id))
	if err != nil || cm == nil {
		return job{}, false, err
	}
	j, err := decodeJob(cm)
	if err != nil || j.op.ID != id {
		return job{}, false, err
	}
	return j, true, nil
}

func (r *kubeRepo) operation(ctx context.Context, p domain.ProjectID, id string) (domain.Operation, error) {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	if _, err := r.settledHead(ctx, p); err != nil {
		return domain.Operation{}, err
	}
	j, ok, err := r.job(ctx, id)
	if err != nil {
		return domain.Operation{}, err
	}
	if !ok || j.op.Project != p {
		return domain.Operation{}, &domain.Error{Kind: domain.NotFound, Msg: "operation not found"}
	}
	return j.op, nil
}

// commit follows the repository contract. It settles the head, checks the
// commit against fresh data, takes new pool addresses, and swaps the head.
// A lost swap gives the addresses back. A commit without state retries a
// lost swap itself, since no caller rebuilt it from a stale load.
func (r *kubeRepo) commit(ctx context.Context, c commit) error {
	if !c.exclusive() {
		return errLostClaim
	}
	ctx, cancel := callCtx(ctx)
	defer cancel()
	for range kubeAttempts {
		err := r.commitOnce(ctx, c)
		if !errors.Is(err, errStale) || c.state != nil {
			return err
		}
	}
	return errStale
}

func (r *kubeRepo) commitOnce(ctx context.Context, c commit) error {
	cur, h, items, err := r.loadHead(ctx, c.project)
	if err != nil {
		return err
	}
	if c.state != nil && h.version != c.base {
		return errStale
	}
	cs := changeSet{ID: randomID("chg-"), Seq: h.seq + 1}
	if done, err := r.opChange(ctx, c, &cs); err != nil || done {
		return err
	}
	var taken []*corev1.ConfigMap
	if c.state != nil {
		if taken, err = r.stateChange(ctx, c.project, cur, *c.state, &cs); err != nil {
			return err
		}
	}
	if err := r.swapHead(ctx, c, h, cs); err != nil {
		// Only a refused swap is known to have written nothing. Any other
		// error may hide a head that did change, so its claims stay; reclaim
		// frees them later if the head never took them.
		if errors.Is(err, errStale) || errors.Is(err, errObjectTooLarge) {
			for _, cm := range taken {
				err = errors.Join(err, r.deleteAt(ctx, cm))
			}
		}
		return err
	}
	if err := r.apply(ctx, c.project, cs); err != nil {
		return err
	}
	r.collect(ctx, items, h.seq)
	if _, err := r.settledHead(ctx, c.project); err != nil {
		return err
	}
	return nil
}

// opChange adds the operation write of c to cs. done reports a settle of a
// job that is already terminal, which writes nothing.
func (r *kubeRepo) opChange(ctx context.Context, c commit, cs *changeSet) (bool, error) {
	switch {
	case c.enqueue != nil:
		k, err := r.get(ctx, keyName(c.project, c.enqueue.op.Key))
		if err != nil {
			return false, err
		}
		if k != nil {
			return false, errKeyTaken
		}
		j := *c.enqueue
		j.seq, j.token = cs.Seq, ""
		d := docOf(j)
		cs.Op, cs.OpCreate = &d, true
	case c.revive != nil:
		cur, ok, err := r.job(ctx, c.revive.op.ID)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, errLostClaim
		}
		cur.op, cur.lane, cur.token = c.revive.op, c.revive.lane, ""
		cur.attempts, cur.notBefore = c.revive.attempts, c.revive.notBefore
		cur.op.Err = nil
		d := docOf(cur)
		cs.Op = &d
	case c.settle != nil:
		cur, ok, err := r.job(ctx, c.settle.op.ID)
		if err != nil {
			return false, err
		}
		if !ok || cur.token != c.settle.token {
			return false, errLostClaim
		}
		if terminal(cur.op.State) {
			return true, nil
		}
		cur.op.State, cur.op.Err = c.settle.op.State, c.settle.op.Err
		cur.attempts, cur.notBefore = c.settle.attempts, c.settle.notBefore
		if cur.op.State == domain.OpPending {
			cur.token = ""
		}
		d := docOf(cur)
		cs.Op = &d
	}
	return false, nil
}

// stateChange adds the resource writes from cur to next to cs and takes every
// pool address and resource id next holds that cur does not. It returns the claims it took.
func (r *kubeRepo) stateChange(
	ctx context.Context, p domain.ProjectID, cur, next domain.State, cs *changeSet,
) ([]*corev1.ConfigMap, error) {
	before, err := documents(cur)
	if err != nil {
		return nil, err
	}
	after, err := documents(next)
	if err != nil {
		return nil, err
	}
	for ref, doc := range after {
		if before[ref] == doc {
			continue
		}
		if err := fitsLimit(r.resourceMap(p, resPut{resRef: ref, State: doc}, before[ref])); err != nil {
			return nil, &domain.Error{Kind: domain.Invalid, Msg: fmt.Sprintf("%s %s is too large to store", ref.Kind, ref.ID)}
		}
		cs.Puts = append(cs.Puts, resPut{resRef: ref, State: doc})
	}
	for ref := range before {
		if _, ok := after[ref]; !ok {
			cs.Puts = append(cs.Puts, resPut{resRef: ref})
		}
	}
	had, has := claimsOf(cur), claimsOf(next)
	for ref := range had {
		if has[ref] {
			continue
		}
		cm, err := r.get(ctx, ref.name())
		if err != nil {
			return nil, err
		}
		if cm != nil && cm.Data["project"] == string(p) {
			cs.Release = append(cs.Release, claimRelease{claimRef: ref, Txn: cm.Data["txn"]})
		}
	}
	var taken []*corev1.ConfigMap
	giveBack := func(err error) error {
		for _, cm := range taken {
			err = errors.Join(err, r.deleteAt(ctx, cm))
		}
		return err
	}
	for ref := range has {
		if had[ref] {
			continue
		}
		cm := r.configMap(ref.name(), ref.Store, projectSet(p),
			map[string]string{"value": ref.Value, "project": string(p), "txn": cs.ID})
		err := r.create(ctx, cm)
		if apierrors.IsAlreadyExists(err) {
			freed, rerr := r.reclaim(ctx, ref)
			if rerr != nil {
				return nil, giveBack(rerr)
			}
			if freed {
				err = r.create(ctx, cm)
			}
		}
		if apierrors.IsAlreadyExists(err) {
			return nil, giveBack(ref.taken())
		}
		if err != nil {
			return nil, giveBack(err)
		}
		taken = append(taken, cm)
		cs.Take = append(cs.Take, ref)
	}
	return taken, nil
}

// swapHead writes cs into the head with CAS. A lost race is errStale.
func (r *kubeRepo) swapHead(ctx context.Context, c commit, h head, cs changeSet) error {
	raw, err := json.Marshal(cs)
	if err != nil {
		return fmt.Errorf("encode change %s: %w", cs.ID, err)
	}
	version := h.version
	if c.state != nil {
		version++
	}
	data := map[string]string{
		"project": string(c.project), "version": strconv.FormatInt(version, 10),
		"seq": strconv.FormatInt(cs.Seq, 10), "change": string(raw),
	}
	if h.cm == nil {
		err = r.create(ctx, r.configMap(headName(c.project), storeHead, nil, data))
	} else {
		next := h.cm.DeepCopy()
		next.Data = data
		err = r.update(ctx, next)
	}
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		return errStale
	}
	return err
}

// collect deletes tombstones older than kubeGrace that no snapshot at seq or
// later reads. A failure leaves them for the next commit or repair.
func (r *kubeRepo) collect(ctx context.Context, items []corev1.ConfigMap, seq int64) {
	cutoff := r.now().Add(-kubeGrace).UnixNano()
	for i := range items {
		cm := &items[i]
		at, err := strconv.ParseInt(cm.Data["deleted"], 10, 64)
		if err != nil || at > cutoff {
			continue
		}
		if s, err := strconv.ParseInt(cm.Data["seq"], 10, 64); err != nil || s > seq {
			continue
		}
		_ = r.deleteAt(ctx, cm) //nolint:errcheck // best effort: a tombstone left behind is collected later
	}
}

func (r *kubeRepo) claim(ctx context.Context, now time.Time, token string) (job, bool, error) {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	for range kubeAttempts {
		jobs, items, err := r.liveJobs(ctx, "pending", "running")
		if err != nil {
			return job{}, false, err
		}
		j, ok := pickRunnable(jobs, now)
		if !ok {
			return job{}, false, nil
		}
		for i := range jobs {
			if jobs[i].op.ID != j.op.ID {
				continue
			}
			j.op.State, j.token = domain.OpRunning, token
			err := r.writeJob(ctx, &items[i], j)
			if err == nil {
				return j, true, nil
			}
			if !apierrors.IsConflict(err) {
				return job{}, false, err
			}
		}
	}
	return job{}, false, nil
}

// writeJob rewrites the job in cm, keeping the change that last wrote it.
func (r *kubeRepo) writeJob(ctx context.Context, cm *corev1.ConfigMap, j job) error {
	var cur jobDoc
	if err := json.Unmarshal([]byte(cm.Data["job"]), &cur); err != nil {
		return fmt.Errorf("network: operation %s is corrupt", cm.Name)
	}
	d := docOf(j)
	d.Rev = cur.Rev
	raw, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode operation %s: %w", j.op.ID, err)
	}
	next := cm.DeepCopy()
	next.Data["job"] = string(raw)
	next.Labels[labelOpState] = opStateLabel(j.op.State)
	return r.update(ctx, next)
}

func (r *kubeRepo) requeue(ctx context.Context) error {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	for range kubeAttempts {
		jobs, items, err := r.liveJobs(ctx, "running")
		if err != nil {
			return err
		}
		lost := false
		for i := range jobs {
			j := jobs[i]
			j.op.State, j.token = domain.OpPending, ""
			err := r.writeJob(ctx, &items[i], j)
			if apierrors.IsConflict(err) {
				lost = true
				continue
			}
			if err != nil {
				return err
			}
		}
		if !lost {
			return nil
		}
	}
	return errStale
}

func (r *kubeRepo) ping(ctx context.Context) error {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	var out corev1.ConfigMapList
	if err := retried(func() error { return r.c.List(ctx, &out, client.InNamespace(r.ns), client.Limit(1)) }); err != nil {
		return fmt.Errorf("ping kubernetes store: %w", err)
	}
	return nil
}

func (r *kubeRepo) close() error { return nil }

// repair finishes every change set a crash left unapplied, deletes pool
// address and resource id claims older than kubeGrace whose project does not
// hold them, and collects old tombstones. It is idempotent.
func (r *kubeRepo) repair(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 4*kubeCallTimeout)
	defer cancel()
	heads, err := r.list(ctx, storeHead, nil)
	if err != nil {
		return err
	}
	for _, cm := range heads {
		p := domain.ProjectID(cm.Data["project"])
		_, h, items, err := r.loadHead(ctx, p)
		if err != nil {
			return fmt.Errorf("repair project %s: %w", p, err)
		}
		r.collect(ctx, items, h.seq)
	}
	for _, store := range []string{storeAddress, storeID} {
		claims, err := r.list(ctx, store, nil)
		if err != nil {
			return err
		}
		for i := range claims {
			if _, err := r.reclaim(ctx, claimRef{Store: store, Value: claims[i].Data["value"]}); err != nil {
				return err
			}
		}
	}
	return nil
}

// stray reports a claim no applied change confirmed that is older than
// kubeGrace. A younger one may belong to a commit still in flight.
func stray(cm *corev1.ConfigMap, now time.Time) bool {
	return cm.Data["confirmed"] == "" && !cm.CreationTimestamp.After(now.Add(-kubeGrace))
}

// reclaim deletes the claim on ref when it is stray and its project does not
// hold ref: a commit took it and then lost its head swap or crashed.
func (r *kubeRepo) reclaim(ctx context.Context, ref claimRef) (bool, error) {
	cm, err := r.get(ctx, ref.name())
	if err != nil || cm == nil {
		return cm == nil && err == nil, err
	}
	if !stray(cm, r.now()) {
		return false, nil
	}
	st, _, _, err := r.loadHead(ctx, domain.ProjectID(cm.Data["project"]))
	if err != nil {
		return false, err
	}
	if claimsOf(st)[ref] {
		return false, nil
	}
	return true, r.deleteAt(ctx, cm)
}
