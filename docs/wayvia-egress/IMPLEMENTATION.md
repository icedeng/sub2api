# 固定 IPv6 出口实施说明

实施分支：`main-wayvia`。方案来源：同工作区 `wayvia.dev/docs/IPv6账号独立出口接入方案.md`。本文件描述代码交付及检查边界，生产网络验收须单独执行。

当前源码版本为 `0.2.24-wayvia`，以 `backend/cmd/server/VERSION` 为准。该版本已在本地合并并验证，见 [v0.2.24 合并记录](../upstream-merge-v0.2.24.md)。

`0.2.23-wayvia` 于 2026-11-18 13:08:30（Asia/Shanghai）发布，见 [v0.2.23 合并及发布记录](../upstream-merge-v0.2.23.md)。发布 `20261118T041537Z`，306/5/1 条迁移；005 编辑保护保持、克隆库保存验证通过，25 个活动固定 IPv6 重启后原地址回显正常，账号配置/宿主服务保持，WS 继续关闭。独立任务 success/0、未回退；真实页面及模型业务仍由人工验收。

## 实现范围

### 2026-11-17：固定 IPv6 OAuth 账号编辑修复（已发布）

2026-11-17 13:38:44（Asia/Shanghai）按用户授权完成服务器发布，编号 `20261117T052440Z`，版本 `0.2.22-wayvia`，构建 `dcc6b3413-accountedit-31ba7c0a7ee6`，镜像 `wayvia/sub2api@sha256:8aa112141cf43200c6f4534d66ad6fa51e161b96fd971dfa2bf98b7a501304ca`。005 已生效；备份克隆库中账号 #10 的旧错误复现、新版配置保存及 Token 防篡改验证通过。正式账号配置、#4/#5/#6/#9/#10 原固定地址与宿主服务保持，WS 继续关闭，公网健康及前端摘要检查通过。等待空闲 240 秒超时后受控重启，切换至就绪约 11.124 秒，任务 success/0、未回退；最终页面编辑待用户验收。备份 `/root/wayvia-deploy-20261117T052440Z`；完整记录见同工作区 `wayvia.dev/releases/20261117T052440Z/DEPLOYMENT.md`。

- 线上只读日志确认：账号 #10 的普通编辑 PUT 多次返回 500，数据库错误为 `EGRESS_CONTROLLED_CREDENTIAL_WRITE_REQUIRED`。同一账号也存在成功保存，不能据此认为所有编辑都失败或已自行恢复。
- 根因：已发布的 002 保护函数将整个 `credentials` JSON 的变化都视作认证凭据变更，而编辑表单也在该对象内保存模型映射、订阅档位等配置。该保护和普通保存服务在上游合并前后未变；本次查明的是原有定制保护与编辑流程的兼容缺口，没有发现它们被合并覆盖。此前检查未覆盖此真实数据库保存场景。
- 新增独立迁移 `005_account_edit_settings.sql`，只允许 `model_mapping`、`compact_model_mapping`、`plan_type`、`intercept_warmup_requests`、`account_scheduling_threshold`、`temp_unschedulable_enabled`、`temp_unschedulable_rules` 的普通编辑。其余键默认继续按认证数据保护，不以敏感字段黑名单放行未知字段。
- 比较基于数据库当前行，不设置 `wayvia.credential_write=controlled`，不绕过身份、代理、固定地址、影子账号和调度保护。Token、身份、有效期、地址等变化仍走原受控流程；并发刷新后的旧 Token 写回仍被拒绝。真正的凭据冲突经 repository 转为 409，提示重新打开编辑窗口或使用固定 IPv6 页面的重新授权流程。
- 001–004 迁移及校验和保持不变。005 只替换保护函数，不改账号、Token、策略或绑定数据；沿用迁移事务和校验和账本，失败整体回滚。未来发布必须包含该迁移，仅替换前端不能修复。若回退应用，保留 005 即可与旧应用兼容；恢复旧保护函数会重新导致普通编辑失败。
- 本地检查：编辑弹窗现有 78 项测试通过（接口为 mock）；隔离 PostgreSQL 上固定 IPv6 模块测试通过，新增回归覆盖旧错误复现、增量迁移、迁移前后账号/策略数据一致、配置保存与删除、认证字段/未知字段/非对象载荷拒绝，以及刷新后旧 Token 写回拒绝；409 错误转换测试通过。修复阶段仅本地操作，随后按用户明确授权完成上述发布；未用正式账号试改配置或主动刷新令牌。

