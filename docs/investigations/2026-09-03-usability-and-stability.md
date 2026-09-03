# Profgate：易用性、operator 完整度與穩定性審視

日期：2026-09-03
範圍：`main` 於 `v0.5.0` 之後（`f542096`）：五份 Accepted spec、`internal/`、`cmd/`、`deploy/`、`docs/`、`.github/`。
方法：十個獨立面向並行審視，各自只讀、各自舉證——
安裝旅程、設定介面、CLI、HTTP 契約、console、可觀測性、測試健康、互動路徑穩定性、PGO 穩定性，
以及一個外部模型對穩定性的獨立審視。
安裝、CLI、console 三個面向對著執行中的 kind 叢集實測；
穩定性面向以拋棄式 `-race` 測試重現主張；
測試健康面向跑了基線、三次重複與 32 核飽和。
之後兩道驗證：每個 `file:line` 引用逐條核對；外部審視獨有的主張由第二個內部審視裁決，結果記在第十節。

這次不找新功能。
四個問題：operator 不讀 Go 原始碼能否完成工作；
安裝、設定、觀測、診斷、復原有沒有缺口；
名稱、預設值、訊息符不符合直覺；
程式是否持續運作、失敗時出聲而非默默出錯。

## 基準數據

| | 數值 |
|---|---|
| 產品 Go 程式碼 | 22,804 行 |
| 測試 Go 程式碼 | 63,798 行（2.80 : 1） |
| Markdown | 17,175 行（specs 11,082） |
| 單元測試 | 基線 1m16s、`-race -count=3`、32 核飽和 1m35s：全綠，零 flake |
| Statement coverage | 89.4%，無套件低於 86% |

## 一、一句話總結

`v0.5.0` 的核心機制經得起檢視：
CAS 與 lease 紀律、store-generation barrier、admission slot 釋放、每條 reload path 的 last-good、
metric label 的有界性、OpenAPI ↔ route table ↔ code registry 的三方機械對照，都在讀碼與測試下站得住。
問題集中在另一層：**機制正確，但 operator 看不到、找不到、或第一次就踩到。**
安裝的第一條 kustomize 指令在乾淨叢集失敗；
CLI 沒有一個 client verb 回應 `--help`；
console 的 Start 按鈕擋不住連點；
三個關鍵 gauge 在最需要的時刻讀不到或讀錯；
deployment.md 沒有故障排除章節。
另有兩個已重現的穩定性缺陷：慢速 client 佔住 admission slot 超過 budget，
以及 artifact 下載被 5 秒 call timeout 整段截斷。

## 二、安裝與升級

依 operator 第一次遇到的順序。
「證據」欄的指令都實際跑過。

| # | 發現 | 證據 | 修法 | 痛感 |
|---|---|---|---|---|
| 1 | **`kubectl apply -k deploy/base` 在乾淨叢集失敗**：base 沒有 Namespace 物件，卻在每個 namespaced 資源硬寫 `namespace: profgate`；server dry-run 七個資源拒五個 | `docs/deployment.md:44`；`grep -rn "kind: Namespace" deploy/base/` 無結果 | docs 或加一個 Namespace 物件 | 高 |
| 2 | **chart 的 `memoryLimitWithoutPGO` 防呆只在 PGO 開啟的分支生效**；`_helpers.tpl` 宣稱壞值「fails at render time」，但預設分支 `--set memoryLimitWithoutPGO=512` 渲染出 `memory: 512`（512 bytes），Pod 立即 OOMKill | `deploy/chart/profgate/templates/_helpers.tpl:252-272`，PGO 關閉分支照抄值 `:302-308`；`deploy/chart_test.go:613-616` 只測 PGO 開啟 | small-code | 高 |
| 3 | chart 對 `ingress.enabled` 缺 host、`pgo.enabled` 缺 `nats.url` 都在 render 時拒絕，但 `auth.mode=basic` 沒有 user、`auth.mode=oidc` 沒有 issuer 卻渲染出一個 CrashLoop 的 Deployment | `helm template --set auth.mode=basic` rc=0，之後 `profgate config validate` rc=2 `auth.basic: at least one user is required` | small-code：同一種 render-time 拒絕 | 中 |
| 4 | 0.5.0 的 port allowlist 從「空即全開」改為「空即只允許預設」，是 breaking change；spec 與 CHANGELOG 都寫了，但 README、deployment.md、chart README 沒有一處連到 CHANGELOG，升級章節也沒提 | `docs/specs/gateway.md:2184`、`CHANGELOG.md:79-92`；`grep -rn CHANGELOG docs/*.md README.md deploy/chart/profgate/README.md` 無結果；`docs/deployment.md:557-584` | docs-only | 中 |
| 5 | `ingress.enabled` + `tls.enabled` 渲染出一個每個請求都失敗的 Ingress；backend-protocol annotation 的警告只在 values.yaml 與 deployment.md，NOTES.txt 不提 | `deploy/chart/profgate/values.yaml:69-74`、`docs/deployment.md:118-124`；rendered NOTES 只印路由 | docs-only（NOTES） | 中 |
| 6 | 根 README 不連 `docs/cli.md`，也沒說 binary 同時是 client；quickstart 沒有任何方式發現 `<ns>` 與 `<svc>` | `README.md:81-88`；`grep -n cli.md README.md` 無結果 | docs-only | 中 |
| 7 | `basic` 模式的 NOTES.txt 說「Every request needs a user from the list below」然後印 realm 而非 user；`auth.basic.users` 為空時不出聲 | `deploy/chart/profgate/templates/NOTES.txt:17-19,28,47-49` | docs-only | 低 |
| 8 | kustomize base 與 chart 的記憶體不一致：base 在 PGO 關閉時保留 1536Mi，chart 512Mi；base 的註解以現在式說「this base's ConfigMap sizes」一個被註解掉的 PGO 區塊 | `deploy/base/deployment.yaml:53-57`；`deploy/deploy_test.go:780-807`；`docs/deployment.md:390` | docs-only | 低 |
| 9 | base 釘 `image: ...:latest`，隱含 `imagePullPolicy: Always`；沒有文件提醒 pin | `deploy/base/deployment.yaml:31` | docs-only | 低 |
| 10 | chart 在 PGO 開啟時把 `limits.memory` 渲染成原始 byte 數，關閉時渲染成 quantity | `helm template`：`memory: 1610612736` vs `memory: 512Mi` | small-code | 低 |

