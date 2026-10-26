import { describe, expect, it } from 'vitest'
import { mergeDepth } from '~/utils/exchange'

// The market.plate keep/clear contract (audit issue #79): a fully
// swept side must clear in the UI, an untouched side must survive.
describe('mergeDepth (market.plate keep/clear contract)', () => {
  const prev = {
    market: 'BTC/USDT',
    bids: [{ price: '50000', amount: '0.5' }],
    asks: [
      { price: '50001', amount: '0.7' },
      { price: '50002', amount: '3' },
    ],
  }

  it('an empty-array side clears that side only', () => {
    const next = mergeDepth(prev, { market: 'BTC/USDT', bids: [], asks: null })
    expect(next.bids).toEqual([])
    expect(next.asks).toEqual(prev.asks)
  })

  it('a null side keeps the last-known levels', () => {
    const next = mergeDepth(prev, { market: 'BTC/USDT', bids: null, asks: null })
    expect(next).toEqual(prev)
  })

  it('present levels replace that side wholesale', () => {
    const next = mergeDepth(prev, {
      market: 'BTC/USDT',
      bids: null,
      asks: [{ price: '50010', amount: '1' }],
    })
    expect(next.asks).toEqual([{ price: '50010', amount: '1' }])
    expect(next.bids).toEqual(prev.bids)
  })

  it('a first publish with no previous depth seeds empty sides', () => {
    const next = mergeDepth(null, { market: 'ETH/USDT', bids: null, asks: null })
    expect(next).toEqual({ market: 'ETH/USDT', bids: [], asks: [] })
  })

  it('a first publish can clear and seed in one shot', () => {
    const next = mergeDepth(null, { market: 'ETH/USDT', bids: [], asks: [{ price: '4000', amount: '2' }] })
    expect(next).toEqual({ market: 'ETH/USDT', bids: [], asks: [{ price: '4000', amount: '2' }] })
  })
})
