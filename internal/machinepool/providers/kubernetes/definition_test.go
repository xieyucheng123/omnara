package kubernetes

import (
	"encoding/json"
	"testing"

	"github.com/omnara-ai/omnara/internal/machinepool/providers"
	"github.com/stretchr/testify/require"
)

const (
	testCA = `-----BEGIN CERTIFICATE-----
MIIDBTCCAe2gAwIBAgIUVcBEnvr929ge+atZ9My834R0c7swDQYJKoZIhvcNAQEL
BQAwEjEQMA4GA1UEAwwHdGVzdC1jYTAeFw0yNjExMjUwNjUyMzFaFw0yNjExMjcw
NjUyMzFaMBIxEDAOBgNVBAMMB3Rlc3QtY2EwggEiMA0GCSqGSIb3DQEBAQUAA4IB
DwAwggEKAoIBAQDRUjfS+Tnx92RpvTlJBTB8+XRqGT109c7eMc+KidNKhcSOEkdF
Cq+pqeENHwec7HPAqqK6DjAaRP1Rj67zBPlhLqLj1SPUVG7YpYpTdWv5j9rlCNuE
N0nt0T6nikjwruKAfqkrqn8pzroyr1wvoHg92JRvOtUdLetyz5lxtBASjJdmdZZ/
1WGbeRr7fDxVxJrlTaGwyqaNsuE6bz5qXNNjMOlxauY9nUY30LSn1WJrtUZzQodX
9ENPwYyAQEiuZ7FWVv0H1UAE0IckUwdKf1z9BkGeK6Mna3nkgge7liKghM84Ezml
UKMOJpQjLIJy4N2EQ0GeJmNLZAsaprQSJJZNAgMBAAGjUzBRMB0GA1UdDgQWBBRP
xF2FO9vS8cPhfINBtk8nE2DyRjAfBgNVHSMEGDAWgBRPxF2FO9vS8cPhfINBtk8n
E2DyRjAPBgNVHRMBAf8EBTADAQH/MA0GCSqGSIb3DQEBCwUAA4IBAQBjT7iF6WZI
qcchrA+vcJNROtn/NCU95TFRi8nLo0jBN98SQf7HFAi/0kRwCdJSvWnspGW++Cx+
HJ5H2Z05HCiNUs0d1ILOVQWZHcshbHfp9DqtVvTTae3ALxuczi0x1RXY1boEGMyf
NjANQCPk0HZV7VEgfLWk1dVrwteMgkA/uaU2l/JoAE3MIhSuINloHw1btpWNyxzv
BpNeQPsDfJohz9Ir5LyQJl+O611ajtXPRnG8sycq+8fh3krrWaZMomzfpkz6AJSU
CFStbqvTaPjw9QgFVBOwSAgn5mxqGQ2fvXAiiClVSQfiAqcypjjFfNr61UD1/Stc
E6IHJphCDbXN
-----END CERTIFICATE-----`
	testToken = "test-service-account-token"
)

func testCredential(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(providerCredential{CA: testCA, Token: testToken})
	require.NoError(t, err)
	return string(raw)
}

func testProviderConfig(namespace string) json.RawMessage {
	config := map[string]any{"api_base_url": "https://192.0.2.1:6443"}
	if namespace != "" {
		config["namespace"] = namespace
	}
	raw, err := json.Marshal(config)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestKubernetesProviderDefaultsNamespace(t *testing.T) {
	machineProvider, err := newProvider(testProviderConfig(""), providers.RuntimeConfig{
		OmnaraAPIURL:      "https://api.omnara.test/v1",
		ProviderAuthToken: testCredential(t),
	})
	require.NoError(t, err)
	require.Equal(t, "default", machineProvider.namespace)
}

func TestKubernetesProviderConfigValidation(t *testing.T) {
	for raw, message := range map[string]string{
		`{}`: "requires api_base_url",
		`{"api_base_url": "http://192.0.2.1:6443"}`:                           "must use https",
		`{"api_base_url": "https://192.0.2.1:6443/api"}`:                      "must not include",
		`{"api_base_url": "https://192.0.2.1:6443", "namespace": "Bad_Name"}`: "must be a DNS label",
	} {
		_, err := newProvider(json.RawMessage(raw), providers.RuntimeConfig{
			OmnaraAPIURL:      "https://api.omnara.test/v1",
			ProviderAuthToken: testCredential(t),
		})
		require.ErrorContains(t, err, message, "config %s", raw)
	}
}

func TestKubernetesProviderDaemonAPIURL(t *testing.T) {
	for raw, want := range map[string]string{
		`{"api_base_url": "https://192.0.2.1:6443", "daemon_api_url": "http://omnara-api.omnara.svc:8080/api/v1"}`: "http://omnara-api.omnara.svc:8080/api/v1",
		`{"api_base_url": "https://192.0.2.1:6443"}`:                                                               "",
	} {
		var config providerConfig
		require.NoError(t, json.Unmarshal(json.RawMessage(raw), &config))
		parsed, err := parseProviderConfig(json.RawMessage(raw))
		require.NoError(t, err)
		require.Equal(t, want, parsed.DaemonAPIURL, "config %s", raw)
	}
	for _, raw := range []string{
		`{"api_base_url": "https://192.0.2.1:6443", "daemon_api_url": "notaurl"}`,
		`{"api_base_url": "https://192.0.2.1:6443", "daemon_api_url": "ftp://x/"}`,
	} {
		_, err := parseProviderConfig(json.RawMessage(raw))
		require.Error(t, err, "config %s", raw)
	}
}

func TestKubernetesProviderCredentialValidation(t *testing.T) {
	for credential, message := range map[string]string{
		``:                     "credentials are required",
		`{`:                    "must be JSON",
		`{"ca": "not-a-cert"}`: "must be a PEM certificate",
	} {
		raw := credential
		_, err := newProvider(testProviderConfig("test-ns"), providers.RuntimeConfig{
			OmnaraAPIURL:      "https://api.omnara.test/v1",
			ProviderAuthToken: raw,
		})
		require.ErrorContains(t, err, message, "credential %s", raw)
	}

	emptyToken, err := json.Marshal(providerCredential{CA: testCA, Token: " "})
	require.NoError(t, err)
	_, err = newProvider(testProviderConfig("test-ns"), providers.RuntimeConfig{
		OmnaraAPIURL:      "https://api.omnara.test/v1",
		ProviderAuthToken: string(emptyToken),
	})
	require.ErrorContains(t, err, "bearer token")
}

func TestKubernetesProviderBearerToken(t *testing.T) {
	token, err := bearerToken(testCredential(t))
	require.NoError(t, err)
	require.Equal(t, testToken, token)
}