沒問題、不要動的：
OCI quickstart 真的能拉到 chart 0.5.0；三條 quickstart `curl` 對預設安裝原樣可用，且 Service 不存在時的 404 說出缺了什麼；
`values.yaml` 每個 key 都說明預設值的理由與反向設定的代價，是全 repo 的標準；
chart 的 boolean helper 紀律（`_helpers.tpl:74-79`）；PGO 開啟時的 NOTES.txt。

## 三、設定介面

Key inventory 兩個方向都乾淨（103 個 struct tag 對 docs 無漂移，47 個 default 全部一致），文件章節順序等於 struct 順序。
問題在回饋。

| # | 發現 | 證據 | 修法 | 痛感 |
|---|---|---|---|---|
| 1 | **`config validate` 在 `pgo.enabled: false` 時仍印出 PGO 尺寸的容器記憶體**：印 `container memory bytes: 1610612736`，chart 渲染 `512Mi`，差三倍；`GatewayMemoryBytes()` 從不讀 `PGO.Enabled`，`chart_test.go` 已經在扣掉 working set 繞過它 | `internal/config/config.go:541-556`；`deploy/chart_test.go:604`；`docs/configuration.md:437`、`deploy/chart/profgate/README.md:120-123` | small-code；程式是 bug，不是文件 | 高 |
| 2 | **「完整設定範例」不是出貨預設值，而下方的句子說它是**：三個 `pgo.limits` 值高於預設；載入它印 4831838208，同一份文件的 `config validate` 範例印 1610612736 | `docs/configuration.md:670-673` vs `:400-411`；宣稱在 `:706`；`:583` | docs-only | 高 |
| 3 | 八個有文件的 `PROFGATE_AUTH_OIDC_{BROWSER,CLI}_*` 環境覆寫，在檔案沒有對應區塊時被靜默丟棄；不存在的 `cookieKeyFile` 路徑 exit 0。value 型的區塊（如 `mapping`）不受影響 | 設 `PROFGATE_AUTH_OIDC_CLIENT_ID` 等三個、無 `browser:` 區塊，exit 0；`docs/configuration.md:22-31` 沒寫這條規則 | docs-only 或 small-code | 中 |
| 4 | YAML decode 錯誤露出 Go 型別名稱而非 key path，且漏掉其他錯誤都帶的檔案路徑：`field logLevl not found in type config.ServerConfig`；`cannot unmarshal !!str \`60s\` into int` 連 key 都沒有 | `docs/configuration.md:14-17` 承諾檔名與出錯的 key | small-code | 中 |
| 5 | `discovery.pprof.port: 0` 被接受並靜默視為「未設定」，文件卻說範圍 1 到 65535；`port: 0` 與 `portName` 並存 exit 0，`port: 6060` 並存則拒絕 | `internal/config/config.go:87`（`min=0`）、`:722` | small-code | 中 |
| 6 | 四個出貨預設值正好坐在自己的上限上，收緊任何一個上限都得同時改第二個 key 才能啟動；`pgo.limits.maxRetention` 文件寫最大 `720h`，在出貨 `jobRetention` 下可達的最大值是 `167h` | 四個重現案例在審視紀錄中 | docs-only | 中 |
| 7 | 單位不一致：`limits.cpuSeconds`、`traceSeconds` 是裸整數，其他時間值都是 duration 字串；`cpuSeconds: 60s` 的錯誤訊息見上列 4 | `internal/config/config.go` | docs-only | 低 |
| 8 | 拼錯的 `PROFGATE_` 變數靜默忽略（`PROFGATE_LOG_LEVL=verbose` exit 0），拼錯的檔案 key 則拒絕 | `internal/config/config.go:566-570` 只手動查兩個已移除的名字 | docs-only | 低 |
| 9 | `realms.<name>.profiles: invalid entry "memory"` 不列出八個合法名稱，`logLevel` 的同型錯誤有列 | 對照案例 | small-code | 低 |
| 10 | `pgo.configAPI` 是字串 enum（`enabled`/`disabled`），旁邊的 `pgo.enabled`、`ui.enabled` 是 boolean | `internal/config/config.go` `PGOConfig` | 記錄，不改（breaking） | 低 |
| 11 | `profgate config valdiate` 印全部 18 個 verb 的 usage 而不指出錯在哪；`--help` exit 2 並印 `flag: help requested` | `cmd/profgate/main.go:59-62` | 併入第四節 | 低 |

不要動的：
cross-key 錯誤訊息（`pgo.defaults.artifact.retention 24h0m0s must be at most pgo.limits.maxRetention 12h0m0s`）是其他訊息該追上的標準；
unknown key 在每一層都拒絕，包括 realm 的 `pgo` 區塊；
已移除的 key 由兩套獨立機制按名拒絕並附遷移文字；
`config validate` 不需要叢集、serviceaccount 檔或網路。

## 四、CLI

20 項發現，全部有可重跑的指令。
Exit code 與 `docs/cli.md:443-453` 逐行相符，`--output json` 在每個讀取 verb 上都是回應 body 的逐位元組複本，失敗請求不留部分檔案——這些不要動。

