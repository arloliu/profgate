# Profgate：功能缺口與 over-engineering 審視

日期：2026-08-27
範圍：`main` 上的四份 Accepted spec、`internal/`、`cmd/`、`deploy/`、`docs/`。
方法：operator 視角、AI agent 視角、實作複雜度三個獨立審視，彙整後抽查關鍵主張；部分項目採納自外部審視。

## 基準數據

| | 行數 |
|---|---|
| 產品 Go 程式碼 | 14,966 |
| 測試 Go 程式碼 | 37,815（2.53 : 1） |
| Markdown | 22,184（specs 8,763、plans 5,850、superseded design 2,590） |
| 「宣稱的核心」（k8s + proxy + admit + ops + tlscert） | ~1,180 |
| PGO + natskv | 5,435（36%），預設關閉 |

---

## 一、欠缺的功能

### A. 從 human operator / SRE 視角

依痛感排序。
「處置」欄註明 spec 是否已宣告為 non-goal / deferred。

| # | 缺口 | 證據 | 處置 | 痛感 |
|---|---|---|---|---|
| 0 | **Auth 與 UI 尚未發布**：都在 `Unreleased`，最新 tag `v0.3.0` | `CHANGELOG.md:8`, `git tag` | — | 高，外部使用者拿不到 |
| 1 | **`oidc` 模式下 `go tool pprof` 完全不能直接用**；需 `curl -H Bearer` 存檔再開；Keycloak 預設 ID token 5 分鐘 | `docs/api.md:605`, `docs/authentication.md:103-111` | deferred：`profgate` CLI device-code flow「另有設計文件」（`docs/specs/auth.md:81-86`），尚不存在 | 高，每日 |
| 2 | **Chart 沒有 Ingress template**，但 values/README/NOTES 都要求 operator「自己的 Ingress」放行 `/ui/`、`/auth/`、`/` | `deploy/chart/profgate/templates/` 無 ingress；`values.yaml:291`, `NOTES.txt:71` | 未提及 | 高，幾乎每個實際安裝都需要 |
| 3 | **Config 無 hot-reload**；改 realm/ACL 即 rollout。chart 用 checksum annotation 強制重啟掩蓋此點 | `docs/configuration.md:123`, `values.yaml:56-62` | deferred（`docs/specs/gateway.md:68`） | 高 |
| 4 | **無 ServiceMonitor/PodMonitor、PrometheusRule、Grafana dashboard**；ops port 刻意不在 Service 內，24 個 metrics 沒東西刮；docs 說 `jwks_age_seconds` 是「alertable form」卻沒 alert | `values.yaml:41-42`, `docs/deployment.md:409` | 未提及 | 中高 |
| 5 | **Console 只讀，PGO 寫入（start/cancel/policy）都要手寫 `curl` + `If-Match` ETag** | `docs/console.md:121-124` | deferred（`docs/specs/ui.md:75-77`） | 中 |
| 6 | **無 per-Service pprof port annotation 自動探索**；異質 port 的 fleet 得放寬全域 allowlist 或每次帶 `port=` | `internal/k8s/` 無任何 annotation 讀取 | 只在 superseded design 的 open question（`profgate-design.md:2524`） | 中 |
| 7 | **無「該 Service 最新 artifact」端點**；且 `artifact.retention` 預設 2h vs `jobRetention` 168h → record 比 artifact 多活 84 倍，「下載上週的 PGO profile」不可能 | `config.go:218`, `docs/configuration.md:364` | 長期儲存是 non-goal（`pgo.md:69`），但預設值失衡未被討論 | 中 |
| 8 | **Rate limit 皆為全域/每 replica，無 per-principal**；一個使用者可吃光 16 個 profile slot | `limits.maxConcurrentProfiles` 等三個 limiter | 未提及 | 中 |
| 9 | Session 在 `exp` 前無法撤銷（最長 24h） | `docs/authentication.md:113-118` | deferred（`auth.md:90`） | 中 |
| 10 | Chart 缺 HPA、`priorityClassName`、`extraVolumes`、`resources.requests` 預設 | `values.yaml:83-96` | 未提及 | 低中 |
| 11 | 無 tracing（無 otel import）、無 multi-cluster、無 mTLS client-cert auth | grep 無 otel；`k8s/client.go:34-40` 單叢集 | tracing/multi-cluster 未提及；mTLS deferred | 低中 |
| 12 | **`/targets` 回空時無法診斷原因**；caller 分不出無 Ready Pod、selector 不符、named port 不存在、cache 未同步，UI 空狀態會被當成系統故障 | `internal/k8s/eligibility.go` 只回合格者 | 未提及 | 中高 |
| 13 | Profile diff/compare：兩次下載無關聯 ID、無「same pod, then/now」 | — | non-goal（`ui.md:70`） | 低，但工作流缺口真實 |

