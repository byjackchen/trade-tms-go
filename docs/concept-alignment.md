# TMS 概念对齐与数据结构映射

> 目标:把系统统一到 **4 个一等概念**(Strategy / Model / Account / Portfolio),并在 DB → domain → engine → API → UI 全栈对齐命名。本文件标出每处的「现状结构 / 是否对齐 / 需要的改动」。
>
> 原则:**用户面向的 4 个名词全栈对齐**。**parity 已弃用**(Python `trade-multi-strategies` 停用,所有 `[MUST-MATCH]` 约束作废),为清晰自由改名。务实取舍:纯数值类型(`Money/Price/Qty`)无改名收益、保留;账本核心(`AccountSnapshot/Position/Allocator/RiskConstraints`)按 **D5(b)** 一并改名到 Portfolio 语义。
>
> **决策状态:全部已定**。A 组(IA)全取建议、B 组(清理力度 D2–D5)全取激进选项(删 Mode / 删 Allocation / 硬切 model_id / 全面改名)。详见 §4。

### 流水线原则(本设计的脊柱)

> **Backtest 的对象永远是 Model;Hyperopt 的对象永远是 Strategy。**

```
Strategy --(Hyperopt 调参)--> tuned param_set
                                         ↘
  strategies + weights + params + risk = Model --(Backtest 验证 / Optimize 联合调参)--> 通过
                                                       ↓ 部署
                                                  Account --运行--> Portfolio(paper/live)
```

- **单策略回测** = 回测其**单成员 Model**(`sepa-only`);无独立 strategy 回测入口(A3)。
- **单策略调参** = Strategies 模块对一个策略 tune,产出 `param_set` 供 Model 引用。
- **多策略联合调参 (joint)** = 优化一个 Model 的成员 = **Models 模块的 "Optimize"**(A2)。

---

## 0. 四个一等概念

| 概念 | 定义 | 性质 | 一句话 |
|---|---|---|---|
| **Strategy** | 一个交易算法 + 可搜索参数规格 + 已解析的 active params | 设计时,代码内置 | "怎么产生信号" |
| **Model** | 命名持久化的组合蓝图:成员(策略+权重+参数引用+启停) + 现金储备 + 组合级 risk | 设计时,可版本化、可复用、**drop-in** | "用哪些策略、各占多少、什么风控" |
| **Account** | 一个券商/sim 账户身份(venue+env+broker_acc_id) | 运行时身份 | "在哪个户头跑" |
| **Portfolio** | 某 Account 跑某 Model 产生的运行时账本:positions + cash + NAV + day PnL | 运行时状态,需对账 | "实际持仓与盈亏" |

关系链:

```
Strategy --(被引用)--> Model --(部署到)--> Account --(运行产生)--> Portfolio
                         ▲                                            │
                         └──────── 回测/调参/paper/live 都 drop-in 它 ─┘
```

---

## 1. 概念 → 现有数据结构映射(as-is)

### 1.1 Strategy ✅ 已对齐,无需改名

| 维度 | 结构 | 位置 |
|---|---|---|
| 逻辑 id | `sepa` / `sector_rotation` / `pairs` / `intraday_breakout` | `internal/params/loader.go:21-26` |
| 引擎 id | `SEPA-UNIVERSE-001` / `SectorRotation-001` / `Pairs-001` / `IntradayBreakoutRunner-000` | `internal/engine/strategyassembly/assembly.go:21-28` |
| 参数规格 | `hyperopt.StrategyParams{Strategy,SchemaVersion,Parameters[]ParamSpec,Constraints}` | `internal/hyperopt/loader.go:50-57` |
| 解析(DB→file→baseline) | `params.Resolver.Resolve()` → `params.Document` | `internal/params/resolve.go:58-113` |
| 参数持久化 | `tms.param_sets`(strategy,version,payload) + `tms.active_params`(strategy PK → param_set_id) | `migrations/000003_strategy.up.sql` |
| API | `GET /strategies`, `/strategies/{id}` → `StrategyMeta` | `internal/api/stores.go:117-139` |
| 注册表 | `strategyRegistry`(4 条 descriptor) | `internal/api/handlers_strategies.go:44-59` |

> 逻辑 id vs 引擎 id 的区分是有意为之(§7.7),**保留**。

### 1.2 Model ⚠️ 概念不存在,散落 + 写死

Model 现在没有任何持久化实体,它被拆成三处:

| Model 的哪部分 | 现状落点 | 位置 |
|---|---|---|
| 成员集合(哪些策略) | **写死**在 `assembleMulti`:SEPA+Sector+Pairs 固定 3 个 | `assembly.go:378-409` |
| 权重(capital_pct) | **写死常量** `allocSEPA 0.40 / allocSector 0.30 / allocPairs 0.20` | `assembly.go:30-42` |
| 单策略的 per-strategy 分配 | 散在每个策略 params 文档的 `Allocation{CapitalPct,Active}` 块 | `internal/params/document.go:28-31` |
| 组合级 risk | **写死常量** `riskMaxSingleName 0.50 / riskConcentration 0.40 / riskDailyLossHalt 0.10`(multi);`DefaultRiskConstraintsConfig` 0.20/0.30/0.05(单策略);sector 特例 50/40/10 | `assembly.go:30-61`, `risk_constraints.go:25-31` |
| "multi"/"joint" 选择 | 魔法字符串 dispatch | `assembly.go:161-183`, `objective.go:321-328` |

**问题:权重有两套来源**(assembly 常量 vs params 文档 Allocation),**risk 有三套写死分支**(multi gate / 单策略 default / sector 特例),并且单策略的 `MultiStrategyGate` 这种 parity 标志靠布尔分支硬接。Model 概念就是来收编这一切的。

### 1.3 Account ✅ 已一等,小幅增列

| 维度 | 结构 | 位置 |
|---|---|---|
| 身份 | `domain.Account{ID,Venue,Env,BrokerAccID,Label}` | `internal/domain/trading.go:77-90` |
| env 轴 | `BrokerEnv` = `sim`/`simulate`/`real` | `trading.go:43-53` |
| 执行轴 | `ExecutionPolicy` = `signal`/`auto` | `trading.go:17-26` |
| 持久化 | `tms.accounts`(id PK, venue, env, broker_acc_id, label) | `migrations/000014_accounts.up.sql:16-30` |
| 关联 | `account_id` FK 已加到 sessions/orders/positions/fills/reconciliation_reports | `000014:35-39` |
| API | `GET /trade/accounts` → 列出 env 分组 | `internal/api/trade_trading.go` |

> 仅需新增:`accounts.default_model_id`(UI 默认绑定的 Model)。

### 1.4 Portfolio ❌ 名字被占用 — 核心冲突

**finance 语义里 Portfolio = 运行时账本**。但代码里:

| 真正的"运行时账本"现在叫什么 | 结构 | 位置 |
|---|---|---|
| 账户快照(NAV/Cash/PnL/Positions) | `domain.AccountSnapshot` | `internal/domain/account.go:34-41` |
| 持仓键 | `StrategySymbol{StrategyID,Symbol}` → `Qty` | `account.go:16-19` |
| 可变持仓 | `accounting.Position`(signedQty/entryNotional/realized) | `internal/accounting/position.go:22-35` |
| 账本健康 | `portfolio.PortfolioHealthSnapshot`(DayPnL/Concentration/Halt) | `internal/portfolio/portfolio.go:43-49` |
| 持久化 | `tms.positions`,`tms.fills`,`GET /trade/account` 快照 | `migrations/000005_live.up.sql` |

**而 `portfolio.Portfolio` 这个类型其实是"下单前风控闸门"**(allocator + risk constraints 的组合管线),**不是账本**:

```go
// internal/portfolio/portfolio.go:9-18
type Portfolio struct {
    allocator       *Allocator
    riskConstraints *RiskConstraints
}
```

它在 engine 里以 `cfg.Portfolio` 字段流转(`live.go`、`eod.go`、`backtest.go`)。**这是必须在本次清理掉的命名冲突**:`Portfolio` 这个词要留给运行时账本,这个闸门应改名 `Gate`。

---

## 2. 命名冲突与概念错位汇总