### 2026-11-14：运行故障自动恢复与真实连接证据（已发布）

2026-11-14 19:51:05（Asia/Shanghai）已完成应用切换，19:51:25 完成服务端及公网核验。发布编号 `20261114T113636Z`，显示版本保持 `v0.2.21-wayvia.0.1`，本机 Docker 构建镜像 `wayvia/sub2api@sha256:8e330259d2309afa0f2297131775754986c5e8b08f7f8ec758bbba4f69665abc`。004 迁移已执行；#3、#4、#5、#6、#7 原固定地址、策略版本和调度均保留，重启后逐地址 HTTPS 回显一致，四容器 healthy。备份 `/root/wayvia-deploy-20261114T113636Z`；详细证据见同工作区 `wayvia.dev/releases/20261114T113636Z/DEPLOYMENT.md`。

- 新增 `004_recovery_evidence.sql`，保留已发布 001–003 的内容及校验和。新增运行恢复队列和连接证据表；旧版明确因宿主检查或重启预检失败而阻断的严格账号可接续恢复，不自动接管其他人工操作失败。
- 宿主每 10 秒检查；HTTP/SSE、WS 的可识别网络异常进入后台恢复队列（每 2 秒处理）。请求主动取消、HTTP 401/429/5xx 等业务状态、普通出口账号不触发该恢复。持久任务按 5 秒起步、最长 5 分钟退避，重启接续幂等操作。已开始的业务请求不会自动重放。
- 恢复操作使用 `system:recover-ipv6` 审计身份，复用原控制操作、刷新排空、宿主地址准备、外部 HTTPS 回显、策略提交及门控收尾。始终使用原 owner、原绑定、原 IPv6；不调用地址轮换或重新分配，不主动刷新或重放 RT，不依赖 ID Token。AT/RT 的日常更新继续由原有令牌生命周期处理。
- 账号 disabled、手动暂停调度、全局关闭时等待；核验结束提交前再次锁定并检查账号状态。恢复不会写账号 schedulable/status，也不会打开用户手动关闭的调度。凭据不确定、身份冲突、地址归属/DAD/回显不匹配、配置拒绝等停止自动恢复；只对明确的暂时网络错误重试。人工轮换、退役或其他策略变更使旧任务失效。
- 修正 req v3.59.0 部分 HTTP/2 内部建连绕过 `SetDial` 的路径：固定 IPv6 客户端的 `SetDialTLS` 也接入相同受控 TCP，再执行原标准/指纹 TLS 握手；涵盖强制 HTTP/2 及池内重连。普通出口客户端不变。
- `httptrace.GotConn` 采集真实请求使用的连接，包括 HTTP 连接池复用；根据实际 socket 两端匹配底层连接编号，兼容标准 TLS 与指纹 TLS。记录 HTTP/SSE 响应头、流 EOF/错误；WS 单独记录握手与后续消息发送，复用会话仍引用同一实际连接。query、refresh、业务用途分别标识。连接回调缺失会明确标为“未取得连接证据”，不会用配置地址填充；若实际源地址与固定绑定不符则关闭连接并停止自动恢复。
- 证据范围是本机 socket 使用的源地址，不声称是上游服务器观测结果。HTTPS 回显仍为独立外部核验。响应头/WS 发送/流 EOF 都不等于模型成功，HTTP 错误状态照实保留。请求编号用于关联同次响应头与流结束，WS 每次消息有独立编号；不保存 Token、请求正文、URL 路径或查询参数。
- 写入采用容量 512 的异步队列，每秒最多 128 条，每批事务有 5 秒超时，不阻塞业务。每分钟清理至每 owner 最近 200 条、全站最近 50,000 条、最长 7 天（清理间隔内可短暂超过条数）；这是滚动诊断记录，不是完整审计。队列满或数据库失败的记录丢弃并计数，界面显示保存异常和本次启动后的全局丢失数量，不把缺记录解释为没有请求。正常停止会尽力排空，突然终止可能丢失尚未落库的尾部记录。
- 只读 `GET /api/v1/admin/wayvia-egress/accounts/:id/evidence?limit=50` 沿用管理员权限及受限密钥禁用规则，不额外要求 step-up。limit 为 1–100；母账号可看同 owner 的影子请求，影子账号仅看自身请求。
- 页面沿用现有单行列表、状态徽标、按钮及出口详情弹窗。列表显示自动恢复进度；详情提供重试次数、最早下次尝试/最近恢复时间、停止原因和真实连接记录，默认仅展示业务、每 5 秒更新，可展开查询/刷新/握手记录。旧地址证据单独标识，不冒充当前绑定。

