import { schemas } from '@omnara/sdk'
import { expect, test, type Page, type Route } from '@playwright/test'

import { currentUser, currentUserOrg, fakeId, machinePool } from '../src/test/fixtures'

const timestamp = '2026-01-01T00:00:00Z'
const apiURL = 'https://k8s.example.test:6443'
const namespace = 'omnara-machines'
const runtimeClass = 'gvisor'
const image = 'registry.example.test/omnara/agent-sandbox:test'
const credentialValue = JSON.stringify({
  ca: '-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----',
  token: 'test-service-account-token',
})

type Captured = {
  secrets: unknown[]
  pools: unknown[]
  checks: unknown[]
}

/**
 * Boots the SPA against an in-browser contract mock. The request bodies are
 * parsed with the generated SDK schemas, so a payload the real API would
 * reject fails the test instead of being waved through.
 */
async function mockAPI(
  page: Page,
  options: { kubernetesPoolsEnabled?: boolean; connectionFails?: boolean } = {},
) {
  const org = currentUserOrg()
  const captured: Captured = { secrets: [], pools: [], checks: [] }
  const secret = {
    id: fakeId('sec'),
    org_id: org.id,
    management_kind: 'tenant',
    owner: { kind: 'org' },
    name: 'kubernetes-service-account',
    kind: 'generic',
    metadata: {},
    current_version_number: 1,
    payload_keys: ['value'],
    created_at: timestamp,
    updated_at: timestamp,
  }

  await page.route('**/api/**', async (route: Route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    const method = request.method()

    if (path === '/api/web-config') {
      return route.fulfill({
        json: { kubernetes_pools_enabled: options.kubernetesPoolsEnabled ?? true },
      })
    }
    if (path === '/api/v1/me') {
      return route.fulfill({ json: currentUser([org]) })
    }
    if (path === '/api/v1/invitations') {
      return route.fulfill({ json: { data: [], next_cursor: null } })
    }
    if (method === 'GET') {
      return route.fulfill({ json: { data: [], next_cursor: null } })
    }
    if (method === 'POST' && /\/secrets$/.test(path)) {
      captured.secrets.push(schemas.zCreateSecretRequest.parse(request.postDataJSON()))
      return route.fulfill({ status: 201, json: secret })
    }
    if (method === 'POST' && /\/machine-pools\/check-connection$/.test(path)) {
      captured.checks.push(schemas.zCheckMachinePoolConnectionRequest.parse(request.postDataJSON()))
      if (options.connectionFails) {
        return route.fulfill({
          status: 422,
          json: {
            error: 'Kubernetes credentials were rejected',
            code: 'upstream_unavailable',
            details: 'dial tcp 10.0.0.1:6443: connect: connection timed out',
          },
        })
      }
      return route.fulfill({ status: 204 })
    }
    if (method === 'POST' && /\/machine-pools$/.test(path)) {
      const body = schemas.zCreateMachinePoolRequest.parse(request.postDataJSON())
      captured.pools.push(body)
      return route.fulfill({
        status: 201,
        json: machinePool({
          name: body.name,
          provider: 'kubernetes',
          provider_config: body.provider_config,
          default_machine_provider_options: body.default_machine_provider_options,
        }),
      })
    }
    return route.fulfill({ status: 204 })
  })

  return captured
}

async function openCreatePoolDialog(page: Page) {
  await page.goto('/machines')
  await expect(page.getByRole('heading', { name: 'Machine pools' })).toBeVisible()
  await page.getByRole('button', { name: 'New pool', exact: true }).first().click()
  const dialog = page.getByRole('dialog', { name: 'New machine pool' })
  await expect(dialog).toBeVisible()
  return dialog
}

async function chooseKubernetes(page: Page) {
  await page.locator('#mpool-provider').click()
  await page.getByRole('option', { name: 'Kubernetes' }).click()
}

