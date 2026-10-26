<template>
  <div class="exchange-page">
    <div v-if="fatalError !== ''" class="exchange-page__fatal">
      {{ fatalError }}
    </div>
    <template v-else>
      <section class="exchange-page__sidebar" aria-label="Markets">
        <el-input v-model="searchKey" placeholder="Search markets" clearable class="exchange-page__search" />
        <el-tabs v-model="activeTab" class="exchange-page__tabs">
          <el-tab-pane label="Favorites" name="favorites" />
          <el-tab-pane v-for="tab in quoteTabs" :key="tab" :label="tab" :name="tab" />
        </el-tabs>
        <el-table :data="visibleMarkets" highlight-current-row :row-key="rowKey" @row-click="selectMarket">
          <el-table-column prop="market" label="Market" min-width="110">
            <template #default="{ row }">
              <button
                class="exchange-page__star"
                :class="{ 'exchange-page__star--on': favorites.has(row.market) }"
                :aria-label="favorites.has(row.market) ? 'Remove from favorites' : 'Add to favorites'"
                @click.stop="toggleFavorite(row.market)"
              >
                ★
              </button>
              {{ row.base }}/{{ row.quote }}
            </template>
          </el-table-column>
          <el-table-column prop="last" label="Last price" min-width="110">
            <template #default="{ row }">
              {{ row.last === '' ? '—' : formatMoneyEnUs(row.last, row.quoteScale) }}
            </template>
          </el-table-column>
          <el-table-column prop="turnover" label="24h turnover" min-width="120">
            <template #default="{ row }">
              {{ formatMoneyEnUs(row.turnover, row.quoteScale) }}
            </template>
          </el-table-column>
        </el-table>
      </section>

      <section class="exchange-page__main">
        <header class="exchange-page__symbol">
          <h1>{{ current?.market ?? 'Loading…' }}</h1>
          <button
            v-if="current"
            class="exchange-page__star"
            :class="{ 'exchange-page__star--on': favorites.has(current.market) }"
            :aria-label="favorites.has(current.market) ? 'Remove from favorites' : 'Add to favorites'"
            @click="toggleFavorite(current.market)"
          >
            ★
          </button>
          <dl v-if="currentTicker" class="exchange-page__stats">
            <div>
              <dt>Last price</dt>
              <dd :class="tickerDirection">
                {{ currentTicker.last === '' ? '—' : formatMoneyEnUs(currentTicker.last, current.quoteScale) }}
              </dd>
            </div>
            <div>
              <dt>24h high</dt>
              <dd>{{ currentTicker.high === '' ? '—' : formatMoneyEnUs(currentTicker.high, current.quoteScale) }}</dd>
            </div>
            <div>
              <dt>24h low</dt>
              <dd>{{ currentTicker.low === '' ? '—' : formatMoneyEnUs(currentTicker.low, current.quoteScale) }}</dd>
            </div>
            <div>
              <dt>24h turnover</dt>
              <dd>{{ formatMoneyEnUs(currentTicker.turnover, current.quoteScale) }}</dd>
            </div>
          </dl>
        </header>

        <div class="exchange-page__chart-tabs" role="tablist" aria-label="Chart intervals">
          <button
            v-for="interval in KLINE_INTERVALS"
            :key="interval"
            role="tab"
            :aria-selected="activeInterval === interval"
            class="exchange-page__interval"
            :class="{ 'exchange-page__interval--active': activeInterval === interval }"
            @click="switchInterval(interval)"
          >
            {{ interval }}
          </button>
        </div>
        <div ref="chartElement" class="exchange-page__chart" />

        <section class="exchange-page__panels" aria-label="Order book and latest trades">
          <div class="exchange-page__book">
            <h2>Order book</h2>
            <ol class="exchange-page__levels exchange-page__levels--asks" aria-label="Asks">
              <li v-for="level in asksShown" :key="level.price" @click="fillFromBook(level.price)">
                <span class="exchange-page__price exchange-page__price--ask">{{
                  formatMoneyEnUs(level.price, priceScale)
                }}</span>
                <span>{{ formatMoneyEnUs(level.amount, amountScale) }}</span>
              </li>
            </ol>
            <p class="exchange-page__mid">{{ midPrice }}</p>
            <ol class="exchange-page__levels" aria-label="Bids">
              <li v-for="level in bidsShown" :key="level.price" @click="fillFromBook(level.price)">
                <span class="exchange-page__price exchange-page__price--bid">{{
                  formatMoneyEnUs(level.price, priceScale)
                }}</span>
                <span>{{ formatMoneyEnUs(level.amount, amountScale) }}</span>
              </li>
            </ol>
          </div>
          <div class="exchange-page__trades">
            <h2>Latest trades</h2>
            <ol>
              <li v-for="trade in tradesShown" :key="trade.id">
                <span
                  class="exchange-page__price"
                  :class="trade.takerSide === 'buy' ? 'exchange-page__price--bid' : 'exchange-page__price--ask'"
                >
                  {{ formatMoneyEnUs(trade.price, priceScale) }}
                </span>
                <span>{{ formatMoneyEnUs(trade.amount, amountScale) }}</span>
                <span class="exchange-page__time">{{ formatDateTime(trade.time) }}</span>
              </li>
            </ol>
          </div>
        </section>

        <section class="exchange-page__orders" aria-label="Order form">
          <div v-if="!loggedIn" class="exchange-page__login-mask">
            <p>Sign in to place orders.</p>
            <NuxtLink to="/login">Log in</NuxtLink>
          </div>
          <div class="exchange-page__sides">
            <form class="exchange-page__side" @submit.prevent="() => submitOrder('buy')">
              <h2>Buy {{ current?.baseCoin ?? '' }}</h2>
              <p class="exchange-page__balance">
                Available: {{ quoteBalance === '' ? '—' : formatMoneyEnUs(quoteBalance, quoteScale) }}
                {{ current?.quoteCoin ?? '' }}
              </p>
              <label>
                Order kind
                <select v-model="buyKind">
                  <option value="limit">Limit</option>
                  <option value="market">Market</option>
                </select>
              </label>
              <label v-if="buyKind === 'limit'">
                Price ({{ current?.quoteCoin ?? '' }})
                <input v-model="buyPrice" inputmode="decimal" autocomplete="off" />
              </label>
              <label>
                {{ buyKind === 'market' ? `Budget (${current?.quoteCoin ?? ''})` : `Amount (${current?.baseCoin ?? ''})` }}
                <input v-model="buyAmount" inputmode="decimal" autocomplete="off" />
              </label>
              <p v-if="buyTotal !== ''" class="exchange-page__total">Total ≈ {{ buyTotal }}</p>
              <p v-if="orderError !== ''" class="exchange-page__form-error" role="alert">{{ orderError }}</p>
              <button type="submit" class="exchange-page__submit exchange-page__submit--buy" :disabled="submitting">
                Buy
              </button>
            </form>
            <form class="exchange-page__side" @submit.prevent="() => submitOrder('sell')">
              <h2>Sell {{ current?.baseCoin ?? '' }}</h2>
              <p class="exchange-page__balance">
                Available: {{ baseBalance === '' ? '—' : formatMoneyEnUs(baseBalance, amountScale) }}
                {{ current?.baseCoin ?? '' }}
              </p>
              <label>
                Order kind
                <select v-model="sellKind">
                  <option value="limit">Limit</option>
                  <option value="market">Market</option>
                </select>
              </label>
              <label v-if="sellKind === 'limit'">
                Price ({{ current?.quoteCoin ?? '' }})
                <input v-model="sellPrice" inputmode="decimal" autocomplete="off" />
              </label>
              <label>
                {{ sellKind === 'market' ? `Amount (${current?.baseCoin ?? ''})` : `Amount (${current?.baseCoin ?? ''})` }}
                <input v-model="sellAmount" inputmode="decimal" autocomplete="off" />
              </label>
              <p v-if="sellTotal !== ''" class="exchange-page__total">Total ≈ {{ sellTotal }}</p>
              <p v-if="orderError !== ''" class="exchange-page__form-error" role="alert">{{ orderError }}</p>
              <button type="submit" class="exchange-page__submit exchange-page__submit--sell" :disabled="submitting">
                Sell
              </button>
            </form>
          </div>
        </section>

        <section class="exchange-page__mine" aria-label="My orders">
          <h2>My orders</h2>
          <div class="exchange-page__mine-tabs" role="tablist" aria-label="Order scope">
            <button
              role="tab"
              :aria-selected="ordersScope === 'current'"
              :class="{ 'exchange-page__interval--active': ordersScope === 'current' }"
              @click="switchOrdersScope('current')"
            >
              Current
            </button>
            <button
              role="tab"
              :aria-selected="ordersScope === 'history'"
              :class="{ 'exchange-page__interval--active': ordersScope === 'history' }"
              @click="switchOrdersScope('history')"
            >
              History
            </button>
          </div>
          <p v-if="!loggedIn" class="exchange-page__muted">Sign in to see your orders.</p>
          <el-table v-else :data="myOrders" :row-key="orderKey">
            <el-table-column prop="market" label="Market" width="110" />
            <el-table-column prop="side" label="Side" width="70" />
            <el-table-column prop="price" label="Price" min-width="100">
              <template #default="{ row }">
                {{ row.price === undefined || row.price === '' ? 'market' : formatMoneyEnUs(row.price, priceScale) }}
              </template>
            </el-table-column>
            <el-table-column prop="amount" label="Amount" min-width="110">
              <template #default="{ row }">{{ formatMoneyEnUs(row.amount, amountScale) }}</template>
            </el-table-column>
            <el-table-column prop="filledAmount" label="Filled" min-width="110">
              <template #default="{ row }">{{ formatMoneyEnUs(row.filledAmount, amountScale) }}</template>
            </el-table-column>
            <el-table-column prop="status" label="Status" width="90" />
            <el-table-column label="Action" width="100">
              <template #default="{ row }">
                <el-button
                  v-if="row.status === 'open'"
                  link
                  type="danger"
                  size="small"
                  :loading="canceling === row.orderId"
                  @click="cancelOrder(row.orderId)"
                >
                  Cancel
                </el-button>
              </template>
            </el-table-column>
          </el-table>
        </section>
      </section>
    </template>
  </div>
