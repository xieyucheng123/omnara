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

type capturedRequest struct {
	method string
	path   string
	query  string
	auth   string
	body   []byte
}

// newWireClient serves the client against a real HTTP server so URL shape,
// auth headers, and JSON bodies are covered instead of being hidden by the
// in-memory fake.
func newWireClient(t *testing.T, status int, response string, captured *capturedRequest) *restClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*captured = capturedRequest{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			auth:   r.Header.Get("Authorization"),
			body:   body,
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return &restClient{
		baseURL:    server.URL,
		namespace:  "test-ns",
		token:      "service-account-token",
		httpClient: server.Client(),
	}
}

func TestKubernetesWirePutSecretKeepsData(t *testing.T) {
	var captured capturedRequest
	client := newWireClient(t, http.StatusOK, `{"metadata":{"name":"machine-env"}}`, &captured)

	err := client.PutSecretOwner(context.Background(), secretObject{
		Metadata: objectMeta{Name: "machine-env", ResourceVersion: "7"},
		Data:     map[string]string{"OMNARA_MACHINE_TOKEN": "token"},
	}, ownerReference{APIVersion: "v1", Kind: "Pod", Name: "machine", UID: "uid-1"})
	require.NoError(t, err)

	require.Equal(t, http.MethodPut, captured.method)
	require.Equal(t, "/api/v1/namespaces/test-ns/secrets/machine-env", captured.path)
	require.Equal(t, "Bearer service-account-token", captured.auth)
	require.NotContains(t, string(captured.body), "stringData")

	var sent secretObject
	require.NoError(t, json.Unmarshal(captured.body, &sent))
	require.Equal(t, map[string]string{"OMNARA_MACHINE_TOKEN": "token"}, sent.Data)
	require.Equal(t, "7", sent.Metadata.ResourceVersion)
	require.Len(t, sent.Metadata.OwnerReferences, 1)
	require.Equal(t, "Pod", sent.Metadata.OwnerReferences[0].Kind)
}

func TestKubernetesWireCreatePodShape(t *testing.T) {
	var captured capturedRequest
	client := newWireClient(t, http.StatusCreated, `{"metadata":{"name":"machine","uid":"uid-1"}}`, &captured)

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
	require.Equal(t, http.MethodPost, captured.method)
	require.Equal(t, "/api/v1/namespaces/test-ns/pods", captured.path)

	var sent podObject
	require.NoError(t, json.Unmarshal(captured.body, &sent))
	require.True(t, *sent.Spec.ShareProcessNamespace)
	require.False(t, *sent.Spec.AutomountServiceAccountToken)
	require.Equal(t, "Never", sent.Spec.RestartPolicy)
	require.Equal(t, "machine-env", sent.Spec.Containers[0].EnvFrom[0].SecretRef.Name)
	require.Equal(t, sent.Spec.Containers[0].Resources.Requests, sent.Spec.Containers[0].Resources.Limits)
}

func TestKubernetesWireDeletePodUsesZeroGrace(t *testing.T) {
	var captured capturedRequest
	client := newWireClient(t, http.StatusOK, `{}`, &captured)

	require.NoError(t, client.DeletePod(context.Background(), "machine"))
	require.Equal(t, http.MethodDelete, captured.method)
	require.Equal(t, "/api/v1/namespaces/test-ns/pods/machine", captured.path)

	var body struct {
		GracePeriodSeconds int64 `json:"gracePeriodSeconds"`
	}
	require.NoError(t, json.Unmarshal(captured.body, &body))
	require.Zero(t, body.GracePeriodSeconds)
}

func TestKubernetesWireConnectionAndStatuses(t *testing.T) {
	var captured capturedRequest
	client := newWireClient(t, http.StatusOK, `{"items":[]}`, &captured)
	require.NoError(t, client.CheckConnection(context.Background()))
	require.Equal(t, http.MethodGet, captured.method)
	require.Equal(t, "/api/v1/namespaces/test-ns/pods", captured.path)
	require.Equal(t, "limit=1", captured.query)

	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusConflict,
		http.StatusUnprocessableEntity,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var request capturedRequest
			c := newWireClient(t, status, `{"message":"rejected"}`, &request)
			err := c.CheckConnection(context.Background())
			require.Error(t, err)
			got, ok := providers.HTTPStatus(err)
			require.True(t, ok)
			require.Equal(t, status, got)
		})
	}
}

func TestKubernetesWireNotFoundIsNotAnErrorForInspectOrDelete(t *testing.T) {
	var captured capturedRequest
	client := newWireClient(t, http.StatusNotFound, `{"message":"not found"}`, &captured)

	_, found, err := client.InspectPod(context.Background(), "missing")
	require.NoError(t, err)
	require.False(t, found)
	_, found, err = client.InspectSecret(context.Background(), "missing")
	require.NoError(t, err)
	require.False(t, found)
	require.NoError(t, client.DeletePod(context.Background(), "missing"))
	require.NoError(t, client.DeleteSecret(context.Background(), "missing"))
}
