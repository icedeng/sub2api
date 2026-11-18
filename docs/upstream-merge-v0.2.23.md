# 上游 v0.2.23 合并记录

日期：2026-11-18（Asia/Shanghai）。分支：`main-wayvia`。本轮完成本地合并与相关检查；随后按用户授权于 2026-11-18 13:08:30（Asia/Shanghai）完成正式发布，详见下文。

## 基线和范围

- 合并前自有基线：`4799987fd741028d561df4f98584d56d346c8a54`。
- 本地备份分支：`codex/backup-wayvia-before-v0.2.23-20261118`，指向上述基线。
- 上游正式标签：`v0.2.23`，提交 `5af9afb588c4b014b3df9db7d4073af1f8ae01b6`，GitHub release 发布时间为 2026-11-18 11:51:33（Asia/Shanghai）。合并固定标签，未额外合入其后的开发提交。
- 版本：`backend/cmd/server/VERSION` 改为 **`0.2.23-wayvia`**。该标签中的 VERSION 仍为 `0.2.22`，上游随后仅用一次提交同步为 `0.2.23`；自有版本按正式标签和根目录 AGENTS.md 规则设置，不丢失 `-wayvia` 后缀。
- 上游相对 v0.2.22 修改 90 个文件，主要涉及模型目录、渠道 Ultra Fast 定价、缓存写入 TTL 计量/计费、流式首输出前空闲超时重试和流处理性能。

## 定制保留检查

合并前相对上游 v0.2.22 有 206 个定制文件，其中 202 个未与此次上游变更重叠。合并后逐文件与自有基线比较，这 202 个文件内容相同。本文及出口实施说明的当前版本提示是核对完成后新增的文档改动。

仅以下 4 个文件存在改动交集：

| 文件 | 合并处理与保留内容 |
| --- | --- |
| `backend/cmd/server/VERSION` | 唯一文本冲突，设置为 `0.2.23-wayvia`。 |
| `backend/internal/service/openai_codex_models_service.go` | 自动合并新增模型元数据；保留固定 IPv6 Route 与模型查询健康观察/故障处理。 |
| `backend/internal/service/openai_gateway_chat_completions_raw.go` | 自动合并上游批量 flush；保留自有 `ClientDisconnect` 结果字段。 |
| `backend/internal/service/openai_gateway_service.go` | 自动合并缓存用量字段；保留健康服务注入与 `ClientInterrupted` 语义。 |

后三个文件额外比较“合并前相对旧上游的自有补丁”和“合并后相对新上游的自有补丁”，新增/删除行内容一致。

重点保留：固定 IPv6 与自动恢复、OAuth 编辑的 005 配置白名单保护和 409 提示、账号健康检查、上游余额查询、Codex 工具上下文归一化和脱敏诊断、桥接续接/中断、注册邀请参数回填。没有用上游文件整体覆盖自有实现。

## 数据库兼容性

- 原有官方 305 条、出口 5 条、余额 1 条迁移内容全部保持，特别是已经发布的 `005_account_edit_settings.sql` 不作空白或内容调整，保持原校验和。
- 新增官方 `252_channel_pricing_ultrafast_multiplier.sql`：为 `channel_model_pricing` 增加可空的 `ultrafast_multiplier NUMERIC(12,6)`，只允许 NULL 或正数；NULL 使用模型默认倍率。未改变原有定价数值。
- 已用本地隔离 PostgreSQL 执行上游现有集成回归：旧定价记录保留、重复迁移、配置值保留、创建/更新/替换和无效值拒绝后的原数据保留均通过。
- 自有固定 IPv6 模块测试连接独立本地 PostgreSQL，重新覆盖旧版 OAuth 编辑错误复现、005 增量迁移、普通配置保存/删除、Token 与身份写入拒绝、旧 Token 不覆盖新 Token。
- 本地合并阶段未连接正式数据库；随后发布已完成正式备份、克隆库演练和迁移核验。旧应用可忽略新增可空字段，无需通过删除列或恢复旧数据库回退。

## 已执行的检查

- 前端 10 个测试文件、182 项测试通过：覆盖本次渠道定价与使用说明改动，以及 OAuth 编辑、固定出口界面、出口证据、注册邀请和 i18n 完整性。
- `vue-tsc --noEmit` 通过。
- 后端 `-tags=unit`：固定 IPv6、余额、健康内容、协议转换、OpenAI 模型工具、迁移包测试通过；handler/admin、handler、repository、service 按本轮上游改动和既有定制测试文件选取的相关回归通过（含缓存计费、Ultra Fast 定价、模型目录、流式空闲超时和健康/Codex 路径）。
- `-tags=integration` 的 `TestChannelUltrafastPricingMigrationPreservesExistingCards` 与 `TestChannelUltrafastPricingCreateUpdateReplace` 通过，使用临时 PostgreSQL/Redis 容器。
- 完整后端 `CGO_ENABLED=0 go build -trimpath` 通过，构建版本为 `0.2.23-wayvia`。
- 最终检查无未解决冲突，合并差异空白检查通过。检查用临时数据库、构建文件已清理。

## 验收边界

没有运行浏览器、真实上游模型请求或全库所有测试；测试通过不等于所有生产业务均已验收。后续部署后重点人工确认：固定 IPv6 OAuth 账号编辑保存、原 Codex 会话/工具续接，以及渠道自定义定价和用量账单。新模型目录支持也不意味着会自动改写现有账号的模型白名单。

## 正式发布（2026-11-18）

13:08:30（Asia/Shanghai）已发布 `0.2.23-wayvia`，发布编号 `20261118T041537Z`，构建 `019fdf2a2-release-7adf715bae5d`，镜像 `wayvia/sub2api@sha256:f6eb4d293db2d101e09b09fbf205f87feadbfc02edbe79fcb94daacc22e04ea7`。独立任务 success/0、未回退；等待空闲后正常切换，约 11.197 秒至健康就绪。

正式备份和克隆库迁移演练通过，新增 252 已生效，现为 306/5/1 条迁移；005 与旧迁移摘要保持。克隆库 OAuth 普通配置保存及 Token 防篡改通过，正式保护函数摘要一致；没有试改正式账号。账号/健康/余额/计划/定价控制与原配置保持，25 个活动固定 IPv6 重启后原地址回显一致、ready，其他容器及宿主服务未重启。

服务器与工作站两域名健康 200、无 Key 模型接口 401、WS 426，新前端五项资源哈希一致。备份 `/root/wayvia-deploy-20261118T041537Z`；完整证据见同工作区 `wayvia.dev/releases/20261118T041537Z/DEPLOYMENT.md`。回退仅换旧镜像、保留兼容迁移及业务数据；真实页面编辑、Codex 续接与定价账单仍由人工验收。
