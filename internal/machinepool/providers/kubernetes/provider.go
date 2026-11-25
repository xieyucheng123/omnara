package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/omnara-ai/omnara/internal/machinepool/providers"
	"github.com/omnara-ai/omnara/internal/publicid"
	"github.com/omnara-ai/omnara/internal/storage/executionstore"
)

const (
	provisioningTimeout   = 2 * time.Minute
	installationLabel     = "omnara-installation"
	machineLabel          = "omnara-machine"
	containerName         = "machine"
	secretNameSuffix      = "-env"
	seccompRuntimeDefault = "RuntimeDefault"
)

var errNotThisMachine = errors.New("kubernetes resource does not belong to this machine")

var (
	_ providers.Provider        = (*provider)(nil)
	_ providers.RuntimeProvider = (*provider)(nil)
)

type provider struct {
	api              apiClient
	namespace        string
	runtimeClassName string
	omnaraAPIURL     string
}

type machineLimits struct {
	cpu    string
	memory string
	image  string
}

func (*provider) ProvisioningTimeout() time.Duration {
	return provisioningTimeout
}

func (*provider) PrepareProvisioning(
	_ context.Context,
	machineProvisioning executionstore.MachineProvisioningConfig,
) (executionstore.MachineResourceFacts, error) {
	if _, err := providerOptionsFromProvisioning(machineProvisioning); err != nil {
		return executionstore.MachineResourceFacts{}, err
	}
	return executionstore.MachineResourceFacts{
		CPU:      machineProvisioning.CPU,
		MemoryMB: machineProvisioning.MemoryMB,
	}, nil
}

func (*provider) ValidateMachineConfig(executionstore.MachineProvisioningConfig, map[string]string) error {
	return nil
}

func (p *provider) ProvisionMachine(
	ctx context.Context,
	installationID, machineID uuid.UUID,
	machineProvisioning executionstore.MachineProvisioningConfig,
	machineToken string,
	machineEnv map[string]string,
	_ bool,
) (providers.ProvisionMachineResult, error) {
	options, err := providerOptionsFromProvisioning(machineProvisioning)
	if err != nil {
		return providers.ProvisionMachineResult{}, err
	}
	name, labels, err := machineIdentity(installationID, machineID)
	if err != nil {
		return providers.ProvisionMachineResult{}, err
	}
	env, err := providers.BuildManagedMachineEnv(p.omnaraAPIURL, machineToken, options.StartupScript, machineEnv)
	if err != nil {
		return providers.ProvisionMachineResult{}, err
	}
	env[providers.ManagedBootstrapScriptEnvVar] = providers.ManagedBootScriptPayload()
	secretName := name + secretNameSuffix
	result := providers.ProvisionMachineResult{ProviderResourceID: name}
	limits := limitsFor(machineProvisioning, options.Image)
	if err := p.ensureSecret(ctx, secretName, labels, env); err != nil {
		return result, err
	}
	created, err := p.api.CreatePod(ctx, p.podObject(name, labels, secretName, options.Image, limits))
	switch {
	case err == nil:
		return result, p.ownSecret(ctx, secretName, labels, created.Metadata.UID)
	case isConflict(err):
		return result, p.resumeExistingPod(ctx, name, labels, secretName, env, limits)
	case isPermanentStatus(err):
		return result, fmt.Errorf("%w: %w", err, providers.ErrPermanent)
	default:
		return result, err
	}
}

func (p *provider) ensureSecret(
	ctx context.Context,
	secretName string,
	labels map[string]string,
	env map[string]string,
) error {
	err := p.api.CreateSecret(ctx, secretObject{
		Metadata:   objectMeta{Name: secretName, Labels: labels},
		StringData: env,
	})
	if err == nil {
		return nil
	}
	if !isConflict(err) {
		return fmt.Errorf("create kubernetes secret %q: %w", secretName, err)
	}
	existing, found, inspectErr := p.api.InspectSecret(ctx, secretName)
	if inspectErr != nil {
		return fmt.Errorf("inspect kubernetes secret %q: %w", secretName, inspectErr)
	}
	if !found {
		return fmt.Errorf("kubernetes secret %q conflicted but was not found", secretName)
	}
	if !ownedBy(existing.Metadata.Labels, labels) {
		return fmt.Errorf("%w: secret %s", errSecretNotMachine, secretName)
	}
	return nil
}

