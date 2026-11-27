// Package kubernetes implements a self-hosted Kubernetes machine pool
// provider: one Pod per machine in a caller-chosen namespace, worked on
// through a small REST client (no client-go). It mirrors the docker provider
// but targets a remote cluster over HTTPS with a ServiceAccount bearer token,
// never kubeconfig files or in-cluster credentials.
package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/omnara-ai/omnara/internal/machinepool/providers"
)

type apiClient interface {
	CheckConnection(context.Context) error
	CreateSecret(context.Context, secretObject) error
	InspectSecret(context.Context, string) (secretObject, bool, error)
	PutSecretOwner(context.Context, secretObject, ownerReference) error
	DeleteSecret(context.Context, string) error
	CreatePod(context.Context, podObject) (podObject, error)
	InspectPod(context.Context, string) (podObject, bool, error)
	DeletePod(context.Context, string) error
	ListPods(context.Context, string) ([]podObject, error)
}

type objectMeta struct {
	Name            string            `json:"name,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	OwnerReferences []ownerReference  `json:"ownerReferences,omitempty"`
}

type ownerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

type secretObject struct {
	Metadata objectMeta `json:"metadata"`
	// StringData is write-only: the API server stores it as Data and never
	// returns it. Data must be modeled too, otherwise a read-modify-write
	// cycle (adopting the Secret's owner) would wipe the Secret's content.
	StringData map[string]string `json:"stringData,omitempty"`
	Data       map[string]string `json:"data,omitempty"`
}

type podObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     podSpec    `json:"spec,omitempty"`
	Status   podStatus  `json:"status,omitempty"`
}

type podSpec struct {
	RuntimeClassName             *string             `json:"runtimeClassName,omitempty"`
	RestartPolicy                string              `json:"restartPolicy,omitempty"`
	ShareProcessNamespace        *bool               `json:"shareProcessNamespace,omitempty"`
	AutomountServiceAccountToken *bool               `json:"automountServiceAccountToken,omitempty"`
	SecurityContext              *podSecurityContext `json:"securityContext,omitempty"`
	Containers                   []containerSpec     `json:"containers,omitempty"`
}

type podSecurityContext struct {
	SeccompProfile *seccompProfile `json:"seccompProfile,omitempty"`
}

type seccompProfile struct {
	Type string `json:"type"`
}

type containerSpec struct {
	Name            string                    `json:"name"`
	Image           string                    `json:"image"`
	Command         []string                  `json:"command,omitempty"`
	EnvFrom         []envFromSource           `json:"envFrom,omitempty"`
	Resources       resourceRequirements      `json:"resources,omitempty"`
	SecurityContext *containerSecurityContext `json:"securityContext,omitempty"`
}

type envFromSource struct {
	SecretRef *secretRef `json:"secretRef,omitempty"`
}

type secretRef struct {
	Name string `json:"name"`
}

type resourceRequirements struct {
	Limits   map[string]string `json:"limits,omitempty"`
	Requests map[string]string `json:"requests,omitempty"`
}

type linuxCapabilities struct {
	Drop []string `json:"drop,omitempty"`
}

type containerSecurityContext struct {
	Capabilities   *linuxCapabilities `json:"capabilities,omitempty"`
	SeccompProfile *seccompProfile    `json:"seccompProfile,omitempty"`
}

type podStatus struct {
	Phase             string            `json:"phase,omitempty"`
	Conditions        []podCondition    `json:"conditions,omitempty"`
	ContainerStatuses []containerStatus `json:"containerStatuses,omitempty"`
}

type podCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type containerStatus struct {
	Name  string         `json:"name"`
	State containerState `json:"state"`
}

type containerState struct {
	Waiting    *containerWaitingState    `json:"waiting,omitempty"`
	Running    *containerRunningState    `json:"running,omitempty"`
	Terminated *containerTerminatedState `json:"terminated,omitempty"`
}

type containerWaitingState struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type containerRunningState struct{}

type containerTerminatedState struct {
	ExitCode int    `json:"exitCode"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
}

type podList struct {
	Items []podObject `json:"items"`
}

type restClient struct {
	baseURL    string
	namespace  string
	token      string
	httpClient *http.Client
}

func (c *restClient) namespacePath(path string) string {
	return "/api/v1/namespaces/" + c.namespace + path
}

