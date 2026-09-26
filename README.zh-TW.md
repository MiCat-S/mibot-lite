# MiBot Lite

[简体中文](README.md) | [English](README.en.md) | **繁體中文** | [日本語](README.ja.md)

一個省記憶體的 Telegram UserBot：一個靜態 Go 執行檔，指令編譯在程式裡，狀態存成 JSON 檔案。

它是 [MiBox](https://github.com/MiCat-S/Mi-Box) 的輕量替身：帳號、工作階段、`config.json` 和指令名稱都一樣，
可以從 MiBox 直接切換過來，設定也能用一條指令搬走。拿掉的是外掛系統：沒有 JavaScript 執行環境，
沒有 SQLite，常駐記憶體不到 Node 版 MiBox 的一半。

| | MiBox（Node） | MiBot Lite |
|---|---|---|
| 常駐記憶體（RSS） | 約 125 MB | **約 30–50 MB** |
| 安裝體積 | node_modules，約 237 個套件 | **一個約 21 MB 的檔案** |
| 外掛執行環境 | V8 | 無 |
| 資料儲存 | better-sqlite3 | JSON 檔案 |
| 影像處理 | sharp（libvips） | 標準函式庫 + `golang.org/x/image` |
| 新增指令 | `.tpm install` | 改程式碼、重新編譯 |

記憶體是在同一台正式環境機器上實測的：剛連上約 30 MB，連續執行幾小時後約 50 MB，其中近 20 MB 是
執行檔本身映射進來的分頁。產生貼圖時臨時啟動的 ffmpeg 子行程不算在內（見[設計取捨](#設計取捨)）。

## 安裝

在伺服器上執行（Linux amd64 / arm64，systemd，root）：

```sh
bash <(curl -fsSL https://raw.githubusercontent.com/MiCat-S/mibot-lite/main/scripts/install.sh)
```

下載、校驗、登入、安裝成服務一步完成；重新執行就是升級，之後也可以在 Telegram 裡傳 `.update run`。
分步流程、設定項目和記憶體限額見 [INSTALL.md](INSTALL.md)（簡體中文）。

- **ffmpeg**：`.eatgif`、`.eat`、`.eat2`、`.t` 需要，`.yvlu` 引用影片時也需要；沒裝不影響其他指令。
  `apt install -y ffmpeg`。
- **從 MiBox 遷移**：ai、sum、whois、別名、前綴等設定可以直接搬過來：

  ```sh
  ./mibot-lite --import-mibox /root/mibot --root /root/mibot-lite
  ```

  同一個帳號可以在兩套程式之間來回切換，但**不能同時執行**：Telegram 會讓同一帳號的兩個連線互相擠掉。
- **重灌系統或換機器**：在 Telegram 裡傳 `.bf`，設定會備份到「儲存的訊息」；新機器在安裝腳本後面加上
  `--restore 備份檔`，原樣還原，不用重新登入。見 [INSTALL.md 第 10 節](INSTALL.md#10-备份与恢复重装系统换机器)。

## 指令

預設前綴是 `.`、`。` 和 `$`，可以用 `.prefix` 修改。每條指令的詳細用法請傳 `.help 指令`，
或在指令後面加上 `--help`。

「可借」一欄指的是能不能透過 [`.sudo` / `.sure`](#借用帳號sudo-和-sure) 借給別人使用：
✓ 是整條指令都能借；**部分**是只能借用查詢、使用的那部分，修改設定的子指令只限本人；空白是只限本人。

**執行與維護**

| 指令 | 作用 | 可借 |
|---|---|---|
| `.ping [網域]` | Telegram 或某個網站的延遲 | ✓ |
| `.status` | 執行狀態卡片（CPU、記憶體、磁碟、Swap） | ✓ |
| `.memory` | 行程記憶體 | ✓ |
| `.sysinfo` | 詳細系統資訊 | |
| `.version` `.ver` | 版本資訊 | ✓ |
| `.help` `.h` | 指令列表或單條指令說明 | ✓ |
| `.update [check\|run\|rollback]` | 檢查、更新或回滾程式 | |
| `.restart` | 重新啟動服務 | |
| `.log` | 匯出執行日誌（已去除敏感資訊） | |
| `.bf` | 備份設定到「儲存的訊息」 | |
| `.prefix` `.alias` | 修改指令前綴、為指令取別名 | |
| `.privacy` | 設定輸出裡 IP 位址的遮蔽方式 | |

**查詢與工具**

| 指令 | 作用 | 可借 |
|---|---|---|
| `.calc 算式` | 四則運算 | ✓ |
| `.rate 貨幣 [目標] [數量]` | 匯率與換算 | ✓ |
| `.tr [語言] 文字` | Google 翻譯，不用設定 | ✓ |
| `.gt [語言] 文字` | AI 翻譯 | ✓ |
| `.whois 網域` | 網域註冊資訊，支援批次查詢 | 部分 |
| `.ip [IP\|網域]` | IP 的位置與電信業者 | ✓ |
| `.bin 卡號前 6–8 碼` | 卡號對應的發卡銀行 | ✓ |
| `.ids` `.dc` | 使用者或對話的資料、所在資料中心 | ✓ |
| `.speedtest` `.st` | 伺服器測速（Ookla 官方 CLI） | 部分 |

**AI**

| 指令 | 作用 | 可借 |
|---|---|---|
| `.ai [search] 問題` | AI 對話與連網搜尋 | 部分 |
| `.sum [數量]` | 群組訊息摘要，也能設定排程 | 部分 |

**訊息與貼圖**

| 指令 | 作用 | 可借 |
|---|---|---|
| `.yvlu` | 把訊息做成語錄貼圖、圖片或限時動態 | 部分 |
| `.eatgif 名稱` | 用雙方大頭貼合成動態貼圖 | 部分 |
| `.eat` `.eat2` | 用大頭貼或圖片產生梗圖貼圖 | 部分 |
| `.sticker` | 把貼圖存進自己的貼圖包 | |
| `.t 文字` | 文字轉語音（`.ts` 選角色，`.tk` 設定 API Key） | |
| `.re [訊息數] [次數]` | 複讀回覆的訊息 | ✓ |
| `.save 連結` | 儲存或轉傳訊息，禁止轉傳的也可以 | |
| `.dme 數量` | 刪除自己的訊息 | |
| `.da` | 批次刪除群組訊息 | |

**群組管理**

| 指令 | 作用 | 可借 |
|---|---|---|
| `.ban` `.unban` `.kick` `.mute` `.unmute` | 在目前群組封鎖、踢出、禁言 | ✓ |
| `.sb` `.unsb` | 在所有管理的群組裡封鎖、解除封鎖 | |
| `.refresh` | 重新整理管理群組快取（平時一天自動更新一次） | |
| `.aban` | 以上幾條的說明 | |

**帳號**

| 指令 | 作用 | 可借 |
|---|---|---|
| `.acn` `.autochangename` | 依時間、天氣自動改暱稱 | |
| `.sudo` `.sure` | 把指令借給名單裡的人，見下文 | |

### 借用帳號：.sudo 和 .sure

兩者都是讓名單裡的人透過你的帳號執行指令：對方在群組裡傳訊息，帳號以你的身分在同一個對話裡
送出指令、回覆同一個目標。`.sudo` 讓對方直接傳指令；`.sure` 範圍更窄，對方的訊息要符合你設定的規則，
還能改寫，例如把群友傳的 `/sb` 變成 `.ban`。

和 MiBox 不同，**能借出去的是一份白名單**，也就是上面表格裡的「可借」一欄。MiBox 讓名單裡的人
執行任何指令，包括 `.sudo add` 本身，被授權的人可以再去授權別人。這裡依照別名展開之後的
真實指令判斷，取個別名也繞不過去；日後新增的指令、新增的子指令預設都借不出去。名單裡的人傳了
不能借的指令，帳號只會回一句沒有權限。

名單存在 `data/sudo.json` 和 `data/sure.json`，`.bf` 備份時會一起帶上。

### IP 遮蔽：.privacy

和 MiBox v2 一樣，帳號送出和編輯的每則訊息裡，IP 位址都會被遮蔽：預設 IPv4 遮蔽後 2 段、
IPv6 遮蔽後 4 段，指向 IP 的連結會被移除，檔名裡的 IP 也一樣。`.privacy ip mask 2 4` 修改遮蔽幾段，
`.privacy ip hide` 整個換成「[IP已隐藏]」。

遮蔽在連線層處理，所有指令的輸出都會經過它。替別人代發的指令（`.sudo`、`.sure`）不遮蔽，
那些位址本來就是對方自己打出來的。

## 設計取捨

### 為什麼不要外掛系統

MiBox 的主程式要載入任意 TypeScript 外掛，所以它必須常駐一個 JS 引擎、一個 SQLite 和一整套
外掛生命週期，這是它記憶體占用的主要來源。MiBot Lite 換掉的就是這個前提：指令不再是可以熱安裝的
元件，而是編譯進執行檔的 Go 函式。代價是新增指令要重新編譯；換來的是更小的常駐記憶體，和一份
不需要沙盒的程式碼。

### 影像與影片

`.yvlu` 和 `.eatgif` 要合成圖片。MiBox 用的是 sharp，它綁定 libvips：一個共用函式庫、一份影像快取和
一個執行緒池，為了一天用幾次的功能常駐在整個行程裡。這裡換成標準函式庫的 `image/draw` 加上
`golang.org/x/image` 的縮放器，執行檔只多 0.7 MB，閒置時不占記憶體。

唯一的外部相依是 ffmpeg：Telegram 的影片貼圖必須是 VP9，用純 Go 編碼不切實際。ffmpeg 是子行程，
不執行時不占記憶體，但**執行時要一百多 MB**。所以 systemd 單元把 `MemoryMax` 設在 512M，
而不是貼著常駐值設定，並且**不設** `MemoryHigh`。後者超過時不會失敗，只會反覆強制回收，把行程
拖慢：一次編碼裡觸發了一萬四千多次，編碼還沒跑完就撞上自己的逾時，也沒留下任何錯誤訊息。

和 MiBox 的兩處差異：

- **eatgif 不再先編碼成 GIF**。原本的實作先把影格編碼成 GIF 再轉成 VP9，中間那一步會把每一格壓到
  256 色。這裡直接把 PNG 影格序列交給 ffmpeg，省掉一次有損的中轉。
- **yvlu 不支援 tgs 動態貼圖轉換**。那條路需要 Python 的 `rlottie-python`，正式環境機器上本來就沒裝，
  移植它等於移植一個目前不能用的功能。其餘行為都對照原版逐條對齊過：多則訊息走真實的歷史紀錄而不是
  連續 ID、轉傳歸屬原作者、部分引用、管理員頭銜、emoji 狀態、大頭貼大圖備援。

## 開發

```sh
go test ./...          # 全部測試
go vet ./...
bash scripts/build.sh  # 帶版本號建置
```

連上真實帳號自我檢查（往「儲存的訊息」傳一組唯讀指令、核對回覆、再刪掉）。同一個帳號不能同時連線兩次，
所以要先停掉服務：

```sh
./mibot-lite --verify --root /部署目錄
```

用真實素材驗證影像合成（預設略過）：

```sh
MIBOT_EATGIF_ASSETS=/path/to/eatgif go test ./internal/imaging/ -run RealAnimation -v
```

程式碼結構：

| 位置 | 內容 |
|---|---|
| `cmd/mibot-lite` | 進入點 |
| `internal/app` | 連線與更新分派 |
| `internal/bot` | Telegram 封裝，包括限流重試和 IP 遮蔽這些連線層中介軟體 |
| `internal/command` | 指令註冊與分派 |
| `internal/commands/<指令>/` | 指令實作，一個目錄一條指令，或共用一套資料的一組指令 |
| `internal/commands/kit` | 指令共用的小工具 |

成組放在一起的指令：`aban` 是全部群組管理指令，`ids` 含 `.dc`，`sudo` 含 `.sure`，`eatgif` 含 `.eat`，
`.t` 在 `tts`，`.status` 等基本指令在 `core`。

新增一條指令：建立一個 `internal/commands/xxx/` 目錄，寫一個 `Register(a *app.App)`，在裡面呼叫
`a.Registry.Register(&command.Command{...})`，再到 `internal/commands/register.go` 的 `RegisterAll`
裡掛上。要讓它能借給別人，還得加進 `internal/commands/sudo/sudo.go` 的白名單。

儲存庫裡的測試只涵蓋不依賴外部環境的純邏輯。用假 Telegram 跑的行為測試、快照測試和連到真實服務的
測試（檔名 `*_local_test.go`，連同 `internal/commands/testkit` 和各指令的 `testdata`）只留在
維護者本機，已寫進 `.gitignore`。

## 授權

LGPL-2.1，與 MiBox 相同。

狀態卡片用的字型是 Noto Sans SC 的子集（`internal/statuscard/NotoSansSC-status-subset.ttf`），
依 SIL Open Font License 1.1 散布，授權條款見同目錄的 `NotoSansSC-OFL.txt`。