func (p *provider) ownSecret(
	ctx context.Context,
	secretName string,
	labels map[string]string,
	podUID string,
) error {
	if podUID == "" {
		return errors.New("kubernetes pod response is missing its uid")
	}
	existing, found, err := p.api.InspectSecret(ctx, secretName)
	if err != nil {
		return fmt.Errorf("inspect kubernetes secret %q: %w", secretName, err)
	}
	if !found {
		return fmt.Errorf("kubernetes secret %q was not found after pod creation", secretName)
	}
	if !ownedBy(existing.Metadata.Labels, labels) {
		return fmt.Errorf("%w: secret %s", errSecretNotMachine, secretName)
	}
	if slices.ContainsFunc(existing.Metadata.OwnerReferences, func(owner ownerReference) bool {
		return owner.Kind == "Pod" && owner.UID == podUID
	}) {
		return nil
	}
	owner := ownerReference{APIVersion: "v1", Kind: "Pod", Name: existingPodName(secretName), UID: podUID}
	if err := p.api.PutSecretOwner(ctx, existing, owner); err != nil {
		return fmt.Errorf("adopt kubernetes secret %q: %w", secretName, err)
	}
	return nil
}

func existingPodName(secretName string) string {
	return strings.TrimSuffix(secretName, secretNameSuffix)
}

func (p *provider) resumeExistingPod(
	ctx context.Context,
	name string,
	labels map[string]string,
	secretName string,
	env map[string]string,
	limits machineLimits,
) error {
	existing, found, err := p.api.InspectPod(ctx, name)
	if err != nil {
		return fmt.Errorf("inspect kubernetes pod %q: %w", name, err)
	}
	if !found {
		return fmt.Errorf("kubernetes pod %q conflicted but was not found", name)
	}
	if !ownedBy(existing.Metadata.Labels, labels) {
		return fmt.Errorf("%w: pod %s", errNotThisMachine, name)
	}
	if got := limitsOf(existing); got != limits {
		return fmt.Errorf(
			"kubernetes pod %q does not have the machine's resource limits: %w",
			name,
			providers.ErrPermanent,
		)
	}
	if err := p.ensureSecret(ctx, secretName, labels, env); err != nil {
		return err
	}
	if err := p.ownSecret(ctx, secretName, labels, existing.Metadata.UID); err != nil {
		return err
	}
	switch existing.Status.Phase {
	case "Pending", "Running":
		return nil
	case "Succeeded", "Failed":
		if err := p.deleteOwnedPod(ctx, name, labels); err != nil {
			return err
		}
		if err := p.deleteOwnedSecret(ctx, secretName, labels); err != nil {
			return err
		}
		return fmt.Errorf(
			"kubernetes pod %q stopped and was removed; provisioning must be retried: %w",
			name,
			providers.ErrResourceReplaced,
		)
	default:
		return fmt.Errorf("kubernetes pod %q is %s", name, podDetail(existing))
	}
}

func (p *provider) podObject(
	name string,
	labels map[string]string,
	secretName, image string,
	limits machineLimits,
) podObject {
	shareProcessNamespace := true
	automountServiceAccountToken := false
	pod := podObject{
		Metadata: objectMeta{Name: name, Labels: labels},
		Spec: podSpec{
			RestartPolicy:                "Never",
			ShareProcessNamespace:        &shareProcessNamespace,
			AutomountServiceAccountToken: &automountServiceAccountToken,
			SecurityContext: &podSecurityContext{
				SeccompProfile: &seccompProfile{Type: seccompRuntimeDefault},
			},
			Containers: []containerSpec{{
				Name:    containerName,
				Image:   image,
				Command: providers.ManagedDaemonLauncherArgs(),
				EnvFrom: []envFromSource{{SecretRef: &secretRef{Name: secretName}}},
				Resources: resourceRequirements{
					Limits:   map[string]string{"cpu": limits.cpu, "memory": limits.memory},
					Requests: map[string]string{"cpu": limits.cpu, "memory": limits.memory},
				},
				SecurityContext: &containerSecurityContext{
					Capabilities:   &linuxCapabilities{Drop: []string{"NET_RAW"}},
					SeccompProfile: &seccompProfile{Type: seccompRuntimeDefault},
				},
			}},
		},
	}
	if p.runtimeClassName != "" {
		pod.Spec.RuntimeClassName = &p.runtimeClassName
	}
	return pod
}

func limitsFor(machineProvisioning executionstore.MachineProvisioningConfig, image string) machineLimits {
	return machineLimits{
		cpu:    fmt.Sprintf("%dm", (*machineProvisioning.CPU)*1000),
		memory: fmt.Sprintf("%dMi", *machineProvisioning.MemoryMB),
		image:  image,
	}
}

func limitsOf(pod podObject) machineLimits {
	limits := machineLimits{}
	if len(pod.Spec.Containers) == 0 {
		return limits
	}
	container := pod.Spec.Containers[0]
	limits.image = container.Image
	limits.cpu = container.Resources.Requests["cpu"]
	limits.memory = container.Resources.Requests["memory"]
	return limits
}

