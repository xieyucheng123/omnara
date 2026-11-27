package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/omnara-ai/omnara/internal/machinepool/providers"
	"github.com/omnara-ai/omnara/internal/storage/executionstore"
)

type fakeAPI struct {
	pods    map[string]podObject
	secrets map[string]secretObject

	checkConnectionErr error
	createSecretErr    error
	putSecretErr       error
	createPodErr       error
	losePodCreate      bool
	deletePodErr       error
	deleteSecretErr    error
	inspectPodErr      error
	listPodsErr        error

	podCreates    []podObject
	secretCreates []secretObject
	lastPutSecret secretObject
	listCalls     int
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{pods: map[string]podObject{}, secrets: map[string]secretObject{}}
}

func (f *fakeAPI) CheckConnection(context.Context) error {
	if f.checkConnectionErr != nil {
		return f.checkConnectionErr
	}
	return nil
}

func transportStatusError(status int, message string) error {
	return providers.WithResponse(
		apiError{StatusCode: status, Message: message},
		providers.HTTPResponse{StatusCode: status},
	)
}

func (f *fakeAPI) CreateSecret(_ context.Context, secret secretObject) error {
	if f.createSecretErr != nil {
		return f.createSecretErr
	}
	name := secret.Metadata.Name
	if _, exists := f.secrets[name]; exists {
		return apiError{StatusCode: http.StatusConflict}
	}
	f.secretCreates = append(f.secretCreates, secret)
	// Emulate the API server: stringData is write-only and comes back as data.
	stored := secret
	stored.Data = secret.StringData
	stored.StringData = nil
	f.secrets[name] = stored
	return nil
}

func (f *fakeAPI) InspectSecret(_ context.Context, name string) (secretObject, bool, error) {
	secret, found := f.secrets[name]
	return secret, found, nil
}

func (f *fakeAPI) PutSecretOwner(_ context.Context, secret secretObject, owner ownerReference) error {
	if f.putSecretErr != nil {
		return f.putSecretErr
	}
	name := secret.Metadata.Name
	existing, found := f.secrets[name]
	if !found {
		return apiError{StatusCode: http.StatusNotFound}
	}
	existing.Metadata.OwnerReferences = []ownerReference{owner}
	existing.Metadata.ResourceVersion = secret.Metadata.ResourceVersion
	// The API server replaces data with the PUT payload, so a read-modify-write
	// that forgets Data must visibly wipe the Secret here.
	existing.Data = secret.Data
	existing.StringData = secret.StringData
	f.lastPutSecret = secret
	f.secrets[name] = existing
	return nil
}

func (f *fakeAPI) DeleteSecret(_ context.Context, name string) error {
	if f.deleteSecretErr != nil {
		err := f.deleteSecretErr
		f.deleteSecretErr = nil
		return err
	}
	delete(f.secrets, name)
	return nil
}

func (f *fakeAPI) CreatePod(_ context.Context, pod podObject) (podObject, error) {
	if f.createPodErr != nil {
		return podObject{}, f.createPodErr
	}
	name := pod.Metadata.Name
	if _, exists := f.pods[name]; exists {
		return podObject{}, apiError{StatusCode: http.StatusConflict}
	}
	f.podCreates = append(f.podCreates, pod)
	pod.Metadata.UID = "uid-" + name
	stored := podObject{
		Metadata: objectMeta{Name: name, Labels: pod.Metadata.Labels, UID: pod.Metadata.UID},
		Spec:     pod.Spec,
		Status:   podStatus{Phase: "Pending"},
	}
	f.pods[name] = stored
	if f.losePodCreate {
		f.losePodCreate = false
		return stored, context.DeadlineExceeded
	}
	return stored, nil
}

func (f *fakeAPI) InspectPod(_ context.Context, name string) (podObject, bool, error) {
	if f.inspectPodErr != nil {
		return podObject{}, false, f.inspectPodErr
	}
	pod, found := f.pods[name]
	return pod, found, nil
}

func (f *fakeAPI) DeletePod(_ context.Context, name string) error {
	if f.deletePodErr != nil {
		err := f.deletePodErr
		f.deletePodErr = nil
		return err
	}
	delete(f.pods, name)
	return nil
}

func (f *fakeAPI) ListPods(_ context.Context, _ string) ([]podObject, error) {
	if f.listPodsErr != nil {
		return nil, f.listPodsErr
	}
	f.listCalls++
	var listed []podObject
	for _, pod := range f.pods {
		listed = append(listed, pod)
	}
	return listed, nil
}

func newTestProvider(api apiClient) *provider {
	return &provider{api: api, namespace: "test-ns", omnaraAPIURL: "https://api.omnara.test/v1", startupObservationWindow: 0}
}

func testMachineProvisioning(t *testing.T, image string, cpu, memoryMB int) executionstore.MachineProvisioningConfig {
	t.Helper()
	options := map[string]json.RawMessage{}
	raw, err := json.Marshal(image)
	if err != nil {
		t.Fatal(err)
	}
	options["image"] = raw
	return executionstore.MachineProvisioningConfig{CPU: &cpu, MemoryMB: &memoryMB, ProviderOptions: options}
}

var errTestTransport = errors.New("test transport failure")