| # | 發現 | 證據 | 修法 | 痛感 |
|---|---|---|---|---|
| 1 | **`profgate auth hash --help` 印 `Password:` 然後永遠等 stdin**；參數從未解析 | `cmd/profgate/auth.go:28-40` 忽略 `args[1:]`，無條件 `readPassword` | small-code | 高 |
| 2 | **沒有任何 client verb 回應 `--help`**：`--help` 當成未知 flag，露出 Go 內部字串 `flag: help requested`，exit 2。47 個 flag 註冊中只有 `serve` 與 `config validate` 的會被印出，十個全域 flag（`--server`、`--output`、`--token-file`…）在 binary 任何輸出中都不存在 | `cmd/profgate/client.go:300` `fs.SetOutput(io.Discard)`；`grep -rn 'PrintDefaults\|ErrHelp' cmd/profgate internal/client` 非測試無結果。spec 沒提 `--help`，也不在四個已記錄的偏離之中 | small-code | 高 |
| 3 | 同一個 binary 有三種 `--help` 行為：client verb 露 `flag: help requested`；帶 positional 的 verb 說 `"--help" where a positional belongs`；`serve`、`config validate`、`version` 印 Go 的 `Usage of <verb>:`。沒有一種 exit 0 | 18 個 verb 全部實測 | small-code | 中 |
| 4 | kubectl 反射動作：`-n <ns>` 錯誤不提示 `--namespace`；`-o json` 在 `profile` 與 `download` 上是「輸出路徑」，所以 `profile ns/svc heap -o json` 寫出一個名叫 `json` 的 gzip pprof 檔並 exit 0 | `cmd/profgate/profile.go:36` | small-code | 中 |
| 5 | `collection get` 對已完成的 Collection 印 `round 0 of 1`：`progress.round` 是進行中 round 的 0-based index，`rounds` 是計數，client 兩者都原樣印 | `internal/pgo/rounds.go:144-145`、`:164`；`cmd/profgate/collect.go:366` | small-code（`p.Round+1`） | 中 |
| 6 | `collection get` 的表格丟掉所有回答「接下來怎樣」的欄位：`expiresAt`、`finishedAt`、`resolvedVersion`、`artifact.bytes`；不用 `--output json` 看不到還有多久能 `download` | `cmd/profgate/collect.go:358-371` 只印 `id`、`state`、`origin`、`progress` | small-code | 中 |
| 7 | `--output json` 下錯誤仍是 stderr 純文字、stdout 空白，script 拿不到機器可讀的失敗 | `docs/specs/cli.md:881-893` 規定錯誤一律一行文字 | spec-revision | 中 |
| 8 | 2xx 但非 envelope 的 body 印 `profgate: HTTP 200 OK` 當錯誤——一個沒說出問題的訊息；符合 spec，所以要動的是 spec | `docs/specs/cli.md:894-895` | spec-revision | 中 |
| 9 | `collect --wait` 表格模式在 stdout 印兩筆（收據與最終紀錄）；`--output json` 下收據哪裡都不印，機器呼叫者在等待結束前拿不到 id | `cmd/profgate/collect.go:225-229`；`docs/cli.md:316`、`docs/specs/cli.md:827` 都說只有最終紀錄上 stdout | small-code | 中 |
| 10 | `targets --explain` 對 selector 選中零個 Pod 的 Service 印兩個空表頭，什麼都不說；JSON 有 `selectorMatched: 0`，表格從不顯示這個把「選不到 Pod」與「選到但全被排除」分開的數字 | 實測；`--output json` 同一呼叫回 `{"targets":[],"selectorMatched":0,"excluded":[]}` | small-code | 中 |
| 11 | `logout` 成功時沉默、無事可做時出聲——反了 | 實測 | small-code | 低 |
| 12 | `login --context <name>` 建立 context 但不選取它，下一個指令就失敗；login 自己的輸出不提 `context use` | `docs/cli.md:56-63` 有寫 | small-code | 低 |
| 13 | `docs/cli.md:192` 說單筆紀錄渲染成 `key: value`，實際與文件自己的範例都是 tab | `cmd/profgate/render.go:15-35` | docs-only | 低 |
| 14 | contexts 檔拒絕時露出 Go 型別：`field bogusKey not found in type client.File` | `docs/cli.md:129` 只承諾「refused by name」 | small-code | 低 |
| 15 | 過期的快取 token 仍觸發 loopback 明文警告，然後才說沒有有效 token | 實測 | small-code | 低 |
| 16 | gateway envelope 原樣轉發時不說下一步，雖然 client 對每個都有對應 verb：`port_not_allowed`（`limits`）、`collection_in_progress`（`collections`）、`no_targets`（`targets --explain`）、`pgo_disabled`；spec 規定原樣轉發，所以要改的是 gateway 端訊息 | `internal/httpapi/server.go:448` | small-code（gateway 訊息） | 低 |
| 17 | `services <不存在的 namespace>` 印表頭 exit 0，與空 namespace 無法區分 | `docs/cli.md:195-196` 的空清單規則 | spec-revision | 低 |
| 18 | `context delete <目前的>` 沉默移除，下一個指令 `no gateway selected` | 實測 | small-code | 低 |
| 19 | 同一概念兩個名字：`collect --body` 與 `pgo policy set --file` | `cmd/profgate/collect.go:43`、`cmd/profgate/policy.go:26`；兩者都寫在 spec | spec-revision | 低 |

## 五、Console

14 項，其中 8 項在瀏覽器實測。
`explain` 診斷、可書籤的 selection、port control、asset 標頭（ETag、`no-cache`、`default-src 'none'` CSP）、`urls.js` 的 encode 紀律——
這些是 console 最好的部分，不要動。

