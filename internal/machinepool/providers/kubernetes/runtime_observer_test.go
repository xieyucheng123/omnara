package kubernetes

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/omnara-ai/omnara/internal/machinepool/providers"
	"github.com/stretchr/testify/require"
)

func runtimeTarget(t *testing.T, installationID, machineID uuid.UUID) providers.RuntimeTarget {
	t.Helper()
	name, _, err := machineIdentity(installationID, machineID)
	if err != nil {
		panic(err)
	}
	return providers.RuntimeTarget{
		InstallationID:      installationID,
		MachineID:           machineID,
		ProviderResourceID:  name,
		MachineProvisioning: testMachineProvisioning(t, testImage, 2, 4096),
	}
}

func setPodPhase(api *fakeAPI, name, phase string) {
	pod := api.pods[name]
	pod.Status.Phase = phase
	api.pods[name] = pod
}

func TestKubernetesRuntimeStateMapping(t *testing.T) {
	for phase, want := range map[string]providers.RuntimeState{
		"Pending":   providers.RuntimeStateTransitional,
		"Running":   providers.RuntimeStateRunning,
		"Succeeded": providers.RuntimeStateTerminated,
		"Failed":    providers.RuntimeStateTerminated,
		"Unknown":   providers.RuntimeStateUnknown,
		"":          providers.RuntimeStateUnknown,
	} {
		api := newFakeAPI()
		installationID, machineID := uuid.New(), uuid.New()
		name, labels, err := machineIdentity(installationID, machineID)
		require.NoError(t, err)
		api.pods[name] = podObject{Metadata: objectMeta{Name: name, Labels: labels}, Status: podStatus{Phase: phase}}
		observation, err := newTestProvider(api).ObserveRuntimeState(
			context.Background(), runtimeTarget(t, installationID, machineID),
		)
		require.NoError(t, err)
		require.Equal(t, want, observation.State, "phase %q", phase)
	}
}

func TestKubernetesRuntimeMissingPodIsTerminated(t *testing.T) {
	api := newFakeAPI()
	installationID, machineID := uuid.New(), uuid.New()
	observation, err := newTestProvider(api).ObserveRuntimeState(
		context.Background(), runtimeTarget(t, installationID, machineID),
	)
	require.NoError(t, err)
	require.Equal(t, providers.RuntimeStateTerminated, observation.State)
}

func TestKubernetesRuntimeTransportFailureIsNotTerminated(t *testing.T) {
	api := newFakeAPI()
	api.inspectPodErr = errTestTransport
	installationID, machineID := uuid.New(), uuid.New()
	_, err := newTestProvider(api).ObserveRuntimeState(
		context.Background(), runtimeTarget(t, installationID, machineID),
	)
	require.Error(t, err)
}

func TestKubernetesRuntimeBulkObservation(t *testing.T) {
	api := newFakeAPI()
	installationID, machineID := uuid.New(), uuid.New()
	name, labels, err := machineIdentity(installationID, machineID)
	require.NoError(t, err)
	api.pods[name] = podObject{
		Metadata: objectMeta{Name: name, Labels: labels},
		Status:   podStatus{Phase: "Running"},
	}
	otherID := uuid.New()
	otherName, otherLabels, err := machineIdentity(installationID, otherID)
	require.NoError(t, err)
	api.pods[otherName] = podObject{
		Metadata: objectMeta{Name: otherName, Labels: otherLabels},
		Status:   podStatus{Phase: "Pending"},
	}
	observations, err := newTestProvider(api).ObserveRuntimeStates(context.Background(), []providers.RuntimeTarget{
		runtimeTarget(t, installationID, machineID),
		runtimeTarget(t, installationID, otherID),
	})
	require.NoError(t, err)
	require.Len(t, observations, 2)
	require.Equal(t, providers.RuntimeStateRunning, observations[0].State)
	require.Equal(t, providers.RuntimeStateTransitional, observations[1].State)
}

func TestKubernetesPendingFailureDetectsSchedulerAndImageReasons(t *testing.T) {
	message, failed := pendingFailure(podObject{Status: podStatus{Conditions: []podCondition{{
		Type: "PodScheduled", Status: "False", Message: "0/3 nodes are available",
	}}}})
	require.True(t, failed)
	require.Contains(t, message, "0/3 nodes are available")

	message, failed = pendingFailure(podObject{Status: podStatus{ContainerStatuses: []containerStatus{{
		State: containerState{Waiting: &containerWaitingState{Reason: "ImagePullBackOff", Message: "pull failed"}},
	}}}})
	require.True(t, failed)
	require.Contains(t, message, "ImagePullBackOff")

	_, failed = pendingFailure(podObject{Status: podStatus{Phase: "Running"}})
	require.False(t, failed)
}