本轮检查：隔离 PostgreSQL 中执行 `go test -race ./internal/wayviaegress`（数据库用例实际运行，非跳过），覆盖迁移、同地址恢复、退避/重启、暂停和凭据不确定门控、证据保留及影子隔离；本机标准 TLS 与 Firefox 指纹 TLS 的 HTTP/2 复用测试通过；repository 相关客户端测试及强制 HTTP/2 防绕路回归通过；相关 WS/service 测试、路由包编译检查通过；前端两组共 10 项组件测试及 vue-tsc 检查通过。旧迁移 001–003 与上次发布快照一致。

开发阶段完成上述本地针对性检查；本次发布完成 Docker 构建、004 迁移、原地址重启回显及本机/公网核验，未主动发送真实模型请求、刷新生产 Token 或注入生产故障。页面效果及真实业务证据仍由人工确认；生产故障演练须在可控窗口另行安排。

### 2026-11-14：新账号全自动启用与 AT-only 支持（已发布）

本节覆盖下方历史记录中的“新账号需手动点击启用”流程。2026-11-14 19:00:03（Asia/Shanghai）已完成应用切换，19:00:28 完成公网核验。发布编号 `20261114T105419Z`，显示版本保持 `v0.2.21-wayvia.0.1`，本机 Docker 构建的固定镜像为 `wayvia/sub2api@sha256:0341377e266f208c368859c72241c44901b555f1547340df34d17bdfd4e04c62`。003 迁移已执行，#5 由后台 `system:auto-ipv6` 首次尝试即完成配置，状态为 `strict_ipv6 / ready`、`schedulable=true`；固定地址与 HTTPS 回显均为 `2606:65c0:10:4c8:f5d0:9e3b:c60b:72e0`。原 #3/#4 地址及调度、主地址和路由保持不变，四容器 healthy。本次未发送真实模型请求或主动刷新 RT；验证的是升级回填自动启用与实际 IPv6 回显。备份为 `/root/wayvia-deploy-20261114T105419Z`，详细证据见同工作区 `wayvia.dev/releases/20261114T105419Z/DEPLOYMENT.md`。

- 新建、单个或批量导入普通 OpenAI OAuth 独立账号时，在同一数据库事务中暂停调度并写入持久任务。后台每 2 秒检查任务，串行调用既有固定出口流程：读取最新凭据、校验身份、分配或复用地址、宿主准备和 HTTPS 回显核验；成功后同事务提交 ready、调度开关及 scheduler outbox，再完成进程门控放行。正常流程不需要管理员点击按钮，也不依赖导入页面保持打开。
- 新增 `003_automatic_enrollment.sql`，不改已发布的 001/002 校验和。迁移接续旧版已导入但仍为 `pending_enrollment`、尚无绑定的新账号，包括当前状态下的 #5；不自动转换历史普通出口账号，也不重新启用已成功配置后被用户手动暂停的账号。已禁用账号保持禁用。
- 有效 Access Token 即可启用固定 IPv6，不再硬性要求 Refresh Token，也不读取 ID Token。AT 的签名、签发者、API audience、有效期和账号身份校验继续保留；有 RT 时复用原受控刷新。只有 AT 的账号在 AT 到期后无法自动续期，需要更新凭据，这是凭据能力限制，与固定 IPv6 配置无关。
- 总开关关闭或基础配置未就绪时，任务等待且账号不调度。首次配置中途失败的账号仍由自动队列收尾，总开关恢复不会跳过身份校验或遗漏开放调度。进程重启通过持久幂等键和操作记录接续；地址已分配时重试复用同一地址。
- 宿主准备、回显、DNS/联网及签名密钥服务的暂时故障按 5 秒起步、最长 5 分钟的退避延迟重试。真实凭据、身份或安全校验失败显示具体原因并保持暂停；刷新结果不确定时禁止自动重放 RT。补全凭据或完成关联授权后可重新排队。失败均不回退普通出口。
- 自动任务以 `system:auto-ipv6` 记录操作审计，仅能处理待首次启用的新账号；私有自动标记不接受 HTTP 参数。人工出口操作仍保留管理员认证和系统 step-up 开关规则。后台自动任务不弹出 TOTP。
- 管理页及账号列表展示“待自动配置 / 自动配置中 / 等待重试 / 配置失败”，自动处理期间隐藏手动启用按钮，统一显示状态原因；已就绪后显示固定地址。手动恢复入口仍用于异常处理及历史账号。

