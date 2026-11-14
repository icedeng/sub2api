# Wayvia 固定 IPv6 出口部署

此目录交付 **Linux 单机、rootful Docker host 网络、单实例、单 `/64` 池** 的扩展部署。代码入口为 `backend/internal/wayviaegress`，管理页面为 `/admin/wayvia-egress`，接口见 `docs/wayvia-egress/openapi.yaml`。

`compose.yaml` 是一个带新 PostgreSQL/Redis 的独立示例，**不是现有生产 compose 的叠加文件**。下述步骤由运维在批准的主机和维护窗口执行；本次开发没有执行生产迁移、配置地址或发送真实 OAuth/模型请求。

## 1. 必要条件

- Linux 主机获准使用一个完整公网 `/64`，供应商允许额外源地址，已验证 NDP、回包、TCP/TLS、必要 ICMPv6 和长连接。只有一个 IPv6 地址的 VPS 不满足条件。
- 主机已有一个可用、非 deprecated 的普通 IPv6，作为 `primary_ipv6`。专用地址按需以 `/128` 添加，不替换主地址、路由或 DNS；`/128` 表示一个地址，不是 128 个地址。
- rootful Docker；禁止 rootless、Docker Desktop、Swarm/Kubernetes 多副本或多个不同状态目录访问同一数据库。应用 host 网络且 UID/GID 固定 `1000:1000`；辅助服务运行于宿主机。
- 宿主安装 iproute2，确认 `/usr/sbin/ip` 是系统提供的可执行文件。辅助服务使用此固定绝对路径，不执行 PATH 中的同名文件。
- systemd 服务使用宿主 `/usr/bin/curl` 和 `/usr/bin/timeout` 检查 Unix socket 启动就绪；启用服务后会在 Docker 启动前完成此检查。`RuntimeDirectoryPreserve=yes` 保留 socket 父目录，防止辅助服务重启导致容器目录挂载失效。若修改 socket 路径，应同步修改 unit 中的就绪检查。
- 经管理员批准的 HTTPS/443 回显服务列表（`probe_urls`），可使用自建服务或经核验的公共服务。每个地址需有 AAAA 和有效证书，GET 返回 HTTP 200，正文只含真实客户端 IPv6 文本，可带换行，最多 256 字节；不跳转、不返回 JSON，不以客户端提交的 Forwarded/X-Forwarded-For 冒充地址。经过 CDN 的服务也必须返回实际客户端地址，不能返回 CDN 到源站的连接地址。
- 出口写操作遵循系统“敏感操作二次验证”开关（`step_up_enabled`）：开启时管理员须先启用 TOTP，复用当前会话的 15 分钟验证授权；关闭时不要求额外验证码。管理员认证和机器凭据禁止写入的限制始终保留。使用 TOTP 时，`TOTP_ENCRYPTION_KEY` 必须是固定 64 位十六进制密钥并备份；已有密钥不得删除或重新生成。
- 停用 OpenAI OAuth Transport 插件；纳管母账号及影子不能设置外部代理，仅支持普通可刷新 OpenAI OAuth。API Key 保持原有出口。

## 2. 构建和准备

沿用根目录 Dockerfile（Go 版本以 `backend/go.mod` 为准，pnpm 9）。从仓库根目录执行：

```sh
docker build -t your-registry/sub2api-wayvia:YOUR_VERSION .
# 按现有镜像发布流程推送，记录不可变 RepoDigest，不使用浮动 latest。
# 辅助服务在有兼容 Go 工具链的构建环境编译，按目标主机选择 amd64/arm64。
cd backend
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/wayvia-egress-agent ./cmd/wayvia-egress-agent
cd ..
```

辅助服务不在应用容器内运行。主机安装示例（只用于首次部署，不覆盖已有密钥和清单）：

```sh
sudo install -d -m 0750 -o root -g 1000 /etc/wayvia-egress
sudo install -d -m 0755 /var/lib/wayvia-egress
sudo install -d -m 0700 -o 1000 -g 1000 /var/lib/wayvia-egress/app
sudo install -d -m 0700 -o root -g root /var/lib/wayvia-egress/agent
sudo install -D -m 0755 /tmp/wayvia-egress-agent /usr/local/libexec/wayvia-egress-agent
sudo install -m 0640 -o root -g 1000 deploy/wayvia/config.example.json /etc/wayvia-egress/config.json
```

编辑配置，替换节点和地址示例值：`2001:db8::/32` 是文档地址，程序会拒绝。`prefix` 必须为规范 `/64` 网络地址，`node_id`、`interface`、主地址和保留地址由运维批准。`probe_urls` 已列出三个公共服务，使用前须在实际服务器核验并排序。保持三个目录分离，应用不得写 agent 清单或 socket 目录。`max_addresses` 包含历史/保留地址，默认 1024；地址不会自动回收。