| # | 冲突/错位 | 现状 | 对齐后 |
|---|---|---|---|
| C1 | `portfolio.Portfolio` 占用了"账本"的名字,实为风控闸门 | `portfolio.Portfolio{allocator,riskConstraints}` + `cfg.Portfolio` | 改名 `riskgate.Gate` + `cfg.Gate`;`Portfolio` 释放给账本 |
| C2 | Model 不存在,权重/成员/风控写死且多源 | assembly 常量 + params.Allocation + 魔法串 | 新增 `Model`/`model_members` 实体,assembly 改读 Model |
| C3 | per-strategy `Allocation` 块散在 params 文档 | `params.Document.Allocation` | 权威权重移到 `model_members`;params 文档的 Allocation 仅作 seed/弃用 |
| C4 | "multi"/"joint" 魔法字符串 | dispatch switch | `model_id`;legacy 串映射到 seed Model(default-multi / 单策略 Model) |
| C5 | risk 三套写死分支 + `MultiStrategyGate` 布尔 parity 标志 | assembly.go 多个 gate 函数 | risk 变成 Model 的 DATA;单策略=单成员 Model;parity 由"引用哪个 Model"表达 |
| C6 | `Mode`(signal/paper/live)与 2D 模型(ExecutionPolicy×Account.Env)并存冗余 | `sessions.mode` + Mode 桥接 | **删 `Mode`**(D2b),全栈用 (exec_policy, account.env);`sessions.mode` 迁到 `exec_policy`+`account_id` |
| C7 | UI 6 顶层 + "Cockpit/Trade" 术语与 4 概念不一致;backtest/hyperopt 混在一起 | sidebar 6 项,`components/trade/*` 混装 | **5 顶层**(流水线);Hyperopt→Strategies,Backtest→Models;`<TradeModule>` 复用;cockpit→portfolio |

---

## 3. 目标对齐:逐层改动

### 3.1 DB 层

**新增表:**
```sql
CREATE TABLE tms.models (
    id            TEXT PRIMARY KEY CHECK (id <> ''),      -- slug, e.g. 'default-multi','sepa-only','sepa-pairs-7030'
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    cash_pct      DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (cash_pct >= 0 AND cash_pct < 1),
    risk_single_name_pct    DOUBLE PRECISION NOT NULL CHECK (risk_single_name_pct > 0 AND risk_single_name_pct <= 1),
    risk_concentration_pct  DOUBLE PRECISION NOT NULL CHECK (risk_concentration_pct > 0 AND risk_concentration_pct <= 1),
    risk_daily_loss_halt_pct DOUBLE PRECISION NOT NULL CHECK (risk_daily_loss_halt_pct > 0 AND risk_daily_loss_halt_pct <= 1),
    risk_max_gross_pct      DOUBLE PRECISION CHECK (risk_max_gross_pct IS NULL OR risk_max_gross_pct > 0),
    risk_max_positions      INTEGER CHECK (risk_max_positions IS NULL OR risk_max_positions > 0),
    version       INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE tms.model_members (
    model_id      TEXT NOT NULL REFERENCES tms.models(id) ON DELETE CASCADE,
    strategy_id   TEXT NOT NULL CHECK (strategy_id IN ('sepa','sector_rotation','pairs','intraday_breakout')),
    weight        DOUBLE PRECISION NOT NULL CHECK (weight > 0 AND weight <= 1),  -- capital_pct
    active        BOOLEAN NOT NULL DEFAULT true,
    param_set_id  BIGINT REFERENCES tms.param_sets(id) ON DELETE RESTRICT,        -- NULL = 用该策略 active params
    PRIMARY KEY (model_id, strategy_id)
);
-- 可加约束:同 model 的 active 成员 Σweight + cash_pct <= 1
```

**改现有表:**
- `ALTER tms.sessions ADD model_id TEXT REFERENCES tms.models(id);` — 该 session 实际跑的 Model(权威历史)
- `ALTER tms.runs ADD model_id TEXT REFERENCES tms.models(id);` — 回测跑的 Model(现有 `strategies TEXT[]` 保留作展示)
- `ALTER tms.accounts ADD default_model_id TEXT REFERENCES tms.models(id);` — UI 默认
- `ALTER tms.hyperopt_studies ADD model_id TEXT REFERENCES tms.models(id);` — 调参针对的 Model(现有 `strategy` CHECK 保留兼容)

**Seed(向后兼容,migration 内插入):**
- `default-multi`:SEPA 0.40 / Sector 0.30 / Pairs 0.20,cash 0.10,risk 0.50/0.40/0.10 —— 对应旧 `strategy=multi`
- `sepa-only`/`sector-only`/`pairs-only`/`orb-only`:单成员 1.0,risk 用各自现状特例(sepa/pairs=default 0.20/0.30/0.05;sector=0.50/0.40/0.10) —— 对应旧 `strategy=sepa` 等

**弃用:** params 文档里的 `allocation.capital_pct/active` 不再作权威(C3),保留读取仅用于 seed 兼容。

### 3.2 domain / engine 层