| # | 發現 | 證據 | 修法 | 痛感 |
|---|---|---|---|---|
| 1 | **滑鼠連點 Start collection 一次完成 arm 與 confirm**：兩段式確認擋不住連點，實測建立了一個 Collection。Cancel 是同樣的程式形狀且不可撤銷 | `internal/ui/static/app.js:622-633`、`:1203-1212`；Confirm start 取代 Start collection 出現在同一位置 | small-code | 高 |
| 2 | **Download 失敗時頁面什麼都不說**：頁面自己顯示「no target: every selected Pod was excluded」卻仍提供 Download；gateway 回 `503 no_targets`，頁面不變。這是 console 唯一比 `curl` 差的地方 | `app.js:1145`（`<a href download>`）、`:901-912` `currentProfileURL` 忽略 `targetSummary.empty` | small-code | 高 |
| 3 | **Collections 表格在寬螢幕上擠在一個 427px 的 grid 欄裡**，旁邊 ~900px 空白；`state` 被截、時間戳與 Cancel 都在畫面外 | 量測 `.table` `scrollWidth` 1771px / `clientWidth` 427px；`app.css:9-14`、`app.js:930-937` | small-code（一行 CSS） | 高 |
| 4 | 頁面沒有任何刷新：Collections 與 targets 每次 selection 只抓一次，從 console 啟動的 Collection 永遠停在 `pending`；實測頁面 4 列、API 5 列。沒有 Refresh 控制 | `app.js:497-503`、`:471-492` | small-code | 中 |
| 5 | armed 狀態的兩個按鈕長得一樣：Confirm start（送 POST）與 Keep（不送）都是 primary 藍。`Keep` 帶 `class="secondary"`，但 vendored classless Pico 沒有 `.secondary` 規則 | `app.css:78-82`；量測 Keep 與 Copy URL 同色 | small-code | 中 |
| 6 | Collections 只顯示一頁至多 100 筆，靜默丟掉 `nextCursor` | `internal/pgo/caches.go:664-667`；`app.js:505-521` 只讀 `body.collections`；`docs/console.md:111` | small-code | 中 |
| 7 | `docs/console.md:104` 說 Copy URL 在 HTTP 頁面下不出現，但同一文件建議的 `http://localhost:8080/ui/` 是 secure context，按鈕會出現 | `docs/console.md:23-30`；`app.js:1080` | docs-only | 中 |
| 8 | `loginURL` 沒有實作 spec 要求的 1024-byte return-path 上限；過長的 ns/svc 讓 gateway 把 return 正規化為 `/`，selection 與 `returned=1` 都丟 | `internal/ui/static/urls.js:103-106`；`internal/auth/wire.go:18,48`；`docs/specs/ui.md:482` | small-code | 中 |
| 9 | 頁面沒有標題、沒有產品名稱、沒有指向 API 或 CLI 對應 verb 的任何提示 | `internal/ui/static/index.html` 無 `<h1>`；spec non-goals 沒排除 | small-code | 低 |
| 10 | `docs/console.md:123` 說 Keep「puts it back as it was」，第一次請求之後就不是了：無法分類的結果下 Keep 會放棄並警告 Collection 可能已存在 | `collectionmodel.js` `startNext`；`app.js:638-640` | docs-only | 低 |
| 11 | `docs/console.md:141` 用「the release that moves the console off its old content-hashed asset URLs」指涉一次遷移而不點名；它就是 `v0.4.0` → `v0.5.0`，僅此一次 | AGENTS.md *No Jargon* | docs-only | 低 |
| 12 | `hints` 表沒有 `too_many_auth` 與 `auth_unavailable`，每個 listing route 都可能回這兩個 | `app.js:55-73`；`docs/specs/ui.md:174-209` | small-code | 低 |
| 13 | `realm_denied` 的提示說「the identity panel shows what it does」，但 listing 的 403 從不重抓 `/v1/whoami`，identity panel 停在舊 realm | `app.js:57` vs `:385-420` | small-code | 低 |

## 六、可觀測性與 runbook

每個 operator 需要的 metric 都已存在，label cardinality 全部有界，`Recorder` interface 的 doc comment 就是 metric 規格，
audit record schema 完整——不要動。
問題是三個關鍵 gauge 在最需要的時刻讀不到或讀錯，以及沒有 runbook。

