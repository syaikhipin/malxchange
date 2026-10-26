import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

// Route-param pages must either watch their params or remount on param
// change. The exchange page does neither on its own, so the app shell
// carries the remount: NuxtPage with a :page-key (audit issue #97 —
// client-side market switches kept the old market's book, chart, and
// orders). This filesystem check pins the shell so the line cannot
// silently disappear.
describe('app shell remounts param routes', () => {
  it('NuxtPage carries a page key', () => {
    const source = readFileSync('src/app.vue', 'utf8')
    expect(source).toMatch(/<NuxtPage[^>]*:page-key/)
  })
})
