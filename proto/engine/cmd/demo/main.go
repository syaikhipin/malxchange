// THROWAWAY PROTOTYPE (branch proto/engine). Drivable playground for the
// ported CoinTrader semantics: `go run ./cmd/demo` then open http://localhost:8090.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	engine "github.com/syaikhipin/malxchange/proto/engine/engine"
)

var spec = engine.MarketSpec{
	Symbol: "BTC/USDT", BaseScale: 8, QuoteScale: 2,
	TickRaw: 1, LotRaw: 10000, // tick 0.01 USDT, lot 0.0001 BTC
}

var (
	book *engine.Book
	log_ []string
)

func reset() {
	book = engine.NewBook(spec)
	log_ = nil
	log("Book reset — empty market, everyone starts fresh.")
}

func log(format string, args ...any) {
	log_ = append(log_, fmt.Sprintf(format, args...))
	if len(log_) > 200 {
		log_ = log_[len(log_)-200:]
	}
}

func id(n int) string { return fmt.Sprintf("O-%d", n) }

func describe(p *engine.PlaceIn) string {
	switch {
	case p.Kind == engine.Limit:
		return fmt.Sprintf("%s limit %s %s BTC @ %s USDT", p.ID, p.Side, p.Amount, p.Price)
	case p.Side == engine.Buy:
		return fmt.Sprintf("%s market buy spending %s USDT", p.ID, p.Amount)
	default:
		return fmt.Sprintf("%s market sell %s BTC", p.ID, p.Amount)
	}
}

// narrate turns events into plain-language log lines.
func narrate(evs []engine.Event) {
	for _, ev := range evs {
		if ev.Rejected != nil {
			log("REFUSED %s — %s.", ev.Rejected.OrderID, ev.Rejected.Reason)
		}
		if ev.Placed != nil {
			log("%s is accepted.", describe(&engine.PlaceIn{ID: ev.Placed.ID, Side: ev.Placed.Side, Kind: ev.Placed.Kind, Price: spec.PriceString(ev.Placed.PriceTicks), Amount: amountOf(ev.Placed)}))
		}
		for _, f := range ev.Fills {
			dust := ""
			if f.BuyTurnoverRaw != f.TurnoverRaw {
				dust = fmt.Sprintf(" — buyer's leftover %s USDT was too small for one more lot, absorbed into this fill (dust)", spec.MoneyString(f.BuyTurnoverRaw-f.TurnoverRaw))
			}
			log("FILL #%d: %s BTC @ %s = %s USDT (taker %s %s, maker %s)%s.",
				f.Seq, spec.AmountString(f.AmountLots), spec.PriceString(f.PriceTicks),
				spec.MoneyString(f.TurnoverRaw), f.TakerSide, takerID(f), makerID(f), dust)
		}
		for _, c := range ev.Completed {
			if c.Kind == engine.Market && c.Side == engine.Buy {
				log("%s is done: spent %s USDT for %s BTC.", c.ID, spec.MoneyString(c.TurnoverRaw), spec.AmountString(c.TradedLots))
			} else {
				log("%s is done: traded %s of %s BTC for %s USDT.", c.ID, spec.AmountString(c.TradedLots), spec.AmountString(c.AmountLots), spec.MoneyString(c.TurnoverRaw))
			}
		}
		if ev.Canceled != nil {
			c := ev.Canceled
			if c.Kind == engine.Limit {
				log("%s canceled — %s BTC @ %s USDT leaves the book.", c.ID, spec.AmountString(c.AmountLots-c.TradedLots), spec.PriceString(c.PriceTicks))
			} else {
				log("%s canceled from the market queue.", c.ID)
			}
		}
		if ev.PlateBuy != nil || ev.PlateSell != nil {
			log("Depth published: %d bid levels, %d ask levels.", plateLen(ev.PlateBuy), plateLen(ev.PlateSell))
		}
	}
}

func plateLen(p *engine.Plate) int {
	if p == nil {
		return 0
	}
	return len(p.Items)
}

func takerID(f engine.Fill) string {
	if f.TakerSide == engine.Buy {
		return f.BuyOrderID
	}
	return f.SellOrderID
}

func makerID(f engine.Fill) string {
	if f.TakerSide == engine.Buy {
		return f.SellOrderID
	}
	return f.BuyOrderID
}

func amountOf(p *engine.PlacedOrder) string {
	if p.Kind == engine.Market && p.Side == engine.Buy {
		return spec.MoneyString(p.BudgetRaw)
	}
	return spec.AmountString(p.AmountLots)
}