本地检查：隔离 Unix socket PostgreSQL 中运行出口包 `go test -race`，数据库测试实际执行；新增用例覆盖 AT-only、导入事务与回滚、迁移回填、总开关与账号禁用、重试复用绑定、凭据更新、刷新不确定、重启接续、成功后调度提交及手动暂停保护。补充总开关暂停/恢复和 JWKS 暂时失败分类回归通过。前端 6 项相关组件测试与 `vue-tsc --noEmit` 通过。开发阶段的上述检查不包含浏览器验收、真实 RT 交换或生产 IPv6 请求验证；本次发布的实际回显证据见本节开头。

部署须备份数据库与既有持久目录，再升级应用；003 会在启动时回填任务，总开关已开启且配置完备时会自动处理等待中的账号。旧发布脚本中“固定 2 条迁移 / 2 个绑定”的断言不再适用，须按实际账号和本次迁移调整发布检查。网络验收应检查账号最终 ready、schedulable、observed_ipv6 与分配地址一致；仅 `/health` 成功不足以证明首次启用完成。

### 2026-11-14：出口二次验证遵循系统开关与表格单行布局（已发布）

2026-11-14 18:06:31（Asia/Shanghai）已发布至 `api.wayvia.dev`，18:07 完成本地及公网核验。发布编号 `20261114T100023Z`，显示版本保持 `v0.2.21-wayvia.0.1`，镜像固定为 `wayvia/sub2api@sha256:5345ca98c7cbef98950acb8fa206f1aa2a1f49901ed2d01dc1faf8ea4eb5c6de`。运行二进制与前端资源哈希匹配本机 Docker 构建；系统二次验证开关保持关闭，未认证出口写请求仍返回 401。账号状态、密钥、迁移、网络和基础设施均保持原值，两个 OAuth 账号仍为 legacy、绑定 0。备份为 `/root/wayvia-deploy-20261114T100023Z`；证据见同工作区 `wayvia.dev/releases/20261114T100023Z/DEPLOYMENT.md`。

- 所有出口写操作及关联重新授权统一读取系统 `step_up_enabled`。关闭时不要求额外 TOTP；开启时复用现有浏览器会话的 15 分钟 step-up 授权，未验证或授权失效时才弹出验证码。
- 管理员认证、受限管理密钥限制、机器凭据禁止写入、账号/owner 身份校验、策略版本和幂等校验均保留。关闭 step-up 不改变登录 2FA，也不放宽固定 IPv6 的业务门控。
- 前端继续使用现有 `useStepUp`，由后端的 `STEP_UP_REQUIRED` 响应触发弹窗；不新增界面开关、数据库迁移或授权有效期。
- 桌面表格将 ID、账号、账号类型、固定 IPv6、最近核验、出口状态、状态说明及操作展开为独立列，每个账号同行展示。沿用 DataTable 样式、横向滚动和移动端响应式布局，复制、详情及操作逻辑不变。

