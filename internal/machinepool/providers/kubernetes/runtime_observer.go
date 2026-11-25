package kubernetes

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/omnara-ai/omnara/internal/machinepool/providers"
)

var _ providers.RuntimeStateObserver = (*provider)(nil)

func (p *provider) ObserveRuntimeStates(
	ctx context.Context,
	targets []providers.RuntimeTarget,
) ([]providers.RuntimeObservation, error) {
	observations := make([]providers.RuntimeObservation, len(targets))
	resourceCounts := make(map[string]int, len(targets))
	machineCounts := make(map[uuid.UUID]int, len(targets))
	owners := make(map[string]struct{}, 1)
	for index, target := range targets {
		observations[index] = target.UnknownObservation()
		resourceCounts[target.ProviderResourceID]++
		machineCounts[target.MachineID]++
		if _, labels, err := machineIdentity(target.InstallationID, target.MachineID); err == nil {
			owners[labels[installationLabel]] = struct{}{}
		}
	}
	listed := make(map[string][]podObject)
	for owner := range owners {
		pods, err := p.api.ListPods(ctx, installationLabel+"="+owner)
		if err != nil {
			return nil, fmt.Errorf("list kubernetes pods for runtime observation: %w", err)
		}
		for _, pod := range pods {
			listed[pod.Metadata.Name] = append(listed[pod.Metadata.Name], pod)
		}
	}
	for index, target := range targets {
		if resourceCounts[target.ProviderResourceID] != 1 || machineCounts[target.MachineID] != 1 {
			continue
		}
		name, labels, err := machineIdentity(target.InstallationID, target.MachineID)
		if err != nil || target.ProviderResourceID != name {
			continue
		}
		matches := listed[name]
		switch {
		case len(matches) == 0:
			observations[index].State = providers.RuntimeStateTerminated
		case len(matches) == 1 && ownedBy(matches[0].Metadata.Labels, labels):
			observations[index].State = runtimeState(matches[0])
		}
	}
	return observations, nil
}

func (p *provider) ObserveRuntimeState(
	ctx context.Context,
	target providers.RuntimeTarget,
) (providers.RuntimeObservation, error) {
	observation := target.UnknownObservation()
	name, labels, err := machineIdentity(target.InstallationID, target.MachineID)
	if err != nil {
		return observation, err
	}
	if target.ProviderResourceID != name {
		return observation, nil
	}
	current, found, err := p.api.InspectPod(ctx, name)
	if err != nil {
		return observation, fmt.Errorf("inspect kubernetes pod %q for runtime observation: %w", name, err)
	}
	if !found {
		observation.State = providers.RuntimeStateTerminated
		return observation, nil
	}
	if ownedBy(current.Metadata.Labels, labels) {
		observation.State = runtimeState(current)
	}
	return observation, nil
}

func runtimeState(pod podObject) providers.RuntimeState {
	// Pods that have not reached a terminal phase keep their machine slot:
	// Pending covers scheduling, image pulls, and container creation.
	switch pod.Status.Phase {
	case "Running":
		// A pod is Running as soon as any container starts; the daemon is
		// the only container, so Running means the machine is up.
		if containerFailed(pod) {
			return providers.RuntimeStateTerminated
		}
		return providers.RuntimeStateRunning
	case "Pending":
		return providers.RuntimeStateTransitional
	case "Succeeded", "Failed":
		return providers.RuntimeStateTerminated
	default:
		return providers.RuntimeStateUnknown
	}
}

func containerFailed(pod podObject) bool {
	for _, container := range pod.Status.ContainerStatuses {
		if terminated := container.State.Terminated; terminated != nil && terminated.ExitCode != 0 {
			return true
		}
	}
	return false
}
