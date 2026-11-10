package network

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/popododo0720/vws-network/internal/domain"
)

type lifecycleState string

const (
	lifecycleName                     = "vws-network-lifecycle"
	lifecycleImporting lifecycleState = "importing"
	lifecycleImported  lifecycleState = "imported"
	lifecycleServing   lifecycleState = "serving"
)

func readLifecycle(ctx context.Context, r *kubeRepo) (*corev1.ConfigMap, error) {
	cm, err := r.get(ctx, lifecycleName)
	if err != nil || cm == nil {
		return cm, err
	}
	switch lifecycleState(cm.Data["state"]) {
	case lifecycleServing:
		if cm.Data["source"] == "" {
			return cm, nil
		}
	case lifecycleImporting, lifecycleImported:
		if len(cm.Data["source"]) == sha256.Size*2 && strings.Trim(cm.Data["source"], "0123456789abcdef") == "" {
			return cm, nil
		}
	}
	return nil, &domain.Error{Kind: domain.Conflict, Msg: "invalid store lifecycle marker"}
}

func importFinished(ctx context.Context, r *kubeRepo) (bool, error) {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	cm, err := readLifecycle(ctx, r)
	if err != nil || cm == nil {
		return false, err
	}
	if lifecycleState(cm.Data["state"]) == lifecycleServing {
		return false, &domain.Error{Kind: domain.Conflict, Msg: "store has started serving; import is forbidden"}
	}
	return lifecycleState(cm.Data["state"]) == lifecycleImported, nil
}

// Atomic Create chooses import or service admission. The source digest prevents
// concurrent or restarted importers from mixing different SQLite snapshots.
func admitLifecycle(ctx context.Context, r *kubeRepo, want lifecycleState, source string) (bool, error) {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	for range 2 {
		cm, err := readLifecycle(ctx, r)
		if err != nil {
			return false, err
		}
		if cm == nil {
			cm = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: lifecycleName, Namespace: r.ns, Labels: map[string]string{labelStore: "lifecycle"},
				},
				Data: map[string]string{"state": string(want), "source": source},
			}
			if err := r.create(ctx, cm); err != nil {
				if apierrors.IsAlreadyExists(err) {
					continue
				}
				return false, err
			}
			return false, nil
		}
		state := lifecycleState(cm.Data["state"])
		if want == lifecycleServing && state != lifecycleImporting {
			return false, nil
		}
		if want == lifecycleImporting {
			if state == lifecycleImported {
				return true, nil
			}
			if state == lifecycleImporting && cm.Data["source"] == source {
				return false, nil
			}
		}
		return false, &domain.Error{Kind: domain.Conflict, Msg: "store lifecycle excludes this import or service start"}
	}
	return false, &domain.Error{Kind: domain.Busy, Msg: "store lifecycle changed during admission"}
}

func completeImport(ctx context.Context, r *kubeRepo, source string) error {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	cm, err := readLifecycle(ctx, r)
	if err != nil {
		return err
	}
	if cm == nil || cm.Data["source"] != source || lifecycleState(cm.Data["state"]) == lifecycleServing {
		return &domain.Error{Kind: domain.Conflict, Msg: "import lost its lifecycle marker"}
	}
	if lifecycleState(cm.Data["state"]) == lifecycleImported {
		return nil
	}
	cm.Data["state"] = string(lifecycleImported)
	return r.update(ctx, cm)
}

func reportImported(out io.Writer) error {
	var w strings.Builder
	w.WriteString("already imported; differences: 0, missing: 0, writes: 0\n")
	return flush(out, &w)
}

func importSource(ctx context.Context, src *sqliteRepo) (string, error) {
	h := sha256.New()
	enc := json.NewEncoder(h)
	ids, err := src.projectIDs(ctx)
	if err != nil {
		return "", err
	}
	slices.Sort(ids)
	for _, id := range ids {
		st, _, err := src.load(ctx, id)
		if err != nil {
			return "", err
		}
		docs, err := documents(st)
		if err != nil {
			return "", fmt.Errorf("import source %s: %w", id, err)
		}
		canonical := make(map[string]string, len(docs))
		for ref, doc := range docs {
			canonical[ref.Kind+"/"+ref.ID] = doc
		}
		if err := enc.Encode(struct {
			ID        domain.ProjectID
			Documents map[string]string
		}{id, canonical}); err != nil {
			return "", fmt.Errorf("hash import project: %w", err)
		}
	}
	jobs, err := src.jobs(ctx)
	if err != nil {
		return "", err
	}
	slices.SortFunc(jobs, func(a, b job) int { return strings.Compare(a.op.ID, b.op.ID) })
	for _, j := range jobs {
		if err := enc.Encode(docOf(j)); err != nil {
			return "", fmt.Errorf("hash import operation: %w", err)
		}
	}
	addresses, err := src.addressesInUse(ctx)
	if err != nil {
		return "", err
	}
	if err := enc.Encode(addresses); err != nil {
		return "", fmt.Errorf("hash import addresses: %w", err)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
