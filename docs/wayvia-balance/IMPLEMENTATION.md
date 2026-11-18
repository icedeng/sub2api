# 上游余额查询实施与上游合并记录

实施日期：2026-11-16（Asia/Shanghai）。维护分支：`main-wayvia`。

本次修改的起始提交：`b893f17384163e66b966fabb5fb7c0f1369f1d2a`。本文件记录源码实现、宿主接入点和实际检查。**首版已完成服务器发布与部署核验，两家真实上游的余额业务仍待人工验收**。设计来源为同工作区 `wayvia.dev/docs/上游余额查询接入方案.md` v1.1；接口契约见 [openapi.yaml](openapi.yaml)。后续修改本模块时同步维护本记录。

## 1. 本次交付边界

- Sub2API `/v1/usage`、`/usage`，New API `/api/usage/token/`、`/api/status`；可选 New API `/api/user/self` 补充钱包。
- 明确类型与自动识别、手动刷新、定时刷新、停用、重新识别、页面内低余额提示；默认关闭账号监控，启用后自动刷新默认 5 分钟。
- 钱包、Key 额度、订阅周期独立表达；缺失、真实零、不限额独立表达。金额、Token 数、原始额度均用十进制字符串传输，不套用本站充值汇率。
- 列表紧凑展示，详情读取已有快照；当前页每 60 秒批量读取本地状态，隐藏列时停止读取。打开详情不访问上游。
- 账户编辑中的余额配置独立保存，不混入账号整体更新载荷；关闭账号编辑窗口不会撤销已经保存的余额设置。查询使用已保存的账号地址、Key、代理。
- 支持通过运维预警规则发送低余额邮件。不含通用自定义查询、旧版账单、Webhook、趋势图、余额汇总或自动停用。

未引用或整仓移植外部二开项目代码，未新增第三方依赖。没有修改模型请求计费、账号倍率、可调度状态、账号导入导出格式或固定 IPv6 出口配置。

## 2. 相对设计稿的存储调整

设计稿建议优先用 `accounts.extra.wayvia_upstream_balance`。实现改用**同一 PostgreSQL 内的独立附属表** `wayvia_upstream_balances`，原因是宿主账号更新会关联调度 outbox/cache，且旧账号编辑载荷可能覆盖 extra。余额刷新不应引发这些副作用。

- `account_id` 为主键，外键引用 `accounts(id)`，物理删除账号时级联删除；账号软删除后自动任务不再处理。
- `config`、`snapshot` 为模块私有 JSONB，`revision` 是余额配置版本，不复用账号的更新时间充当配置版本。
- `query_ciphertext`、`query_identity` 仅存模块表，不放进普通账号 JSON、前端状态、账号导出或共享元数据缓存。
- 使用独立迁移账本 `wayvia_balance_schema_migrations` 和事务级 advisory lock，不占用官方迁移序号。初始迁移为 `backend/internal/wayviabalance/migrations/001_balance.sql`。
- 迁移与 ledger 更新在同一事务内完成，已有迁移校验和不符会拒绝模块初始化。失败只令余额接口返回 503，不阻止原服务启动；运行日志提供模块及阶段信息，不输出凭据。
- 已发布迁移禁止原地改写；后续只追加 `002_*.sql` 等模块迁移。回滚旧应用时保留附属表与账本，避免重装时重复建表或丢失历史。

## 3. 文件归属与宿主接入点

### 3.1 新增的独立文件