</template>

<script setup lang="ts">
import {
  KLINE_INTERVALS,
  marketName,
  orderErrorMessage,
  marketRouteSlug,
  marketFromRouteSlug,
  mergeDepth,
  type BookLevel,
  type Depth,
  type KlineBar,
  type KlineInterval,
  type MarketInfo,
  type MemberOrder,
  type PublicTrade,
  type Ticker,
} from '~/utils/exchange'
import { createKlineChart, type KlineChart } from '~/components/KlineChart'
import { formatDateTime, formatMoneyEnUs } from '~/utils/format'
import { compareMoney, formatMoney, parseDecimal } from '~/utils/money'
import { useExchangeSocket, type ExchangeSocket, type SocketEnvelope } from '~/composables/useExchangeSocket'

const MARKETS_PATH = '/market/markets'
const TICKERS_PATH = '/market/tickers'
const BALANCES_PATH = '/uc/wallet/accounts'
const KLINE_PATH = '/market/kline'
const ORDERS_PATH = '/uc/orders'
const TRADES_LIMIT = 30

const { apiFetch } = useApi()
const { loggedIn } = useSession()
const favorites = useFavorites()
const route = useRoute()

interface BalanceRow {
  coin: string
  available: string
  frozen: string
}

const markets = ref<MarketInfo[]>([])
const tickers = ref<Record<string, Ticker>>({})
const current = ref<MarketInfo | null>(null)
const fatalError = ref('')
const searchKey = ref('')
const activeTab = ref('')
const activeInterval = ref<KlineInterval>('1m')
const ordersScope = ref<'current' | 'history'>('current')
const myOrders = ref<MemberOrder[]>([])
const balances = ref<BalanceRow[]>([])
const depth = ref<Depth | null>(null)
const trades = ref<PublicTrade[]>([])
const seenTradeIds = ref<Record<string, boolean>>({})
const buyKind = ref<'limit' | 'market'>('limit')
const sellKind = ref<'limit' | 'market'>('limit')
const buyPrice = ref('')
const buyAmount = ref('')
const sellPrice = ref('')
const sellAmount = ref('')
const orderError = ref('')
const submitting = ref(false)
const canceling = ref('')
const chartElement = ref<HTMLElement | null>(null)