func (c *restClient) CheckConnection(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, c.namespacePath("/pods"), url.Values{"limit": {"1"}}, nil, nil)
}

func (c *restClient) CreateSecret(ctx context.Context, secret secretObject) error {
	return c.do(ctx, http.MethodPost, c.namespacePath("/secrets"), nil, secret, nil)
}

func (c *restClient) InspectSecret(ctx context.Context, name string) (secretObject, bool, error) {
	var response secretObject
	err := c.do(ctx, http.MethodGet, c.namespacePath("/secrets/"+url.PathEscape(name)), nil, nil, &response)
	if isNotFound(err) {
		return secretObject{}, false, nil
	}
	return response, err == nil, err
}

func (c *restClient) PutSecretOwner(ctx context.Context, secret secretObject, owner ownerReference) error {
	secret.Metadata.OwnerReferences = []ownerReference{owner}
	return c.do(ctx, http.MethodPut, c.namespacePath("/secrets/"+url.PathEscape(secret.Metadata.Name)), nil, secret, nil)
}

func (c *restClient) DeleteSecret(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, c.namespacePath("/secrets/"+url.PathEscape(name)), nil, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

func (c *restClient) CreatePod(ctx context.Context, pod podObject) (podObject, error) {
	var response podObject
	err := c.do(ctx, http.MethodPost, c.namespacePath("/pods"), nil, pod, &response)
	return response, err
}

func (c *restClient) InspectPod(ctx context.Context, name string) (podObject, bool, error) {
	var response podObject
	err := c.do(ctx, http.MethodGet, c.namespacePath("/pods/"+url.PathEscape(name)), nil, nil, &response)
	if isNotFound(err) {
		return podObject{}, false, nil
	}
	return response, err == nil, err
}

func (c *restClient) DeletePod(ctx context.Context, name string) error {
	// Grace period zero mirrors Docker's force-remove: the machine's
	// processes are SIGKILLed and the Pod disappears immediately instead
	// of lingering in Terminating while the daemon shuts down. Pass it as a
	// query parameter, not a DeleteOptions body: admission policies that
	// inspect object.metadata reject body-carrying DELETEs.
	err := c.do(
		ctx, http.MethodDelete, c.namespacePath("/pods/"+url.PathEscape(name)),
		url.Values{"gracePeriodSeconds": {"0"}}, nil, nil,
	)
	if isNotFound(err) {
		return nil
	}
	return err
}

func (c *restClient) ListPods(ctx context.Context, labelSelector string) ([]podObject, error) {
	var response podList
	err := c.do(
		ctx, http.MethodGet, c.namespacePath("/pods"),
		url.Values{"labelSelector": {labelSelector}}, nil, &response,
	)
	if err != nil {
		return nil, err
	}
	return response.Items, nil
}

func (c *restClient) do(
	ctx context.Context,
	method, path string,
	query url.Values,
	body, out any,
) error {
	requestURL := c.baseURL + path
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}
	response, err := providers.DoHTTPResponse(
		ctx, c.httpClient, providers.Kubernetes, method, requestURL,
		map[string]string{"Authorization": "Bearer " + c.token},
		body,
	)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var status struct {
			Message string `json:"message"`
			Reason  string `json:"reason"`
			Code    int    `json:"code"`
		}
		_ = json.Unmarshal(response.Body, &status)
		message := status.Message
		if message == "" {
			message = status.Reason
		}
		return providers.WithResponse(apiError{StatusCode: response.StatusCode, Message: message}, response)
	}
	if out == nil || len(response.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(response.Body, out); err != nil {
		return fmt.Errorf("decode kubernetes response: %w", err)
	}
	return nil
}

type apiError struct {
	StatusCode int
	Message    string
}

func (e apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("kubernetes API returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("kubernetes API returned HTTP %d: %s", e.StatusCode, e.Message)
}

func isStatus(err error, statusCode int) bool {
	var apiErr apiError
	return errors.As(err, &apiErr) && apiErr.StatusCode == statusCode
}

func isNotFound(err error) bool {
	return isStatus(err, http.StatusNotFound)
}

func isPermanentStatus(err error) bool {
	var apiErr apiError
	return errors.As(err, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 &&
		apiErr.StatusCode != http.StatusRequestTimeout && apiErr.StatusCode != http.StatusTooManyRequests
}

func isConflict(err error) bool {
	return isStatus(err, http.StatusConflict)
}
