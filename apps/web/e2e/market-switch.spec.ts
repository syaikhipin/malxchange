// The trading page must follow the market in the URL when the member
// switches markets in the sidebar (audit issue #97): the page used to
// keep the old market's book, chart, kline channel, orders, and
// balances, because nothing remounted or watched the route param — a
// client-side param navigation reused the component wholesale. A full
// page.goto always remounted, so this leg must navigate by clicking.
import { expect, test } from '@playwright/test'

test('switching markets in the sidebar re-resolves the page', async ({ page }) => {
  await page.goto('/exchange/btc_usdt')
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('BTC/USDT')

  // Client-side navigation — the exact path that kept the old market
  // before the fix.
  await page.getByRole('row').filter({ hasText: 'ETH/USDT' }).first().click()
  await expect(page).toHaveURL(/\/exchange\/eth_usdt/)
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('ETH/USDT')

  // And back — the remount must work in both directions.
  await page.getByRole('row').filter({ hasText: 'BTC/USDT' }).first().click()
  await expect(page).toHaveURL(/\/exchange\/btc_usdt/)
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('BTC/USDT')
})
