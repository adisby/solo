import { expect, test } from '@playwright/test';
import { registerVerified } from './support/auth';

// Temporary verification for the Windows pairing command tab added in the
// Add Computer dialog. Mirrors the session setup used by the existing suites
// (real API registration, real PostgreSQL, real frontend) but starts after the
// registration UI, which this host cannot complete without a mail provider.
const apiBase = process.env.SOLO_E2E_API_URL ?? 'http://127.0.0.1:8080';

test.describe('Add Computer pairing command targets', () => {
  test.skip(process.env.SOLO_E2E_PUBLIC_REMOTE !== '1', 'requires the make-managed frontend, API, and PostgreSQL stack');
  test.setTimeout(120000);

  test('offers macOS/Linux and Windows commands and copies the selected one', async ({ page, request }) => {
    const suffix = Date.now().toString(36);
    const email = `pairing-targets-${suffix}@solo.local`;
    const password = 'SoloPublic-2026!';

    const verified = await registerVerified(request, apiBase, { data: { email, password, display_name: `Pairing ${suffix}` } });
    expect(verified.ok()).toBe(true);
    const auth = await verified.json() as { access_token: string; refresh_token: string };

    await page.addInitScript(({ accessToken, refreshToken }) => {
      localStorage.setItem('access_token', accessToken);
      localStorage.setItem('refresh_token', refreshToken);
    }, { accessToken: auth.access_token, refreshToken: auth.refresh_token });

    await page.goto('/computers');
    // A freshly registered account already owns a seeded "My computer" in the
    // pending state, so Add Computer reuses it and opens the dialog straight on
    // the commands instead of asking for a name.
    await page.getByRole('button', { name: /Add Computer/i }).click();
    await expect(page.getByRole('heading', { name: 'Pair a Computer' })).toBeVisible();

    const freshCommand = page.locator('pre').first();
    const installedCommand = page.locator('pre').nth(1);

    // Default target is macOS/Linux.
    await expect(freshCommand).toContainText('curl -fsSL');
    await expect(freshCommand).toContainText('scripts/install.sh');
    await expect(freshCommand).toContainText('bash -s -- connect');
    await expect(installedCommand).toContainText('solo daemon connect --server');

    // Switching to Windows swaps both commands.
    await page.getByRole('button', { name: 'Windows' }).click();
    await expect(freshCommand).toContainText('scripts/install.ps1');
    await expect(freshCommand).toContainText('-Connect -Server');
    await expect(freshCommand).toContainText('-ComputerId');
    await expect(freshCommand).toContainText('-Token');
    await expect(installedCommand).toContainText('solo daemon connect --server');
    await expect(freshCommand).not.toContainText('curl -fsSL');

    // Copy uses the currently selected target.
    await page.getByRole('button', { name: 'Copy' }).first().click();
    await expect(page.getByText('Command copied.')).toBeVisible();
    const clipboard = await page.evaluate(() => navigator.clipboard.readText());
    expect(clipboard).toContain('scripts/install.ps1');
    expect(clipboard).toContain('-Connect -Server');

    // Switching back restores the macOS/Linux command.
    await page.getByRole('button', { name: 'macOS / Linux' }).click();
    await expect(freshCommand).toContainText('scripts/install.sh');
    await expect(freshCommand).toContainText('curl -fsSL');
  });
});
