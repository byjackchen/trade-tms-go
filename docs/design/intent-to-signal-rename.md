# Design: `signal` vs `intent` 术语归位

状态: **已定方向 / 待实施**（本文档仅设计，尚未改代码） · 作者: byjackchen · 日期: 2026-06-17
范围: **不考虑与旧版 Python 的 parity**（[MUST-MATCH] 约束作废，可自由改名）。

### 决策记录 (2026-06-17)
1. ✅ 认可 §2 归类：A = **signal**，B/C = **intent**，及 §1 标尺。
2. ✅ **两步走，但目标是彻底对齐**：Go 内部 → 外部契约(DB/wire/UI) 全部做完。
   语义只对齐一半后患无穷，故 §3 外部契约改名是**承诺项，非可选后续**。
3. ✅ 概念 B（would-be orders）**结构化**为 `IntentRecord(symbol+side+qty)` 并暴露，
   让 `intent` 一词真正落到它该在的地方。

## 1. 问题不是"把 intent 换成 signal"，而是"两个概念各自归位"

算法交易流水线：

```
Signal(判断) → Intent(意图: 带 side+qty 的待下单) → Order(已提交) → Fill → Position
```

判别规则（本设计的核心标尺）：

> **带可执行的 side+qty、是"将要下的一张单"→ intent。
> 是判断/诊断快照（state / strength / grade / z-score，"策略在想什么"）→ signal。**

当前代码把这两个概念**名字搞反了**：判断快照叫了 `*Intent`，而真正 intent 形状的
东西（would-be order / 脚本指令）却没占用 intent 这个词。去掉 Python parity 后，
应当让两个词各自归位。

## 2. 代码里实际存在三个东西（按标尺归类）

| # | 现状符号 | 实际内容 | 正确术语 | 现名对错 |
|---|---|---|---|---|
| A | `domain.SignalIntent` 家族<br/>(SEPA/Pairs/Sector/ORB) | state, strength(0..100), proximity, grade, z-score, rank…（判断/诊断快照，"what is the strategy thinking"，UI 按 (symbol,strategy_id) newest-wins 展示） | **signal** | ❌ 错（无 side/qty 却叫 Intent） |
| B | `NoopExecutor` 的 would-be orders<br/>(`SubmitMarket*` / `WouldSubmitCount`) | 策略本该下的市价单（**side+qty**，signal 模式只计数不下单） | **intent** | ⚠️ 名字缺失（doc 已称其 "intents vs would-be-orders"） |
| C | `engine.Intent` (`strategy.go:185`)<br/>UI `BacktestIntent` | `Date,Ticker,Side,Qty` 一条脚本市价单指令 | **intent** | ✅ 对 |

**关键洞察**：A 被错叫成 intent；B 才是真正的 intent，却没用这个词；C 用对了。
所以正确的动作是 **把 A 从 `*Intent` 改成 `*Signal`**，并把 **B 扶正为真正的
`IntentRecord`**。一旦 A 改名，C(`engine.Intent`) 与 A 的**同名异义碰撞自动消失**——
代码里 `Intent` 将**只剩"带 side+qty 的待下单"一种含义**（B/C）。

> A 里少数字段偏"计划"味（Sector `TargetWeight`、SEPA `PivotPrice/StopPrice/
> RiskPct`），但整条记录是 per-symbol 的**判断+计划注解快照**，供 UI 展示
> "策略在想什么"，不是一张待提交的单。按标尺仍归 **signal**。

## 3. 归位后的目标命名

### 3.1 概念 A：signal（当前错叫 intent → 全部改，含外部契约）

**Go 符号**
- `domain`: `SignalIntent → Signal`，`SEPASignalIntent → SEPASignal`，
  `PairsSignalIntent → PairsSignal`，`SectorRotationIntent → SectorRotationSignal`,
  `IntradayBreakoutIntent → IntradayBreakoutSignal`，`New*SignalIntent() → New*Signal()`