let chart: KlineChart | null = null
let socket: ExchangeSocket | null = null
let disposed = false

const quoteTabs = computed(() => [...new Set(markets.value.map((m) => m.quoteCoin))])

const visibleMarkets = computed(() => {
  const base =
    activeTab.value === 'favorites' || activeTab.value === ''
      ? markets.value.filter((m) => favorites.has(m.market))
      : markets.value.filter((m) => m.quoteCoin === activeTab.value)
  const key = searchKey.value.trim().toUpperCase()
  if (key === '') {
    return base
  }
  return base.filter((m) => m.baseCoin.toUpperCase().startsWith(key) || m.market.toUpperCase().startsWith(key))
})

const currentTicker = computed(() => (current.value ? (tickers.value[current.value.market] ?? null) : null))

const tickerDirection = computed(() => {
  if (!currentTicker.value || currentTicker.value.last === '') {
    return ''
  }
  if (compareMoney(currentTicker.value.last, currentTicker.value.low) <= 0) {
    return 'exchange-page__price--ask'
  }
  if (compareMoney(currentTicker.value.last, currentTicker.value.high) >= 0) {
    return 'exchange-page__price--bid'
  }
  return ''
})

const priceScale = computed(() => current.value?.quoteScale ?? 2)
const amountScale = computed(() => current.value?.baseScale ?? 8)
const quoteScale = computed(() => current.value?.quoteScale ?? 2)