第 12 項的解法是 `?explain=true` 回聚合原因與數量，不揭露 Pod identity，API、CLI、UI 共用同一份答案。

HTTPS 額外摩擦：port-forward 時憑證是 DNS name，`curl` 要 `--resolve`，`go tool pprof` 沒有等價選項（`docs/api.md:44-46`）。

### B. 從 AI agent 視角（用它來自主診斷 / 抓 PGO）

已具備、不必再提的：JSON error `{error, code}` + 39 個穩定 code、`/v1/namespaces`、`/v1/limits`、`/v1/whoami`（含 pgo 權限 flag）。
另有 Bearer JWT 非互動認證、JSON audit log、ETag/If-Match。
這對 agent 相當友善。

真正缺的（spec 未宣告處置）：

1. **無 request/correlation id**（audit record 無 id、無 `X-Request-Id`）— 拿到 `503 target_changed` 後無法把重試對到 operator 的 log。
2. **Collection create 無 `Idempotency-Key`**（`429 collection_in_progress` 部分緩解）。
3. **Collection 完成只能 busy-poll**：無 `?wait=`、無 long-poll、無 webhook。
   Collection 動輒數分鐘，這是 agent ergonomics 最大缺口。
4. **`limit_exceeded` 的欄位名只在 message 字串**（`pgo_policy.go:312-323`），而 docs 說 message 可能變動 → agent 得 regex 人類文字。
   內部 `pgo.Violation` 本來是結構化的，輸出時被壓平。
5. Ops listener 回 `text/plain`（`ops.go:19,27`），與 `/v1` 的 JSON envelope 不一致。
6. Collections listing 無 `state=`/`since=` 過濾。
7. 無 OpenAPI / JSON schema、無 CLI（docs 明言無，但對 agent 產生 client 而言仍是缺口）。

Spec 已明確 non-goal 的：文字/top-N 渲染、diff、pagination（cap 100）、CORS、API key。

### C. 文件對不上實作

- `docs/api.md:52,86,690` 三處說 `/v1` 有「七條路由」；實際有 11 條（`server.go:50-56,213-217`）。
  error table 的 `route_unknown` 定義因此是錯的。
  這是唯一找到的實質文件缺陷；console 的每項承諾都在 `app.js` 驗證到。

---

## 二、Over-engineering

### 排序（依可刪行數）