生成独立分配密钥（32 个随机字节，可用 `openssl rand 32` 写到临时文件后 `install -m 0640 -o root -g 1000` 安装为 `/etc/wayvia-egress/allocation.key`）。**不要覆盖已存在的密钥。** `key_id` 与备份记录一致；运行绑定以数据库为准，不能换密钥后重算已有地址。

```sh
sudo install -m 0644 deploy/wayvia/wayvia-egress-agent.service /etc/systemd/system/wayvia-egress-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now wayvia-egress-agent
sudo systemctl status wayvia-egress-agent --no-pager
```

agent 启动仅监听 Unix socket，不分配地址。它校验请求进程 UID、固定前缀/接口、owner、绑定版本、地址上限；先持久保存所有权，再添加 deprecated `/128` 并等待 DAD。不提供任意 shell、路由、FREEBIND 或自动删除地址接口。

### 多回显服务与测速排序

`probe_urls` 支持1至8个不同地址，按配置顺序尝试。示例包含 `https://api6.ipify.org`、`https://ipv6.icanhazip.com`、`https://v6.ident.me`。列表非空时优先使用列表；只有未提供列表时才兼容旧 `probe_url`，不会把旧值额外拼入列表。

在实际出口服务器执行（脚本可单独上传，只需 Python 3 和 curl）：

```sh
python3 deploy/wayvia/rank-probes.py --source 已绑定的主公网IPv6 --rounds 5 > probe-ranking.json
```

脚本强制指定 IPv6、HTTPS/1.1、新连接、证书验证和无代理，不添加地址、不携带账号凭据、不跟随跳转。
每个服务做5轮，轮换请求顺序，按成功次数优先、成功请求总耗时中位数次之排序；总耗时含 DNS/TCP/TLS/响应读取。
将输出的 `probe_urls` 写入部署配置。脚本不修改配置或启用账号；主地址测速不能替代账号专用地址的纳管核验。

运行时首站成功即结束；连接失败、超时、非200或响应格式异常会尝试下一个配置地址，全部失败则保持阻断。
返回有效但不匹配的 IP、实际 socket 源地址错误或目的地址安全校验失败时立即阻断，不能被下一站成功掩盖。
所有尝试使用同一绑定 IPv6，无代理、IPv4 降级和重定向。多地址单站最多5秒，整轮最多15秒（到期即停止，可能尚未尝试完全部地址）；旧单地址仍最多15秒。
排序由运维测速后固定保存，不在页面 GET、后台监控或每次业务请求中自动测速/改序。配置更新后重启应用使其生效。

## 3. 启动独立 Docker 示例

在仓库根目录，先复制 `deploy/wayvia/.env.example` 为该目录 `.env` 并填写秘密及已发布镜像摘要。示例中的 `REPLACE_*` 都不是可用秘密；JWT/数据库/Redis 可分别用 `openssl rand -hex 32` 生成，不复用分配密钥。

```sh
sudo install -d -m 0700 -o 1000 -g 1000 deploy/wayvia/data
docker pull your-registry/sub2api-wayvia@sha256:ACTUAL_DIGEST
sh deploy/wayvia/check-image.sh your-registry/sub2api-wayvia@sha256:ACTUAL_DIGEST
docker compose --env-file deploy/wayvia/.env -f deploy/wayvia/compose.yaml config --quiet
docker compose --env-file deploy/wayvia/.env -f deploy/wayvia/compose.yaml up -d --no-build
curl --fail http://127.0.0.1:8080/health
```

应用仅监听 `127.0.0.1:8080`；使用现有宿主反向代理提供 HTTPS。PG/Redis 仅绑定 `127.0.0.1:15432/16379`。无 `NET_ADMIN`、无 Docker socket 挂载。预先创建 `/run/wayvia-egress` 的 agent 服务必须早于容器启动，避免 Docker 自动创建错误所有者的目录。

首次启动自动执行官方迁移，再执行独立 `wayvia_schema_migrations` 校验与事务迁移；为现存母账号创建显式 legacy，默认全局关闭。数据库账号需要建表、函数、触发器权限。扩展迁移错误会保留后台诊断、阻断 OAuth；`/health` 成功**不等于**出口就绪，需检查 `/api/v1/admin/wayvia-egress/status` 的 `configured/error` 和账号状态。

既有部署升级：先备份并停止原实例和相关出站任务；复用原数据库、Redis、JWT/TOTP、业务数据目录，修改主机连接地址及卷映射。不要启动此示例的新库代替原库，不自动迁移生产数据。

## 4. 账号操作

1. 在独立管理页核对节点、前缀、错误及能力版本，开启“允许运行纳管 OAuth”。无严格账号时此操作不会自动纳管任何账号。
2. 取消母账号代理、确认没有同身份独立账号。导入的未知身份/过期 ID Token 通过“关联授权”恢复可信身份；导入过程不隐式刷新。配置扩展后无账号的原始 RT 刷新入口拒绝，脚本改用已有账号刷新入口。
3. 点击“纳管”，确认流中断并完成 TOTP。返回 202 只表示已接受；等待 `completed` 且 `ready=true`。失败显示 blocked，修复网络/权限后用新幂等键“恢复核验”；不能改数据库状态伪装 ready。
4. GET、页面刷新、复制和历史查询只读；“探测”才主动请求固定回显，每账号至少间隔 30 秒。它会暂时停止该账号业务；暂停期间不接受普通探测。
5. 暂停会关闭新业务、WS/HTTP 存量连接，等待已发出的刷新保存。退出纳管必须额外确认恢复原出口；保留旧地址及绑定。轮换只由管理员主动触发，不因 403/429 自动换地址。

