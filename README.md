# trade-tms-go

Trade Management System（Go 版）。目标：将 Python 参考实现
`trade-multi-strategies` 完整移植为单一静态二进制 `tms`，语义与参考实现
精确一致（除非显式标注 IMPROVE），不删减任何功能。

## 架构概览

单二进制、多子命令；所有部署形态（数据导入、回测、hyperopt、live 节点、
HTTP API）都是 `tms <subcommand>`：

```
cmd/tms/                  CLI 入口（cobra）：version / migrate / import /
                          backtest / hyperopt / live / eod / api
internal/
  domain/                 核心值类型：bar、signal、order、fill、position（零依赖）
  core/                   事件、clock 抽象、交易日历等跨层原语
  engine/                 确定性事件循环 —— 回测与 live 共用同一代码路径
  exec/                   ExecutionClient：回测模拟成交 + moomoo paper/live
  strategy/               Strategy 接口与全部策略移植（centralized-params 方案）
  indicators/             技术指标，与 pandas 语义逐位对齐（golden tests）
  portfolio/              仓位、风险限额、多策略资金分配与组合记账
  data/                   Sharadar 导入、TimescaleDB repository、Parquet（arrow-go）
  adapters/               外部集成：Nasdaq Data Link、moomoo OpenD、Redis streams
  hyperopt/               参数空间、trial 调度、study 持久化（runs/ 布局兼容）
  runs/                   回测/hyperopt/EOD 运行工件管理
  api/                    chi + coder/websocket，对应参考实现的 FastAPI 层
  app/                    进程级设施：zerolog、信号上下文、优雅停机、版本信息
  config/                 唯一配置入口：.env 加载 + MissingConfig 快速失败
  db/                     pgx v5 连接池 + golang-migrate（migrations 内嵌于二进制）
```

依赖方向：`domain` 不依赖任何 internal 包；`engine` 只面向接口；
I/O 全部收敛在 `adapters` / `data` / `db`；任何包不直接读 `os.Getenv`，
统一经 `internal/config`。

## 基础设施

- PostgreSQL：timescale/timescaledb（pg16），宿主端口 **55432**
- Redis 7，宿主端口 **56379**
- API 宿主端口 **18080**，UI 宿主端口 **13000**（本项目保留端口，勿改）
- 迁移内嵌在二进制中，`tms migrate up` 是 schema 变更进入环境的唯一途径

## 快速开始

```bash
# 1) 配置
cp .env.example .env   # 按需修改；真实环境变量优先于 .env

# 2) 启动基础栈（postgres + redis，并自动执行迁移）
make compose-up        # 即 docker compose up -d --build --wait postgres redis migrate

# 3) 本地构建与验证
make build             # 产出 bin/tms（注入版本信息）
make test              # go test -race ./...
make vet fmt-check     # 静态检查

# 4) 试用 CLI
bin/tms version
bin/tms migrate status # 需要 .env 指向 55432 的 compose 库
bin/tms import --help  # 导入命令骨架（实现于 P0 数据阶段）

# 5) 集成测试 / 收尾
make itest             # compose up + go test -tags integration
make compose-down
```

尚未实现的子命令（backtest / hyperopt / live / eod / api）会以非零退出码
显式报 `not implemented`，不会静默假装成功。

## Docker

```bash
make docker-build      # 多阶段构建：golang:1.26 -> distroless static (nonroot)
docker run --rm tms:dev version
```

## 开发约定

- 验收标准：ACCURATE（与 Python 参考语义一致）/ COMPLETE（不删功能）/
  NO SIMPLIFICATION / PRODUCTION-GRADE（错误处理、context 取消、优雅停机、
  结构化日志、正常路径零 panic）。
- 指标与策略以参考实现产出的 golden 输出做回归校验。
- 配置缺失在启动期以 `MissingConfig` 快速失败，错误信息附设置指引。
