# ex-member-sandbox

独立本地会员沙箱，Go + SQLite。提供注册、登录、邀请链接及会员数据，模拟真实用户系统向 ExCommission 投递注册事件。

## 运行

需要 Go 1.25 或更新版本，使用纯 Go SQLite 驱动，不需要 C 编译器。只绑定本机回环地址。

1. 先启动支持注册接入的 ExCommission，显式启用 `LOCAL_REGISTRATION_ENABLED=true`，设置独立 `LOCAL_REGISTRATION_KEY`。
2. 将 `config.example.json` 复制为 `.local/config.json`，填写本地服务端令牌、签名密钥、租户、应用和佣金地址。私有配置权限设为 `0600`。这些值必须与佣金服务的受控来源一致，示例中的文字不能直接用于运行。
3. 启动：

```sh
mkdir -p .local
go build -o .local/member-server ./cmd/server
.local/member-server -config .local/config.json
```

默认访问 `http://127.0.0.1:18280/`。注册后稍候即可看到代理编号。点击“生成我的邀请链接”，让另一会员通过该链接注册，可验证直属关系。

如果已有本地佣金沙箱的私有 `runtime.json`，可以显式指定它生成配置，工具不打印、不覆盖凭据：

```sh
python3 tools/init_local.py --commission-config /你的本地目录/runtime.json
```

本地数据库与原始注册事件保存在 `.local`，不进入 Git。停止时发送 SIGTERM/SIGINT；重启使用相同数据库和配置即可继续处理原事件。会话仅在内存中，重启后重新登录；注册数据不会丢失。

## 数据与边界

- `internal/app/store.go`：独立会员存储和事务 outbox。
- `internal/app/commission.go`：唯一佣金接入边界，按原始字节签名投递，并校验响应身份。
- `internal/app/server.go`：本地注册登录、HttpOnly/SameSite 会话和同源校验。
- `internal/app/web`：注册页、当前身份、邀请链接和最近会员数据。

会员与注册事件同一 SQLite 事务提交。事件固定 `event_id`、外部会员编号、注册时点和邀请码，重试保持原载荷和幂等键。佣金侧原子创建用户映射、代理及首次上下级关系。

临时网络错误、5xx、408、429自动退避重试。处理中的事件保存30秒租约，崩溃后到期续投；旧租约不能覆盖新处理结果。业务4xx标记 BLOCKED，保留本地会员和原邀请事实，不改为无上级或重新注册来伪装成功。此演示没有手工修改或重放被拒绝事实的入口。

浏览器不持有佣金令牌或签名密钥。密码使用独立随机盐及 PBKDF2-SHA256（600000轮）保存，接口不返回密码材料。本地会员列表只展示姓名、编号、同步状态和关系，不返回其他会员邮箱。写接口校验固定 Host 和同源 Origin；服务不接受对外绑定，也不跟随上游重定向。

## 接口

- `POST /api/register`：邮箱、会员名称、密码、可选邀请码。201表示本地注册完成，代理接入状态需要查询，不代表已同步成功。
- `POST /api/login`、`POST /api/logout`：本地会话。
- `GET /api/me`：当前会员及接入状态，必须登录。
- `GET /api/members`：最近100位沙箱会员的公开演示字段，必须登录。
- `POST /api/invitation`：仅为当前会话会员生成链接，代理接入成功后可用。
- `GET /healthz`：进程状态；`GET /readyz`：本地SQLite可用性。

JSON写请求使用 `Content-Type: application/json`，并携带与 `PublicURL` 一致的 `Origin`。接入状态为 PENDING、PROCESSING、SUCCEEDED、BLOCKED；具体拒绝原因在当前会员的 `reason` 字段保留，未知网络结果不冒充成功。

## 验证

```sh
go test -race ./...
go vet ./...
```

测试覆盖注册事务、重复邮箱不覆盖邀请、上游失败原事件重试、签名、租约恢复与旧结果隔离、永久拒绝、重启持久性、登录退出、同源防护及敏感字段不泄漏。真实本地佣金联调另外验证根代理→邀请链接→子会员注册→直属及祖先链一致性，不将测试桩当成真实集成结果。

【涉及其他业务】对接 ExCommission 的注册来源、用户映射和代理关系。仅供本地测试，不接真实会员平台、充值、提款、钱包、投注或发奖；不宣称具备生产身份或容量保障。当前使用显式授权的本地沙箱身份，真实部署需要独立来源和服务身份。

## 独立有效会员事实适配工具

`tools/qualification_source.py` 是可选的本地来源适配器，不改会员注册服务、原注册 outbox 或会员数据库表结构。它只读确认会员存在，合成成功充值/已结算有效投注事实，并保存到另一个 `.local/qualification-source.db` 后签名投递。它不充值真实钱包，不创建真实游戏订单。

先启动启用 `LOCAL_QUALIFICATION_ENABLED=true` 的佣金扩展服务，再显式生成私有配置：

```sh
python3 tools/qualification_source.py init \
  --commission-config /本地佣金扩展目录/.local/runtime.json \
  --member-config /正在运行的会员项目/.local/config.json
python3 tools/qualification_source.py create-policy \
  --deposit-minor 5000 --valid-bet-minor 10000 --venue SPORTS
```

上述门槛表示充值50 USD且有效投注100 USD。规则不可覆盖，后续新规则在初始化时指定新的 `--policy-id`；不能只改门槛覆盖已有规则。

登记一笔模拟成功充值和一笔模拟已结算有效投注：

```sh
python3 tools/qualification_source.py record --member-id 现有会员编号 \
  --business-id demo-deposit-001 --type DEPOSIT_SUCCEEDED --amount-minor 5000
python3 tools/qualification_source.py record --member-id 现有会员编号 \
  --business-id demo-bet-001 --type BET_SETTLED --amount-minor 10000 --venue SPORTS
python3 tools/qualification_source.py query --member-id 现有会员编号
```

`DELIVERED`仅表示事实被达标模块接受，应以查询的 `is_effective`、`qualified_at` 判断是否满足双门槛；无邀请归属的根会员不算受邀有效会员。此工具未接入原网页，因此原会员页面不会自动新增达标信息。

同一业务号重复执行保留首次业务时间和原载荷；改变金额、会员或场馆拒绝。网络故障保留PENDING，可以执行 `deliver` 按原事件重试；4xx业务错误保留BLOCKED，修复依赖或配置后显式执行 `deliver --retry-blocked`。这个CLI不带后台调度，不伪装自动持续重试，生产来源需要可靠outbox消费者。租户/应用/来源/规则/币种指纹隔离已有源事实，配置变化不会误投旧作用域数据。

适配器测试：`python3 tools/test_qualification_source.py`，覆盖原事件重试、订单不可覆盖、未知会员拒绝、永久错误保留及跨作用域隔离。真实联调已验证充值30、投注100仍未达标，再充值20达标，重复订单不增加累计。退款、注单撤销和资格撤回未实现，不应发送负金额或覆盖旧事实。

【涉及其他业务】新增事实来源只调用佣金有效会员模块；原注册、会话、会员表及钱包逻辑未修改。新增代码位于独立功能分支，与原运行实例隔离。