| 路径 | 职责与合并要求 |
| --- | --- |
| `backend/internal/wayviabalance/types.go` | DTO、配置、快照、内部记录；金额字符串、可选值和旧 JSON 兼容性在此维护 |
| `backend/internal/wayviabalance/manager.go` | 装配、只读账号加载、配置 CAS、刷新、定时工作者、生命周期；不得接入核心计费或自动改账号状态 |
| `backend/internal/wayviabalance/query.go` | 固定查询路径、认证隔离、宿主 HTTP 客户端/代理/TLS 复用、响应上限 |
| `backend/internal/wayviabalance/coordination.go` | Redis 账号锁、站点串行名额、共享冷却、公开元数据缓存 |
| `backend/internal/wayviabalance/parse.go`、`url.go` | 严格字段语义、十进制处理、HTTPS 与子路径构造 |
| `backend/internal/wayviabalance/store.go`、`migrate.go`、`migrations/` | 模块附属表、原子写入与独立迁移 |
| `backend/internal/wayviabalance/alerts.go` | 按平台或分组只读采样余额快照，检查账号身份和快照时效，用十进制比较各账号阈值 |
| `backend/internal/wayviabalance/*_test.go` | 解析、传输隔离、退避、过载、取消、退出、配置/结果 CAS 和可选 PostgreSQL 集成回归 |
| `backend/internal/server/routes/wayvia_balance.go`、`wayvia_balance_test.go` | 独立管理路由及认证/受限密钥/step-up 回归 |
| `frontend/src/api/admin/wayvia-balance.ts` | 独立 API 契约、金额格式化、字符串阈值比较、错误文案白名单 |
| `frontend/src/composables/useUpstreamBalanceCache.ts` | 当前页缓存批读、取消、旧请求保护、页面共用时钟 |
| `frontend/src/components/account/UpstreamBalanceCell.vue` | 钱包/Key 摘要、详情、刷新及冷却状态 |
| `frontend/src/components/account/UpstreamBalanceSettings.vue` | 可折叠设置、独立保存、补充凭据与冲突提示 |
| `frontend/src/i18n/locales/{zh,en}/admin/upstreamBalance.ts` | 本模块独立中英文词条 |
| `docs/wayvia-balance/` | 实施维护记录及 OpenAPI；接口或接入点变化后同步更新 |

### 3.2 修改的宿主文件（合并时逐项检查）

| 路径 / 符号 | 本次修改 | 合并检查 |
| --- | --- | --- |
| `.gitignore` / docs 例外规则 | 放行 `docs/wayvia-balance/**` | 保留维护记录与契约的版本跟踪；其余本地 docs 忽略规则不变 |
| `backend/cmd/server/wire.go` / `initializeApplication`、`provideCleanup` | 添加 `wayviabalance.Provide`；将 Manager 交给 Router 和 cleanup | 保留构造依赖；在 DB/Redis 关闭前执行 `Manager.Close()` |
| `backend/cmd/server/wire_gen.go` | 由 Wire 实际重新生成 | 不手工解决生成代码冲突；先合并 wire.go 与构造函数，再重新生成 |
| `backend/cmd/server/wire_gen_test.go` / `TestProvideCleanup_WithMinimalDependencies_NoPanic` | 为新增 Manager 依赖补 nil 参数 | 与 cleanup 签名同步，不删除原生命周期回归 |
| `backend/internal/server/http.go` / `ProvideRouter` | 新增 Manager 参数，传入 SetupRouter | 随官方 Provider 签名演进，仅保留这一条依赖传递 |
| `backend/internal/server/router.go` / `SetupRouter`、`registerRoutes` | 可选 Manager 参数及独立路由注册 | 保留 `/api/v1/admin/wayvia-balance`；旧调用方可不传 Manager；不能漏挂管理中间件 |
| `backend/internal/server/middleware/audit_log.go` / `auditSensitiveReads` | 添加两条缓存 GET 的敏感读取审计 | 保留两条动作 `admin.upstream_balance.read`；写请求继续走原审计脱敏，`query_token` 命中既有 token 敏感键规则 |
| `backend/internal/service/ops_alert_evaluator_service.go`、`ops_upstream_balance_alert.go`、`wire.go` | 注入余额采样器并接入现有运维事件和邮件 | 保留 `OpsUpstreamBalanceReader` 的 Wire 绑定、未知状态处理和同一规则的告警去重 |
| `backend/internal/handler/admin/ops_alerts_handler.go`、前端 `OpsAlertRulesCard.vue`、`api/admin/ops.ts` 和中英文 `admin/ops.ts` | 注册 `upstream_low_balance_account_count` | 保留正整数账号数阈值、`>=` 比较和对应词条 |
| `frontend/src/views/admin/AccountsView.vue` | 添加上游余额列、批读 composable、单元格与更新事件 | 检查表格插槽、可见列设置及 Account 类型是否变动；不要把上游请求放进每行渲染 |
| `frontend/src/components/account/EditAccountModal.vue` | 挂载独立设置组件，转发 `balanceUpdated` | 保留独立保存；不能把密文或令牌塞进主账号 payload |
| `frontend/src/i18n/locales/{zh,en}/admin/accounts.ts` | 各增加一次 import 和 `upstreamBalance` 属性 | 保留词条命名空间，避免复制到官方通用词条中 |