func (p *provider) InspectMachine(
	ctx context.Context,
	installationID, machineID uuid.UUID,
	_ executionstore.MachineProvisioningConfig,
	providerResourceID string,
) (string, bool, error) {
	name, labels, err := machineIdentity(installationID, machineID)
	if err != nil {
		return "", false, err
	}
	if providerResourceID != "" && providerResourceID != name {
		return "", false, fmt.Errorf("%w: %s", errNotThisMachine, providerResourceID)
	}
	existing, found, err := p.api.InspectPod(ctx, name)
	if err != nil {
		return "", false, err
	}
	if !found {
		return "", false, nil
	}
	if !ownedBy(existing.Metadata.Labels, labels) {
		return "", false, fmt.Errorf("%w: pod %s", errNotThisMachine, name)
	}
	return name, true, nil
}

func (p *provider) CheckConnection(ctx context.Context, _ executionstore.MachineProvisioningConfig) error {
	err := p.api.CheckConnection(ctx)
	if err == nil {
		return nil
	}
	if status, ok := providers.HTTPStatus(err); ok &&
		(status == http.StatusUnauthorized || status == http.StatusForbidden) {
		return fmt.Errorf(
			"kubernetes credentials were rejected (HTTP %d): the ServiceAccount bearer token is expired or invalid: %w",
			status,
			err,
		)
	}
	if isNotFound(err) {
		return fmt.Errorf("kubernetes namespace %q was not found: %w", p.namespace, err)
	}
	return err
}

func (p *provider) DeleteMachine(
	ctx context.Context,
	installationID, machineID uuid.UUID,
	_ executionstore.MachineProvisioningConfig,
	providerResourceID string,
) error {
	if providerResourceID == "" {
		return errors.New("provider resource id is required")
	}
	name, labels, err := machineIdentity(installationID, machineID)
	if err != nil {
		return err
	}
	if providerResourceID != name {
		return fmt.Errorf("%w: %s", errNotThisMachine, providerResourceID)
	}
	existing, found, err := p.api.InspectPod(ctx, name)
	if err != nil {
		return err
	}
	if found {
		if !ownedBy(existing.Metadata.Labels, labels) {
			return fmt.Errorf("%w: pod %s", errNotThisMachine, name)
		}
		if err := p.api.DeletePod(ctx, name); err != nil {
			return err
		}
	}
	return p.deleteOwnedSecret(ctx, name+secretNameSuffix, labels)
}

func (p *provider) deleteOwnedPod(ctx context.Context, name string, labels map[string]string) error {
	existing, found, err := p.api.InspectPod(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if !ownedBy(existing.Metadata.Labels, labels) {
		return fmt.Errorf("%w: pod %s", errNotThisMachine, name)
	}
	return p.api.DeletePod(ctx, name)
}

func (p *provider) deleteOwnedSecret(ctx context.Context, secretName string, labels map[string]string) error {
	existing, found, err := p.api.InspectSecret(ctx, secretName)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if !ownedBy(existing.Metadata.Labels, labels) {
		return fmt.Errorf("%w: secret %s", errSecretNotMachine, secretName)
	}
	return p.api.DeleteSecret(ctx, secretName)
}

func machineIdentity(installationID, machineID uuid.UUID) (string, map[string]string, error) {
	name, err := providers.MachineAllocationName(installationID, machineID)
	if err != nil {
		return "", nil, err
	}
	installationOwner, err := publicid.Encode(publicid.KindInstallation, installationID)
	if err != nil {
		return "", nil, err
	}
	machineOwner, err := publicid.Encode(publicid.KindMachine, machineID)
	if err != nil {
		return "", nil, err
	}
	return name, map[string]string{installationLabel: installationOwner, machineLabel: machineOwner}, nil
}

func ownedBy(actual, expected map[string]string) bool {
	return actual[installationLabel] == expected[installationLabel] &&
		actual[machineLabel] == expected[machineLabel]
}

// podDetail renders a one-line human summary of a pod that is neither
// Pending nor Running, favoring scheduler and image-pull reasons so setup
// mistakes surface instead of a bare phase.
func podDetail(pod podObject) string {
	if message := pendingMessage(pod); message != "" {
		return message
	}
	return "phase " + pod.Status.Phase
}

func pendingMessage(pod podObject) string {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == "PodScheduled" && condition.Status == "False" && condition.Message != "" {
			return "unschedulable: " + condition.Message
		}
	}
	for _, container := range pod.Status.ContainerStatuses {
		if waiting := container.State.Waiting; waiting != nil {
			switch waiting.Reason {
			case "ContainerCreating", "PodInitializing", "ImagePullBackOff", "ErrImagePull",
				"CreateContainerConfigError", "CreateContainerError", "InvalidImageName":
				if waiting.Message != "" {
					return waiting.Reason + ": " + waiting.Message
				}
				return waiting.Reason
			}
		}
		if terminated := container.State.Terminated; terminated != nil && terminated.Message != "" {
			return terminated.Reason + ": " + terminated.Message
		}
	}
	return ""
}