| # | 發現 | 證據 | 修法 | 痛感 |
|---|---|---|---|---|
| 1 | **`pgo.enabled` 且 NATS 不可達時，`profgate_pgo_synced` 是不存在，不是 0**：gauge 在 `startPGO` 內註冊，只在 preflight 成功後到達，而 preflight 對 `ErrUnavailable` 永遠重試。`ProfgatePGONotSynced` 在它命名的那個故障裡沉默，同時 `/readyz` 503、rollout 卡住 | `cmd/profgate/serve.go:668`、`:490`、`:597-614` | docs-only：在既有 `pgoEnabled` guard 內加一條 `profgate_nats_connected == 0` 規則 | 高 |
| 2 | **`profgate_tls_certificate_expiry_seconds` 無條件註冊，只在 `server.tls` 下寫入**，所以 chart 預設安裝下永遠讀 0（Unix epoch）；`docs/deployment.md:218-219` 承諾它讓停滯的 rotation 在到期前可見，沒有任何 threshold 規則做得到 | `internal/metrics/prometheus.go:150-153`；`internal/tlscert/loader.go:149` 是唯一寫入者；`cmd/profgate/serve.go:243-258` | small-code：seed 為 NaN，`jwksAge` 已是這個模式 | 高 |
| 3 | **`profgate_discovery_synced` 只在啟動時 0→1 一次**，`HasSynced` 是 latch；`ProfgateNotReady` 只涵蓋「開機從未 sync」，annotation 卻寫「A profgate replica is not serving」——四個 readiness gate 中三個可以在 gauge=1 時變紅，執行中的 API server 故障不翻任何 gauge | `cmd/profgate/serve.go:475` 是唯一呼叫點；`internal/k8s/cluster.go:70`；`serve.go:172-173`；`deploy/chart/profgate/templates/prometheusrule.yaml:22-31` | docs-only（改措辭）或 small-code（`ready()` 支撐的 gauge） | 高 |
| 4 | 六種故障模式已有 metric、沒有 alert：TLS reload 失敗、憑證將到期、執行中 NATS 斷線、authenticator unavailable、所有 upstream Pod 拒絕、auth rate limiter 飽和 | `prometheusrule.yaml` 四條規則；`profgate_tls_reloads_total{result="failed"}`、`profgate_nats_connected`、`profgate_auth_failures_total`、`profgate_requests_total{code=…}` 都存在 | docs-only（chart rules + README 表） | 中 |
| 5 | **`docs/deployment.md` 與 `docs/authentication.md` 沒有故障排除或 failure-scenarios 章節**；全 repo 唯一的故障表在 `docs/specs/gateway.md:2439`，寫行為但幾乎不寫該看哪個 metric 或哪行 log。auth 或 TLS 原因的 CrashLoop 找不到對應的段落 | `grep -n "^#\{1,4\} " docs/deployment.md docs/authentication.md`；PGO 的 CrashLoop-vs-never-Ready 指引只在 `NOTES.txt:105-139` | docs-only | 中 |
| 6 | `code` label 的值域對 operator 沒有文件：40 個 envelope code、7 個 audit-only outcome、一個 `upstream_<status>` 家族，文件只點名一個 | `internal/httpapi/codes.go:11-95,100-105`；`internal/proxy/proxy.go:171`；`docs/deployment.md:439,464` | docs-only | 中 |
| 7 | store 層故障沒有 operator 指引：bucket 在執行中被刪或重建、從備份還原、NATS 維護——什麼會丟、什麼自癒、in-flight owner 會中止，只在 spec 裡 | `docs/specs/pgo.md:2883-2888,3694`；`docs/deployment.md`、`docs/pgo.md`、`deploy/nats/README.md` 只有權限行 | docs-only | 中 |
| 8 | 任何 NATS 斷線，再短暫，都中止這個 replica 擁有的每個 Collection 並消耗一次 attempt；NATS rolling restart 可以達到 `attempts_exhausted`。spec 有寫，operator 文件只描述「outage」 | `internal/pgo/worker.go:439,468-485`、`internal/natskv/client.go:162-167`；`docs/specs/pgo.md:1450`；`docs/pgo.md:310-316`、`docs/deployment.md:356` | docs-only | 中 |
| 9 | 失敗的 PGO sample 只在 debug 記錄且不帶 Collection id，開了 debug 也接不回它屬於哪個 Collection | `internal/pgo/rounds.go:411-412`；`cmd/profgate/serve.go:669-679` | small-code | 低 |
| 10 | 兩行 httpapi 失敗 log 漏掉同儕都有的識別欄位：`authenticator failed` 無 `requestId`；`idempotency receipt is not readable` 無 `requestId` 也無 namespace/service | `internal/httpapi/auth.go:86`；`internal/httpapi/pgo_collections.go:633` | small-code | 低 |
| 11 | `profgate_confirm_total` 與 `profgate_profiles_in_flight` 文件寫得像涵蓋全部，實際只觀測互動路徑；PGO sampling 確認 Pod 與抓 profile 都不碰它們 | `internal/pgo/rounds.go:448`；`internal/httpapi/server.go:710-738`；`docs/deployment.md:441-442` | docs-only | 低 |
| 12 | 沒有 Grafana dashboard 或範例查詢；chart README 的 alert 表是 operator 唯一拿到的 expression。spec 沒宣告為 non-goal | `grep -rli "grafana\|dashboard"` 只命中 spec 對 Pyroscope/Parca 的句子 | docs-only（查詢附錄）；dashboard 本身是 feature，roadmap 決定 | 低 |

## 七、互動路徑穩定性

Admission 在每條路徑釋放（含 `panic(http.ErrAbortHandler)` unwind）；
header deadline 對 body 的 `time.AfterFunc` 結算沒有 goroutine 也沒有 channel，被 200 次並發測試釘住；
shutdown 順序（readiness 503 → drain → bounded `Shutdown` → `Close` → informer 最後取消）正確且有四個 `TestServe` drain 子測試；
每條 reload path 都保留 last-good；
partial Kubernetes 物件的每個 pointer 都有 guard；共享狀態只有 atomic pointer 指向不可變 snapshot。
這些不要動。