type step struct {
	Label string
	Cmd   engine.Command
}

func place(id_, side, kind, price, amount string, member string) engine.Command {
	return engine.Command{Place: &engine.PlaceIn{ID: id_, Member: member, Side: parseSide(side), Kind: parseKind(kind), Price: price, Amount: amount}}
}

func parseSide(s string) engine.Side {
	if s == "sell" {
		return engine.Sell
	}
	return engine.Buy
}

func parseKind(s string) engine.Kind {
	if s == "market" {
		return engine.Market
	}
	return engine.Limit
}

type scenario struct {
	Title       string
	Description string
	Steps       []step
}

var scenarios = []scenario{
	{
		Title: "First come, first served",
		Description: "Two sellers offer the SAME price. One buyer arrives and takes 0.0300 BTC. " +
			"Watch which seller fills first: the one who arrived EARLIER (time priority within a price), " +
			"and the buyer pays the sellers' asking price, not a price of its own.",
		Steps: []step{
			{Label: "S1 sells 0.0200 BTC @ 50,000.00 (arrives first)", Cmd: place("S1", "sell", "limit", "50000.00", "0.0200", "alice")},
			{Label: "S2 sells 0.0200 BTC @ 50,000.00 (arrives second)", Cmd: place("S2", "sell", "limit", "50000.00", "0.0200", "bob")},
			{Label: "B1 buys 0.0300 BTC @ 50,000.00 limit", Cmd: place("B1", "buy", "limit", "50000.00", "0.0300", "carol")},
		},
	},
	{
		Title: "Market buy sweeps levels and burns dust",
		Description: "Asks are stacked at 50,000.00 and 50,010.00. A market buy arrives with a 1,010.00 USDT budget: " +
			"it eats the cheap level entirely, one lot of the expensive level, and its last ~5 USDT cannot buy a " +
			"whole 0.0001 BTC lot — the leftover is absorbed into the final fill (legacy dust rule) and the order completes.",
		Steps: []step{
			{Label: "S1 sells 0.0200 BTC @ 50,000.00", Cmd: place("S1", "sell", "limit", "50000.00", "0.0200", "alice")},
			{Label: "S2 sells 0.0300 BTC @ 50,010.00", Cmd: place("S2", "sell", "limit", "50010.00", "0.0300", "bob")},
			{Label: "B1 market buy with 1,010.00 USDT budget", Cmd: place("B1", "buy", "market", "", "1010.00", "carol")},
		},
	},
	{
		Title: "You get the maker's price",
		Description: "A buyer willing to pay 50,050.00 meets a seller asking 50,000.00: the fill happens at the " +
			"RESTING price 50,000.00 — the buyer keeps the improvement. A second buyer bidding below the best ask " +
			"does not trade at all and waits on the book.",
		Steps: []step{
			{Label: "S1 sells 0.0100 BTC @ 50,000.00", Cmd: place("S1", "sell", "limit", "50000.00", "0.0100", "alice")},
			{Label: "B1 buys 0.0100 BTC @ 50,050.00 (willing to overpay)", Cmd: place("B1", "buy", "limit", "50050.00", "0.0100", "carol")},
			{Label: "B2 buys 0.0100 BTC @ 49,990.00 (under the ask)", Cmd: place("B2", "buy", "limit", "49990.00", "0.0100", "dave")},
		},
	},
	{
		Title: "Cancel changes who trades next",
		Description: "B1 bids 49,990.00, B2 bids 49,980.00. A seller matches B1 (best bid) for part. Then B1's order " +
			"is canceled — when the next seller arrives at 49,980.00, it is B2 who fills. Canceling re-prices the queue.",
		Steps: []step{
			{Label: "B1 buys 0.0200 BTC @ 49,990.00", Cmd: place("B1", "buy", "limit", "49990.00", "0.0200", "carol")},
			{Label: "B2 buys 0.0200 BTC @ 49,980.00", Cmd: place("B2", "buy", "limit", "49980.00", "0.0200", "dave")},
			{Label: "S1 sells 0.0100 BTC @ 49,990.00 (meets best bid)", Cmd: place("S1", "sell", "limit", "49990.00", "0.0100", "alice")},
			{Label: "Cancel B1 (0.0100 BTC still waiting)", Cmd: engine.Command{CancelID: "B1"}},
			{Label: "S2 sells 0.0200 BTC @ 49,980.00", Cmd: place("S2", "sell", "limit", "49980.00", "0.0200", "bob")},
		},
	},
	{
		Title: "Wrong sizes are refused at the door",
		Description: "Orders must be exact multiples of the lot size (0.0001 BTC) and prices of the tick size (0.01 USDT). " +
			"Anything else is refused BEFORE touching the book — no partial rounding, no silent trimming.",
		Steps: []step{
			{Label: "Try to buy 0.00005 BTC (half a lot)", Cmd: place("X1", "buy", "limit", "50000.00", "0.00005", "eve")},
			{Label: "Try to bid 50,000.005 (half a tick)", Cmd: place("X2", "buy", "limit", "50000.005", "0.0100", "eve")},
			{Label: "Try to market-sell 0.00015 BTC", Cmd: place("X3", "sell", "market", "", "0.00015", "eve")},
			{Label: "A proper 0.0100 BTC bid @ 50,000.00", Cmd: place("B1", "buy", "limit", "50000.00", "0.0100", "eve")},
		},
	},
	{
		Title: "Trading halt slams the door",
		Description: "While trading is halted, every new order is refused outright (in production the api service " +
			"auto-cancels them back to the member). Resume re-opens the book.",
		Steps: []step{
			{Label: "Halt trading", Cmd: engine.Command{Halt: true}},
			{Label: "Try to place a bid", Cmd: place("B1", "buy", "limit", "50000.00", "0.0100", "carol")},
			{Label: "Resume trading", Cmd: engine.Command{Resume: true}},
			{Label: "Same bid now works", Cmd: place("B1", "buy", "limit", "50000.00", "0.0100", "carol")},
		},
	},
}

