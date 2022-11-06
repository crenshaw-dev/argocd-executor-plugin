package argocd

import (
	"testing"

	"github.com/argoproj/gitops-engine/pkg/utils/kube"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
)

func Test_getRefreshType(t *testing.T) {
	t.Parallel()

	var no *string
	normal := string(v1alpha1.RefreshTypeNormal)
	hard := string(v1alpha1.RefreshTypeHard)

	t.Run("no refresh", func(t *testing.T) {
		assert.Equal(t, no, getRefreshType(false, false))
	})

	t.Run("normal refresh", func(t *testing.T) {
		assert.Equal(t, &normal, getRefreshType(true, false))
	})

	t.Run("hard refresh", func(t *testing.T) {
		assert.Equal(t, &hard, getRefreshType(false, true))
		assert.Equal(t, &hard, getRefreshType(true, true))
	})
}

func Test_groupObjsByKey(t *testing.T) {
	t.Parallel()

	t.Run("single", func(t *testing.T) {
		localObjs := []*unstructured.Unstructured{
			{
				Object: map[string]interface{}{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"metadata": map[string]interface{}{
						"name":      "my-deployment",
						"namespace": "my-namespace",
					},
				},
			},
		}
		grouped, err := groupObjsByKey(localObjs, localObjs, "my-namespace")
		require.NoError(t, err)
		assert.Equal(t, map[kube.ResourceKey]*unstructured.Unstructured{
			kube.ResourceKey{
				Group:     "apps",
				Kind:      "Deployment",
				Name:      "my-deployment",
				Namespace: "my-namespace",
			}: localObjs[0],
		}, grouped)
	})
}