| # | 發現 | 證據 | 修法 | 痛感 |
|---|---|---|---|---|
| 1 | **停止讀取的 client 佔住 handler、admission slot 與 Pod 連線超過整體 budget**；16 個這種 client（預設 `limits.maxConcurrentProfiles`）讓之後每個 profile 請求 `429 too_many_profiles`，直到它們掛斷。**已重現**：2 秒 budget 後 handler 仍阻塞 8 秒，只在 client socket 關閉時返回；加 `SetWriteDeadline(budget)` 後 2.0 秒返回 | `internal/proxy/proxy.go:173` 在 `w.Write` 阻塞；`cmd/profgate/serve.go:234` 只設 `ReadHeaderTimeout`；spec `docs/specs/gateway.md:981-983` 承諾 budget「bounds ... body streaming」 | small-code | 高 |
| 2 | **client-go informer 與 reflector 的失敗從不進 gateway 的 logger**：它們以 glog 文字上 stderr，在 `server.logLevel` 與 JSON 契約之外。**已重現**：每個 Pod list 都失敗 4 秒，`HasSynced=false`，gateway slog 為空，stderr 有三行 `reflector.go:227 "Failed to watch"` | 非測試碼無 `klog` import；`cmd/profgate/serve.go:105`；spec `gateway.md:1443` | small-code：`klog.SetSlogLogger` | 中 |
| 3 | PGO JSON route 以 `io.ReadAll(MaxBytesReader)` 讀 body，server 只設 `ReadHeaderTimeout`：client 送完 header 後慢慢滴一個不完整的 64 KiB body，可以無限期佔住一個 handler goroutine。**已重現**：handler 在 `decodeBody` 阻塞到 client 掛斷；經 `ResponseController` 設 1 秒 read deadline 後 1 秒返回。與上列 1 是兩個修法、同一種手法（見第十節） | `internal/httpapi/pgo.go:206`；`cmd/profgate/serve.go:234` | small-code | 中 |
| 4 | 兩個 server 都沒設 `ErrorLog`，`net/http` 自己的錯誤（TLS handshake 失敗、recovered panic）經 `log.Printf` 以文字上 stderr | `cmd/profgate/serve.go:234-235`；rule 200 | small-code | 低 |
| 5 | 兩個 listener 都沒有 `IdleTimeout`：不送東西的 keep-alive 連線持有到 process 結束 | 同上 | small-code | 低 |
| 6 | upstream transport 沒有 `IdleConnTimeout` 也沒有全域 `MaxIdleConns`：pool 隨每個曾被 profile 的 Pod 成長，只有 TCP keepalive 回收沒 FIN 就消失的 peer | `internal/proxy/proxy.go:86-90`；spec `gateway.md:977` 只點名 `ResponseHeaderTimeout` 是刻意不設 | small-code | 低 |
| 7 | 兩個 outcome 歸因錯誤：client 在 confirm 讀取期間斷線被記成 `profgate_confirm_total{result="unavailable"}` 並 audit `503 discovery_unavailable`；drain deadline 關閉的連線被 audit 成 `client_gone` | `internal/k8s/confirm.go:44`；`internal/httpapi/server.go:728-736`；`internal/proxy/proxy.go:174` | small-code | 低 |
| 8 | 每條 fatal 啟動路徑在退出前睡 `server.drainDelay`（預設 5 秒），雖然 `/readyz` 從未變 200、沒有 endpoint 指到這裡；每次 crash-loop 慢這麼多，log 還說「draining; waiting for endpoint removal」 | `cmd/profgate/serve.go:380-383`；呼叫點 `:449,:461,:467,:484,:493`；spec `gateway.md:1635` 給的跳過理由在這裡同樣成立 | small-code | 低 |
| 9 | `deadlineGuard` 對忽略 context 的函式只能放棄、不能停止：每次 deadline 到期漏一個 goroutine。生產環境的 client-go 會傳遞 context，觸發條件是推測 | `internal/k8s/client.go:80`、`internal/k8s/confirm.go:27` | 記錄；不修 | 低 |

## 八、PGO 與 NATS 穩定性

CAS 紀律、lease 與 cutoff、generation barrier、publication order、sweeper 的 freshness rule、shutdown drain、記憶體上限——
在讀碼與既有測試下全部站得住，不要動。

| # | 發現 | 證據 | 修法 | 痛感 |
|---|---|---|---|---|
| 1 | **artifact 下載被 5 秒 call timeout 整段截斷**：`objView.Get` 把 `callTimeout` 放在整個 stream 上，nats.go 每個 chunk 都檢查 context；32 MiB 的 profile 經 port-forward 以每秒幾 MB 下載必定失敗為 `artifact_stream_failed`，沒有任何可調參數。上傳同樣受限。**已重現**：2 MiB 讀到 64 KiB 後 `nats: timeout` | `internal/natskv/client.go:24,815,831-854`；`internal/httpapi/pgo_collections.go:1038-1051` | small-code：短 deadline 只管建立，傳輸跟隨請求或 lease 生命週期 | 高 |
| 2 | Watch 重開每 50 ms 重試，無 backoff 無 jitter 無上限：bucket 不存在時每個 watch 每個 replica 每秒約 20 個 consumer-create 請求，可以持續數小時；多個 replica 同步重試。process 層的重試 backoff 也沒有 jitter。**已重現**：三個 `PROFGATE_JOBS` watch 每秒 58.7 次失敗開啟。log 有正確去重 | `internal/natskv/client.go:27-28,694-714`；`cmd/profgate/serve.go:515`；spec `:2863` 說「fixed interval」但沒給數 | small-code | 中 |
| 3 | 一個 replica 在被中止的 owner 仍在跑時 reclaim 自己的 Collection，第一個 owner 退出時刪掉第二個 owner 的 `inFlight` 項目；`Drain` 不等它就返回。需要 `maxActiveCollections >= 2`（預設 1）。**已重現**：`inFlight` 空、一個 slot active、`Drain` 20 µs 返回 | `internal/pgo/worker.go:469-471,476-479`、`:334-345` | small-code | 低 |
| 4 | `Drain` 在 renewal 進行中時以舊 cutoff 等待：owner 通過 `stopping()` 檢查後阻塞在 `Update`，drain 開始並讀一次 `cutoffAt`，renewal 成功延長了 durable lease，drain timer 不變，`Drain` 在舊 cutoff 返回而 NATS 仍把 Collection 指派給這個 replica。**已重現**：`Drain` 返回時紀錄的 lease 還有 24 秒，工作未取消。預設下每個擁有的 Collection 每 20 秒有一次這個窗口；後果是該 Collection 以新 attempt 重跑，其他 replica 的 reclaim 上限仍成立 | `internal/pgo/worker.go:232-246` 只讀一次 `cutoffAt` | small-code：drain timer 觸發時重讀 cutoff | 中 |
| 5 | publication 在請求 context 下執行；client 在第一筆與最後一筆寫入之間斷線，留下一個 `initializing` 紀錄擋住該 Service 約 65 秒（`429 collection_in_progress`、scheduler `busy`），直到 scan 把它 fail 成 `not_published` | `internal/httpapi/pgo_collections.go:572`；`internal/pgo/publisher.go:268-315`；`internal/natskv/client.go:429-437`；`internal/pgo/caches.go:582-593` | small-code | 低 |
| 6 | worker scan 在每次 `job.*` 送達時重讀每個 nonterminal 紀錄，store 讀取隨活躍 Collection 數平方成長：預設上限 64 時每 replica 每秒約 200 個 `Get` | `internal/pgo/worker.go:167-181,285-299`；`internal/pgo/caches.go:108-124,385-388` | small-code | 低 |
| 7 | probe sweep 每分鐘每 replica 列出兩個 KV bucket 的所有 key、client 端過濾，5 秒 deadline 截斷時靜默跳過，大 bucket 上 probe 殘留可能永不清除 | `internal/natskv/client.go:568-591`；`internal/pgo/sweeper.go:420-424`（對照 `:466-471` 有 log） | small-code | 低 |
| 8 | 發現到的 `completed → expired` 轉換寫入失敗被靜默丟棄：讀者確認 artifact 不在，conditional update timeout，請求回 410 或走到更舊的 artifact，durable 紀錄仍是 `completed`，沒有 warning 或 metric，後續讀者重複同樣的事 | `internal/pgo/runtime.go:453`；`internal/httpapi/pgo_collections.go:1079`；`internal/pgo/sweeper.go:255` | small-code：把預期的 CAS 落敗與 `ErrUnavailable`/deadline 分開 | 低 |
| 9 | 保留的 job 紀錄在每個 replica 快取，除 `jobRetention` 外沒有上限；單一 Service 的 listing 對整個 job map 分配並在 cache mutex 下排序。裁決見第十節：現實速率下不是缺陷 | `internal/pgo/caches.go:161,743,816`；`internal/config/config.go:541` | small-code：per-Service index | 低 |