核心账号 Entity、Repository、Account DTO、官方 schema 迁移目录没有修改。没有重新组织官方路由或账号表单；接入处使用 `Wayvia` 注释标识，不给每行代码添加冲突标记。

## 4. 上游合并操作顺序

1. 先按项目现有流程合并官方更新，保留本模块独立目录和附属表迁移，不覆盖其他 Wayvia 扩展。
2. 按 3.2 表逐项核对接入点。上游若原生增加余额功能，先对齐钱包/Key/订阅语义、单位、认证与数据迁移方案，再决定复用，不直接以同名字段替换本模块数据。
3. 检查宿主契约：`AccountRepository.GetByID/GetByIDs` 是否仍加载当前代理；账号 credentials JSON、账号及代理表字段、只读行锁顺序、`Account.ApplyHeaderOverrides/GetHeaderOverrides` 是否变化。
4. 检查 `HTTPUpstream.DoWithTLS`、禁止重定向/公共主机上下文、URL allowlist、TLS Profile 的语义是否变化。余额请求必须继续遵守账号出站限制，不能增加携带凭据的重定向或隐式协议探测。
5. 检查 `AdminAuthMiddleware`、`AdminComplianceGuard`、受限管理密钥语义、`StepUpAuthMiddleware` 及审计脱敏；不能只保留路径而漏掉权限。写入遵循宿主 `step_up_enabled`，关闭时沿用宿主行为，开启时复用现有会话授权。
6. 在 `backend/cmd/server` 执行 `go run -mod=readonly github.com/google/wire/cmd/wire` 重新生成 `wire_gen.go`，不要只 cherry-pick 旧生成文件。
7. 执行第 7 节的定向检查。若上游改了 accounts/proxies schema，还应在专用临时 PostgreSQL 验证本模块 SQL；不能只依赖 mock。
8. 在实际两家上游各选一个明确授权的账号做只读核对，确认钱包/Key 身份、单位、子路径、代理和限流，再发布。保留原镜像与持久加密配置，更新本记录的基线和检查结论。

## 5. 运行、权限与资源边界

### 刷新与失败隔离

- 每进程最多 4 个上游余额工作者；满载时手动请求返回缓存和 `query_busy`，不在后台无限排队。定时器每 30 秒查最多 100 个到期账号，固定 4 个工作者处理，不按全账号数创建 goroutine。
- 本轮总时限 20 秒，单个 HTTP 请求最多 8 秒，响应体最多 64 KiB（多读 1 字节识别超限）；取消后的结果写回另有 2 秒上限。退出先取消，再等待实际 singleflight 工作结束，最后关闭依赖。
- 账号及公开元数据锁租约 30 秒，释放校验持有者。同账号同进程 singleflight 合并；其他副本返回刷新中与旧缓存；共享协调不可用时停止新上游查询。
- 站点按规范化 `https://host:有效端口` 归并，跨 Key、代理、反向代理子路径共用初始并发 1。该值是首版保守值，两家真实站点核验后再评估调整；此限制不修改模型转发的限流配置。
- 余额、公开元数据、补充钱包接口均取得站点名额。429 和 5xx 结束相应请求，并共享冷却；有效秒数或 HTTP 日期 `Retry-After` 与本地退避取较晚者。秒数超过 Go duration 可表达范围或格式无效时按无效头走本地退避，不截成一年。
- 失败退避从 1 分钟指数增加并错峰，当前指数封顶 256 分钟；手动刷新另有 10 秒冷却。普通成功按配置间隔加 0–30 秒错峰；`next_retry_at` 兼作正常自动刷新到期时间，并非所有未来时间都禁止手动刷新。
- 失败保留上次成功结果及原成功时间，显示过期或部分数据状态，不伪造余额零。站点冷却写回到期时间，避免同站点账号反复占据到期批次页首。
- 定时任务只处理 active、已启用监控且开启自动刷新的 API Key 账号。停用监控保留历史快照并停止新定时请求，正在执行的旧结果会被配置版本校验拒绝；已发出的请求无法撤回。