async function fillKubernetesProviderStep(page: Page) {
  await page.locator('#mpool-name').fill('k8s-e2e')
  await chooseKubernetes(page)
  await page.locator('#mpool-provider-scope').fill(apiURL)
  await page.locator('#mpool-provider-namespace').fill(namespace)
  await page.locator('#mpool-provider-runtime-class').fill(runtimeClass)

  await page.getByRole('combobox', { name: 'Kubernetes bearer token' }).click()
  await page.getByRole('button', { name: 'New secret', exact: true }).click()
  await page.locator('#credential-secret-name').fill('kubernetes-service-account')
  await page.locator('#credential-secret-value').fill(credentialValue)
  await page.getByRole('button', { name: 'Create secret', exact: true }).click()
  await expect(page.getByRole('combobox', { name: 'Kubernetes bearer token' })).toContainText(
    'kubernetes-service-account',
  )
}

async function finishPoolForm(page: Page) {
  await page.getByRole('button', { name: 'Continue', exact: true }).click()
  await page.locator('#mpool-image').fill(image)
  await page.getByRole('button', { name: 'Continue', exact: true }).click()
  // Capacity keeps its valid 1 vCPU / 1 GB / 3 machines defaults.
  await page.getByRole('button', { name: 'Continue', exact: true }).click()
  await page.getByRole('button', { name: 'Create pool', exact: true }).click()
}

test.describe('Kubernetes machine pool', () => {
  test.beforeEach(async ({ context, baseURL }) => {
    // The browser SDK mirrors this cookie into X-Omnara-Csrf.
    await context.addCookies([
      { name: 'omnara_csrf', value: 'browser-session', url: new URL(baseURL!).origin },
    ])
  })

  test('creates a pool through the UI and checks the connection', async ({ page }) => {
    const captured = await mockAPI(page)
    await openCreatePoolDialog(page)
    await fillKubernetesProviderStep(page)

    // The debounced connection check runs once the credential and scope are valid.
    await expect(page.getByText('Connected', { exact: true }).first()).toBeVisible({ timeout: 15000 })

    await finishPoolForm(page)
    await expect(page.getByRole('dialog', { name: 'New machine pool' })).toBeHidden()

    expect(captured.secrets).toHaveLength(1)
    expect(captured.secrets[0]).toMatchObject({
      owner: { kind: 'org' },
      name: 'kubernetes-service-account',
      material: { kind: 'generic', value: credentialValue },
    })

    // The connection check runs as soon as the provider step is valid, before
    // the image step, so it carries the API URL and credential but not the image.
    expect(captured.checks).toHaveLength(1)
    expect(captured.checks[0]).toMatchObject({
      provider: 'kubernetes',
      provider_auth_secret_id: fakeId('sec'),
      provider_config: {
        api_base_url: apiURL,
        namespace,
        runtime_class_name: runtimeClass,
      },
    })

    expect(captured.pools).toHaveLength(1)
    expect(captured.pools[0]).toMatchObject({
      name: 'k8s-e2e',
      provider: 'kubernetes',
      provider_config: {
        api_base_url: apiURL,
        namespace,
        runtime_class_name: runtimeClass,
      },
      default_machine_cpu: 1,
      default_machine_memory_mb: 1024,
      max_total_machines: 3,
      default_machine_provider_options: { image },
    })
    // The pool must reference the secret the form just created.
    expect(captured.pools[0]).toMatchObject({ provider_auth_secret_id: fakeId('sec') })
  })

  test('hides Kubernetes when the deployment flag is off', async ({ page }) => {
    await mockAPI(page, { kubernetesPoolsEnabled: false })
    await openCreatePoolDialog(page)
    await page.locator('#mpool-provider').click()
    await expect(page.getByRole('option', { name: 'Kubernetes' })).toHaveCount(0)
    // The other providers stay selectable, so this is gating, not a broken dialog.
    await expect(page.getByRole('option', { name: 'Docker' })).toHaveCount(0)
    await expect(page.getByRole('option', { name: 'Modal' })).toBeVisible()
  })

  test('surfaces a rejected credential as a connection error', async ({ page }) => {
    await mockAPI(page, { connectionFails: true })
    await openCreatePoolDialog(page)
    await fillKubernetesProviderStep(page)

    await expect(page.getByText('Kubernetes credentials were rejected')).toBeVisible({
      timeout: 15000,
    })
    await expect(page.getByText(/connection timed out/)).toBeHidden()
    await page.getByText('Details', { exact: true }).click()
    await expect(page.getByText(/connection timed out/)).toBeVisible()
  })
})