- `engine`: `IntentEvaluator → SignalEvaluator`，`EvaluateIntentJSON → EvaluateSignalJSON`
- `livengine`: `intent.go → signal.go`，`IntentRecord → SignalRecord`，
  `IntentSink → SignalSink`，`EmitIntent → EmitSignal`，`MemSink.Intents → Signals`，
  telemetry `EmittedIntents → EmittedSignals`
- `publish`: `IntentJSON → SignalJSON`，`SignalIntentEnvelope → SignalEnvelope`，
  `TopicSignalIntent`、`PublishSignalIntent → PublishSignal`
- `runner`: `eod.go` `IntentRows/IntentsEmitted → SignalRows/SignalsEmitted`

**外部契约（承诺改，破坏性，带兼容窗口 → 见 §6 Phase 2）**
- DB 表 `tms.signal_intents → tms.signals`；列 `intent → signal`；
  索引 `signal_intents_*_idx → signals_*_idx`（含幂等唯一索引 mig 000010）
- Redis topic `data.SignalIntentUpdate → data.SignalUpdate`；信封 `intent_json → signal_json`
- HTTP JSON 字段 `json:"intent" → json:"signal"`（api/trade.go）

### 3.2 概念 B：intent 的真正归宿（新增结构 → 见 §6 Phase 3）

把 `NoopExecutor` 当前丢弃/仅计数的 would-be order **结构化为一等记录**：

```go
// IntentRecord 是一张"本该下、未提交"的订单意图（signal 模式产物）。
// auto 模式下同一条意图会变成真实 domain.Order；signal 模式下它只被记录。
type IntentRecord struct {
    StrategyID string            // engine 策略 id（allocator key）
    AsOf       time.Time         // 评估该意图的 bar 时间戳 (UTC)
    Symbol     string
    Side       domain.SignalSide // LONG / SHORT / FLAT
    Qty        domain.Qty
}

// IntentSink 接收 would-be order 意图，与 SignalSink 平行。
type IntentSink interface {
    EmitIntent(ctx context.Context, rec IntentRecord) error
}
```

- 采集点：`NoopExecutor.SubmitMarket/SubmitMarketSignal` 已经拿到
  (symbol, side, qty, strategyID, ts)，现状丢弃；改为记录成 `IntentRecord`，
  由 session 在每个时间戳 flush 时 `EmitIntent`（与 signal 发射并列）。
- `WouldSubmitCount` 保留为快捷计数；语义由"intents vs would-be-orders"正名为
  "intent count"。
- 持久化/wire（可选，Phase 3 内评估）：DB 表 `tms.intents` + Redis topic
  `data.IntentUpdate`，与 signals 表对称。**默认先做内存/telemetry 层，
  DB/wire 暴露按 cockpit 需求决定。**

### 3.3 概念 C：保留 `Intent`

`engine.Intent`（脚本指令）+ UI `BacktestIntent` **名实相符，一处不动**。
A 改名后不再与之碰撞。自动文本替换会污染 C —— 必须逐符号语义改。

## 4. 爆炸半径（量化，已排除 `.claude/worktrees/`）

