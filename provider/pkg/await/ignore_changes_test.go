// Copyright 2016-2026, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package await

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	ignoreTestOurs   = "pulumi-kubernetes-12345678"
	ignoreTestTheirs = "image-deploy"
)

// ignoreTestObject parses a JSON object.
func ignoreTestObject(t *testing.T, doc string) map[string]any {
	t.Helper()
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(doc), &obj))
	return obj
}

// ignoreTestLive returns a live object with the given managed fields, as a
// map from the field manager to its fieldsV1 document.
func ignoreTestLive(t *testing.T, doc string, managed map[string]string) *unstructured.Unstructured {
	t.Helper()
	live := &unstructured.Unstructured{Object: ignoreTestObject(t, doc)}
	var entries []metav1.ManagedFieldsEntry
	for _, manager := range []string{ignoreTestOurs, ignoreTestTheirs} {
		fields, ok := managed[manager]
		if !ok {
			continue
		}
		entries = append(entries, metav1.ManagedFieldsEntry{
			Manager:    manager,
			Operation:  metav1.ManagedFieldsOperationApply,
			APIVersion: "apps/v1",
			FieldsType: "FieldsV1",
			FieldsV1:   &metav1.FieldsV1{Raw: []byte(fields)},
		})
	}
	live.SetManagedFields(entries)
	return live
}

