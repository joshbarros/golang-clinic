import { test, expect } from '@playwright/test';
import { ADMIN, adminToken, signIn } from './support';

test('open a session, book a visit, see the referral letter, cancel it', async ({ page }) => {
  const run = Date.now().toString(36).toUpperCase();
  const code = `E2E-${run}`;
  const reference = `PT-${run}`;

  await signIn(page, ADMIN);
  await page.getByLabel('Session code').fill(code);
  await page.getByLabel('Total slots').fill('8');
  await page.getByRole('button', { name: 'Open session' }).click();

  const remaining = page.getByTestId('selected-remaining');
  await expect(page.getByRole('heading', { name: `Book a visit in ${code}` })).toBeVisible();
  await expect(remaining).toHaveText('8');

  await page.getByLabel('Patient reference').fill(reference);
  await page.getByLabel('Slots', { exact: true }).fill('3');
  await page.getByRole('button', { name: 'Book visit' }).click();

  const row = page.locator(`tr[data-reference="${reference}"]`);
  await expect(row.getByTestId('status')).toHaveText('Confirmed', { timeout: 20_000 });
  await expect(row.getByTestId('letter')).toHaveText(/RL-[0-9A-F]{12}/);
  await expect(remaining).toHaveText('5');

  // Nothing is kept in the browser: a fresh load reads the same state from the API.
  // Nothing sensitive is stored either: the refresh cookie is HttpOnly, so a reload signs back in through it.
  expect(await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }))).toBe(
    '{}',
  );
  await page.reload();
  await expect(page.getByTestId('whoami')).toContainText(ADMIN.email);
  await page.getByRole('button', { name: new RegExp(`^${code}`) }).click();
  const reloaded = page.locator(`tr[data-reference="${reference}"]`);
  await expect(reloaded.getByTestId('status')).toHaveText('Confirmed');
  await expect(page.getByTestId('selected-remaining')).toHaveText('5');

  await reloaded.getByRole('button', { name: 'Cancel' }).click();
  await expect(reloaded.getByTestId('status')).toHaveText('Cancelled');
  await expect(page.getByTestId('selected-remaining')).toHaveText('8');
  // The issued letter is not rolled back.
  await expect(reloaded.getByTestId('letter')).toHaveText(/RL-[0-9A-F]{12}/);
  const headers = { Authorization: `Bearer ${await adminToken(page.request)}` };
  const appointments = await page.request.get(`/api/appointments?sessionCode=${code}`, { headers });
  const [appointment] = await appointments.json();
  const letter = await page.request.get(`/api/letters/${appointment.id}`, { headers });
  expect((await letter.json()).status).toBe('Issued');
});