本地检查通过：新增出口校验回归覆盖开关开/关、设置更新后立即生效、会话授权复用与失效、跨会话隔离、未启用 TOTP、机器凭据/缺少身份及依赖异常；原系统开关转换和 step-up 中间件相关测试通过，routes 包编译通过（过滤命令无匹配路由测试）。Vue SFC 脚本/模板编译、`git diff --check`、本机 Docker 完整构建及离线核验通过。本次未执行浏览器、真实 RT 刷新或固定出口业务验收。

### 2026-11-14：固定出口改用 Access Token（已发布）

2026-11-14 17:39（Asia/Shanghai）已发布至 `api.wayvia.dev`，发布编号 `20261114T093110Z`，显示版本保持 `v0.2.21-wayvia.0.1`。本机 Docker 完整构建 Linux/amd64，固定镜像为 `wayvia/sub2api@sha256:7f036cf032495cee73791ee555fd7585585997459636b0570da7a6a65ec6734c`，构建标识为 `530c5e3a4-wayvia-37cc3c63e934`。运行二进制、镜像、本机及公网健康、管理页和资源哈希核验通过；环境与密钥、原账号状态、两条扩展迁移、新账号保护触发器和 IPv6 网络均保留。全局开关保持开启，两个 OAuth 账号仍为 legacy、绑定为 0，未代替管理员启用固定出口。备份在 `/root/wayvia-deploy-20261114T093110Z`，发布证据见同工作区 `wayvia.dev/releases/20261114T093110Z/DEPLOYMENT.md`。

- 启用操作重新读取数据库中的最新凭据，以 `access_token` 的有效期和身份判断，不再要求 `id_token` 存在或有效。保存的旧 ID Token 仅保留兼容性，不参与出口、刷新和关联授权判断。
- 数据库中的 AT 使用官方 JWKS 校验 RS256、issuer、API audience `https://api.openai.com/v1`、有效期及 `chatgpt_user_id`；不能使用 ID Token 的 OAuth client_id 作为 AT audience。身份仍须与原账号及已确认的 owner 一致，Token 更新不重新分配地址。
- 提前刷新窗口由现有 OpenAI Token Provider 的 3 分钟常量注入。兼容原 `expires_at` 格式，签名 AT 自带的期限兜底；有效且不在刷新窗口内的 AT 不消耗 RT。已过期或缺少 AT 时按原客户端进行受控 RT 交换，旧 AT/ID Token 的有效性不作为刷新前置条件。
- 启用前刷新也持有 owner 锁，并在锁内重读凭据；并发刷新已经保存新 AT 时直接复用。尚未启用的新账号只能由经过管理员认证、按系统开关执行的二次验证和版本校验的启用操作取得 Token 端点专用控制租约；普通业务、查询、预热及后台刷新仍被待启用门控拒绝。
- 已启用固定 IPv6 的账号刷新继续走原固定地址，未就绪/暂停时不退回普通出口。Token 响应的身份与期限从 AT 提取，保存新 RT 前不再额外联网查询 JWKS；来源可信的响应直接进入既有 CAS、调度 outbox 和持久标记协议。保存失败或身份不一致仍进入 `credential_uncertain`。
- 新授权、关联重新授权和刷新结果使用同一 AT 解析入口，允许上游不返回 ID Token；未配置扩展时保留原项目行为。新账号仍在地址准备和回显成功后才能显式开放调度。

本次没有数据库迁移、界面或版本号变更。发布前生产两个账号的 AT 已只读确认 RS256 签名、API audience、身份和有效期正常；部署仅替换应用镜像，未调用真实 RT、未修改账号状态。

本地检查通过：使用专用 Unix socket PostgreSQL 执行整个出口包 `go test -race`，数据库用例实际运行；覆盖旧 AT/ID Token 到期后的刷新、无 ID Token、AT 验签与身份冲突、提前刷新期限、并发时重读最新 AT、RT 轮换保存、新账号保持暂停及固定出口禁止回退。service/repository 的相关 OAuth/刷新回归通过，routes 包编译通过（该过滤命令无匹配路由测试）；`git diff --check` 通过。没有进行生产 RT 刷新或固定 IPv6 业务验收。

### 2026-11-14：新账号首次业务请求保护与管理页调整（已发布）