## 九、驗證閘門與契約

單元測試在基線、三次重複、32 核飽和下全綠；唯一刻意 timing-raced 的測試（`internal/proxy/proxy_test.go:503-538`）斷言的是 outcome invariant 而非計數，撐過飽和。
OpenAPI ↔ route table ↔ code registry 由五個獨立比對加九種變異的 drift test 機械封閉；39 個 code 的 status 對應零差異；403-vs-404 的 realm 設計與文件一致。
不要動這些。

| # | 發現 | 證據 | 修法 | 痛感 |
|---|---|---|---|---|
| 1 | `TestRoundsDecodeHeapDelta` 在 `-race` 下跳過，而 repo 每一條測試指令都帶 `-race`：這個 decoder 記憶體回歸守衛從未在任何地方執行 | `internal/pgo/rounds_test.go:818-821`；`internal/pgo/race_on_test.go:1,8`；`mise.toml:33` | small-code | 中 |
| 2 | `check.yml` 的快速閘門（`check && lint && test`）只在 `push` 觸發；repo 是公開的，fork 來的 PR 只觸發 `pull_request`，因此從不跑 lint、單元測試、check——只跑 e2e `current` lane 與 prose | `.github/workflows/check.yml:1-13`；`docs/specs/gateway.md:2098-2103` 沒處理 fork | small-code | 中 |
| 3 | `If-Match` 是 `PUT`/`DELETE .../pgo` 必要的 header，程式與 `docs/api.md` 一致，OpenAPI 只在 description 文字提到、從未宣告為 parameter；`Idempotency-Key` 有宣告且有測試守衛，`If-Match` 沒有 | `internal/httpapi/openapi.json`；`internal/httpapi/pgo_policy.go:112,203`；`openapi_test.go:559-582` 只走 `query` 與 `path` | small-code | 中 |
| 4 | `.agents/rules/900-design-and-review-loops.md:32` 說「No CI invokes it yet」，`check.yml` 自 `9632a40`（2026-08-23）起就跑 `mise run check`，比該行最後一次編輯早五天 | `git log` | docs-only | 低 |
| 5 | `docs/decisions/e2e-without-framework.md` 自己的 Revisit 說尺寸觸發已成立、拆分未做；`test/e2e/harness_test.go` 1,676 行 | `wc -l` | small-code（拆檔） | 低 |
| 6 | `docs/api.md:121,1002` 以未受檢的散文寫路由數（fifteen、three），今天正確，沒有東西釘住它 | `internal/httpapi/routes.go:46-74` | docs-only 或 check 規則 | 低 |
| 7 | `docs/pgo.md:147` 說 `nextCursor` 分頁，`:263` 說「offers no pagination」；程式有分頁 | `internal/httpapi/pgo_collections.go:432-436` | docs-only | 低 |

## 十、外部審視獨有主張的裁決

外部模型提出四項內部審視沒有提出的主張。
每項由第二個內部審視以 `-race` 拋棄式測試裁決。