**改名(C1):**
- `internal/portfolio` 包内:`Portfolio` 类型 → `Gate`(建议把 gate 相关迁到 `internal/riskgate`,或包内保留但类型改名)。涉及 `NewPortfolio`→`NewGate`、`cfg.Portfolio`→`cfg.Gate`(`livengine.Config`、`livetrade.TradeSessionConfig`、`engine.Config`、`strategyassembly.Assembly.Portfolio`)。
- `PortfolioHealthSnapshot` 语义正确(账本健康),保留名字,可随包归位。

**新增(C2):**
- `internal/model` 包:`Model{ID,Name,Members[]Member,CashPct,Risk}`、`Member{StrategyID,Weight,Active,ParamSetID}`、`Risk{SingleNamePct,ConcentrationPct,DailyLossHaltPct,MaxGrossPct?,MaxPositions?}` + `Validate()`。
- `model.Store`(CRUD,读/写 `tms.models`+`model_members`)。

**重构(C4/C5):**
- `strategyassembly`:`Assemble(Input{Strategy string})` → `Assemble(Input{Model model.Model, ...instruments})`。`assembleMulti`/`assembleSEPA`/`assembleSector`/`assemblePairs`/`assembleORB`/`strategyGate`/`loneSectorPortfolio`/`singleStrategyPortfolio`/`multiStrategyPortfolio` 合并为单一 `assembleFromModel`:从 `model.Members` 建 `Allocator`,从 `model.Risk` 建 `RiskConstraints`。常量 `allocSEPA…`、`riskMaxSingleName…` 删除(成为 seed 数据)。
- **硬切(D4b)**:不留 legacy 字符串垫片;`Assemble` 只认 `Model`;旧 `strategy=` 入参在 API 层删除。
- 删除 `MultiStrategyGate` parity 标志(D1):单策略 hyperopt 直接用其单成员 Model 的 gate,不再强制套 multi。
- **改名(D5b)**:`AccountSnapshot`→`Portfolio`(或 `PortfolioSnapshot`),其上的 `StrategyPosition`/`NetPositionAcrossStrategies` 等方法随迁;`portfolio.PortfolioHealthSnapshot` 留在账本语义侧。**保留**:`Allocator`/`RiskConstraints`(进 `riskgate` 包)、`StrategyAllocation`、Money/Price/Qty(纯数值)。

### 3.3 API 层

- **新增** `/models` CRUD:`GET /models`、`GET /models/{id}`、`POST /models`、`PUT /models/{id}`、`DELETE /models/{id}`。
- `POST /backtests`:**只认 `model_id`**(硬切 D4b);删 `strategy=` 入参。单策略回测 = 传单成员 Model 的 id。
- `POST /hyperopt`:**单策略** tune(`strategy=sepa|sector_rotation|pairs`,Strategies 模块用);**joint** 改为 `POST /models/{id}/optimize`(Models 模块"Optimize",优化该 Model 成员参数)。`promote` 写回到 Model 成员的 `param_set_id`(或单策略 active_params)。
- `GET /strategies`:删 `capital_pct`/`active` 字段(那是 Model 成员属性,由 `/models` 提供)(C3)。
- **账本视角(C1/Portfolio)**:`GET /trade/account` + `/trade/positions` + `/trade/health` 构成一个 Account 的 **Portfolio**;新增聚合 `GET /trade/portfolio?account_id=` 一次返回 {snapshot, positions, health}。术语 "cockpit"→"portfolio"。

### 3.4 UI 层(C7)— 5 顶层(流水线)

- sidebar 5 项:`Systems & Data` / `Strategies` / `Models` / `Paper Trade` / `Live Trade`。
- **① Systems & Data**:Health(合并 `ops/system-health`+`trade/system-panel`)· Data(`/data/*`)· Jobs(`ops/job-queue`)· Audit(`ops/audit-log`)。
- **② Strategies**(调参):每策略 tab = `/strategies/{id}` 详情 + 该策略 watchlist + live 状态 + **Hyperopt(tune)**。ORB tab 仅详情(无 hyperopt、无 EOD watchlist)(A4)。
- **③ Models**(组合+验证):Model 编辑器/Composer(选策略·配权重·绑 param_set·设 risk)+ **Backtest** + **Optimize(joint)** + 结果内嵌抽屉。
- **④ Paper Trade / ⑤ Live Trade**:同一 `<TradeModule account={…}/>`,展示该 Account 的 **Portfolio**(positions/account/blotter/fills/recon/health)+ Desk(下单/平仓/sync)。Paper 绑 sim/simulate、Live 绑 real,实盘激活门禁不变。
- 组件重组:`components/trade/*` → `components/portfolio/*`(账本视图,Paper/Live 共用);watchlist+hyperopt → `components/strategies/*`;composer+backtest+optimize → `components/models/*`;`components/data|ops` → `components/systems/*`。

