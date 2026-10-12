package network

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func closeOrFail(t testing.TB, f func() error) {
	t.Helper()
	if err := f(); err != nil {
		t.Error(err)
	}
}

// nestedStringMap reads a map of strings from a cluster object. Absent and
// mistyped read as empty so the assertion that follows reports the difference.
func nestedStringMap(obj map[string]any, fields ...string) map[string]string {
	v, _, _ := unstructured.NestedStringMap(obj, fields...) //nolint:errcheck // absent and mistyped both read as empty
	return v
}