| 主張 | 裁決 | 依據 |
|---|---|---|
| 同一 store generation 下，watch open 失敗後的重試會把 gap 期間刪除的 key 留在 cache 裡並回報 synced | **駁回**，剩極窄殘餘。失敗的 open 確實不移動 generation（`internal/natskv/client.go:612-620`），但重試的 replay 送達該 key 的 delete marker（`WatchFiltered` 未設 `IgnoreDeletes`，`:653,783`；bucket TTL 被 `internal/natskv/preflight.go:107` 禁止），已刪的 key 被移除——已重現。殘餘只在 delete marker 於 1 到 30 秒的啟動 backoff 窗口內被 purge 時發生；該窗口內沒有消費者讀部分 cache（`internal/pgo/runtime.go:131`、`internal/pgo/scheduler.go:147` 都要求四個 cache 全部 synced）。這與 `v0.5.0` 對 watch cut 路徑已接受的殘餘相同 | 重現測試在審視紀錄 |
| `Drain` 在 renewal in flight 時以舊 cutoff 返回 | **確認**，嚴重度中，已列入第八節 4 | `internal/pgo/worker.go:232-246` |
| PGO route 的 body 讀取沒有 deadline | **確認**，嚴重度中，已列入第七節 3。與第七節 1 是兩個修法、同一種手法：`httpapi/pgo.go` 的 read deadline、`proxy.go:173` 與 artifact 下載的 write deadline，修法是兩者都經 `http.ResponseController` 按 route 設定（今天 `proxy.go:173` 是一個沒有 deadline 的 `io.Copy`）。server 層的 `ReadTimeout`/`WriteTimeout` 不是共用修法：它會取消長時間的 profile handler | `net/http/server.go:729,982-1041` |
| 保留的 job 紀錄 cardinality 無上限，listing 在 mutex 下對全部紀錄分配與排序 | **數字確認，現實速率下不是缺陷**。數字來自 2026-09-03 對 `internal/pgo` 跑的拋棄式 benchmark，不在樹內。每筆 339 bytes。10 個 Service、每天 4 次、保留 7 天：280 筆，每次 list 29 µs。預設 on-demand 上限撐滿 7 天：100,800 筆、33 MiB、每次 list 31 ms 且在 `c.mu` 下配置 10.8 MiB，`Live` 20 ms。只有設定極限（每分鐘 600 × 90 天）才到 OOM，而 NATS 儲存會先失敗。數量由 `jobRetention`（預設 168h，上限 2160h）與每 replica 的 token bucket 界定。per-Service index 是便宜且獨立的修法 | `internal/pgo/caches.go:161,743,816` |

## 十一、Spec 與程式的差距（非缺陷，需要決定）

- **Collector 拆分修訂已 Accepted 但未實作**：
  `docs/specs/pgo.md:1190-1242,3749-3800` 描述 collector Deployment、heartbeat、`profgate_pgo_collector_available` gauge 與 `503 collector_unavailable`；
  程式裡 `internal/httpapi/codes.go:92` 的常數無人使用，`cmd/profgate/serve.go:701-703` 把所有迴圈跑在 gateway 內，
  但 OpenAPI enum 與 console JS（`internal/ui/static/collectionmodel.js:210`、`app.js:65`）都已經帶著它。
  延後的決定只記在 `docs/decisions/collection-stays-in-the-gateway.md`；讀 spec 的人必須自己知道要打折扣。
  spec 該在原地標明這段修訂的狀態。
- **CLI spec 需要修訂**才能修 CLI 一節裡標為 spec-revision 的四項：
  `--output json` 下的錯誤形狀、2xx 非 envelope 的訊息、不存在的 namespace 的回應、`--body` 與 `--file` 的統一。
- **`/v1` 回應不帶 build 版本**，console 無法指出 rollout 中是哪個 replica 回答的。
  這是新欄位，是 feature；console 沒有它仍可用。不列入。

## 十二、不做的事

- 任何新的 API route、新的 chart 資源（HPA、ServiceMonitor 之外的東西）、Grafana dashboard 檔。
  第六節 12 只要求一頁範例查詢。
- 改 `pgo.configAPI` 為 boolean、改 `cpuSeconds` 為 duration：都是 breaking，收益是一致性。記錄，不排程。
- 替換 `deadlineGuard`：生產環境的觸發條件是推測。
- Job 紀錄的 cardinality 上限：第十節裁決為僅在設定極限下才成問題；per-Service index 併入第八節的 scan 修法，不另設上限。

## 十三、建議優先序

執行順序與進度以 [`docs/plans/roadmap.md`](../plans/roadmap.md) 為準。
原則：先修 operator 第一次就會踩到的（安裝、`--help`、三個 gauge），再修已重現的穩定性缺陷，再補 runbook，最後是 spec 修訂帶動的 CLI 與 console 調整。

1. 安裝路徑：kustomize base 的 Namespace、chart 記憶體防呆、auth 模式的 render-time 拒絕、NOTES 與 README 的連結、升級章節指向 CHANGELOG。
2. `config validate` 的記憶體算式與文件範例；decode 錯誤訊息帶 key path 與檔名；`port: 0`。
3. CLI `--help`：每個 verb 一致地印 flag 並 exit 0；`auth hash` 解析參數；`-o`/`-n` 提示；`round` 顯示；`collection get` 表格欄位；
  `collect --wait` 的收據；`targets --explain` 的 `selectorMatched`。
4. 可觀測性真實性：TLS expiry gauge seed NaN；`profgate_nats_connected` 規則；`discovery_synced` 的語意或措辭；六條缺的 alert；`code` label 值域；
  `deployment.md` 故障排除章節，含 store 層故障與 NATS 斷線的 attempt 成本。
5. 互動路徑穩定性：profile 串流的 write deadline 與 PGO route 的 body read deadline（按 route 經 `ResponseController`，
  不是 server 層 timeout）；klog → slog；`ErrorLog`；`IdleTimeout` 與 `IdleConnTimeout`；outcome 歸因；fatal 啟動不睡 drainDelay。
6. PGO 穩定性：artifact 傳輸 deadline；watch 重開的 backoff 與 jitter；drain timer 重讀 cutoff；self-reclaim 的 `inFlight`；
  publication 脫離請求 context；probe sweep 的 log；expired 轉換失敗的 log 與 metric。
7. Console：連點防護；Download 失敗顯示；表格版面；刷新；分頁；Keep 的樣式；return-path 上限；hints 補齊；docs/console.md 三處修正。
8. 驗證閘門：`-race` 下跳過的測試；fork PR 的 `check` job；`If-Match` OpenAPI parameter；rule 900 的過時句子；harness 拆檔。
9. Spec 修訂：collector 修訂在 spec 內標明狀態；CLI spec 三處；之後才改 CLI 與 gateway 訊息。