---

## 4. 决策点(✅ 全部已定)

> 用户确认:**A 组全取建议、B 组全取激进选项**。parity 弃用,原 D1 消解。

**A 组(IA):**
- **A1 ✅** 顶层 5 个平铺(Systems & Data / Strategies / Models / Paper Trade / Live Trade)。
- **A2 ✅** joint hyperopt 进 Models 作 "Optimize this model";Strategies 只做单策略 tune。
- **A3 ✅** 回测只经 Model(单策略回测 = 单成员 Model),无独立 strategy 回测入口。
- **A4 ✅** ORB:Strategies 里仅详情 tab(无 hyperopt、无 EOD watchlist);可作单成员 Model 回测。

**B 组(清理力度):**
- **D2 ✅(b)** 删 `Mode`,全栈用 (exec_policy, account.env);`sessions.mode` 迁到 `exec_policy`+`account_id`。
- **D3 ✅(b)** 删 params 文档 `Allocation` 块,Model 唯一权威。
- **D4 ✅(b)** 硬切 `model_id`,删 legacy `strategy=` 入参(UI 同步切)。
- **D5 ✅(b)** 整包 `portfolio`→`riskgate`(`Portfolio`→`Gate`)+ `AccountSnapshot`→`Portfolio` 语义全面改名。

---

## 5. 改动清单与影响面(rename map)

| 改动 | 类型 | 影响文件(估) | 风险 |
|---|---|---|---|
| 新增 `tms.models`/`model_members` + seed | migration | 1 migration | 低(纯加) |
| `accounts.default_model_id`,`sessions/runs/hyperopt_studies.model_id` | migration ALTER | 1 migration | 低 |
| `internal/model` 包 + Store | 新增 | ~3 文件 | 低 |
| `portfolio.Portfolio`→`Gate`(+可能整包→`riskgate`) | 改名 | `internal/portfolio/*`、`engine.Config`、`livengine`、`livetrade`、`strategyassembly`、`runner/{live,eod,assembly}`、`backtest` handler | **中**(机械改名,编译器兜底) |
| `assembleFromModel` 重构 | 重构 | `strategyassembly/assembly.go`、`objective.go`、`runner/assembly.go`、`backtest.go`、`hyperopt.go` | **中**(单策略 gate 口径有意改变,见 D1 注) |
| `/models` CRUD + backtest/hyperopt 接 `model_id` | API | `internal/api/*`、handlers | 中 |
| UI 4 顶层重组 + `<TradeModule>` + Model 编辑器 | 前端 | `ui/src/app/*`、`ui/src/components/*`、`sidebar`、hooks | **高**(体量大,但多为搬迁) |
| `cockpit`→`portfolio` 术语 | 前端改名 | `ui/src/components/trade/*` | 低 |

**回归护栏**:assembly 重构后跑 backtest/hyperopt 行为回归,确认 `default-multi`(=旧 multi)结果与改前一致。单策略 gate 口径有意改变(不再强套 multi gate),属预期,不视为回归。

---

## 6. 相位(实施顺序)

0. **DB + domain Model**:migration(`models`/`model_members` + seed + `model_id` 列 + 删 `Mode`/Allocation)+ `internal/model` + Store
1. **engine 对齐**:`portfolio`→`riskgate`(`Portfolio`→`Gate`)+ `AccountSnapshot`→`Portfolio`(D5b)+ `assembleFromModel` 重构(删魔法串/MultiStrategyGate)+ 行为回归(最高风险,先做并验证)
2. **API**:`/models` CRUD + `POST /backtests` 只认 model_id + 单策略 hyperopt + `POST /models/{id}/optimize` + 删 legacy
3. **① Systems & Data**(前端搬迁,低风险)
4. **② Strategies**(每策略 tab + watchlist + Hyperopt)
5. **③ Models**(Composer + Backtest + Optimize + 内嵌结果)
6. **④⑤ Paper/Live Trade**(`<TradeModule>` 双入口 + cockpit→portfolio + 路由 301)

> A4:ORB 在 Strategies 仅详情 tab。回归护栏见 §5。
