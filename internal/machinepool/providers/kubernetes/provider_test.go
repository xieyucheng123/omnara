package kubernetes

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/omnara-ai/omnara/internal/machinepool/providers"
	"github.com/omnara-ai/omnara/internal/storage/executionstore"
	"github.com/stretchr/testify/require"
)

const testImage = "buildpack-deps:24.04-curl"

func provision(
	t *testing.T,
	api *fakeAPI,
	installationID, machineID uuid.UUID,
) (providers.ProvisionMachineResult, error) {
	t.Helper()
	return newTestProvider(api).ProvisionMachine(
		context.Background(),
		installationID,
		machineID,
		testMachineProvisioning(t, testImage, 2, 4096),
		"machine-token",
		map[string]string{"APP_ENV": "test"},
		true,
	)
}

func TestKubernetesProvisionCreatesPodWithSecretAndHardenedSpec(t *testing.T) {
	api := newFakeAPI()
	installationID, machineID := uuid.New(), uuid.New()
	result, err := provision(t, api, installationID, machineID)
	require.NoError(t, err)
	name, labels, err := machineIdentity(installationID, machineID)
	require.NoError(t, err)
	require.Equal(t, name, result.ProviderResourceID)

	require.Len(t, api.secretCreates, 1)
	secret := api.secretCreates[0]
	require.Equal(t, name+"-env", secret.Metadata.Name)
	require.Equal(t, labels, secret.Metadata.Labels)
	require.Equal(t, "machine-token", secret.StringData["OMNARA_MACHINE_TOKEN"])
	require.Equal(t, "test", secret.StringData["APP_ENV"])
	require.Contains(t, secret.StringData, providers.ManagedBootstrapScriptEnvVar)

	require.Len(t, api.podCreates, 1)
	pod := api.podCreates[0]
	require.Equal(t, name, pod.Metadata.Name)
	require.Equal(t, labels, pod.Metadata.Labels)
	require.Nil(t, pod.Spec.RuntimeClassName)
	require.Equal(t, "Never", pod.Spec.RestartPolicy)
	require.NotNil(t, pod.Spec.ShareProcessNamespace)
	require.True(t, *pod.Spec.ShareProcessNamespace)
	require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
	require.False(t, *pod.Spec.AutomountServiceAccountToken)
	require.Equal(t, seccompRuntimeDefault, pod.Spec.SecurityContext.SeccompProfile.Type)

	require.Len(t, pod.Spec.Containers, 1)
	container := pod.Spec.Containers[0]
	require.Equal(t, testImage, container.Image)
	require.Equal(t, providers.ManagedDaemonLauncherArgs(), container.Command)
	require.Len(t, container.EnvFrom, 1)
	require.Equal(t, name+"-env", container.EnvFrom[0].SecretRef.Name)
	require.Equal(t, map[string]string{"cpu": "2000m", "memory": "4096Mi"}, container.Resources.Requests)
	require.Equal(t, container.Resources.Requests, container.Resources.Limits)
	require.Equal(t, []string{"NET_RAW"}, container.SecurityContext.Capabilities.Drop)
	require.Equal(t, seccompRuntimeDefault, container.SecurityContext.SeccompProfile.Type)

	stored, found, err := api.InspectSecret(context.Background(), name+"-env")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, stored.Metadata.OwnerReferences, 1)
	// The API server only returns data, never stringData: adopting the Secret
	// must not wipe the machine environment.
	require.Equal(t, "machine-token", stored.Data["OMNARA_MACHINE_TOKEN"])
	require.Equal(t, "test", stored.Data["APP_ENV"])
	// The owner-adoption PUT must carry the data back, not just the owner.
	require.Equal(t, "machine-token", api.lastPutSecret.Data["OMNARA_MACHINE_TOKEN"])
	require.Equal(t, "test", api.lastPutSecret.Data["APP_ENV"])
	require.Equal(t, "Pod", stored.Metadata.OwnerReferences[0].Kind)
	require.Equal(t, "uid-"+name, stored.Metadata.OwnerReferences[0].UID)
}

func TestKubernetesProvisionSetsRuntimeClassName(t *testing.T) {
	api := newFakeAPI()
	machineProvider := newTestProvider(api)
	machineProvider.runtimeClassName = "gvisor"
	installationID, machineID := uuid.New(), uuid.New()
	_, err := machineProvider.ProvisionMachine(
		context.Background(), installationID, machineID,
		testMachineProvisioning(t, testImage, 2, 4096), "machine-token", nil, true,
	)
	require.NoError(t, err)
	require.Len(t, api.podCreates, 1)
	require.NotNil(t, api.podCreates[0].Spec.RuntimeClassName)
	require.Equal(t, "gvisor", *api.podCreates[0].Spec.RuntimeClassName)
}

func TestKubernetesProvisionAdoptsResourcesAfterLostCreateResponse(t *testing.T) {
	api := newFakeAPI()
	api.losePodCreate = true
	installationID, machineID := uuid.New(), uuid.New()
	_, err := provision(t, api, installationID, machineID)
	require.Error(t, err)

	result, err := provision(t, api, installationID, machineID)
	require.NoError(t, err)
	name, _, err := machineIdentity(installationID, machineID)
	require.NoError(t, err)
	require.Equal(t, name, result.ProviderResourceID)
	require.Len(t, api.pods, 1)
	require.Len(t, api.secrets, 1)
}