| 层 | 规模 | 归属概念 |
|---|---|---|
| Go `intent` 总出现 | ~2180 处 / 270 文件（含测试） | 多数是 A(→signal)，少量 C(intent 保留) |
| 非测试 Go 文件 | 188 | — |
| 热点目录 | runner(12) engine(9) livengine(8) api(8) domain(7) strategy/*(各5-7) publish(5) | A 为主 |
| DB schema | 表 `signal_intents`+列 `intent`+4 索引 (mig 000005/000010/000014) | A |
| Wire/Redis | `data.SignalIntentUpdate`/`SignalIntentEnvelope`/`intent_json`/`json:"intent"` | A |
| UI/TS | 14 处 —— **全是 `BacktestIntent`(概念 C)，不改** | C |
| docs | 16 个 md（api-ws-redis §2.4/2.6/5.8/5.9 等） | A 为主 |

> 自动文本替换会污染 C。必须**逐符号语义重命名**，靠编译器保证完整。

## 5. 最终目标态（一句话）

- `Signal*`（domain/livengine/engine/publish/DB `signals`/wire `SignalUpdate`）
  = 判断/诊断快照（概念 A）。
- `Intent*`（livengine `IntentRecord/IntentSink`（B）+ `engine.Intent`/UI
  `BacktestIntent`（C））= 带 side+qty 的待下单。
- 两词在**全栈**（Go + DB + wire + UI + docs）一致，无同名异义。

## 6. 实施阶段（承诺全做，分 3 个 PR 顺序推进）

### Phase 1 — Go 内部归位（A → Signal），边界冻结
- 改 §3.1 全部 **Go 符号**（A → Signal）。
- DB 列、Redis topic、HTTP 字段、`json:` tag **暂保持 `intent`**（本阶段冻结，
  避免代码 rename 与协议迁移混在一个 PR）。
- 验收：`go build ./... && go test ./...` 全绿；grep 确认 `signal_intents`/
  `data.SignalIntentUpdate`/`intent_json`/`json:"intent"` **零改动**。

### Phase 2 — 外部契约对齐（A），一次性迁移（无兼容窗口）
> 原则（2026-06-17 锁定）：**数据一次定迁移，不考虑向后兼容**。无双写、无别名、
> 无并行窗口 —— 一个迁移直接 rename 到位。
- DB：单个迁移直接 `ALTER TABLE tms.signal_intents RENAME TO tms.signals` +
  `RENAME COLUMN intent TO signal` + 索引重命名（幂等唯一索引/历史数据随表保留）。
- Wire：Redis topic 直接改 `data.SignalUpdate`；信封字段 `signal_json`；
  HTTP `json:"signal"`。旧 topic/字段**不保留**。
- UI：同一批次切到新字段/topic（无兼容期）；docs api-ws-redis §5.9 等同步。
- 验收：UI 端到端走新契约；旧名全仓 grep 归零。

### Phase 3 — 概念 B 结构化（intent 归位）
- 新增 `livengine.IntentRecord` + `IntentSink`（§3.2）；`NoopExecutor` 记录
  would-be order 并经 session flush `EmitIntent`。
- `WouldSubmitCount` 文案正名。
- 可选：`tms.intents` 表 + `data.IntentUpdate` topic（按 cockpit 需求，本阶段内评估）。
- 验收：signal 模式同时产出 signals(A) 与 intents(B)，二者语义清晰分离；
  consistency_test 覆盖 intent 发射的 stream==replay 一致性。

## 7. 风险

- **误伤概念 C**：`engine.Intent`/`BacktestIntent` 必须保留。→ 逐符号 rename，
  禁用全局 sed，Phase 1 边界校验。
- **Phase 1→2 过渡期内外不一致**（Go=signal / wire=intent）：可接受过渡态，
  Phase 2 收尾消除；不长期停留。
- **Phase 2 DB 迁移**：历史数据 + 幂等唯一索引重建 + 兼容窗口（建表→双写→切读→
  下线），单独评审，不可省兼容窗口。
- **测试体量**（270 文件）：每个 PR 纯 rename/纯新增，不混逻辑改动，编译期兜底。

## 8. 待办（实施前确认）

1. Phase 3 的 `tms.intents` 表 + `data.IntentUpdate` topic：本轮就建，还是先只做
   内存/telemetry 层、wire/DB 暴露延后？（§3.2 默认后者）
2. ~~Phase 2 兼容窗口~~ —— **已定**：一次性迁移，无兼容窗口、无别名（见 Phase 2）。
3. ✅ **已定**：`IntentRecord.Side` 用 `domain.SignalSide`(LONG/SHORT/FLAT)。
   理由：FLAT 是 intent/signal 层的目标状态（"平仓"），order 层无此概念——
   平仓的 BUY/SELL 取决于当前净持仓，由 `OrderSideFor` 在执行时解析
   (`enums.go:101`)。OrderSide(BUY/SELL) 表达不了"平仓意图"，故用 SignalSide。