| # | 機制 | 位置 | 行數 | 評估 |
|---|---|---|---|---|
| 1 | **PGO 多 replica 無 leader 協調**：lease/claim/renew、slot racing + jitter、4 個 KV watch cache、7 道 sweeper pass | `internal/pgo/*` + `internal/natskv/*` | 5,435（可省 ~4,000） | 見下方討論 |
| 2 | **自製 OIDC stack**：discovery、JWKS 雙路徑 refresh + 雙鎖、自訂 HTTP client、驗證 | `auth/{discovery,jwks,issuer,verify,oidc}.go` | 1,097（可省 ~700） | `coreos/go-oidc` 可替代，保留 `jwks.go:222-289` 的 key 硬化（min RSA 2048、alg↔curve、重複 kid）— 那 65 行是真價值 |
| 3 | **Cookie 手寫 uint16 length-prefix 二進位框架**（含 `panic`）+ 雙 key 輪替 + 30s file poller + fingerprint gauge | `cookie.go:200-274`, `:39-98`, `poll.go`, `metrics/prometheus.go:121` | ~255 | AES-GCM seal 本身 50 行 stdlib 沒問題；框架換 `json.Marshal`，8h TTL 的 session 用「重啟即失效」即可 |
| 4 | **Config 表面積**：~81 個 leaf key、60 個 env override；12 個 `pgo.limits.*` 幾乎只為 chart 記憶體算式存在 | `config/config.go` 997 行，docs 661 行 | 可省 ~200 | 12 個 key 在 `config_test.go` 無驗證覆蓋 |
| 5 | **NATS preflight 探針**：每個 bucket 寫/watch/刪 probe key 與 object，再由 sweeper 專門清 probe | `natskv/preflight.go:40-284`, `sweeper.go:370` | ~330 | 第一次真實 Put 就會驗到同樣的事 |
| 6 | **`versionPolicy` 單值 enum** `oneof=strict`，穿過 `pgo/policy.go` 五處 | `config.go:270` | 小 | 純推測性泛化 |
| 7 | **Crockford base32 手刻 ID**（69 行）給沒人會手抄的 collection id | `pgo/id.go` | 69 | `hex` + `crypto/rand` 一行 |
| 8 | 23 個 auth audit reason 常數 + 為測試一致性而存在的 `Reasons()`；全 repo ~76 個 outcome 字串常數 | `auth/auth.go:48-77` | — | 可讀性尚可，但維護面大 |
| 9 | **Port 選擇設定模型**：`port`/`portName`/`allowedPorts`/`allowedPortNames` 四個旋鈕、空 allowlist 等於全開、無法直接禁用 `portName` | `docs/configuration.md:68-97` | — | 縮成單一 `allowedSelections` + `allowAny`，default-deny |
| 10 | **UI asset tree hash**：對整棵 asset tree 做 SHA-256，rollout 期間 HTML/asset 可能跨 build | `internal/ui/ui.go:163-180`, `docs/console.md:107-112` | ~60 + 測試 | 穩定路徑 + ETag + `no-cache` |
| 11 | 8 個獨立 timer/backoff 迴圈（k8s preflight、OIDC discovery、watch reopen、JWKS ×2、cookie poll、tls poll、lease renew、sweeper） | 散佈 | — | 個別合理，合起來難推理 |

第 2 項的一個常見反駁是「auth 對外 seam 小，所以不是 over-engineering」。
seam 小是事實，但不代表內部 1,097 行自製 transport 是必要的；兩者不衝突。

第 3 項只主張換掉手寫框架。
雙 key 輪替、file poller、fingerprint gauge 成本低，屬低優先，可保留。

第 9 項要動，得連同 `AGENTS.md` 的 permission invariant 一起改：原文就寫著「allowlist 為空時接受任何 port」，`docs/specs/gateway.md` 同步修訂。

### 測試與文件面的過重

- `deploy/chart_test.go` 2,518 行 Go 測 748 行 YAML 的 chart；`helm template` + golden file 即可。
- `test/e2e/harness_test.go` 1,607 行 / 58 個 func。
  ADR `docs/decisions/e2e-without-framework.md` 以「幾百行」為前提且寫明「超過一次讀完的量就 revisit」— 觸發條件已成立，未 revisit。
  lane 機制本身（~230 行）不重。
- Fixtures：`pgo/fixtures_test.go` 1,931 + `httpapi/fixtures_test.go` 1,485。
  `cmd/profgate/serve_test.go` 2,251 行對 `serve.go` 756 行。
- `internal/ui` 測試比 4.3 : 1，其中 ~750 行在審計 vendored JS。
- **`docs/specs/profgate-design.md` 2,590 行 `Status: Superseded`**，是 repo 最大檔。
  **五份 `Status: Done` plan 共 5,850 行**留在樹內。
  這 8.4k 行是 git 已保存的歷史，卻與 8.7k 行活 spec 競爭 grep 結果。
- 單一實作的 interface：`httpapi.Upstream`、`k8s.Runtime`、`auth.AuthRoutes`（僅 `*OIDC`，存在只為讓 `Deps.AuthRoutes` 可為 nil）。
  其餘 interface（`Authenticator` ×3、`Recorder`、`KV`、`Clock`）都有多實作，合理。