2026-11-14 16:32（Asia/Shanghai）已发布至 `api.wayvia.dev`，发布编号 `20261114T082348Z`，显示版本仍为 `v0.2.21-wayvia.0.1`，镜像固定为 `wayvia/sub2api@sha256:a07a651f24eba085b5772c834c5caa3ad982da798d99785293584b044896738f`。扩展迁移现为 2 条；001 校验和未变，新账号保护触发器已生效。本地与公网健康、真实前端资源哈希、应用实例锁和原账号状态检查通过。线上总开关已由用户开启，本次保持开启；两个已有 OAuth 账号仍为 legacy、绑定为 0，未代替管理员启用固定出口，未调用真实上游模型。发布备份在 `/root/wayvia-deploy-20261114T082348Z`，详细证据在同工作区 `wayvia.dev/releases/20261114T082348Z/DEPLOYMENT.md`。

- 新增 `002_require_ipv6_for_new_accounts.sql`，不修改已发布的 001 校验和。升级前已有账号保持原策略；升级后新建、批量导入、复制或转为 OpenAI OAuth 的独立账号默认 `schedulable=false`、`require_ipv6=true`、`pending_enrollment`（界面显示“待启用”）。API Key 和其他平台不受影响。
- 数据库保护覆盖所有写入口，出站 Route 另行阻断待启用账号。服务未配置、全局暂停、地址准备或回显失败均不会回退到普通出口；手动/批量开启调度、修改类型或母账号、切回 legacy 不能绕过。
- 管理员开启固定出口服务后，对新账号点击“启用并开放调度”，按系统开关完成 TOTP 验证、身份核验、地址准备及外部回显成功后，在同一数据库事务中提交 ready、调度开关和 scheduler outbox。随后完成快照发布和操作收尾，才放行请求。`202` 只代表已受理。普通核验、轮换、重启、全局恢复不会擅自打开账号调度。
- `enable_scheduling` 默认为 false，只允许在 `mode=strict_ipv6` 时显式请求。仅开放此母账号且提交时仍须为 active；不启用 disabled 账号，也不修改影子账号的调度开关。
- 新影子账号只能在母账号已有可用固定绑定后创建，继承相同 IPv6，并将母账号锁定为必须使用固定出口，避免日后恢复普通出口影响新影子。旧账号不会因升级被静默切换地址。
- 重新授权保存凭据后仍保持待启用或阻断。首次启用失败且从未真正启用固定出口时，允许走授权专用控制流程补全凭据；已经启用过的账号必须使用原固定地址交换凭据。保证范围是首次**业务请求**，不包括创建账号前的浏览器登录和首次授权交换。
- 管理页复用 TablePageLayout、DataTable、Select、Pagination、BaseDialog、ConfirmDialog、复制提示与 TOTP 组件。节点/地址池/能力版本默认折叠；列表只保留主要状态和常用操作；历史地址、重新授权、地址轮换、已删除账号与操作记录按需打开。界面统一使用“启用固定 IPv6”，不再使用“纳管”。
- 管理页与全局控制共用状态快照，后台轮询不清空表格，慢请求不能覆盖更新的筛选结果。账号列表增加“待启用”中文状态，创建及编辑页说明暂停规则。

本轮检查记录：隔离 PostgreSQL 的完整出口包 `go test -race` 通过，新增用例覆盖批量导入、调度/类型绕过、重新授权、首次失败后的恢复、ready 同事务开放、disabled/未确认保持暂停、outbox 失败回滚及新影子保护。repository/service/routes 编译检查、`vue-tsc --noEmit`、3 项前端回归通过。实际组件的本地示例预览已检查确认弹窗、搜索空状态、账号详情以及桌面/窄屏布局；预览构建通过。检查独立于生产验证，未在生产启用或修改任何账号。

