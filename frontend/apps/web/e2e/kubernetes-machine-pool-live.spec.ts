import { readFileSync } from 'node:fs'

import { expect, test } from '@playwright/test'

const statePath = process.env.OMNARA_E2E_STATE
const credentialPath = process.env.OMNARA_E2E_K8S_CREDENTIAL
const live = Boolean(statePath && credentialPath)

const credential = credentialPath ? readFileSync(credentialPath, 'utf8').trim() : ''
const apiURL = process.env.OMNARA_E2E_K8S_API_URL ?? 'https://kubernetes.default.svc:443'

test.use({ storageState: statePath })

test('creates a Kubernetes pool against the real API and cluster', async ({ page }) => {
  test.skip(!live, 'OMNARA_E2E_STATE and OMNARA_E2E_K8S_CREDENTIAL are required')
  test.setTimeout(120_000)

  const suffix = Date.now().toString(36)
  const poolName = `k8s-live-${suffix}`
  const secretName = `k8s-live-${suffix}`

  await page.goto('/machines')
  await expect(page.getByRole('heading', { name: 'Machine pools' })).toBeVisible()

  await page.getByRole('button', { name: 'New pool', exact: true }).first().click()
  const dialog = page.getByRole('dialog', { name: 'New machine pool' })
  await expect(dialog).toBeVisible()

  await page.locator('#mpool-name').fill(poolName)
  await page.locator('#mpool-provider').click()
  await page.getByRole('option', { name: 'Kubernetes' }).click()
  await page.locator('#mpool-provider-scope').fill(apiURL)

  await page.getByRole('combobox', { name: 'Kubernetes bearer token' }).click()
  await page.getByRole('button', { name: 'New secret', exact: true }).click()
  await page.locator('#credential-secret-name').fill(secretName)
  await page.locator('#credential-secret-value').fill(credential)
  await page.getByRole('button', { name: 'Create secret', exact: true }).click()

  // This is the real API calling the real cluster API server with the supplied
  // CA and ServiceAccount token; a mocked or short-circuited check shows up here.
  await expect(page.getByText('Connected', { exact: true }).first()).toBeVisible({
    timeout: 60_000,
  })

  await page.getByRole('button', { name: 'Continue', exact: true }).click()
  await page.locator('#mpool-image').fill('registry.k8s.io/pause:3.10')
  await page.getByRole('button', { name: 'Continue', exact: true }).click()
  await page.getByRole('button', { name: 'Continue', exact: true }).click()
  await page.getByRole('button', { name: 'Create pool', exact: true }).click()
  await expect(dialog).toBeHidden()

  await expect(page.getByText(poolName, { exact: true }).first()).toBeVisible()

  const orgID = await page.evaluate(
    () =>
      (window as unknown as { omnaraBrowserContext?: { organizationIds: string[] } })
        .omnaraBrowserContext?.organizationIds[0],
  )
  expect(orgID).toBeTruthy()
  const response = await page.request.get(`/api/v1/orgs/${orgID ?? ''}/machine-pools`)
  expect(response.ok()).toBeTruthy()
  const body = (await response.json()) as {
    data: { name: string; provider: string; provider_config: Record<string, unknown> }[]
  }
  const pool = body.data.find((candidate) => candidate.name === poolName)
  expect(pool?.provider).toBe('kubernetes')
  expect(pool?.provider_config).toMatchObject({ api_base_url: apiURL })
})