type plateRow struct {
	Price  string `json:"price"`
	Amount string `json:"amount"`
	Orders string `json:"orders"`
}

func plateJSON(p *engine.Plate) []plateRow {
	rows := []plateRow{}
	if p == nil {
		return rows
	}
	for _, it := range p.Items {
		rows = append(rows, plateRow{Price: spec.PriceString(it.PriceTicks), Amount: spec.AmountString(it.AmountLots), Orders: "—"})
	}
	return rows
}

type openRow struct {
	ID, Side, Kind, Price, Amount, Traded, Remaining string
	Arrival                                          uint64
}

func stateJSON() map[string]any {
	st := book.State()
	opens := []openRow{}
	for _, o := range st.OpenLimit {
		opens = append(opens, openRow{ID: o.ID, Side: o.Side.String(), Kind: o.Kind.String(),
			Price: spec.PriceString(o.PriceTicks), Amount: spec.AmountString(o.AmountLots),
			Traded: spec.AmountString(o.TradedLots), Remaining: spec.AmountString(o.AmountLots - o.TradedLots), Arrival: o.Seq})
	}
	sort.SliceStable(opens, func(i, j int) bool { return opens[i].Arrival < opens[j].Arrival })
	mkts := []openRow{}
	for _, o := range st.OpenMkt {
		amt := spec.AmountString(o.AmountLots)
		if o.Kind == engine.Market && o.Side == engine.Buy {
			amt = spec.MoneyString(o.BudgetRaw-o.TurnoverRaw) + " USDT left"
		}
		mkts = append(mkts, openRow{ID: o.ID, Side: o.Side.String(), Kind: o.Kind.String(), Price: "market", Amount: amt,
			Traded: spec.AmountString(o.TradedLots), Remaining: amt, Arrival: o.Seq})
	}
	return map[string]any{
		"symbol": spec.Symbol, "halted": st.Halted,
		"tick": "0.01 USDT", "lot": "0.0001 BTC",
		"bids": plateJSON(st.PlateBuy), "asks": plateJSON(st.PlateSell),
		"openLimit": opens, "openMarket": mkts,
		"log": log_, "seq": st.Seq,
	}
}

func apply(cmds []engine.Command) {
	evs := book.Batch(cmds)
	narrate(evs)
}