| 范围 | 入口与行为 |
| --- | --- |
| 数据与身份 | `backend/internal/wayviaegress/migrations` 独立校验和迁移；pool/policy/binding/operation；owner按可信chatgpt_user_id固定；影子继承，API Key原出口；缺失策略失败关闭。 |
| 地址与主机 | HMAC-SHA256长度编码分配，数据库唯一约束，最多64次候选；固定前缀和接口的Unix socket agent；SO_PEERCRED校验UID，deprecated `/128`、DAD、普通主地址核验；不改路由、不设置FREEBIND。 |
| 请求生命周期 | HTTP/SSE、TLS指纹、req、WS握手及池预热、刷新、模型/额度/订阅/隐私/邀请查询接入统一Route；连接按owner/账号/源地址/版本/用途隔离，停流关闭实际TCP并清池，迟到拨号拒绝。 |
| 严格门控 | 仅AAAA/tcp6/source bind，无IPv4/代理静默回退；暂停不恢复legacy；图片、Alpha Search、Live入口拒绝严格账号；OAuth Transport插件互斥。 |
| 普通源保护 | 公共HTTP、SOCKS/CONNECT、TLS指纹、req和WS在代理认证/TLS前检查实际LocalAddr，拒绝专用历史地址及批准池内非普通主地址。 |
| 凭据写入 | owner锁覆盖网络和保存；120秒独立刷新网络、5秒CAS；刷新标记先落盘，异常进入credential_uncertain；CAS与调度outbox同事务；补全字段单独白名单CAS，不覆盖新令牌/暂停状态。 |
| 控制操作 | 幂等键、乐观版本、202操作查询；终态提交与放行串行；pending及未结束数据库操作重启后阻断；局部恢复缩减marker；全局批次失败保持阻断。 |
| 恢复 | 旧绑定显式恢复；原软删除母账号ID/owner恢复且保持disabled/credential_uncertain；一次性关联授权会话绑定管理员与策略版本，TOTP、可信身份验签、原地址交换、直接保存不返回Token。 |
| 页面 | 独立管理页、账号出口列、账号编辑入口、系统设置统一开关；状态筛选、复制地址、观测时间、历史绑定/恢复、软删除恢复、近期操作和脱敏失败原因。 |
| 发布 | Linux host compose、systemd agent、不可变镜像检查脚本、`--wayvia-capabilities` 离线标识；禁止内置官方更新及两种二进制回滚。 |

## 兼容性取舍

- 回显支持有序 `probe_urls`，兼容旧 `probe_url`。连接/超时/状态码/格式失败可换回显服务，同一绑定 IPv6 不变；有效但不匹配的地址和网络安全错误直接阻断。示例加入 ipify、icanhazip、ident.me，部署脚本通过实际服务器多轮测速输出排序；运行时不隐式重排。多地址单站5秒、整轮15秒。
- 配置扩展后，普通可刷新OpenAI OAuth的刷新也进入受控保存协议，以覆盖“第一次纳管时刷新仍在执行”的竞态。未知身份或无法验签的导入资料需要关联授权；无账号原始Refresh Token联网入口拒绝。未配置扩展时，已有legacy入口保留原流程。
- 新身份首次授权仍是纳管前流程，不保证浏览器或第一次code交换使用未来地址；配置扩展时验签、查重后不再自动执行隐私/订阅联网补全，保存账号后再按账号查询。
- 重新授权是显式恢复操作；严格账号即使全局暂停也只能在原绑定预检通过后访问固定token端点。成功保存不自动开放严格业务。
- 固定出口影子账号当前随母账号一并停流和删除。强制固定出口账号不能为删除影子而切回普通出口。软删除恢复不恢复原分组关联、影子或计划任务，需人工配置。硬删除/丢失策略或绑定需恢复一致备份，不能重新分配 owner 掩盖数据丢失。
- 所有管理转换串行，单批预算180秒；大批恢复超时后逐账号恢复。agent最多地址数包含历史地址，不自动回收。节点/前缀/接口不能原地替换。
- `/health`仅是应用健康。`ready`表示本进程绑定预检和无凭据回显通过，不表示所有上游能力已经业务验收。页面的“已接入”仅表示代码覆盖。
- 现有Web后端管理路由及受限密钥校验保留；出口写操作按系统 `step_up_enabled` 决定是否要求 TOTP，开启时管理员须已配置2FA；机器凭据始终禁止写入。客户端关闭/超时不撤销已接受的异步任务。

## 本次检查记录

