# Content contract — api ⇄ admin-api (ticket #45)

The member content surfaces (announcements, help) are owned by
admin-api (spec #25) and served read-only by api over internal RPC.
This document is the wire contract BOTH sides implement: api's client
(internal/api/content.go) is tested against it, and admin-api (#25)
serves it. Shape sources: the legacy CMS pages (Notice/NoticeItem,
Help/HelpList/HelpDetail — `research/frontend-inventory.md`) reduced to
English-only v1 (ADR-0013: no `lang`, no `titleCN`/`titleEN`).

## Addressing

- api calls `MAL_API_ADMIN_BASE_URL` (env, default empty = proxy off)
  + the paths below — e.g. `http://admin:8080/internal/content/help`.
  In compose the admin service is reachable at `http://admin:8080`.
- The paths carry the `/internal/content` prefix as SERVED (handlers on
  the admin service's internal mux register without the `/internal`
  prefix, per the boot harness's StripPrefix convention).
- Transport: plain HTTP on the compose network; the network boundary
  is the auth (Caddy never proxies `/internal`).

## Member routes served by api (public, anonymous — story 26)

Caddy proxies `/api/*` to api:8080 without stripping, so these patterns
carry the prefix on the api listener.

| Route | Answers |
|---|---|
| `GET /api/announcements?pageNo=&pageSize=` | announcement page |
| `GET /api/announcements/{id}` | announcement detail |
| `GET /api/help` | help categories |
| `GET /api/help/categories/{cate}?pageNo=&pageSize=` | category's article page |
| `GET /api/help/articles/{id}` | help article |

Pagination: `pageNo` default 1 (≥ 1), `pageSize` default 10 (1–50).
Outside those bounds api answers 400 without calling upstream; inside,
the values are forwarded verbatim.

## admin-api internal routes (what #25 implements)

| Route | Answers |
|---|---|
| `GET /internal/content/announcements?pageNo=&pageSize=` | announcement page |
| `GET /internal/content/announcements/{id}` | announcement detail, 404 if unknown |
| `GET /internal/content/help` | help categories |
| `GET /internal/content/help/categories/{cate}?pageNo=&pageSize=` | category's article page |
| `GET /internal/content/help/articles/{id}` | help article, 404 if unknown |

Statuses: `200` with the document, `404` for an unknown id (body
ignored by api). Anything else — or a transport/decode failure — is
DEGRADED (below). A `304`/etag model does not exist; no caching is
negotiated.

## JSON shapes

`id` values are opaque strings (admin-api mints them; the legacy
numeric ids do not survive). `createTime` is RFC3339 UTC. `content`
fields are the CMS-stored HTML, passed through literally (no
JSON-escaping of `<`/`>`/`&` beyond encoding). Arrays are never null:
empty is `[]`. There is no `{code,message,data}` envelope — success
bodies are the bare documents.

**Announcement page** (listings, both services):
```json
{"content":[
  {"id":"a-2","title":"Second maintenance window","createTime":"2026-10-12T08:00:00Z"},
  {"id":"a-1","title":"Listing announced","createTime":"2026-10-10T12:30:00Z"}
],"totalElements":2}
```

**Announcement detail** (info = the document; back/next = adjacent
summaries for prev/next navigation, null at the ends):
```json
{"info":{"id":"a-2","title":"Second maintenance window","createTime":"2026-10-12T08:00:00Z",
  "content":"<p>Read-only mode on Sunday.</p>"},
 "back":{"id":"a-1","title":"Listing announced","createTime":"2026-10-10T12:30:00Z"},
 "next":null}
```

**Help categories** (the help-center overview; cate is the category id,
a non-empty opaque string — the legacy numeric 0..5 shape, as strings):
```json
[{"cate":"0","title":"Getting started",
  "items":[{"id":"h-1","title":"How to deposit","createTime":"2026-10-01T09:00:00Z"}]},
 {"cate":"1","title":"Fees","items":[]}]
```

**Help article** (cate repeats the owning category):
```json
{"id":"h-1","title":"How to deposit","createTime":"2026-10-01T09:00:00Z",
 "content":"<p>Deposits credit after the Coin's confirmation count.</p>","cate":"0"}
```

**Member error envelope** (api's public surface, on 4xx):
```json
{"message":"announcement not found"}
```
(`help article not found`; the pagination 400 names its bounds.)
English complete sentences per ADR-0013.

## Degradation (ticket #45's binding rule)

An unconfigured (`MAL_API_ADMIN_BASE_URL` empty) or failing upstream is
indistinguishable from an empty content set:

- listings answer **200** with the empty page (`{"content":[],"totalElements":0}`),
  the help overview answers `[]` — the front renders empty states, never errors;
- details answer **404** `{"message":"announcement not found"}` /
  `{"message":"help article not found"}`;
- the failure logs ONE warning per state change (fall → warn, recovery
  → info), never per request.

Unknown-but-reachable ids (upstream 404) answer 404 identically to the
degraded case — a read-only CMS cannot distinguish them and need not.

## v1 scope notes

- No `lang` parameter anywhere (English-only, ADR-0013).
- No pinned/top articles (the legacy HelpDetail sidebar's 置顶 concept
  is cut; the help overview's items are the category's articles).
- No search, no admin writes on this surface — members only read.