func TestKubernetesProvisionReplacesStoppedPod(t *testing.T) {
	api := newFakeAPI()
	installationID, machineID := uuid.New(), uuid.New()
	_, err := provision(t, api, installationID, machineID)
	require.NoError(t, err)
	name, _, err := machineIdentity(installationID, machineID)
	require.NoError(t, err)
	stopped := api.pods[name]
	stopped.Status.Phase = "Failed"
	api.pods[name] = stopped

	_, err = provision(t, api, installationID, machineID)
	require.ErrorIs(t, err, providers.ErrResourceReplaced)
	require.NotContains(t, api.pods, name)
	require.NotContains(t, api.secrets, name+"-env")

	_, err = provision(t, api, installationID, machineID)
	require.NoError(t, err)
	require.Contains(t, api.pods, name)
}

func TestKubernetesProvisionFailsFastOnSettingsKubernetesRejects(t *testing.T) {
	api := newFakeAPI()
	api.createPodErr = apiError{StatusCode: http.StatusBadRequest, Message: "invalid runtime class name: runsc"}
	_, err := provision(t, api, uuid.New(), uuid.New())
	require.ErrorIs(t, err, providers.ErrPermanent)
	require.ErrorContains(t, err, "invalid runtime class name")
	require.Empty(t, api.pods)
}

func TestKubernetesProvisionNeverAdoptsPodWithDroppedLimits(t *testing.T) {
	api := newFakeAPI()
	api.losePodCreate = true
	installationID, machineID := uuid.New(), uuid.New()
	_, err := provision(t, api, installationID, machineID)
	require.Error(t, err)

	name, _, err := machineIdentity(installationID, machineID)
	require.NoError(t, err)
	weakened := api.pods[name]
	weakened.Spec.Containers[0].Resources.Requests["cpu"] = "100m"
	api.pods[name] = weakened

	_, err = provision(t, api, installationID, machineID)
	require.ErrorIs(t, err, providers.ErrPermanent)
}

func TestKubernetesProvisionRefusesResourcesOwnedByAnotherMachine(t *testing.T) {
	api := newFakeAPI()
	installationID, machineID := uuid.New(), uuid.New()
	name, labels, err := machineIdentity(installationID, machineID)
	require.NoError(t, err)
	foreign := map[string]string{installationLabel: labels[installationLabel], machineLabel: "mch_other"}
	api.pods[name] = podObject{Metadata: objectMeta{Name: name, Labels: foreign}}

	_, err = provision(t, api, installationID, machineID)
	require.ErrorIs(t, err, errNotThisMachine)
	require.Len(t, api.podCreates, 0)
}

func TestKubernetesInspectNeverReportsMissingOnTransportFailure(t *testing.T) {
	api := newFakeAPI()
	api.inspectPodErr = errTestTransport
	_, found, err := newTestProvider(api).InspectMachine(
		context.Background(), uuid.New(), uuid.New(), testMachineProvisioning(t, testImage, 2, 4096), "",
	)
	require.Error(t, err)
	require.False(t, found)
}

func TestKubernetesCheckConnectionExplainsRejectedCredentials(t *testing.T) {
	api := newFakeAPI()
	api.checkConnectionErr = transportStatusError(http.StatusUnauthorized, "Unauthorized")
	err := newTestProvider(api).CheckConnection(context.Background(), testMachineProvisioning(t, testImage, 2, 4096))
	require.ErrorContains(t, err, "token is expired or invalid")

	api.checkConnectionErr = transportStatusError(http.StatusForbidden, "pods is forbidden")
	err = newTestProvider(api).CheckConnection(context.Background(), testMachineProvisioning(t, testImage, 2, 4096))
	require.ErrorContains(t, err, "RoleBinding")

	api.checkConnectionErr = transportStatusError(http.StatusNotFound, "not found")
	err = newTestProvider(api).CheckConnection(context.Background(), testMachineProvisioning(t, testImage, 2, 4096))
	require.ErrorContains(t, err, "namespace")
}

func TestKubernetesDeleteRemovesPodAndSecret(t *testing.T) {
	api := newFakeAPI()
	installationID, machineID := uuid.New(), uuid.New()
	result, err := provision(t, api, installationID, machineID)
	require.NoError(t, err)
	machineProvisioning := testMachineProvisioning(t, testImage, 2, 4096)
	machineProvider := newTestProvider(api)
	require.NoError(t, machineProvider.DeleteMachine(
		context.Background(), installationID, machineID, machineProvisioning, result.ProviderResourceID,
	))
	require.Empty(t, api.pods)
	require.Empty(t, api.secrets)
	require.NoError(t, machineProvider.DeleteMachine(
		context.Background(), installationID, machineID, machineProvisioning, result.ProviderResourceID,
	))
}