func main() {
	reset()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(pageHTML))
	})
	http.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stateJSON())
	})
	http.HandleFunc("/api/cmd", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Op     string `json:"op"`
			ID     string `json:"id"`
			Member string `json:"member"`
			Side   string `json:"side"`
			Kind   string `json:"kind"`
			Price  string `json:"price"`
			Amount string `json:"amount"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		switch req.Op {
		case "place":
			apply([]engine.Command{place(req.ID, req.Side, req.Kind, req.Price, req.Amount, req.Member)})
		case "cancel":
			apply([]engine.Command{{CancelID: req.ID}})
		case "halt":
			apply([]engine.Command{{Halt: true}})
			log("Trading HALTED.")
		case "resume":
			apply([]engine.Command{{Resume: true}})
			log("Trading resumed.")
		case "reset":
			reset()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stateJSON())
	})
	http.HandleFunc("/api/scenario", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
			Upto int    `json:"upto"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		for _, sc := range scenarios {
			if sc.Title != req.Name {
				continue
			}
			reset()
			log("Scenario: %s", sc.Title)
			for i := 0; i <= req.Upto && i < len(sc.Steps); i++ {
				log("→ %s", sc.Steps[i].Label)
				apply([]engine.Command{sc.Steps[i].Cmd})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stateJSON())
	})
	http.HandleFunc("/api/scenarios", func(w http.ResponseWriter, r *http.Request) {
		type scDTO struct {
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Steps       []string `json:"steps"`
		}
		var out []scDTO
		for _, sc := range scenarios {
			var steps []string
			for _, s := range sc.Steps {
				steps = append(steps, s.Label)
			}
			out = append(out, scDTO{sc.Title, sc.Description, steps})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
	fmt.Println("PROTOTYPE playground: http://localhost:8090  (Ctrl-C to stop)")
	http.ListenAndServe(":8090", nil)
}

var pageHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>Matching engine prototype — malxchange</title>
<style>
:root{--fg:#1c2733;--muted:#6b7a8c;--border:#d8dee6;--surface:#f6f8fa;--accent:#0b6e99;--success:#1a7f4b;--error:#b3261e}
*{box-sizing:border-box}
body{font:15px/1.55 -apple-system,Segoe UI,Roboto,sans-serif;color:var(--fg);max-width:1180px;margin:24px auto;padding:0 20px}
h1{font-size:22px;margin:0 0 4px}h2{font-size:15px;margin:0 0 8px;color:var(--muted);text-transform:uppercase;letter-spacing:.05em}
.sub{color:var(--muted);margin:0 0 20px}
.cols{display:grid;grid-template-columns:1fr 1fr;gap:20px}
.panel{border:1px solid var(--border);border-radius:8px;padding:14px;background:var(--surface)}
table{border-collapse:collapse;width:100%;font-variant-numeric:tabular-nums}
td,th{padding:3px 8px;border-bottom:1px solid var(--border);text-align:right}
th:first-child,td:first-child{text-align:left}
.ask td:first-child{color:var(--error)} .bid td:first-child{color:var(--success)}
button{font:inherit;border:1px solid var(--border);background:#fff;border-radius:6px;padding:6px 12px;cursor:pointer}
button:hover{border-color:var(--accent);color:var(--accent)}
button.step{display:block;width:100%;text-align:left;margin:6px 0}
button.step[disabled]{opacity:.45;cursor:default}
input,select{font:inherit;padding:5px 7px;border:1px solid var(--border);border-radius:6px;width:100%}
form.inline{display:grid;grid-template-columns:repeat(4,1fr);gap:8px;margin:8px 0}
.row{display:flex;gap:8px;flex-wrap:wrap;margin:8px 0}
#log{font:12.5px/1.6 ui-monospace,Menlo,monospace;max-height:340px;overflow:auto;background:#fff;border:1px solid var(--border);border-radius:6px;padding:10px}
#log .fill{color:var(--success)} #log .rej{color:var(--error)}
.tabs{display:flex;gap:6px;flex-wrap:wrap;margin:10px 0}
.tabs button.on{background:var(--accent);color:#fff;border-color:var(--accent)}
.desc{color:var(--muted);font-size:13.5px}
.badge{font-size:12px;color:#fff;background:var(--error);border-radius:4px;padding:1px 6px;display:none}
label{font-size:12.5px;color:var(--muted);display:block;margin-bottom:2px}
</style></head><body>
<h1>Matching engine prototype <span class="badge" id="haltBadge">HALTED</span></h1>
<p class="sub">Does the ported CoinTrader semantics feel right, on an int64 tick/lot book? BTC/USDT · tick 0.01 USDT · lot 0.0001 BTC · every action below re-renders the full state.</p>

<div class="cols">
<div class="panel"><h2>Asks (sellers waiting)</h2><table id="askT"></table>
<h2 style="margin-top:14px">Bids (buyers waiting)</h2><table id="bidT"></table>
<h2 style="margin-top:14px">Market queue (no price, never shown in depth)</h2><div id="mktQ" class="desc">empty</div></div>
<div class="panel"><h2>What just happened</h2><div id="log"></div></div>
</div>

<div class="panel" style="margin-top:20px"><h2>Free play</h2>
<form class="inline" id="pf" onsubmit="return doPlace()">
<div><label>ID</label><input id="f_id" value="M-1"></div>
<div><label>Side</label><select id="f_side"><option>buy</option><option>sell</option></select></div>
<div><label>Kind</label><select id="f_kind"><option>limit</option><option>market</option></select></div>
<div><label>Price (limit only)</label><input id="f_price" value="50000.00"></div>
<div><label>Amount (BTC — or USDT budget for market buy)</label><input id="f_amount" value="0.0100"></div>
<div style="grid-column:span 3"><label>&nbsp;</label><button type="submit">Place order</button></div>
</form>
<div class="row">
<button onclick="api({op:'halt'})">Halt trading</button>
<button onclick="api({op:'resume'})">Resume</button>
<button onclick="api({op:'reset'})">Reset book</button>
<span style="align-self:center" class="desc">Cancel: click an order below</span>
</div>
<h2 style="margin-top:10px">Open limit orders</h2><table id="openT"></table>
</div>

<div class="panel" style="margin-top:20px"><h2>Guided walkthroughs</h2>
<div class="tabs" id="tabs"></div>
<p class="desc" id="scDesc"></p>
<div id="steps"></div>
</div>

<script>
let scs=[], cur=null, upto=-1;
async function api(body){const r=await fetch('/api/cmd',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});render(await r.json());}
async function loadScenarios(){scs=await (await fetch('/api/scenarios')).json();const t=document.getElementById('tabs');
scs.forEach((s,i)=>{const b=document.createElement('button');b.textContent=s.title;b.onclick=()=>show(i);t.appendChild(b);});
t.children[0].classList.add('on');cur=0;upto=-1;document.getElementById('scDesc').textContent=scs[0].description;renderSteps();refresh();}
async function show(i){document.querySelectorAll('#tabs button').forEach((b,j)=>b.classList.toggle('on',j===i));cur=i;upto=-1;
document.getElementById('scDesc').textContent=scs[i].description;renderSteps();
await fetch('/api/scenario',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:scs[i].title,upto:-1})});
refresh();}
function renderSteps(){const s=scs[cur],el=document.getElementById('steps');el.innerHTML='';
if(!s||!s.steps)return;
for(let i=0;i<s.steps.length;i++){const b=document.createElement('button');b.className='step';
b.textContent=(i+1)+'. '+s.steps[i];b.disabled=i>upto+1;b.onclick=()=>step(i);el.appendChild(b);}}
async function step(i){upto=i;const r=await fetch('/api/scenario',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:scs[cur].title,upto})});render(await r.json());renderSteps();}
function esc(s){return String(s).replace(/[&<>"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]))}
function plate(el,rows){el.innerHTML=rows.length?'<table><tr><th>price (USDT)</th><th>amount (BTC)</th></tr>'+
rows.map(r=>'<tr><td>'+esc(r.price)+'</td><td>'+esc(r.amount)+'</td></tr>').join('')+'</table>':'<span class="desc">empty</span>'}
function render(st){
plate(document.getElementById('askT'),st.asks);plate(document.getElementById('bidT'),st.bids);
document.getElementById('haltBadge').style.display=st.halted?'inline':'none';
const mq=document.getElementById('mktQ');mq.innerHTML=st.openMarket.length?st.openMarket.map(o=>esc(o.id+' · '+o.side+' market · '+o.amount)).join('<br>'):'empty';
const log=document.getElementById('log');
log.innerHTML=(st.log||[]).slice(-60).reverse().map(l=>{const cls=/FILL/.test(l)?'fill':(/REFUSED|HALT/.test(l)?'rej':'');return '<div class="'+cls+'">'+esc(l)+'</div>'}).join('');
const ot=document.getElementById('openT');
ot.innerHTML=st.openLimit.length?'<table><tr><th>id</th><th>side</th><th>price</th><th>remaining</th><th></th></tr>'+
st.openLimit.map(o=>'<tr><td>'+esc(o.id)+'</td><td>'+esc(o.side)+'</td><td>'+esc(o.price)+'</td><td>'+esc(o.Remaining||o.remaining)+'</td><td><button onclick="api({op:\'cancel\',id:\''+esc(o.id)+'\'})">cancel</button></td></tr>').join('')+'</table>':'<span class="desc">none</span>';
}
async function refresh(){render(await (await fetch('/api/state')).json());}
function doPlace(){api({op:'place',id:f_id.value,side:f_side.value,kind:f_kind.value,price:f_price.value,amount:f_amount.value});return false}
loadScenarios();
</script>
</body></html>`