管理操作全局串行；一个批次共用 180 秒预算，大量账号恢复可能超时并全部保持阻断，之后按账号恢复。刷新网络最多 120 秒、CAS 保存最多 5 秒；compose 的停止预算为 240 秒。不要 `kill -9` 或缩短停止期限。

## 5. 恢复流程

| 情况 | 操作 |
| --- | --- |
| agent/地址丢失 | 修复 agent、主地址和供应商网络；点击“恢复核验”。不自动切 IPv4。 |
| 轮换新地址失败 | 打开“历史绑定 / 恢复”，选择本 owner 的 active/retained 旧绑定；仍经过停流、agent 和回显。pending 地址不能当作已验证旧绑定。 |
| `credential_uncertain` | 不重放旧 Refresh Token；点击“重新授权”，登录原身份。严格账号须原绑定预检通过，成功后仍 blocked，再恢复核验。全局暂停时此显式操作只有 token 端点控制权限，不放行业务。 |
| 软删除后恢复 | 从“可恢复的已删除账号”恢复原账号 ID/owner，仍 disabled、不可调度且凭据待确认；先恢复历史绑定（解除同 owner 的 agent 退役），再关联授权、恢复核验，最后人工配置分组并启用账号。影子、分组关联、计划任务不自动恢复。 |
| 退出后再次纳管 | 使用原绑定并重新校验签名身份，不生成第二个 owner。 |
| pending 文件/未完成操作 | 重启保守阻断；凭据待确认或已退役状态保留，其余变为 blocked。恢复账号事务未提交时仍从已删除账号入口重试。部分全局任务恢复后只清对应 owner 标记。禁止手工删除 marker 来放行。 |
| 缺策略、缺绑定、清单损坏或硬删除 | 停止业务，从同一窗口的一致备份恢复；不能自动创建 legacy/新 owner，不支持通过界面把别的账号接到墓碑 owner。 |
| 分配密钥丢失 | 已有地址仍按数据库核验，新增/轮换失败；从原备份找回密钥，不重算旧地址。 |

删除严格母账号前自动执行退役、保存刷新并等待业务结束；超出浏览器超时后到最近操作查看结果，再重试删除。严格影子单独删除需先退出母账号纳管。凭据待确认必须先恢复确认，不能通过删除跳过。agent 退役保留地址；人工移除仅在停流及确认无连接后执行。

## 6. 备份、发布和回滚

- 一致备份包括官方账号及关联表、全部 `wayvia_*` 表、`settings.wayvia_ipv6_egress`、分配密钥/key_id、配置、agent 清单、app pending/refresh 文件、JWT/TOTP 秘密和镜像摘要。先暂停并等待操作完成，再停应用，保持同一备份窗口。
- 升级前运行 `check-image.sh`，核对候选扩展迁移兼容性；暂停、等待刷新保存、停止旧进程，再启动新镜像。**不做滚动双实例**。共享目录的 flock 防同目录双活，运维仍须保证没有不同目录实例连接同库。
- 应用内官方更新、备份二进制回滚、指定版本回滚均拒绝。Docker 镜像替换无法被应用阻止，发布流程必须先运行能力检查。检查脚本只验证扩展能力标识，不代表数据库兼容性或真实 IPv6 验收。
- 仅回滚到保留扩展且迁移兼容的已验收镜像。回到官方原版必须另行维护：严格账号及影子在官方可识别状态中禁用，检查手工刷新/授权等旁路仍不可调用；必要时隔离或移除凭据可用副本。未完成该专项验证不得让原版接单。
- `/var/lib/wayvia-egress/app/instance.lock` 保留到进程退出，不能删除锁文件强行双开。不要 `docker compose down -v`。

## 7. 本地检查与待验收

开发检查、源码覆盖范围和限制见 `docs/wayvia-egress/IMPLEMENTATION.md`。手工上线验收至少覆盖：两个账号及影子实际源地址、HTTP/SSE/WS/查询/刷新全链路；长流暂停和迟到刷新；崩溃、地址丢失、轮换恢复、重启和一致备份恢复。需要隔离环境的真实 Linux 网络条件，不在生产构造故障。

本版本不支持严格账号的图片、Alpha Search、Live；发送前明确拒绝。新身份首次浏览器授权/首次 code 交换发生在纳管前，不承诺使用未来绑定。除兼容 API Key 的公共 TCP 保护外，不承诺任意插件或自定义二进制获得内核级账号隔离。