func pendingPodWith(
	t *testing.T, api *fakeAPI, p *provider, installationID, machineID uuid.UUID,
) (string, map[string]string, string) {
	t.Helper()
	name, labels, err := machineIdentity(installationID, machineID)
	require.NoError(t, err)
	secretName := name + secretNameSuffix
	api.secrets[secretName] = secretObject{
		Metadata: objectMeta{Name: secretName, Labels: labels},
		Data:     map[string]string{"OMNARA_MACHINE_TOKEN": "machine-token"},
	}
	limits := limitsFor(testMachineProvisioning(t, testImage, 2, 4096), testImage)
	pod := p.podObject(name, labels, secretName, testImage, limits)
	pod.Metadata.UID = "uid-" + name
	api.pods[name] = pod
	return name, labels, secretName
}

func TestKubernetesPendingPodReportsUnschedulable(t *testing.T) {
	api := newFakeAPI()
	p := newTestProvider(api)
	installationID, machineID := uuid.New(), uuid.New()
	name, labels, secretName := pendingPodWith(t, api, p, installationID, machineID)
	pod := api.pods[name]
	pod.Status = podStatus{
		Phase: "Pending",
		Conditions: []podCondition{{
			Type: "PodScheduled", Status: "False",
			Message: "0/4 nodes are available: 4 Insufficient cpu",
		}},
	}
	api.pods[name] = pod

	limits := limitsFor(testMachineProvisioning(t, testImage, 2, 4096), testImage)
	err := p.resumeExistingPod(context.Background(), name, labels, secretName, map[string]string{}, limits)
	require.ErrorContains(t, err, "unschedulable")
	require.ErrorContains(t, err, "Insufficient cpu")
	require.NotErrorIs(t, err, providers.ErrPermanent)
}

func TestKubernetesPendingPodReportsImagePullBackOff(t *testing.T) {
	api := newFakeAPI()
	p := newTestProvider(api)
	installationID, machineID := uuid.New(), uuid.New()
	name, labels, secretName := pendingPodWith(t, api, p, installationID, machineID)
	pod := api.pods[name]
	pod.Status = podStatus{
		Phase: "Pending",
		ContainerStatuses: []containerStatus{{
			Name: containerName,
			State: containerState{Waiting: &containerWaitingState{
				Reason: "ImagePullBackOff", Message: "Back-off pulling image",
			}},
		}},
	}
	api.pods[name] = pod

	limits := limitsFor(testMachineProvisioning(t, testImage, 2, 4096), testImage)
	err := p.resumeExistingPod(context.Background(), name, labels, secretName, map[string]string{}, limits)
	require.ErrorContains(t, err, "ImagePullBackOff")
	require.NotErrorIs(t, err, providers.ErrPermanent)
}

func TestKubernetesPendingPodTreatsInvalidImageAsPermanent(t *testing.T) {
	api := newFakeAPI()
	p := newTestProvider(api)
	installationID, machineID := uuid.New(), uuid.New()
	name, labels, secretName := pendingPodWith(t, api, p, installationID, machineID)
	pod := api.pods[name]
	pod.Status = podStatus{
		Phase: "Pending",
		ContainerStatuses: []containerStatus{{
			Name:  containerName,
			State: containerState{Waiting: &containerWaitingState{Reason: "InvalidImageName"}},
		}},
	}
	api.pods[name] = pod

	limits := limitsFor(testMachineProvisioning(t, testImage, 2, 4096), testImage)
	err := p.resumeExistingPod(context.Background(), name, labels, secretName, map[string]string{}, limits)
	require.ErrorIs(t, err, providers.ErrPermanent)
}

func TestKubernetesValidateMachineConfigRejectsInvalidEnvNames(t *testing.T) {
	p := newTestProvider(newFakeAPI())
	require.NoError(t, p.ValidateMachineConfig(
		executionstore.MachineProvisioningConfig{}, map[string]string{"GOOD_NAME": "1"},
	))
	err := p.ValidateMachineConfig(executionstore.MachineProvisioningConfig{}, map[string]string{"BAD.NAME": "1"})
	require.ErrorContains(t, err, "BAD.NAME")
}

func TestKubernetesWaitForPodStartupReturnsWhenRunning(t *testing.T) {
	api := newFakeAPI()
	p := newTestProvider(api)
	p.startupObservationWindow = time.Second
	installationID, machineID := uuid.New(), uuid.New()
	name, _, _ := pendingPodWith(t, api, p, installationID, machineID)
	pod := api.pods[name]
	pod.Status = podStatus{Phase: "Running"}
	api.pods[name] = pod
	require.NoError(t, p.waitForPodStartup(context.Background(), name))
}

func TestKubernetesWaitForPodStartupReportsPendingTimeout(t *testing.T) {
	api := newFakeAPI()
	p := newTestProvider(api)
	p.startupObservationWindow = 10 * time.Millisecond
	installationID, machineID := uuid.New(), uuid.New()
	name, _, _ := pendingPodWith(t, api, p, installationID, machineID)
	err := p.waitForPodStartup(context.Background(), name)
	require.ErrorContains(t, err, "did not start")
}