const asksShown = computed(() => [...(depth.value?.asks ?? [])].reverse().slice(0, 12))
const bidsShown = computed(() => (depth.value?.bids ?? []).slice(0, 12))
const tradesShown = computed(() => trades.value.slice(0, TRADES_LIMIT))

const midPrice = computed(() => {
  const best = bidsShown.value[0]
  if (!best || !current.value) {
    return '—'
  }
  return formatMoneyEnUs(best.price, priceScale.value)
})

const baseBalance = computed(() => balances.value.find((b) => b.coin === current.value?.baseCoin)?.available ?? '')
const quoteBalance = computed(
  () => balances.value.find((b) => b.coin === current.value?.quoteCoin)?.available ?? '',
)

function formTotal(kind: 'limit' | 'market', price: string, amount: string): string {
  if (current.value === null || amount.trim() === '') {
    return ''
  }
  try {
    if (kind === 'market') {
      return `${formatMoney(amount.trim(), quoteScale.value)} ${current.value.quoteCoin}`
    }
    if (price.trim() === '') {
      return ''
    }
    const total = parseDecimal(price.trim()).mul(parseDecimal(amount.trim()))
    return `${formatMoney(total.toString(), quoteScale.value)} ${current.value.quoteCoin}`
  } catch {
    return ''
  }
}

const buyTotal = computed(() => formTotal(buyKind.value, buyPrice.value, buyAmount.value))
const sellTotal = computed(() => formTotal(sellKind.value, sellPrice.value, sellAmount.value))

function rowKey(row: { market: string }): string {
  return row.market
}

function orderKey(row: MemberOrder): string {
  return row.orderId
}

function toggleFavorite(market: string): void {
  if (favorites.has(market)) {
    favorites.remove(market)
  } else {
    favorites.add(market)
  }
}

function selectMarket(row: MarketInfo): void {
  void navigateTo(`/exchange/${marketRouteSlug(row)}`)
}

function fillFromBook(price: string): void {
  buyPrice.value = price
  sellPrice.value = price
}

function channelsFor(market: string): string[] {
  const channels = [
    'ticker',
    `depth.${market}`,
    `trades.${market}`,
    `kline.${market}.${activeInterval.value}`,
  ]
  if (loggedIn.value) {
    channels.push('orders')
  }
  return channels
}

function resubscribe(): void {
  if (!socket || !current.value) {
    return
  }
  for (const channel of channelsFor(current.value.market)) {
    socket.subscribe(channel)
  }
}

async function bootstrap(fetchAll: boolean): Promise<void> {
  const market = current.value
  if (!market) {
    return
  }
  try {
    const [depthAnswer, tradesAnswer, tickerAnswer, klines] = await Promise.all([
      apiFetch<Depth>(`/market/depth/${market.market}`),
      apiFetch<PublicTrade[]>(`/market/trades/${market.market}?limit=${TRADES_LIMIT}`),
      apiFetch<Ticker>(`/market/ticker/${market.market}`),
      apiFetch<KlineBar[]>(`${KLINE_PATH}/${market.market}?interval=${activeInterval.value}`),
    ])
    if (current.value?.market !== market.market) {
      return
    }
    depth.value = depthAnswer
    seenTradeIds.value = {}
    trades.value = []
    for (const trade of tradesAnswer) {
      if (!seenTradeIds.value[trade.id]) {
        seenTradeIds.value[trade.id] = true
        trades.value.push(trade)
      }
    }
    tickers.value = { ...tickers.value, [market.market]: tickerAnswer }
    chart?.updateHistory(klines, activeInterval.value)
    if (fetchAll && loggedIn.value) {
      await refreshMyOrders()
    }
  } catch (error) {
    orderError.value = error instanceof Error ? error.message : 'Market data could not be loaded.'
  }
}