以下检查在本地完成，未连接生产数据库、未配置真实IPv6、未调用真实Token或模型接口：

- `go test -race ./internal/wayviaegress -count=1 -timeout 90s`：通过；通过显式 `WAYVIA_TEST_DSN` 连接本次专用Unix socket PostgreSQL，每例独立schema并清理。覆盖迁移重复执行/checksum、触发器约束、缺策略/影子/暂停、源地址保护、请求租约、迟到连接、101双工、取消后CAS、首次纳管刷新收尾、CAS失败/重启阻断、部分marker恢复、无marker未结束操作恢复、软删除原owner恢复、恢复事务回滚后重启重试、关联授权版本、outbox失败和元数据CAS。
- 相关既有Go回归：OpenAIWSConnPool、OpenAIOAuth、OAuthRefreshAPI、RefreshAccountToken、BuildAccountCredentials及新增Wayvia会话测试通过；涉及的service/repository/routes/admin/server包编译通过。路由和server包在该过滤命令下仅编译，并非端到端测试。
- 前端 `vue-tsc --noEmit`：通过。未启动浏览器、Playwright或完整前端构建。
- Linux/amd64 agent交叉编译：通过。应用 `--wayvia-capabilities` 离线运行输出 `wayvia-egress-v1`。compose/OpenAPI YAML解析及内部引用检查、部署脚本shell语法和 `git diff --check`：通过。没有运行Docker镜像或Linux辅助服务。

参考检查命令（从仓库根目录按需执行；使用自己的隔离测试库）：

```sh
cd backend
WAYVIA_TEST_DSN='host=/path/to/test/socket port=55439 user=test dbname=test sslmode=disable' \
  go test -race ./internal/wayviaegress -count=1 -timeout 90s
go test ./internal/service ./internal/repository ./internal/server/routes ./internal/handler/admin ./cmd/server \
  -run 'Test(Wayvia|OpenAIWSConnPool|OpenAIOAuth|OAuthRefreshAPI|RefreshAccountToken|BuildAccountCredentials)' -count=1 -timeout 90s
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/wayvia-egress-agent ./cmd/wayvia-egress-agent
go run ./cmd/server --wayvia-capabilities
cd ../frontend
pnpm exec vue-tsc --noEmit
```

没有设置 `WAYVIA_TEST_DSN` 时数据库用例会skip；不能将skip视为迁移验证通过。不要用生产DSN运行测试。

## 上线前人工验收重点

1. 在批准Linux网络验证两个固定IPv6、NDP/DAD/回包与回显；同身份母/影子一致，不同owner不串用，API Key普通源不串入专用地址。验证HTTP/SSE、WS及预热、Token刷新、账号查询的真实源地址。
2. 活跃长流/刷新期间暂停、退出、轮换、断开浏览器；确认流终止、新请求拒绝、迟到凭据保存，失败不回退IPv4。验证管理员权限、TOTP、同身份重新授权和旧绑定恢复。
3. 维护窗口验证进程重启、地址丢失、agent不可用、单活锁、一致备份恢复及错误镜像发布阻断。此次仅编译和本地测试，未完成真实Docker/agent/上游链路验收，不能仅凭代码检查将生产账号标记ready。

## 来源与协议核对

- 算法与总体思路参考 `DR-lin-eng/sub2api-no2api@5e97bf0d0e3359df264d9df04ec0b8274b0fd965` 的 `docs/IPV6_EGRESS.md`；本模块独立实现，不直接移植其启动补池/降级策略。
- 2026-11-14读取 `https://auth.openai.com/.well-known/openid-configuration` 和 `https://auth.openai.com/.well-known/jwks.json`：issuer为`https://auth.openai.com`、签名RS256、token endpoint为`https://auth.openai.com/api/accounts/oauth/token`。扩展配置有效时直接使用该endpoint且禁止Token POST重定向/自动重试；未配置时保留原项目端点。
- 公开发现文档核对不等于真实用户Token验签与IPv6业务验证。req v3.59.0的Dial及Transport包装接口按当前模块缓存源码接入。

部署命令、权限、备份和恢复操作见 `deploy/wayvia/README.md`；管理接口见同目录 `openapi.yaml`。
