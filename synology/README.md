# FileBrowser Quantum — Synology DSM 套件（fork 專用）

這個目錄與下列檔案是本 fork 相對於上游 [gtsteffaniak/filebrowser](https://github.com/gtsteffaniak/filebrowser) **唯一**的差異，全部都是「新增檔案」，沒有修改任何上游既有檔案：

| 路徑 | 用途 |
|---|---|
| `synology/` | `.spk` 打包腳本、DSM 整合設定、本文件 |
| `backend/internal/synothumb/` | 把影片縮圖寫入 Synology `@eaDir` 的產生器（只用標準函式庫 + ffmpeg） |
| `backend/cmd/synology_thumbs.go` | 啟動掛勾（設 `FILEBROWSER_SYNOLOGY_THUMBNAILS=true` 才會啟用） |
| `.github/workflows/synology-spk.yaml` | 自動跟上游、建置並發佈 `.spk` |

## 套件內容

- FileBrowser Quantum（靜態連結，前端內嵌，含 PDF 預覽）
- FFmpeg / ffprobe：直接取自上游 Docker 映像檔所用的同一個版本，會跟著上游自動更新
- DSM 主選單圖示、啟停腳本、防火牆服務定義（連接埠 8580）
- `/filebrowser/` 路徑註冊到 DSM 自己的網頁伺服器（讓 QuickConnect 可用）

需求：DSM 7.2 以上。x86_64（Intel/AMD）與 aarch64（ARM 64 位元）各有一個檔案。

## 安裝

1. 到本 repo 的 **Releases** 下載對應 CPU 的 `.spk`（不確定 CPU 請查 Synology 官方「我的 Synology NAS 使用何種 CPU」）。
2. DSM → **套件中心 → 手動安裝** → 選檔。會出現「非 Synology 驗證的第三方套件」警告，按同意。
3. 安裝完成訊息會顯示登入帳號 `admin` 與**隨機產生的密碼**（也存在 `/var/packages/FileBrowserQuantum/var/environment`）。
4. 授權共用資料夾（DSM 7 的第三方套件不能以 root 執行，預設看不到任何共用資料夾）：
   **控制台 → 共用資料夾 → 選資料夾 → 編輯 → 權限** → 左上下拉選單從「本機使用者」改成「**系統內部使用者**」→ 找到 `FileBrowserQuantum` → 勾「可讀寫」。
   - 想讓 Synology Drive「我的雲端硬碟」裡的影片也有縮圖，要授權 `homes`。
   - 只給「唯讀」也能瀏覽，但就無法寫入 `@eaDir` 縮圖。
5. 套件中心把套件**停用再啟用**一次。每次啟動時會自動把「有權限讀取的共用資料夾」各列為一個來源。

## 開啟方式

| 方式 | 網址 |
|---|---|
| DSM 主選單圖示 | 自動選擇下面可用的那一個 |
| 經 DSM（區網、QuickConnect 都走這個） | `https://<NAS 或 QuickConnect 位址>/filebrowser/` |
| 直接連接埠（區網／自行做連接埠轉送） | `http://<NAS>:8580/filebrowser/` |

## 從套件中心的「套件來源」安裝與更新

Workflow 每次發佈後會更新一份套件清單，放在本 repo 的 GitHub Pages（`gh-pages` 分支）。加入後，套件會出現在套件中心的「社群」分頁，有新版時 DSM 會顯示「更新」。

一次性設定：

1. GitHub repo → **Settings → Pages** → Source 選 *Deploy from a branch*，Branch 選 `gh-pages`、資料夾 `/ (root)` → Save。
2. DSM → **套件中心 → 設定 → 套件來源 → 新增**，名稱自訂，位置依 NAS 的 CPU 與想跟的通道擇一：

| CPU | 通道 | 位置 |
|---|---|---|
| Intel / AMD | stable | `https://freewifi-tw.github.io/filebrowser/x86_64.json` |
| Intel / AMD | beta | `https://freewifi-tw.github.io/filebrowser/beta-x86_64.json` |
| ARM 64 位元 | stable | `https://freewifi-tw.github.io/filebrowser/aarch64.json` |
| ARM 64 位元 | beta | `https://freewifi-tw.github.io/filebrowser/beta-aarch64.json` |

注意：

- 清單是靜態檔，無法依 DSM 回報的架構過濾，所以**一定要選對 CPU 的網址**；選錯時安裝會被 DSM 拒絕，不會裝壞。
- 尚未實機驗證：靜態主機只能回應 GET。若 DSM 是用 POST 查詢套件來源，GitHub Pages 會拒絕，套件就不會出現在「社群」分頁——那時仍可用上面的手動安裝。
- 清單只提供正常版（不含 `-portonly`）。

## 更新

- 下載新版 `.spk` → 套件中心 → 手動安裝 → 選檔即可原地升級。
- 設定、資料庫、快取都在 `/var/packages/FileBrowserQuantum/var/`，升級與移除都會保留。
- 套件版本格式為 `<上游版本>-<建置流水號>`，例如 `2.1.5-0012`；流水號只增不減。

## 跟上上游

`.github/workflows/synology-spk.yaml` 每天執行一次：

1. 列出上游的 `vX.Y.Z-stable` 與 `vX.Y.Z-beta` tag，各取最新一個。
2. 這個 fork 還沒發佈過的，就直接 checkout **上游該 tag 的原始碼**，把 overlay（上表的 Go 檔）複製進去。
3. 建置前端、以與上游 `.goreleaser.yaml` 相同的參數編譯 amd64 / arm64 靜態二進位檔。
4. 從上游 `_docker/Dockerfile` 指定的 ffmpeg 映像檔取出 ffmpeg / ffprobe。
5. 跑縮圖產生器的測試（用打包進去的 ffmpeg）、在 glibc 容器內試跑兩種架構的二進位檔。
6. 打包 `.spk`，發佈成 Release `<上游 tag>-syno.<流水號>`（beta 標為 pre-release）。

因為原始碼是建置當下才從上游抓的，**這個 fork 不需要先合併上游**就能出新版，也不會有合併衝突。
想讓 fork 的 `main` 在 GitHub 上看起來也是最新的，按 GitHub 的 **Sync fork** 即可（overlay 都是新增檔案，不會衝突）。

手動觸發：Actions → *synology spk* → Run workflow，可指定上游 tag、強制重建、是否發佈。
修改 `synology/` 或 overlay 並推到 `main` 也會自動重建最新版。

第一次使用前請在 GitHub 上：

- **Actions 分頁啟用 workflow**（fork 預設是停用的）。
- 把上游帶來的其他 workflow（`main release`、`tag update` 等）在 Actions 頁面逐一 **Disable**——它們需要上游的 Docker Hub 金鑰，在 fork 上只會失敗。
- 注意：repo 連續 60 天沒有任何活動時，GitHub 會自動停用排程，屆時到 Actions 頁面重新啟用即可。

## 影片縮圖（`@eaDir`）

啟用後（套件預設啟用）有兩條路徑同時運作，替還沒有縮圖的影片在
`<資料夾>/@eaDir/<影片檔名>/` 寫入縮圖：

- **即時監看**（inotify）：影片寫完關閉或被搬進資料夾的幾秒內就處理，不論是從 FileBrowser、Synology Drive、SMB 或手機備份進來。
- **定期掃描**：每 30 分鐘掃一次所有來源作為保底（例如資料夾數量超過系統 `fs.inotify.max_user_watches` 上限時，超出的部分只靠掃描；記錄檔會有一行提示）。

寫入的檔案：

| 檔案 | 尺寸上限 | 讀取者（依社群資料，非 Synology 官方規格） |
|---|---|---|
| `SYNOVIDEO_VIDEO_SCREENSHOT.jpg` | 852×480 | File Station / DS file |
| `SYNOFILE_THUMB_S` / `_M` / `_XL.jpg` | 120 / 360 / 1280 | File Station / DS file |
| `SYNOPHOTO_THUMB_SM` / `_M` / `_XL.jpg` | 240 / 320 / 1280 | Synology Photos |

行為：

- 已存在且不比影片舊的縮圖（包含 DSM 自己產生的）**不會被覆寫**。
- 定期掃描時，修改時間在 2 分鐘內的檔案先跳過（可能還在上傳／同步）；即時監看則是等檔案被關閉後才處理。
- 略過 `@eaDir`、`#recycle`、`#snapshot` 等 `@`、`#`、`.` 開頭的資料夾。
- 產生成功後會刪掉同目錄的 `*.fail`（DSM 的「轉檔失敗」標記）。
- 失敗的影片在檔案沒變動前不會重試（重啟套件後會再試一次）。

設定（寫在 `/var/packages/FileBrowserQuantum/var/environment`，改完重啟套件）：

| 變數 | 預設 | 說明 |
|---|---|---|
| `FILEBROWSER_SYNOLOGY_THUMBNAILS` | `true` | 設成其他值即關閉 |
| `FILEBROWSER_SYNOLOGY_THUMBNAIL_PATHS` | 全部來源 | 只掃描這些資料夾，以 `:` 分隔 |
| `FILEBROWSER_SYNOLOGY_THUMBNAIL_INTERVAL` | `30m` | 定期掃描的間隔 |
| `FILEBROWSER_SYNOLOGY_THUMBNAIL_WATCH` | `true` | 設成 `false` 關閉即時監看，只靠定期掃描 |

## 尚未在實機驗證的部分

這個套件是照 Synology 開發者文件與其他套件的實作寫的，建置流程有自動測試，但**沒有在真的 NAS 上裝過**。第一次安裝時請確認下面三點：

1. **`/filebrowser/` 路徑（QuickConnect 的關鍵）**
   用的是文件有記載的 `web-config` 資源（`type: dsm`），但找不到其他第三方套件使用它的實例。
   - 若套件**裝不起來**，改裝同一版的 `-portonly.spk`（不動 DSM 網頁伺服器，只能用 8580 連接埠，QuickConnect 不通）。
   - 若裝得起來但 `https://<NAS>:5001/filebrowser/` 打不開，主選單圖示會自動退回 8580 連接埠。
   - 這個資源會讓 DSM 在套件安裝、啟動、停止時重新載入 nginx（DSM 網頁會瞬斷一下）。萬一安裝後 DSM 網頁異常，從套件中心（或 SSH `synopkg uninstall FileBrowserQuantum`）移除套件即可還原。
2. **Synology Drive 是否採用 `@eaDir` 縮圖**
   File Station 有多個社群回報有效；Drive 找不到任何正反資料。請放一支影片到 Drive 資料夾，等掃描完成後看 File Station、Drive 網頁版、Drive App 是否出現縮圖。
3. **套件使用者能否寫入既有的 `@eaDir`**
   `@eaDir` 由 DSM 以 root 建立；一般情況權限是全開的，但若記錄檔出現大量 `permission denied`，表示該共用資料夾需要另外調整權限。

## 檔案位置與除錯

| 項目 | 位置 |
|---|---|
| 記錄檔 | `/var/packages/FileBrowserQuantum/var/filebrowser.log` |
| 設定檔 | `/var/packages/FileBrowserQuantum/var/config.yaml` |
| 環境變數（管理員密碼、縮圖設定） | `/var/packages/FileBrowserQuantum/var/environment` |
| 資料庫 | `/var/packages/FileBrowserQuantum/var/database.sqlite` |
| 預覽圖快取 | `/var/packages/FileBrowserQuantum/var/cache/` |

- `config.yaml` 第一行是 `# managed-by-package` 時，每次啟動都會重新產生（來源＝有權限的共用資料夾）。想自己維護設定，把第一行刪掉即可。
- 連接埠 8580 與路徑 `/filebrowser` 同時寫在啟停腳本、nginx 設定、防火牆定義與啟動頁，要改必須一起改後重新打包。
- 完全移除資料：移除套件後刪除 `/volume1/@appdata/FileBrowserQuantum`。各資料夾內已寫入的 `@eaDir` 縮圖不會被移除。

## 本機打包

```bash
synology/build-spk.sh --arch amd64 --version 2.1.5-0001 --upstream-tag v2.1.5-beta \
    --bin-dir <含 filebrowser、ffmpeg、ffprobe 靜態執行檔的資料夾> \
    --icon frontend/public/img/icons/favicon.png --out-dir out
```

請在 Linux（或 WSL）上執行；在 Windows 的 Git Bash 上打包出來的檔案會沒有執行權限。