async function refreshMyOrders(): Promise<void> {
  if (!loggedIn.value) {
    myOrders.value = []
    return
  }
  try {
    myOrders.value = await apiFetch<MemberOrder[]>(`${ORDERS_PATH}/${ordersScope.value}`)
  } catch (error) {
    orderError.value = error instanceof Error ? error.message : 'Your orders could not be loaded.'
  }
}

async function refreshBalances(): Promise<void> {
  if (!loggedIn.value) {
    balances.value = []
    return
  }
  try {
    balances.value = await apiFetch<BalanceRow[]>(BALANCES_PATH)
  } catch {
    balances.value = []
  }
}

async function submitOrder(side: 'buy' | 'sell'): Promise<void> {
  const market = current.value
  if (!market) {
    return
  }
  const kind = side === 'buy' ? buyKind.value : sellKind.value
  const price = side === 'buy' ? buyPrice.value.trim() : sellPrice.value.trim()
  const amount = side === 'buy' ? buyAmount.value.trim() : sellAmount.value.trim()
  if (amount === '' || (kind === 'limit' && price === '')) {
    orderError.value = 'Enter a price and an amount before placing the order.'
    return
  }
  orderError.value = ''
  submitting.value = true
  try {
    const body: Record<string, string> = { market: market.market, side, kind, amount }
    if (kind === 'limit') {
      body.price = price
    }
    await apiFetch<{ orderId: string }>(ORDERS_PATH, { method: 'POST', body })
    if (side === 'buy') {
      buyAmount.value = ''
    } else {
      sellAmount.value = ''
    }
    await Promise.all([refreshMyOrders(), refreshBalances()])
  } catch (error) {
    orderError.value = orderErrorMessage(error instanceof Error ? error.message : '')
  } finally {
    submitting.value = false
  }
}

async function cancelOrder(orderId: string): Promise<void> {
  canceling.value = orderId
  try {
    await apiFetch(`${ORDERS_PATH}/${orderId}/cancel`, { method: 'POST' })
    await Promise.all([refreshMyOrders(), refreshBalances()])
  } catch (error) {
    orderError.value = orderErrorMessage(error instanceof Error ? error.message : '')
  } finally {
    canceling.value = ''
  }
}

async function switchInterval(interval: KlineInterval): Promise<void> {
  if (activeInterval.value === interval || !current.value) {
    activeInterval.value = interval
    return
  }
  socket?.unsubscribe(`kline.${current.value.market}.${activeInterval.value}`)
  activeInterval.value = interval
  socket?.subscribe(`kline.${current.value.market}.${interval}`)
  try {
    const klines = await apiFetch<KlineBar[]>(`${KLINE_PATH}/${current.value.market}?interval=${interval}`)
    chart?.updateHistory(klines, interval)
  } catch (error) {
    orderError.value = error instanceof Error ? error.message : 'The chart could not be loaded.'
  }
}

function switchOrdersScope(scope: 'current' | 'history'): void {
  ordersScope.value = scope
  void refreshMyOrders()
}

function applyEnvelope(envelope: SocketEnvelope): void {
  if (!current.value || typeof envelope.data !== 'object' || envelope.data === null) {
    return
  }
  const market = current.value.market
  const data = envelope.data as Record<string, unknown>
  if (envelope.channel === 'ticker' && typeof data.market === 'string') {
    tickers.value = { ...tickers.value, [data.market]: data as unknown as Ticker }
    return
  }
  if (envelope.channel === `depth.${market}`) {
    depth.value = mergeDepth(
      depth.value,
      data as { market: string; bids: BookLevel[] | null; asks: BookLevel[] | null },
    )
    return
  }
  if (envelope.channel === `trades.${market}`) {
    const trade = data as unknown as PublicTrade
    if (typeof trade.id === 'string' && !seenTradeIds.value[trade.id]) {
      seenTradeIds.value[trade.id] = true
      trades.value = [trade, ...trades.value].slice(0, TRADES_LIMIT)
    }
    return
  }
  if (envelope.channel === `kline.${market}.${activeInterval.value}`) {
    chart?.updateFromChannel(data as unknown as KlineBar)
    return
  }
  if (envelope.channel === 'orders') {
    void refreshMyOrders()
    void refreshBalances()
  }
}