### 關於 #1 的持平說法

- NATS 作 lease store 是 **no-write-RBAC 不變量逼出來的**（`coordination.k8s.io/Lease` 需 create/update），不是設計錯誤。
- 真正的問題在上一層：chart `replicaCount: 2` + 「每個 replica 跑三個迴圈、無 standing leader」（`docs/pgo.md:277`）。
  PGO 是 6h 一次的背景批次。
  若 PGO 跑在單獨的 1-replica Deployment，lease/claim/slot race/orphan sweep/四個 watch cache 全部可拆。
  artifact 只需 ~200 行 Object Store wrapper。
- 但這與 Accepted 的 `docs/specs/pgo.md` 決策相衝（spec 選了對稱 replica 以避免 leader failover）。
  要動它必須開 spec 修訂，不是重構。
- 較可落地的路線是拆分而非刪除：保留 concurrency correctness，把 collector 拆成可選 deployment/subcommand。
  再提供 small/standard/large preset，把 lease、grace period、memory budget 收進內部。
  `profgate config validate` 範例算出 122465 秒 grace 與 4 GiB（`docs/configuration.md:459-468`）是內部正確性成本外溢的證據。
- 營運成本不在行數裡：operator 得預建 JetStream cluster、三個 store（精確的 `nats kv add` 參數）、NKey 帳號。
  「NATS 掛了會擋升級」（`docs/deployment.md:290`）對一個 build 最佳化功能是很大的依賴。

### 不是 over-engineering（合理的複雜度）

- `admit/gate.go` 53 行 semaphore + `sync.Once` 防重複 release。
- `tlscert/loader.go` poll-and-hash 而非 fsnotify，理由寫在 `:143-152`（kubelet symlink 換 inode）。
- `proxy/proxy.go` 的 pinned transport、`DisableCompression`、header/body 分離 deadline — 60s streaming CPU profile 必需。
- `metrics/prometheus.go` 12 個 vec，label 全有界，**沒有 namespace/service/pod label** — cardinality 紀律良好。
- `k8s/eligibility.go` + `confirm.go` 就是「只從 ready、未終止 Pod 取 profile」的實體。
- `httpapi/realm.go` 67 行 ACL matcher。
- `scripts/check-repo.py` 的 import-boundary 檢查支撐安全不變量，非冗餘。
- `.agents/rules/` 是 trigger map 不是文件複本；docs 三層生命週期紀律清楚，`docs/api.md` 與 spec 的重疊是刻意的讀者分流。

---

## 三、建議優先序

這份排序已全部執行或撤銷，結果是 `v0.5.0`（見 `CHANGELOG.md`）；
承載它的 roadmap 已離開樹，
依 [`finished-documents-leave-the-tree.md`](../decisions/finished-documents-leave-the-tree.md) 以 `git` 取回。

1. 修 `docs/api.md` 「七條路由」；發布 `v0.4.0`（Auth + UI）。
2. Chart 補 Ingress、PodMonitor、PrometheusRule、`resources.requests`。
3. Port 選擇改 default-deny（修訂 gateway spec 與不變量文字）。
4. `profgate` CLI + OIDC device-code login（新 spec）。
5. Target explain 診斷（API + CLI + UI 共用）。
6. Machine contract：`X-Request-Id`、結構化 error details、`Idempotency-Key`、collection `?wait=`、latest artifact、filters、OpenAPI。
7. UI：PGO start/cancel、browser-level 測試、asset 改穩定路徑。
8. 小型減重：`versionPolicy`、`pgo/id.go`、cookie 框架、NATS preflight 探針；`chart_test.go` 改 golden file；revisit e2e harness。
9. 文件減重：superseded design 與 Done plan 移出樹（需一份 decision record 修改 `docs/README.md` 的生命週期政策）。
10. OIDC transport 改 `go-oidc` — 已撤銷。
    逐函式比對見 [`2026-08-28-oidc-library.md`](2026-08-28-oidc-library.md)：替換刪不掉任何一行，反而多寫約 80 行 glue 與一個 module。
11. PGO 拆分為可選 deployment + preset（修訂 pgo spec）。
