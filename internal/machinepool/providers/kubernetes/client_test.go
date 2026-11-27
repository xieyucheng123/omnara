package kubernetes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/omnara-ai/omnara/internal/machinepool/providers"
	"github.com/stretchr/testify/require"
)

// newWireClient serves the client against a real HTTP server so URL shape,
// auth headers, and JSON bodies are covered instead of being hidden by the
// in-memory fake.
func newWireClient(t *testing.T, handler http.HandlerFunc) (*restClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &restClient{
		baseURL:    server.URL,
		namespace:  "test-ns",
		token:      "service-account-token",
		httpClient: server.Client(),
	}, server
}

func TestKubernetesWirePutSecretKeepsData(t *testing.T) {
	var got map[string]any
	client, _ := newWireClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPut, r.Method)
		require.Equal(t, "/api/v1/namespaces/test-ns/secrets/machine-env", r.URL.Path)
		require.Equal(t, "Bearer service-account-token", r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &got))
		require.NotContains(t, string(body), "stringData")
		_, _ = w.Write([]byte(`{"metadata":{"name":"machine-env"}}`))
	})

	err := client.PutSecretOwner(context.Background(), secretObject{
		Metadata: objectMeta{Name: "machine-env", ResourceVersion: "7"},
		Data:     map[string]string{"OMNARA_MACHINE_TOKEN": "token"},
	}, ownerReference{APIVersion: "v1", Kind: "Pod", Name: "machine", UID: "uid-1"})
	require.NoError(t, err)

	require.Equal(t, map[string]any{"OMNARA_MACHINE_TOKEN": "token"}, got["data"])
	metadata := got["metadata"].(map[string]any)
	require.Equal(t, "7", metadata["resourceVersion"])
	owners := metadata["ownerReferences"].([]any)
	require.Equal(t, "Pod", owners[0].(map[string]any)["kind"])
}

func TestKubernetesWireCreatePodShape(t *testing.T) {
	var pod map[string]any
	client, _ := newWireClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/v1/namespaces/test-ns/pods", r.URL.Path)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &pod))
		_, _ = w.Write([]byte(`{"metadata":{"name":"machine","uid":"uid-1"}}`))
	})

	share := true
	automount := false
	created, err := client.CreatePod(context.Background(), podObject{
		Metadata: objectMeta{Name: "machine"},
		Spec: podSpec{
			ShareProcessNamespace:        &share,
			AutomountServiceAccountToken: &automount,
			RestartPolicy:                "Never",
			Containers: []containerSpec{{
				Name:    "machine",
				Image:   "image",
				EnvFrom: []envFromSource{{SecretRef: &secretRef{Name: "machine-env"}}},
				Resources: resourceRequirements{
					Requests: map[string]string{"cpu": "1", "memory": "1024Mi"},
					Limits:   map[string]string{"cpu": "1", "memory": "1024Mi"},
				},
			}},
		},
	})
	require.NoError(t, err)
	require.Equal(t, "uid-1", created.Metadata.UID)

	spec := pod["spec"].(map[string]any)
	require.Equal(t, true, spec["shareProcessNamespace"])
	require.Equal(t, false, spec["automountServiceAccountToken"])
	containers := spec["containers"].([]any)
	container := containers[0].(map[string]any)
	envFrom := container["envFrom"].([]any)
	require.Equal(t, "machine-env", envFrom[0].(map[string]any)["secretRef"].(map[string]any)["name"])
}

func TestKubernetesWireDeletePodUsesZeroGrace(t *testing.T) {
	var body map[string]any
	client, _ := newWireClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodDelete, r.Method)
		require.Equal(t, "/api/v1/namespaces/test-ns/pods/machine", r.URL.Path)
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &body))
		w.WriteHeader(http.StatusOK)
	})
	require.NoError(t, client.DeletePod(context.Background(), "machine"))
	require.EqualValues(t, 0, body["gracePeriodSeconds"])
}

func TestKubernetesWireConnectionAndStatuses(t *testing.T) {
	client, _ := newWireClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/api/v1/namespaces/test-ns/pods", r.URL.Path)
		require.Equal(t, "1", r.URL.Query().Get("limit"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	require.NoError(t, client.CheckConnection(context.Background()))

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			c, _ := newWireClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"rejected"}`))
			})
			err := c.CheckConnection(context.Background())
			require.Error(t, err)
			got, ok := providers.HTTPStatus(err)
			require.True(t, ok)
			require.Equal(t, status, got)
		})
	}
}

func TestKubernetesWireNotFoundIsNotAnErrorForInspectOrDelete(t *testing.T) {
	client, _ := newWireClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	})
	_, found, err := client.InspectPod(context.Background(), "missing")
	require.NoError(t, err)
	require.False(t, found)
	_, found, err = client.InspectSecret(context.Background(), "missing")
	require.NoError(t, err)
	require.False(t, found)
	require.NoError(t, client.DeletePod(context.Background(), "missing"))
	require.NoError(t, client.DeleteSecret(context.Background(), "missing"))
}