async function loadInitial(): Promise<void> {
  try {
    const [marketList, tickerMap] = await Promise.all([
      apiFetch<MarketInfo[]>(MARKETS_PATH),
      apiFetch<Record<string, Ticker>>(TICKERS_PATH),
    ])
    markets.value = marketList
    tickers.value = tickerMap
    favorites.load()
  } catch (error) {
    fatalError.value = error instanceof Error ? error.message : 'The market list could not be loaded.'
    return
  }
  const pair = Array.isArray(route.params.pair) ? route.params.pair[0] : route.params.pair
  const resolved =
    (typeof pair === 'string' && marketFromRouteSlug(pair, markets.value)) ?? markets.value[0] ?? null
  if (!resolved) {
    fatalError.value = 'No Markets are listed yet.'
    return
  }
  current.value = resolved
  if (activeTab.value === '') {
    activeTab.value = resolved.quoteCoin
  }
  void bootstrap(true)
  void refreshBalances()
}

useHead({ title: 'Exchange' })

onMounted(() => {
  if (chartElement.value) {
    chart = createKlineChart(chartElement.value)
  }
  socket = useExchangeSocket(applyEnvelope, () => void bootstrap(true))
  socket.connect()
  resubscribe()
  void loadInitial().then(() => {
    resubscribe()
  })
  window.addEventListener('resize', () => chart?.resize())
})

onUnmounted(() => {
  disposed = true
  socket?.disconnect()
  socket = null
  chart?.destroy()
  chart = null
})
</script>

<style scoped lang="scss">
.exchange-page {
  display: grid;
  grid-template-columns: 300px 1fr;
  gap: 16px;
  padding: 16px;
}

.exchange-page__sidebar {
  min-width: 0;
}

.exchange-page__main {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.exchange-page__symbol {
  display: flex;
  align-items: baseline;
  gap: 12px;
  flex-wrap: wrap;

  h1 {
    font-size: 20px;
    margin: 0;
  }
}

.exchange-page__stats {
  display: flex;
  gap: 24px;
  margin: 0;
  flex-wrap: wrap;

  dt {
    font-size: 12px;
    color: var(--text-secondary);
  }

  dd {
    margin: 0;
    font-size: 15px;
  }
}

.exchange-page__star {
  background: none;
  border: none;
  cursor: pointer;
  color: var(--text-secondary);
  font-size: 16px;
  padding: 0 4px;

  &--on {
    color: var(--accent);
  }
}

.exchange-page__chart-tabs,
.exchange-page__mine-tabs {
  display: flex;
  gap: 4px;
}

.exchange-page__interval {
  background: none;
  border: none;
  cursor: pointer;
  padding: 4px 8px;
  color: var(--text-secondary);

  &--active {
    color: var(--accent);
    font-weight: 600;
  }
}

.exchange-page__chart {
  height: 420px;
}

.exchange-page__panels {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 16px;
}

.exchange-page__book,
.exchange-page__trades,
.exchange-page__orders,
.exchange-page__mine {
  h2 {
    font-size: 14px;
    margin: 0 0 8px;
  }
}

.exchange-page__levels,
.exchange-page__trades ol {
  list-style: none;
  margin: 0;
  padding: 0;
  max-height: 288px;
  overflow-y: auto;
}

.exchange-page__levels li,
.exchange-page__trades li {
  display: flex;
  justify-content: space-between;
  padding: 2px 0;
  cursor: pointer;
}

.exchange-page__price--ask {
  color: #f15057;
}

.exchange-page__price--bid {
  color: #00b275;
}

.exchange-page__mid {
  font-size: 16px;
  margin: 6px 0;
}

.exchange-page__time {
  color: var(--text-secondary);
  font-size: 12px;
}

.exchange-page__sides {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 16px;
}

.exchange-page__side {
  display: flex;
  flex-direction: column;
  gap: 8px;

  label {
    display: flex;
    flex-direction: column;
    gap: 4px;
    font-size: 13px;
  }

  input,
  select {
    padding: 8px;
    border: 1px solid var(--border);
    border-radius: 4px;
    background: var(--surface);
    color: inherit;
  }
}

.exchange-page__submit {
  padding: 10px;
  border: none;
  border-radius: 4px;
  cursor: pointer;
  font-size: 15px;
  color: #fff;

  &--buy {
    background: #00b275;
  }

  &--sell {
    background: #f15057;
  }

  &:disabled {
    opacity: 0.6;
    cursor: default;
  }
}

.exchange-page__fatal,
.exchange-page__muted,
.exchange-page__form-error,
.exchange-page__login-mask {
  font-size: 14px;
}

.exchange-page__form-error {
  color: #f15057;
}

.exchange-page__login-mask {
  border: 1px dashed var(--border);
  border-radius: 4px;
  padding: 12px;
  margin-bottom: 12px;
}
</style>