### 元数据、URL 与金额

- `/api/status` 成功字段缓存 60 分钟，失败标记 1 分钟；元数据锁复查缓存。键使用站点根路径、代理 URL 和允许的安全请求头摘要，不含模型 Key；余额与钱包快照始终按账号隔离。
- 公开请求只允许 `User-Agent`、`Accept-Language` 覆写，不继承业务认证、Cookie 或自定义秘密头。补充查询的 Bearer 与用户编号仅用于同源固定钱包路径。
- 仅 HTTPS，保留反向代理子路径，移除末尾模型 API `/v1`，拒绝用户信息、query、fragment、点段、编码路径与重复分隔符。编码子路径首版保守拒绝，不自行解码猜测。
- 继续使用宿主出站 URL 校验、代理和 TLS Profile；禁止重定向。默认只允许公网主机，只有宿主明确启用并允许私网的 URL allowlist 时沿用其私网许可。
- New API 没有有效换算依据时保留 `quota` 原始单位；不使用过期元数据生成新金额。快照保存实际元数据，之后新缓存不反改旧快照。Sub2API 无单位时只保留原值；订阅 `_usd` 字段才明确使用 USD。
- 不用 Key 无限推断钱包无限，不用 `remaining` 无条件当钱包。今日费用只读 `actual_cost`，不退回不同口径的 `cost`。

### 凭据与原子写入

- 仅完整管理员可读写；受限管理密钥始终禁止。管理认证、面板限流、合规检查、审计、写操作 step-up 均复用宿主。管理结果带 `Cache-Control: no-store`。
- 补充令牌加密复用 `SecretEncryptor`（AES-256-GCM）；必须持久配置现有 `totp.encryption_key` / `TOTP_ENCRYPTION_KEY`（32 字节，64 个十六进制字符）。自动临时生成密钥时拒绝保存补充令牌；切勿为本功能重新生成或替换现有生产密钥，否则也会影响已有 TOTP 数据。
- `/api/user/self` 返回的用户编号必须与配置一致。系统**不能自动证明该令牌与模型 Key 属于同一个上游用户**，管理员需要人工确认；不支持网页登录密码或模拟登录。
- 身份摘要绑定平台、类型、credentials、代理 ID 与有效网络配置；变更后隐藏旧快照，下一次保存/刷新清空旧补充令牌及识别缓存。
- 配置写入使用 expected revision；结果写入再次只读锁定并复核账号/代理、配置 revision、身份和上次尝试时间，避免新配置或新结果被迟到请求覆盖。配置冲突返回 409 `CONFIG_CHANGED`，前端要求重载。
- 模块日志只记录脱敏类别/账号 ID，不保存原始上游响应。API 只返回令牌“已配置”，不返回明文、密文或内部身份摘要；备份附属表时应按敏感数据保护。

## 6. 开启、停用与回滚

