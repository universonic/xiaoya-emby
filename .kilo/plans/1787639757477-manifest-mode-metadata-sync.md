# 元数据下载改造计划：清单模式 + 短路 + 原子下载

## 背景与目标

借鉴 `xiaoyaDev/xiaoya_emd_go` 的架构，将元数据发现机制从"递归爬取镜像 nginx autoindex HTML"（每目录 1 GET，串行 walk，数万次请求）升级为"单次下载预构建清单 `/.scan.list.gz`"（约 6.8MB / 70 万行，O(1) 请求），同时保留现有爬取逻辑作为回退路径。

## 已验证的事实（实测，2026-08-25）

- 镜像均提供 `/.scan.list.gz`：`emby.xiaoya.pro` 与 `icyou.eu.org` 内容完全一致（size 6838337），HEAD 返回 200 + `last-modified` + `etag`，`server: cloudflare`。
- 清单行格式：`YYYY-MM-DD HH:MM /绝对路径`，仅文件、无目录、无 size/etag，分钟级精度。
- **清单时间戳基准是 UTC-4（美东 EDT），不是 GMT 也不是北京时**：同一文件清单记 `06:13`，HEAD `Last-Modified` 为 `10:13:49 GMT`，分钟精确吻合。→ DB 必须记录时间基准，禁止跨基准比较。
- 清单顶层目录只有 8 个：动漫/每日更新/电影/电视剧/纪录片/纪录片（已刮削）/综艺/音乐。**不含** 我们默认 `sPaths` 中的 `/115`、`/ISO`、`/PikPak`（这些路径在镜像 HTML 中存在）。
- 镜像清单约每 2 小时重新生成（两镜像 last-modified 相差 2h）。

## 决策记录（已与用户确认）

| 决策点 | 结论 |
|---|---|
| 改动范围 | 清单模式+自动回退、整轮短路、原子下载、并发可配置。**不做**回收站、DoH/DoT、带宽限速 |
| 存量 DB 迁移 | **B：时差推断迁移**（见下文） |
| 清单未覆盖路径 | **警告跳过 + `--force-crawl` 开关**；不实现混合模式 |

## 详细设计

### 1. 镜像探测与选择（重写 `validateMirrors`）

现状：16 个镜像 × 5 次 GET 串行探测，仅按延迟排序。

改为：

- 并发探测所有镜像，每镜像：
  - `HEAD /.scan.list.gz`（3s 超时，最多 2 次）：成功 → 记录 `(lastModified, latency)`，进入清单候选。
  - HEAD 失败时再 `GET /` 做现有内容检查（响应含"每日更新"）→ 进入爬取候选（保留旧行为）。
- **新鲜度分组**：清单候选中仅保留 `lastModified == 全组最新` 的镜像，按延迟升序作为 `manifestMirrors`（取前 3 足够，但可全保留）。
- 其余 HTML 有效的镜像按延迟排序为 `crawlMirrors`（回退用）。
- `--force-crawl` 时跳过 manifest 探测，只做 HTML 检查（行为与现版一致，但探测并发化）。
- `Run()` 的 10 分钟后台重验复用同一套探测逻辑。
- 任何候选为空时不立即报错：清单候选空 → 自动回退爬取模式；两者皆空 → 报错。

### 2. 整轮短路（新增）

- `.metadata.db` 新增 meta 表：`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT)`。
- key：`scan_list_last_modified`（GMT 字符串，header 原值）、`time_base`（`manifest` | `http`）。
- `Sync()` 进入时（清单模式）：若 `time_base == "manifest"` 且 `scan_list_last_modified` == 本轮最优镜像清单的 Last-Modified → 记录日志并**跳过整个下载阶段**，返回 nil。
  - 注意：仅跳过下载；`config.go` 后续的 compareMetadata/syncMetadata 照常执行（Alist 侧的 purge 状态独立于元数据镜像，可能变化）。
  - 用精确相等而非 emd_go 的 30 分钟容差：最坏情况只是多解析一次 70 万行本地文本（秒级），无正确性风险。

### 3. 清单下载与解析（`syncManifest`，新增）

- 从 `manifestMirrors[0]` GET `/.scan.list.gz`；失败/解析为零条 → 依次尝试下一镜像；全部失败 → 降级调用现有爬取逻辑（见 §6）。
- 解析：gzip → 逐行 `strings.SplitN(line, " ", 3)`（兼容路径含空格；避免 emd_go 正则+Fields 的隐患）→ `time.Parse("2006-01-02 15:04", ...)`（得到 UTC 基准的分钟级 unix ts，自洽即可）。
- 过滤：路径须以某个选中根目录为前缀（`selectedPaths` 规范化为 `/电影/` 形式做前缀匹配）。
- 解析过程中构建**清单顶层目录集合**；对 `selectedPaths` 中不在集合内的路径（如 `/115`）：每轮 `slog.Warn` 提示"清单未覆盖，已跳过，可使用 --force-crawl"。

### 4. 变更检测与时差推断迁移（方案 B）

- 常态比较（DB 行与清单同基准）：`清单ts > dbModified` → 下载；`==` → 跳过。（均为分钟精度，同基准，无需容差。）
- 本地文件丢失检查保留：DB 有记录但磁盘 `os.Stat` 失败 → 重下载。
- **迁移（首轮清单模式，即 `time_base != "manifest"` 且 files 表非空时）**：
  - 对每个同时存在于 DB 与清单的路径：`delta = manifestTs - dbModified`；`nearest = round(delta/3600)*3600`；若 `|delta - nearest| <= 90` 秒 → 视为未变更，批量 `UPDATE files SET modified = manifestTs`（单事务），不下载；否则视为已变更 → 下载。
  - 原理：未变更文件的 delta ≈ 纯时区偏移（整小时 ± 分钟截断误差 ≤60s）；已变更文件的 delta 叠加了真实时间差。已知盲点：变更时间恰好落在整小时边界 ±90s 内会漏判（概率极低，接受）。
  - 迁移是幂等的：清单↔http 双向基准切换都适用（回退爬取写入 GMT 后再回清单模式会再触发一次，开销仅为一次全表 UPDATE）。