func TestHandleSSAIgnoreFields(t *testing.T) {
	// The live object runs images that another manager set after the last
	// Pulumi update. The inputs still hold the images of the Pulumi state.
	const liveDeployment = `{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": {"name": "web", "annotations": {"example.com/rev": "live"}},
		"spec": {
			"replicas": 3,
			"template": {"spec": {"containers": [
				{"name": "web", "image": "web:live", "args": ["--live"]},
				{"name": "sidecar", "image": "sidecar:live"}
			]}}
		}
	}`
	const inputs = `{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": {"name": "web", "annotations": {"example.com/rev": "state"}},
		"spec": {
			"replicas": 1,
			"template": {"spec": {"containers": [
				{"name": "web", "image": "web:state", "args": ["--state"]},
				{"name": "sidecar", "image": "sidecar:state"}
			]}}
		}
	}`

	// Field sets. "Rest" is every field of the inputs but the two images.
	const (
		oursRest = `{
			"f:metadata": {"f:annotations": {"f:example.com/rev": {}}},
			"f:spec": {"f:replicas": {}, "f:template": {"f:spec": {"f:containers": {
				"k:{\"name\":\"web\"}": {".": {}, "f:name": {}, "f:args": {}},
				"k:{\"name\":\"sidecar\"}": {".": {}, "f:name": {}}
			}}}}
		}`
		oursAll = `{
			"f:metadata": {"f:annotations": {"f:example.com/rev": {}}},
			"f:spec": {"f:replicas": {}, "f:template": {"f:spec": {"f:containers": {
				"k:{\"name\":\"web\"}": {".": {}, "f:name": {}, "f:image": {}, "f:args": {}},
				"k:{\"name\":\"sidecar\"}": {".": {}, "f:name": {}, "f:image": {}}
			}}}}
		}`
		theirsWebImage = `{"f:spec": {"f:template": {"f:spec": {"f:containers": {
			"k:{\"name\":\"web\"}": {"f:image": {}}
		}}}}}`
		theirsBothImages = `{"f:spec": {"f:template": {"f:spec": {"f:containers": {
			"k:{\"name\":\"web\"}": {"f:image": {}},
			"k:{\"name\":\"sidecar\"}": {"f:image": {}}
		}}}}}`
		theirsReplicasAndAnnotation = `{
			"f:metadata": {"f:annotations": {"f:example.com/rev": {}}},
			"f:spec": {"f:replicas": {}}
		}`
		oursContainersOnly = `{"f:spec": {"f:template": {"f:spec": {"f:containers": {
			"k:{\"name\":\"web\"}": {".": {}, "f:name": {}, "f:image": {}, "f:args": {}},
			"k:{\"name\":\"sidecar\"}": {".": {}, "f:name": {}, "f:image": {}}
		}}}}}`
		theirsSidecar = `{"f:spec": {"f:template": {"f:spec": {"f:containers": {
			"k:{\"name\":\"sidecar\"}": {".": {}, "f:name": {}, "f:image": {}}
		}}}}}`
		oursWebOnly = `{"f:spec": {"f:template": {"f:spec": {"f:containers": {
			"k:{\"name\":\"web\"}": {".": {}, "f:name": {}, "f:image": {}, "f:args": {}}
		}}}}}`
	)

	// container returns a container of the inputs after the call.
	container := func(t *testing.T, obj map[string]any, index int) map[string]any {
		t.Helper()
		containers, found, err := unstructured.NestedSlice(obj, "spec", "template", "spec", "containers")
		require.NoError(t, err)
		require.True(t, found)
		require.Greater(t, len(containers), index)
		return containers[index].(map[string]any)
	}

	tests := []struct {
		name    string
		live    string
		inputs  string
		managed map[string]string
		ignore  []string
		check   func(t *testing.T, got map[string]any)
	}{
		{
			name:    "a list item field that only another manager owns is dropped",
			managed: map[string]string{ignoreTestOurs: oursRest, ignoreTestTheirs: theirsWebImage},
			ignore:  []string{"spec.template.spec.containers[0].image"},
			check: func(t *testing.T, got map[string]any) {
				assert.NotContains(t, container(t, got, 0), "image")
				assert.Equal(t, "web", container(t, got, 0)["name"])
				// The other container is not ignored, so it keeps its input.
				assert.Equal(t, "sidecar:state", container(t, got, 1)["image"])
			},
		},
		{
			name:    "a list item field that both managers own takes the live value",
			managed: map[string]string{ignoreTestOurs: oursAll, ignoreTestTheirs: theirsWebImage},
			ignore:  []string{"spec.template.spec.containers[0].image"},
			check: func(t *testing.T, got map[string]any) {
				assert.Equal(t, "web:live", container(t, got, 0)["image"])
			},
		},
		{
			name:    "a list item field that only this manager owns takes the live value",
			managed: map[string]string{ignoreTestOurs: oursAll},
			ignore:  []string{"spec.template.spec.containers[0].image"},
			check: func(t *testing.T, got map[string]any) {
				assert.Equal(t, "web:live", container(t, got, 0)["image"])
			},
		},
		{
			name:    "a wildcard covers every list item",
			managed: map[string]string{ignoreTestOurs: oursRest, ignoreTestTheirs: theirsBothImages},
			ignore:  []string{"spec.template.spec.containers[*].image"},
			check: func(t *testing.T, got map[string]any) {
				assert.NotContains(t, container(t, got, 0), "image")
				assert.NotContains(t, container(t, got, 1), "image")
			},
		},
		{
			name: "a list item is matched by its key when the live order differs",
			live: `{
				"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "web"},
				"spec": {"template": {"spec": {"containers": [
					{"name": "sidecar", "image": "sidecar:live"},
					{"name": "web", "image": "web:live"}
				]}}}
			}`,
			managed: map[string]string{ignoreTestOurs: oursAll},
			ignore:  []string{"spec.template.spec.containers[0].image"},
			check: func(t *testing.T, got map[string]any) {
				assert.Equal(t, "web:live", container(t, got, 0)["image"])
				assert.Equal(t, "sidecar:state", container(t, got, 1)["image"])
			},
		},
		{
			name:    "an item of a list with no recorded elements takes the live value by index",
			managed: map[string]string{ignoreTestOurs: oursAll},
			ignore:  []string{"spec.template.spec.containers[0].args[0]"},
			check: func(t *testing.T, got map[string]any) {
				assert.Equal(t, []any{"--live"}, container(t, got, 0)["args"])
			},
		},
		{
			name:    "a list item that only another manager owns is dropped",
			managed: map[string]string{ignoreTestOurs: oursWebOnly, ignoreTestTheirs: theirsSidecar},
			ignore:  []string{"spec.template.spec.containers[1]"},
			check: func(t *testing.T, got map[string]any) {
				containers, _, err := unstructured.NestedSlice(got, "spec", "template", "spec", "containers")
				require.NoError(t, err)
				require.Len(t, containers, 1)
				assert.Equal(t, "web", containers[0].(map[string]any)["name"])
			},
		},
		{
			name:    "a field outside a list that only another manager owns is dropped",
			managed: map[string]string{ignoreTestOurs: oursContainersOnly, ignoreTestTheirs: theirsReplicasAndAnnotation},
			ignore:  []string{"spec.replicas"},
			check: func(t *testing.T, got map[string]any) {
				_, found, err := unstructured.NestedFieldNoCopy(got, "spec", "replicas")
				require.NoError(t, err)
				assert.False(t, found)
			},
		},
		{
			name:    "a field outside a list that this manager owns takes the live value",
			managed: map[string]string{ignoreTestOurs: oursAll},
			ignore:  []string{"spec.replicas"},
			check: func(t *testing.T, got map[string]any) {
				replicas, _, err := unstructured.NestedFieldNoCopy(got, "spec", "replicas")
				require.NoError(t, err)
				assert.EqualValues(t, 3, replicas)
			},
		},
		{
			name:    "a map key with a dot is one path element",
			managed: map[string]string{ignoreTestOurs: oursContainersOnly, ignoreTestTheirs: theirsReplicasAndAnnotation},
			ignore:  []string{`metadata.annotations["example.com/rev"]`},
			check: func(t *testing.T, got map[string]any) {
				annotations, _, err := unstructured.NestedMap(got, "metadata", "annotations")
				require.NoError(t, err)
				assert.NotContains(t, annotations, "example.com/rev")
			},
		},
		{
			name:    "a path that the inputs and the live object lack changes nothing",
			managed: map[string]string{ignoreTestOurs: oursAll},
			ignore:  []string{"spec.template.spec.containers[5].image", "spec.template.spec.initContainers[0].image"},
			check: func(t *testing.T, got map[string]any) {
				assert.Equal(t, "web:state", container(t, got, 0)["image"])
				_, found, err := unstructured.NestedFieldNoCopy(got, "spec", "template", "spec", "initContainers")
				require.NoError(t, err)
				assert.False(t, found)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			liveDoc, inputsDoc := tt.live, tt.inputs
			if liveDoc == "" {
				liveDoc = liveDeployment
			}
			if inputsDoc == "" {
				inputsDoc = inputs
			}
			live := ignoreTestLive(t, liveDoc, tt.managed)
			c := &UpdateConfig{
				ProviderConfig: ProviderConfig{FieldManager: ignoreTestOurs},
				Inputs:         &unstructured.Unstructured{Object: ignoreTestObject(t, inputsDoc)},
				IgnoreChanges:  tt.ignore,
			}

			require.NoError(t, handleSSAIgnoreFields(c, live))
			tt.check(t, c.Inputs.Object)
		})
	}
}