1. 部署前保留当前镜像和原配置，确认同一 PostgreSQL 的迁移权限、共享 Redis 可用；使用补充查询时确认现有持久加密密钥，不能另行重置。
2. 首次启动自动执行模块独立迁移。所有账号默认未启用，只有管理员显式启用后才会查询上游；没有批量自动启用逻辑。
3. 在账号编辑内选择实际协议、保存余额配置，再手动刷新。钱包未提供时按权限说明处理，不把 Key 额度当钱包。
4. 临时停用：关闭对应账号“启用余额监控”并单独保存；或只关闭自动刷新保留手动查询。界面隐藏列只暂停页面缓存读取，不会关闭后台监控。
5. 应用回滚：恢复原镜像与原配置，保留两个模块表及加密密钥。旧应用不引用附属表，不需逆向修改 accounts；Redis `wayvia:balance:v1:` 锁和缓存自动过期，无需清空共享 Redis。
6. 若永久移除功能，先停止所有运行本模块的副本并备份敏感数据，再另行审批数据清理；本次不提供自动 DROP 或删除凭据的部署动作。

## 7. 本次实际检查与待人工核验

已执行：

- `gofmt`、最终差异及 `git diff --check`。
- `go test -race ./internal/wayviabalance -count=1 -timeout=120s`：解析、金额、URL、公开头隔离、元数据缓存/过期/退避、429、故障保留、调度、过载、取消退出、配置/结果 CAS。
- 路由认证/受限密钥/step-up 定向测试，宿主 step-up、合规中间件及 cleanup 定向测试；受影响 server/cmd 包编译。
- 专用临时 PostgreSQL 14.23：幂等迁移/checksum、配置与同配置结果 CAS、停用拒写、账号/代理身份冲突、reset、Due；测试 schema 中启用 accounts 写保护触发器。临时库已停止清理，没有访问现有数据库。常规包测试只有显式设置 `WAYVIA_BALANCE_TEST_DSN` 才运行该集成项。
- Wire 在 `backend/cmd/server` 实际重新生成，无 go.mod/go.sum 变化。
- 前端 `vue-tsc --noEmit` 与 `localeKeyCompleteness.spec.ts`（3 项）；没有启动浏览器、完整构建或部署。

人工核验重点（尚未执行）：

1. 两家实际上游的钱包/Key/订阅、币种换算、今日用量，与同一账号上游后台核对；提供补充令牌时核对同用户归属与只读权限。
2. 管理页启用、保存、刷新、关闭、重新打开，以及更改地址/Key/代理后旧数据显示与令牌绑定；核对两家站点的子路径、代理和限流要求。
3. 部署环境多副本同时刷新与 Redis 故障边界，确认版本装配、共享协调配置和业务转发未受影响。本地 mock/临时库检查不代替此项。

## 8. 2026-11-16 服务器发布

2026-11-16 11:38:48（Asia/Shanghai）完成发布核验，编号 `20261116T032413Z`，运行镜像 `wayvia/sub2api@sha256:40597f3be578a50bdf81ef3fd25b96ad004ab45508f9d60d34d9152ba725e3b7`，构建标识 `b893f1738-wayvia-59e400f006b6`。独立余额迁移已执行，默认监控关闭；既有官方/出口迁移、账号与固定地址保持。切换时品牌配置保持，切换后独立更新的 WayVia 条款已按其记录复核并保留。四容器健康，本机/公网健康、静态资源哈希和未认证接口拒绝检查通过。备份 `/root/wayvia-deploy-20261116T032413Z`。本次未执行真实上游余额查询、模型请求、主动 Token 刷新或浏览器视觉验收；详细证据见同工作区 `wayvia.dev/releases/20261116T032413Z/DEPLOYMENT.md`。

## 9. 运维低余额邮件

在「运维监控 → 预警规则」选择「上游余额不足账号数」，最少账号数默认 1，可限定分组。规则复用每个账号的低余额提示阈值及余额快照，按其钱包显示单位比较。规则的持续时间、静默、冷却与收件人沿用运维告警配置。邮件事件记录已确认不足的账号编号，同一规则的告警保持活跃期间只触发一次。

钱包金额缺失、仅有 Key 额度或原始额度、查询失败、快照过期或账号身份发生变化时，钱包状态按未知处理。只有未知值足以改变判定时才暂停判断。关闭监控或移除阈值会将账号移出规则范围。采样仅读取现有模块表，无需新迁移。