- 本轮成功完成后写 `time_base=manifest`、`scan_list_last_modified=<本轮清单 header 值>`。
- 爬取模式运行的轮次写 `time_base=http`（`modified` 仍来自 Last-Modified，现状不变）。

### 5. 下载执行（改造 `download`）

- **原子写入**：写 `目标路径.tmp` → 关闭 → `os.Rename(.tmp, 目标)` → `os.Chtimes(目标, manifestTs)`（清单模式）或 Last-Modified（爬取模式）。失败时删除 `.tmp`。
- DB 记录：`modified` 存清单 ts（清单模式）/ Last-Modified（爬取模式）；`size`/`etag` 仍取自 GET 响应头（保持现有的精确校验字段）。
- `Sync()` 开始时扫一遍 `downloadDir` 删除残留 `.tmp`（仅在本次运行要处理的选中路径子树内，避免误删）。
- 保留：每请求 3 次重试（3s/10s 退避）、镜像间顺序故障转移、`fsError` 本地错误不重试镜像、失败列表全局重试最多 5 轮（goto FINAL 结构可保留或重构为循环）。
- **并发可配置**：新增 `--download-workers`（int，默认 0 = 现有 `min(NumCPU,8)`），作用于下载 worker 池。

### 6. 回退路径

- 触发条件：`--force-crawl`；或所有镜像清单探测均失败；或所有清单镜像下载/解析失败。
- 回退时调用现有 `Sync()` 爬取主体（原样保留，含 Walk/HEAD/goquery 逻辑），仅共享新的原子下载写入与 worker 配置。
- 回退轮写 `time_base=http`，不写 `scan_list_last_modified`。

### 7. 清理（cleanup）正确性

- 现状：cleanup 删除"DB 有但 remoteMap 没有"的行。
- 清单模式下 `remoteMap` 只含清单覆盖路径 → **必须**把"未覆盖的选中路径"的存量 DB 行并入 remoteMap，否则 `/115` 等文件会被误删。实现：cleanup 遍历 local 时，跳过路径前缀属于未覆盖选中根目录的行。
- 爬取模式 remoteMap 语义不变。

## 代码改动点

- `engine/metadata.go`
  - 新增：meta 表 helpers（`getMeta`/`setMeta`）、`probeMirrors`（并发探测+分组）、`fetchScanList`（含镜像间故障转移）、`parseScanList`、`syncManifest`、`migrateTimeBase`、`.tmp` 清理。
  - 改造：`validateMirrors`（调用 probeMirrors）、`Sync`（模式分支：syncManifest / 现有爬取主体提取为 `syncCrawl`）、`download`（原子写 + Chtimes + modified 来源按模式区分）。
  - `NewMetadataCrawler` 增加参数：`forceCrawl bool`、`workers int`。
  - `Run()` 重验逻辑复用新探测。
- `engine/config.go`
  - 新 flags：`--force-crawl`（bool，默认 false）、`--download-workers`（int，默认 0=自动）。
  - `downloadMetadata` 透传新参数。
- `engine/doc.go`：版本号 → `v0.2.0`。
- `README.md`：补充新 flags、清单模式行为说明（含 `/115` 等路径的警告与 `--force-crawl` 用法）、首轮回迁说明。
- 不改：`browser.go`（指纹伪装/解压是独有优势，保留）、`alist.go`、compare/sync 两阶段逻辑。

## 失败模式与边界

- 清单格式突变（解析零条/正则失配）→ 该镜像视为清单不可用，转移；全部失败 → 回退爬取。
- 部分镜像清单陈旧 → 新鲜度分组天然排除（只取最新组），避免 cleanup 误删。
- 下载中断留 `.tmp` → 下轮启动清理；`.tmp` 永不进 DB。
- 迁移后同一分钟内内容再变 → 漏更至下一分钟（清单本身分钟精度，固有）。
- 用户磁盘上的文件 mtime 将被设为清单时间（UTC-4 基准的绝对时刻，偏 4 小时，纯展示层面，Emby 不敏感）。
- 镜像无清单且 `--force-crawl` 未开 → 自动回退，功能不缺失。

## 验证计划

1. `make linux-amd64` / 本机 `go build ./...` 通过，`go vet` 通过。
2. 用临时 `-D` 目录对真实镜像跑 `--mode 4`：
   - 首轮（旧 DB 或空 DB）：确认走清单模式、迁移日志、下载数合理、DB `time_base=manifest`。
   - 第二轮：确认日志出现"数据一致，跳过"。
   - `--force-crawl` 一轮：确认走爬取且结果与旧版一致。
3. 手动删除一个已下载文件 → 重跑确认仅补下该文件。
4. 构造 DB 中存在但清单不存在的路径 → 开 `--cleanup` 确认删除；构造 `/115` 存量行 → 确认**不被**误删。
5. 下载中 `kill` 进程 → 确认 `.tmp` 残留且下轮被清理、无半文件。
6. `docker build` 镜像通过（CGO sqlite 依赖不变）。

## 不在本次范围

回收站、DoH/DoT、带宽限速、Web UI、`ignoredDirs`/`ignoredExtentions`（现有 TODO，保持行为不变）、strm 校验与 URL 重写逻辑。
