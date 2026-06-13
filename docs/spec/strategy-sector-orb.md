# 策略规格:sector_rotation(行业轮动)与 intraday_breakout(ORB 开盘区间突破)

> 提取自 Python 参考仓库 `trade-multi-strategies`(只读)。本文档面向 Go 实现者:
> 不接触 Python 源码也必须能实现字节级等价(byte-equivalent)的系统。
>
> 标签约定:
> - **[MUST-MATCH]** — Go 必须逐字复刻该行为,含所有边界情况、舍入、排序、字符串格式。
> - **[IMPROVE]** — 原实现的已知弱点;Go 可以改进,但必须同时记录"原行为"与"改进方案",
>   且改进必须可通过配置/构建开关回退到原行为以便对照验证。
>
> 所有引用格式为 `相对路径:行号`(相对 Python 参考仓库根目录)。

---

## 0. 两层架构契约(Eng-D2)

两套策略都遵循同一契约:**纯逻辑层 SignalGenerator(SG)** + **薄封装 Runner 层**。

- SG 层(`signal.py`)零交易框架依赖,接口为 `on_bar(Bar) -> list[Signal]`,
  外加 `evaluate_intent(as_of)`、`state_summary()`、`state_dict()`、`load_state(d)`。
  测试用 AST 强制零 nautilus 依赖(tests/strategies/intraday_breakout/test_signal.py:616-637)。**[MUST-MATCH]**(Go 中对应:策略包不得依赖执行引擎包)
- Runner 层把引擎 Bar 翻译为纯 Bar、把 Signal 翻译为市价单,并发布状态/意图快照。

### 0.1 共享线格式类型(来自 `src/strategies/sepa/signal.py`)

`SignalSide`(src/strategies/sepa/signal.py:62-67)**[MUST-MATCH]**:

```
LONG = "LONG"; FLAT = "FLAT"; SHORT = "SHORT"   # 字符串枚举;两策略都不使用 SHORT
```

`Bar`(src/strategies/sepa/signal.py:70-83)**[MUST-MATCH]**:

| 字段 | 类型 | 语义 |
|---|---|---|
| symbol | string | ticker,如 "XLK" |
| ts | datetime,**tz-aware UTC** | 见各策略对 ts 的解释(ORB 把它当 bar 开始时间) |
| open/high/low/close | Decimal(十进制定点数,非 float) | 价格 |
| volume | int | 成交量 |

`Signal`(src/strategies/sepa/signal.py:86-101)**[MUST-MATCH]**:

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| symbol | string | — | |
| ts | datetime UTC | — | 触发 bar 的 ts(原样透传,不改写) |
| side | SignalSide | — | |
| target_qty | int | — | 有符号;两策略只产生 ≥0 |
| reason | string | — | 人类可读;**格式精确规定见各策略节** |
| confidence | float | 1.0 | 两策略不设置(恒为默认 1.0) |
| grade | nullable | None | 两策略不设置 |
| stop_price | Decimal 或 null | None | 仅 ORB 入场 LONG 信号携带 |

### 0.2 Runner 共通行为(`src/strategies/_base/runner.py`)

- 引擎 Bar → 纯 Bar 翻译(runner.py:63-79)**[MUST-MATCH]**:
  `symbol = str(bar_type.instrument_id.symbol)`;`ts = ts_event 纳秒 → UTC datetime`;
  OHLC 经 `Decimal(str(x))`(即十进制字符串再解析,保留引擎渲染的小数位);`volume = int(...)`。
- `on_bar` 模板(runner.py:120-144)**[MUST-MATCH]** 顺序:
  1. 记录 `_last_close[symbol] = close`;
  2. `for signal in sg.on_bar(bar): _submit_for_signal(signal)`(逐个、按 SG 返回顺序提交);
  3. 发布 `StrategyStateUpdate`(`json.dumps(state_summary(), default=str)`);
  4. 发布 `SignalIntentUpdate`(对 `evaluate_intent(ts)` 的每个意图 `asdict`+`json.dumps(default=str)`)。
  发布步骤的任何异常只记日志、不得影响交易路径(runner.py:146-244)。**[MUST-MATCH]**
- 组合闸门 `_gate`(runner.py:85-114):若注入了 Portfolio 服务,先以
  `ProposedOrder(strategy_id=str(self.id), symbol, side, qty=target_qty, price=_last_close.get(symbol, 0), ts)`
  询问风控;拒绝则不下单。**[MUST-MATCH]**(语义:闸门在 Signal→Order 翻译之前)

---

## 1. 参数文件与超参搜索(两策略共用机制)

参数基线为 JSON 文件 `src/strategies/params/baseline/<strategy>.json`,由
`src/strategies/params/loader.py` 加载。

- 解析规则(loader.py:22-25, 157-184)**[MUST-MATCH]**:
  `schema_version` 只允许 `1`;`type ∈ {float,int,str,list}`;`search` 仅允许出现在
  numeric 类型(float/int)上;`constraints` 只允许 `clamp_high`/`clamp_low`
  (两策略的 `constraints` 均为空数组)。
- 目录解析顺序(loader.py:64-96)**[MUST-MATCH]**:显式 `params_dir` >
  环境目录 `TMS_STRATEGY_PARAMS_DIR`(仅当该目录下存在对应 json)> 包内 baseline。
- `defaults_dict` 返回 `{name: default}`(loader.py:187-189)。
- `suggest_with`(loader.py:192-230)**[MUST-MATCH]**:
  仅对 `search != null` 的参数调用 `trial.suggest_float/suggest_int`,
  **trial 参数名带策略前缀** `f"{strategy}.{name}"`(如 `sector_rotation.top_k`);
  返回值只含被搜索的键,调用方需 `{**defaults_dict, **suggest_with}` 合并。
- 调参产物 `write_tuned_params`(loader.py:233-272):只覆写 `default` 与 `metadata`
  (`source="tuned"`、`tuned_from_study/trial`),search 范围与 constraints 原样保留,
  输出 `json.dumps(indent=2, sort_keys=False)`。**[MUST-MATCH]**
- **注意**:`src/research/search_spaces.py:16` 的 `StrategyName = Literal["sepa","sector_rotation","pairs"]`
  —— `intraday_breakout` **未注册**进 Optuna 搜索空间(其 JSON 里写了 search 范围但当前优化器不会用)。
  **[MUST-MATCH]**(完整复刻:保留 ORB 的 search 元数据,但默认优化器不遍历 ORB)

---

## 2. 策略 A:sector_rotation(行业轮动)

来源:`src/strategies/sector_rotation/{signal.py,intent.py,nautilus_runner.py,__init__.py}`,
基线参数 `src/strategies/params/baseline/sector_rotation.json`,
测试 `tests/strategies/sector_rotation/{test_signal.py,test_intent.py,test_runner_config.py}`。

### 2.1 概要

11 只 Select Sector SPDR ETF 的宇宙;每个自然月第一根 bar 触发再平衡;按 N 根 bar
回报排名,等权持有 top-K;只对"变更项"发信号(退出发 FLAT、新进发 LONG、留任不动)。
日线级别(生产环境 bar 类型为 `*-1-DAY-LAST-EXTERNAL`,src/runner/backtest_runner.py:79、
src/runner/live_runner.py:322-323)。**[MUST-MATCH]**

### 2.2 默认宇宙 **[MUST-MATCH]**(signal.py:50-62;声明顺序即 tie-break 顺序,见 2.6)

```
("XLK","XLF","XLE","XLV","XLY","XLP","XLU","XLB","XLI","XLRE","XLC")
 科技  金融  能源  医疗  可选  必选  公用  材料  工业  地产   通信
```

恰好 11 个(test_signal.py:80-83)。

### 2.3 配置参数表

SG 配置 `SectorRotationSignalGeneratorConfig`(signal.py:65-91,frozen):

| 参数 | 类型 | 默认(baseline JSON) | hyperopt 搜索范围 | 校验 | 来源 |
|---|---|---|---|---|---|
| equity_provider | 无参回调 → Decimal | **必填,无默认** | — | 必须 callable,否则 `TypeError`("equity_provider")。构造时**不得调用**它(signal.py:79-83) | signal.py:73 |
| universe | tuple[str] | 11 只 ETF(见 2.2) | 不可搜索(type=list) | 非空,否则 `ValueError`("universe") | sector_rotation.json |
| momentum_lookback | int | **63**(≈3 个月交易日) | int **[42, 126]** | ≥2,否则 `ValueError`("momentum_lookback") | sector_rotation.json |
| top_k | int | **3** | int **[2, 5]** | `1 ≤ top_k ≤ len(universe)`,否则 `ValueError`("top_k") | sector_rotation.json |
| timezone | str | **"America/New_York"** | 不可搜索 | 无校验 | sector_rotation.json |

**[MUST-MATCH]** 上述默认、范围、校验消息关键字(测试用 `pytest.raises(match=...)` 锚定,
test_signal.py:86-122)。

**[MUST-MATCH][IMPROVE]** `timezone` 字段在 sector_rotation 的**任何逻辑里都未被使用**
——月份翻转用的是 `bar.ts.date()` 即 **UTC 日期**(signal.py:132),不做时区换算。
原行为:UTC 日界定月份。改进方案:按配置时区换算后取日期(对美股日线,UTC 收盘日期与
交易日一致性通常无碍,但跨午夜 UTC 的 bar 时间戳会错月)。Go 默认必须用 UTC 日期。

JSON 还携带 `allocation: {capital_pct: 0.30, active: true}`(sector_rotation.json),
与装配层一致:SEPA 0.40 / SectorRotation 0.30 / Pairs 0.20、现金 0.10;风控
`max_single_name_pct=0.50, concentration_pct=0.40, daily_loss_halt_pct=0.10`
(src/runner/strategy_assembly.py:230-258)。**[MUST-MATCH]**

### 2.4 内部状态 **[MUST-MATCH]**(signal.py:107-122)

| 字段 | 类型 | 初始化 |
|---|---|---|
| _history | map[symbol] → 定长队列 Decimal,**maxlen = momentum_lookback + 1** | 构造时为每个宇宙符号建空队列 |
| _last_close | map[symbol] → Decimal | 空 |
| _last_universe_date | date 或 null | null。**全宇宙共享**的最近 bar 日期(月翻转锚点) |
| _current_positions | map[symbol] → int 股数 | 构造时每个宇宙符号置 0 |
| _intent_generation | int | 0 |

### 2.5 on_bar 算法 **[MUST-MATCH]**(signal.py:128-158)

```
on_bar(bar):
  1. bar.symbol 不在 _history(即不在宇宙)→ 返回 [],状态完全不变(含 _last_universe_date)。
  2. bar_date = bar.ts.date()            # UTC 日期,见 2.3 的 IMPROVE 注记
  3. is_first_bar_of_new_month =
        _last_universe_date != null AND bar_date.month != _last_universe_date.month
     # 只比较"月份数字"!同月不同年(隔整 12 个月的数据缺口)不会触发。见 [IMPROVE] 注记。
  4. signals = []
     if is_first_bar_of_new_month AND _has_full_warmup():
        signals = _compute_rebalance_signals(bar.ts)     # 先再平衡,**后**摄入本根 bar
  5. _history[bar.symbol].append(bar.close)   # 队列满时左端弹出
     _last_close[bar.symbol] = bar.close
     _last_universe_date = bar_date
  6. return signals
```

- **再平衡先于摄入新月 bar [MUST-MATCH]**:再平衡用的是"上月末的快照"——所有符号
  (包括触发符号自身)的历史与 `_last_close` 都还停留在上一交易日。测试锚点:
  equity=100000、top_k=2,日序列 `AAA: 100+i`、`BBB: 50+0.5i`,起始 1/4 共 30 天,
  2/1(day idx 28)触发,定价用 day idx 27 的收盘(AAA=127.0、BBB=63.5),
  `target_qty == int(50000 // 127.0) == 393`、`int(50000 // 63.5) == 787`
  (test_signal.py:331-356)。
- 暖机闸门 `_has_full_warmup()`(signal.py:153-158)**[MUST-MATCH]**:
  **每个**宇宙符号的历史长度 ≥ `momentum_lookback + 1` 才允许再平衡。
  若月翻转时暖机未完成,**本月不再补触发**(下一次机会是下个月翻转;
  test_signal.py:156-166)。
- 同月内只触发一次:第一根新月 bar 处理后 `_last_universe_date` 已更新为新月日期,
  当月后续 bar 的 `month != month` 为假(test_signal.py:204-224)。**[MUST-MATCH]**
- **[IMPROVE]** 月翻转只比较 month 字段:若数据缺口恰好跨 12 个月(2024-03 → 2025-03)
  不会再平衡。原行为如上;改进方案:比较 `(year, month)` 元组。Go 默认按原行为。
- **[IMPROVE]** `_current_positions` 是 SG 内部记账,从不与真实成交回报对账
  (部分成交/拒单会漂移)。原行为:纯内部计数;改进方案:由 Runner 回写成交。
  (Runner 侧的 FLAT 翻译用真实净头寸,见 2.10,已部分缓解。)

### 2.6 再平衡算法 `_compute_rebalance_signals(ts)` **[MUST-MATCH]**(signal.py:164-233)

```
1. 计算回报:对每个宇宙符号 sym(按宇宙声明顺序遍历):
     old = _history[sym][0]; new = _history[sym][-1]
     if old <= 0: 跳过该符号(不参与排名,可能导致它缺席 top-K)
     returns[sym] = float((new - old) / old)        # Decimal 除法后转 float64
   # 回报窗口 = 恰好 momentum_lookback 个 bar 间隔(队列首尾)
2. returns 为空 → 返回 []。
3. ranked = 按 returns 值降序排序;排序必须**稳定**,并列时保持插入顺序
   = 宇宙声明顺序(Python sorted 稳定性)。new_topk = ranked 前 top_k 的符号集合。
4. currently_held = { sym : _current_positions[sym] > 0 }
5. 先发 FLAT,后发 LONG;两组内部均按 symbol 字母升序(sorted)。
   FLAT(currently_held - new_topk,按字母序):
     held_qty = _current_positions[sym]; _current_positions[sym] = 0
     Signal(symbol=sym, ts=ts, side=FLAT, target_qty=0,
            reason=f"Sector Rotation rebalance :: closing {sym} (was {held_qty} sh, no longer in top-{top_k})")
6. LONG(new_topk - currently_held,按字母序):
     equity = float(equity_provider())              # 每次再平衡现取,严禁缓存
     target_value = equity / top_k                  # float 除法
     price = float(_last_close[sym]); price <= 0 → 跳过
     target_shares = int(target_value // price)     # float 地板除后截断
     target_shares <= 0 → 跳过(不发信号、不记仓位)
     _current_positions[sym] = target_shares
     mom_pct = returns[sym] * 100.0
     Signal(symbol=sym, ts=ts, side=LONG, target_qty=target_shares,
            reason=f"Sector Rotation rebalance :: top-{top_k} entry, {momentum_lookback}-bar return {mom_pct:+.2f}%")
            # {mom_pct:+.2f} = 带符号、两位小数,如 "+30.00%"、"-15.00%"
7. 留任符号(同时在 currently_held 与 new_topk)**不发任何信号、股数不变**
   (test_signal.py:280-323)。
```

要点:
- **信号顺序 [MUST-MATCH]**:所有 FLAT 在所有 LONG 之前;组内按 symbol 字典序。
- **每次再平衡都调用 equity_provider [MUST-MATCH]**(PA-D1 不变量,test_signal.py:488-539):
  4 倍权益 → 股数约 4 倍(`//` 截断容差内)。
- **[IMPROVE]** 留任持仓不按新权益重新定尺寸 → 权重随时间漂移,"等权"只对新进腿成立。
  原行为:留任完全不动;改进方案:可选的全量目标重定(emit 调整单)。
- **[IMPROVE]** `old <= 0` 时该符号被静默剔除出排名(与 evaluate_intent 中
  `old <= 0 → return 0.0` 的处理**不一致**,signal.py:176-177 vs 276-279)。
  Go 默认各自照抄两种行为。

### 2.7 evaluate_intent(as_of) **[MUST-MATCH]**(signal.py:239-327, intent.py)

返回 `list[SectorRotationIntent]`,**每个宇宙符号恰一条,顺序 = 宇宙声明顺序**。
每次调用先 `_intent_generation += 1`(所有条目共享同一 generation,严格单调,
test_intent.py:140-144)。

意图字段(intent.py:27-41):`symbol, state, strength(0..100), proximity_to_trigger_pct
(可空), updated_at(=as_of), generation, strategy_id="sector_rotation", momentum_score=0.0,
rank=0(1=最佳;0=未排名/暖机), target_weight=0.0, current_weight=0.0`。

状态枚举(intent.py:15-21):`no_setup / forming / buy / hold / exit / stop_hit`
(stop_hit 本策略不用)。

逻辑:
1. 暖机未完成(任一符号短缺)→ **全部** NO_SETUP、strength=0、proximity=null、
   rank=0、target_weight=0、current_weight=0(防 UI 闪烁;test_intent.py:67-72)。
2. 否则:`returns[sym] = old<=0 ? 0.0 : float((new-old)/old)`;
   `ranked_syms = sorted(universe, key=returns, 降序)`(稳定,tie→宇宙顺序);
   `rank = 1 起的名次`;`top_set = ranked_syms[:top_k]`;`target_w = 1.0/top_k`。
3. `equity = float(equity_provider())`;
   `current_weight[sym] = equity > 0 ? qty * float(_last_close.get(sym, 0)) / equity : 0.0`。
4. 每符号状态机:
   - in_top 且持有(qty>0)→ HOLD
   - in_top 且未持有 → BUY
   - 不 in_top 且持有 → EXIT
   - 否则 rank ≤ top_k + 2 → FORMING("差两名进 top-K")
   - 否则 → NO_SETUP
5. `proximity = float((top_k - rank) / max(n,1) * 100.0)`(低于线为负);
   `strength = strength_from_rank(rank, n)`;`momentum_score = returns[sym]`;
   `target_weight = in_top ? target_w : 0.0`。

`strength_from_rank(rank, total)`(intent.py:43-49)**[MUST-MATCH]**:

```
if total <= 1 or rank <= 1: return rank == 1 ? 100.0 : 0.0
if rank >= total:           return 0.0
return max(0.0, 100.0 - (rank-1)/(total-1)*100.0)     # 线性:rank1→100,rank total→0
```

### 2.8 state_summary() **[MUST-MATCH]**(signal.py:329-350;test_signal.py:414-480)

恰好 4 个键(JSON 可序列化):

```json
{
  "current_holdings": {"<sym>": <int qty>},   // 仅 qty>0 的符号
  "last_universe_date": "YYYY-MM-DD" | null,  // ISO 日期
  "top_k": <int>,
  "universe_size": <int>
}
```

### 2.9 state_dict() / load_state() **[MUST-MATCH]**(signal.py:356-400)

```json
{
  "config": {
    "universe": [...],                       // list,声明顺序
    "momentum_lookback": <int>,
    "top_k": <int>,
    "equity_at_snapshot": <float>,           // 保存时现调 equity_provider();不序列化闭包
    "timezone": "<str>"
  },
  "history": {"<sym>": ["<decimal str>", ...]},   // 每符号按时间序的收盘字符串;键序=宇宙序
  "last_close": {"<sym>": "<decimal str>"},
  "last_universe_date": "YYYY-MM-DD" | null,
  "current_positions": {"<sym>": <int>}
}
```

- `equity_at_snapshot` 必须存在且 `account_size` 键**必须不存在**(test_signal.py:401-406)。
- `load_state`:history 重建为 maxlen=lookback+1 的队列;**config 不从 dict 恢复**
  (恢复方自带新 config 与新 equity_provider);所有宇宙符号确保有 history 队列与
  positions 条目(缺省 0);缺失键按空 dict/null 容错(`d.get(...)`)。
- Decimal 字符串必须保持写入时的精度刻度(round-trip 后 `==` 与列表逐项相等,
  test_signal.py:374-398)。

### 2.10 Runner 层 **[MUST-MATCH]**(nautilus_runner.py)

- `SectorRotationRunnerConfig`:`bar_types`(驱动订阅)、`momentum_lookback`、`top_k`、
  `timezone`;**没有 universe 字段、没有 account_size 字段**(test_runner_config.py:86-100)。
  `to_sg_config()` 从 `bar_types` 按顺序提取 symbol 组成 universe
  (nautilus_runner.py:43-63;test_runner_config.py:47-50)。
- equity_provider 闭包:`portfolio.account(venue).balance_total(USD)`,
  venue 取**第一个** bar_type 的 venue(nautilus_runner.py:84-90)。
- 订单翻译(nautilus_runner.py:115-149):
  - LONG → 市价买,数量=target_qty,**TimeInForce=GTC**;
  - FLAT → 读真实净头寸 `portfolio.net_position(instrument_id)`,为 0 则不下单;
    数量=|net|,方向 net>0 卖 / net<0 买(用真实净头寸而非信号 qty);
  - 未知 symbol 的信号静默丢弃;闸门拒绝则不下单。
- `on_start` 订阅全部 bar_types;`on_stop` 对每个 instrument `close_all_positions`。
- 日志行格式:`[SectorRot] LONG {qty} {instrument_id} :: {reason}` /
  `[SectorRot] FLAT (close {net_qty}) {instrument_id} :: {reason}`。

---

## 3. 策略 B:intraday_breakout(ORB 开盘区间突破)

来源:`src/strategies/intraday_breakout/{signal.py,intent.py,nautilus_runner.py,__init__.py}`,
基线参数 `src/strategies/params/baseline/intraday_breakout.json`,
测试 `tests/strategies/intraday_breakout/{test_signal.py,test_intent.py}`。

### 3.1 概要

单标的、只做多、纯日内(EOD 前必平,无隔夜)。算法:取每个交易时段开盘后前
`range_minutes` 分钟的最高/最低构成开盘区间并记录区间内平均每根 bar 成交量;
区间锁定后,收盘价放量上破区间高点即做多;持仓后按 硬止损/区间低点 取较紧者止损、
R 倍数止盈、EOD 强平。**Bar 周期:分钟级**(测试用 5 分钟 bar;
区间窗口按 bar 的时间戳判定,与 bar 周期解耦——任何 `local_ts < range_end`
的 bar 都计入区间)。**[MUST-MATCH]**

会话开盘时间**硬编码 09:30 交易所本地时间**(signal.py:47-48):

```
_SESSION_OPEN_HOUR = 9; _SESSION_OPEN_MINUTE = 30
```

**[MUST-MATCH]**(注释明示:ORB 是美股形态,别的市场应另立策略模板)。

### 3.2 配置参数表 **[MUST-MATCH]**(signal.py:51-90;intraday_breakout.json)

| 参数 | 类型 | 默认 | hyperopt 搜索范围(JSON 中声明) | 校验(违反 → ValueError,消息含参数名) |
|---|---|---|---|---|
| symbol | str | 必填 | — | — |
| equity_provider | 回调 → Decimal | 必填 | — | 须 callable,否则 `TypeError`("equity_provider") |
| risk_pct | float | **1.0**(单笔风险占权益 %) | float **[0.5, 2.0]** | `0 < x ≤ 100` |
| range_minutes | int | **30** | int **[15, 60]** | `≥ 1` |
| vol_multiple | float | **1.5** | float **[1.0, 3.0]** | `> 0` |
| profit_target_r | float | **2.0**(R 倍止盈) | float **[1.0, 4.0]** | `> 0` |
| hard_stop_pct | float | **1.0**(入场价的 %) | float **[0.5, 3.0]** | `0 < x ≤ 50` |
| eod_exit_time | str "HH:MM" | **"15:55"**(交易所本地) | 不可搜索 | 必须可拆为 `0≤H≤23`、`0≤M≤59`,否则 `ValueError`("eod_exit_time")(如 "25:00"、"noon" 均拒绝) |
| timezone | str IANA | **"America/New_York"** | 不可搜索 | 必须是合法时区名,否则 `ValueError`(消息含 "timezone") |

注意:如 §1 所述,ORB 未注册进 Optuna `SEARCH_SPACES`,搜索范围目前仅是元数据。
JSON 无 `allocation` 块;ORB 也未接入 live/backtest 装配
(`src/runner/strategy_assembly.py` 只装配 SEPA/SectorRotation/Pairs)。**[MUST-MATCH]**
(完整性说明:Go 版可暴露 ORB 装配,但默认组合权重里没有它。)

### 3.3 内部状态 **[MUST-MATCH]**(signal.py:99-112)

| 字段 | 类型 | 初值 |
|---|---|---|
| _current_session_date | date(交易所本地)或 null | null |
| _range_bars_count | int | 0 |
| _range_high / _range_low | Decimal 或 null | null |
| _range_locked | bool | false |
| _range_total_volume | int | 0 |
| _avg_volume | float | 0.0(锁定时 = total/count,float 除法) |
| _position_qty | int | 0 |
| _entry_price / _stop_price / _target_price | Decimal | 0 |
| _last_seen_close | Decimal 或 null | null(供 evaluate_intent 用) |
| _intent_generation | int | 0 |

### 3.4 on_bar 算法 **[MUST-MATCH]**(signal.py:118-171)

```
on_bar(bar):
  1. bar.symbol != config.symbol → 返回 [](_last_seen_close 也不更新)。
  2. _last_seen_close = bar.close
     local_ts = bar.ts 换算到 config.timezone(IANA,自动处理 DST)
     bar_date = local_ts.date()                      # 注意:本地日期(与行业轮动的 UTC 日期不同)
  3. 会话切换:若 _current_session_date == null 或 bar_date != _current_session_date:
       a. 若 _position_qty > 0:append FLAT 信号(reason="session boundary",
          target_qty=旧持仓,ts=新会话这根 bar 的 ts)——防御性清扫,正常 EOD 流程不会走到。
       b. _reset_session(bar_date):会话日期=新日期,区间五元组清零/置 null/false,
          持仓与 entry/stop/target 清零(signal.py:290-301)。
       c. 继续处理本根 bar(信号列表里已有该 FLAT)。
  4. session_open = local_ts.replace(hour=9, minute=30, second=0, microsecond=0)
     range_end = session_open + range_minutes 分钟
  5. 若 local_ts < range_end(严格小于):本 bar 计入开盘区间 → _extend_range(bar),
     返回当前 signals(区间期内不做任何进出场判断)。
     # bar.ts 按「bar 开始时间」语义;若部署方用收盘时间戳,边界 bar(收盘=range_end)
     # 会被排除在区间外 —— 源码注释称这是保守取舍(signal.py:146-151)。
  6. 否则(已越过区间窗口):
       若未锁定 → _lock_range()(幂等)。
       仍未锁定(本会话区间窗口内一根 bar 都没收到,如中途接入)→ 返回 signals,
       即**整个会话跳过进出场逻辑**(每根 bar 都会再尝试锁定,但 count 恒 0,永不锁定)。
  7. _position_qty > 0 → signals += _maybe_exit(bar, local_ts)
     否则             → signals += _maybe_enter(bar, local_ts)
  8. return signals
```

- `_extend_range`(signal.py:177-183)**[MUST-MATCH]**:
  `range_high = max(高)`、`range_low = min(低)`(首 bar 直接赋值);
  `total_volume += int(volume)`;`count += 1`。
- `_lock_range`(signal.py:185-190)**[MUST-MATCH]**:
  `count == 0 或 range_high == null` → 不锁定;否则 `avg_volume = total/count`(float),
  `locked = true`。锁定发生在**第一根越过窗口的 bar** 上,不是定时器
  (test_signal.py:197-207:区间末 bar 处理完后 locked 仍为 false)。
- **[IMPROVE]** 任何 `local_ts < range_end` 的 bar 都计入区间——包括 09:30 之前的
  盘前 bar(如 09:00 的 bar 也满足 `< 10:00`)。原行为:盘前 bar 会污染开盘区间;
  改进方案:加 `local_ts >= session_open` 下界过滤。Go 默认照抄原行为
  (生产数据流仅推送盘中 bar 时两者等价)。
- **[IMPROVE]** `local_ts.replace(hour=9, ...)` 在 DST 切换日按本地挂钟时间定位 09:30,
  ZoneInfo 语义下结果正确;但若 bar 时间戳本身落在不存在/重复的本地时刻,Python 的
  fold 语义生效。Go 实现用 `time.Date(y,m,d,9,30,0,0,loc)` 等价构造即可;测试仅覆盖
  冬令时日期(test_signal.py:10-12)。

### 3.5 入场 `_maybe_enter(bar, local_ts)` **[MUST-MATCH]**(signal.py:196-248)

```
1. local_ts >= eod_dt → 返回 []     # EOD 当口及之后禁止开新仓(test_signal.py:384-397)
   eod_dt = local_ts.replace(hour=HH, minute=MM, second=0, microsecond=0)  # 解析 eod_exit_time
2. range_high 或 range_low 为 null → []
3. 突破条件(都是严格不等):
     bar.close >  range_high                  # 等于不算(test_signal.py:232-245)
     bar.volume > _avg_volume * vol_multiple  # int 与 float 乘积比较;等于不算
   任一不满足 → []
4. entry = bar.close                                        # Decimal
   hard_stop = entry * (1 - Decimal(str(hard_stop_pct))/100) # 全 Decimal 算术
   stop = max(range_low, hard_stop)        # max 取「更紧」的止损(离 entry 更近)
   stop >= entry → [](退化情形)
5. stop_distance = entry - stop                              # Decimal
   target = entry + stop_distance * Decimal(str(profit_target_r))
6. equity = float(equity_provider())
   risk_dollar = equity * (risk_pct / 100)                   # float
   shares = int(risk_dollar // float(stop_distance))         # float 地板除→截断
   shares < 1 → []
7. 置状态:_position_qty=shares; _entry_price=entry; _stop_price=stop; _target_price=target
8. 返回 [Signal(ts=bar.ts, symbol, side=LONG, target_qty=shares, stop_price=stop,
     reason=f"ORB breakout: close {entry} > range_high {range_high}, "
            f"vol {bar.volume} > avg {_avg_volume:.0f} * {vol_multiple} "
            f":: stop {stop}, target {target}")]
   # {avg:.0f} 四舍五入到整数;{vol_multiple} 按 float repr(默认 "1.5");
   # Decimal 字段按其精确刻度渲染(见 §4)
```

数值锚点(test_signal.py:253-303):equity=100000、risk_pct=1.0、hard_stop_pct=1.0、
profit_target_r=2.0;区间 high=101.0/low=99.0、avg_vol=1,000,000;突破 bar close=102.0、
vol=2,000,000:
`hard_stop = 102.0 × 0.99 = 100.98`(> range_low 99 → stop=100.98);
`stop_distance = 1.02`;`risk = 1000`;`shares = int(1000 // 1.02) = 980`;
`target = 102 + 2 × 1.02 = 104.04`。
hard_stop_pct=5.0 时 `102 × 0.95 = 96.9 < 99` → stop = range_low = 99.0。

**[IMPROVE]** 步骤 6 把 Decimal 距离转 float 再做地板除(注释:与 SEPA 定尺寸一致)。
原行为:float64 算术;改进方案:全程 Decimal。Go 默认照抄 float64 路径以保证股数一致。

### 3.6 出场 `_maybe_exit(bar, local_ts)` **[MUST-MATCH]**(signal.py:254-278)

优先级严格如下,**每根 bar 至多一个出场信号**,命中即返回:

1. **EOD**:`local_ts >= eod_dt` → FLAT,reason = `f"EOD exit at {eod_exit_time}"`
   (如 "EOD exit at 15:55";EOD 优先于价格止损/止盈)。
2. **止损**:`bar.low <= _stop_price`(盘中触及即算,含相等)→ FLAT,
   reason = `f"stop hit at {stop}"`。
3. **止盈**:`bar.high >= _target_price`(含相等)→ FLAT,
   reason = `f"target hit at {target}"`。
4. 否则 []。

同一根 bar 同时满足止损与止盈时**止损优先**(保守)。**[MUST-MATCH]**

`_make_flat_signal`(signal.py:266-278)**[MUST-MATCH]**:
`Signal(ts=bar.ts, symbol, side=FLAT, target_qty=平仓前持仓股数, reason=...)`;
随后将 `_position_qty/_entry/_stop/_target` 全部清零。

**[IMPROVE]** 出场以 bar 的 low/high 触及判定但成交按市价单在下一时刻撮合,
且 FLAT 信号不携带触发价。原行为如上;改进方案:出场信号附带触发价并支持限价/止损单。

### 3.7 evaluate_intent(as_of) **[MUST-MATCH]**(signal.py:307-399, intent.py)

返回**单个** `IntradayBreakoutIntent`(非列表)。每次调用 `_intent_generation += 1`。

公共字段:`symbol, updated_at=as_of, generation, strategy_id="intraday_breakout",
orb_high=_range_high, orb_low=_range_low, atr_at_open=null(保留位,从不计算),
entry_window_end`。

`entry_window_end`:若 `_current_session_date != null`,
= 该日期 + eod_exit_time 在 config.timezone 构造的本地时刻 **换算为 UTC**;否则 null。

判定序(短路):
1. `range_high == null 或 range_low == null 或 未锁定` → NO_SETUP,strength=0,proximity=null。
2. `_position_qty > 0` → HOLD,strength=100,proximity=null。
3. 已过 EOD(`as_of` 换算到本地 ≥ window_end_local)→ NO_SETUP,strength=0,proximity=null。
4. `_last_seen_close == null` → FORMING,strength=50,proximity=null。
5. `last > orb_high` → BUY,strength=100,
   `proximity = float((last - orb_high)/orb_high * 100)`(> 0)。
6. 否则 FORMING(`last == orb_high` 也走这里):
   `width = orb_high - orb_low`;
   `strength = width > 0 ? clamp(float((last-orb_low)/width*100), 0, 100) : 50.0`;
   `proximity = float((last - orb_high)/orb_high * 100)`(≤ 0)。

### 3.8 state_summary() **[MUST-MATCH]**(signal.py:401-424;test_signal.py:508-571)

恰好 10 个键:

```json
{
  "symbol": "<str>",
  "session_date": "YYYY-MM-DD" | null,
  "range_high": "<decimal str>" | null,
  "range_low": "<decimal str>" | null,
  "range_locked": <bool>,
  "avg_volume": <float>,
  "position_qty": <int>,
  "entry_price": "<decimal str>" | null,   // 仅持仓时给值,否则 null
  "stop_price":  "<decimal str>" | null,   // 同上
  "target_price":"<decimal str>" | null    // 同上
}
```

### 3.9 state_dict() / load_state() **[MUST-MATCH]**(signal.py:430-478)

```json
{
  "current_session_date": "YYYY-MM-DD" | null,
  "range_bars_count": <int>,
  "range_high": "<decimal str>" | null,
  "range_low": "<decimal str>" | null,
  "range_locked": <bool>,
  "range_total_volume": <int>,
  "avg_volume": <float>,
  "position_qty": <int>,
  "entry_price": "<decimal str>",      // 注意:无仓时为 "0"(非 null)
  "stop_price": "<decimal str>",
  "target_price": "<decimal str>",
  "config": {
    "symbol", "risk_pct", "range_minutes", "vol_multiple", "profit_target_r",
    "hard_stop_pct", "eod_exit_time", "timezone",
    "equity_at_snapshot": <float>      // 保存时现调 equity_provider;不得有 account_size 键
  }
}
```

`load_state`:config 不从 dict 恢复(新建时注入);缺失键容错默认
(count=0、locked=false、qty=0、价格="0"、high/low=null);
`_last_seen_close` 与 `_intent_generation` **不持久化**(重启后分别为 null/0)。
**[MUST-MATCH]** 圆程测试逐字段断言(test_signal.py:460-500)。

### 3.10 Runner 层 **[MUST-MATCH]**(nautilus_runner.py)

- `IntradayBreakoutRunnerConfig`:`instrument_id`、`bar_type` + §3.2 的 7 个策略参数
  (默认值与 SG 一致);`to_sg_config(symbol, equity_provider)` 一对一透传,
  instrument_id/bar_type 不下传。
- equity 闭包同 sector(venue = instrument_id.venue)。
- 订单翻译:LONG → 市价买 target_qty;FLAT → 真实净头寸平仓(0 则跳过);
  **TimeInForce=DAY**(日内专属;其余策略用 GTC,nautilus_runner.py:14-18,158,175)。
  SHORT 不支持。日志:`[IntradayBreakout] LONG {qty} {id} :: {reason}` /
  `[IntradayBreakout] FLAT (close {net_qty}) {id} :: {reason}`。
- `_runner_ticker()` 返回本标的 symbol(为未来 per-ticker 自定义数据路由预留);
  当前**不**订阅 RegimeUpdate/MarketCapUpdate/EarningsBlackoutUpdate。
- `on_start` 订阅单 bar_type;`on_stop` `close_all_positions(instrument_id)`。
- API 意图模式(src/api/schemas.py:574-580):`strategy_id="intraday_breakout"`,
  `orb_high/orb_low/atr_at_open: Decimal|null`,`entry_window_end: datetime|null`。

---

## 4. 数值与字符串精度规则(两策略通用)**[MUST-MATCH]**

Go 实现若要 byte-equivalent,必须复刻以下三套算术:

1. **Decimal(十进制定点)**:Bar 价格、区间高低、entry/stop/target、行业轮动的
   close 历史。构造一律来自十进制字符串(`Decimal(str(x))`)。算术遵循
   "结果刻度=操作数刻度规则":乘法刻度相加(`102.0 × 0.99 → 100.980`,**三位小数**),
   加减取较大刻度。**序列化(state_dict/state_summary/reason)用该精确刻度的字符串**
   ——即 stop 在 JSON 里是 `"100.980"` 而非 `"100.98"`;数值比较则按值
   (`Decimal("100.980") == Decimal("100.98")`)。除法((new-old)/old)按 28 位
   有效数字十进制上下文,结果再转 float64。
   Go 建议:`shopspring/decimal` 可保留刻度做加减乘;除法需固定 28 位精度后转 float。
2. **float64**:回报排名值、equity、风险额、股数地板除(`int(a // b)`,
   即 `math.Floor(a/b)` 后截断为 int——a、b 均已转 float64)、avg_volume、
   strength/proximity。乘除顺序照源码(如 `equity * (risk_pct / 100)` 先除后乘)。
3. **格式化**:`{x:+.2f}`(带符号两位小数,Go: `fmt.Sprintf("%+.2f", x)`);
   `{x:.0f}`(四舍六入五成双?Python format 对 float 用 round-half-even,
   Go `%.0f` 同为 half-even,一致);float 直插(`{vol_multiple}`)按 Python repr
   最短表示(1.5→"1.5", 2.0→"2.0";Go: `strconv.FormatFloat(x,'g',-1,64)` 时 2.0
   渲染为 "2" —— 需特判,见 Open questions)。
4. **日期/时间**:日期 ISO `YYYY-MM-DD`;UTC datetime 序列化经 `json.dumps(default=str)`
   即 Python `str(datetime)` 格式(`2024-01-08 14:30:00+00:00`,注意是空格分隔而非 "T")
   ——影响 StrategyStateUpdate/SignalIntentUpdate 的 JSON 载荷。

---

## 5. 完整性核对清单(防"静默丢功能")

Go 版必须包含(逐项对应上文):

- [ ] 两个 SG 的 `on_bar / evaluate_intent / state_summary / state_dict / load_state` 五件套
- [ ] 两个 Runner 的配置、to_sg_config 映射、订单翻译(GTC vs DAY)、on_start/on_stop、
      日志行、`_gate` 接入、状态/意图发布(JSON 经 default=str)
- [ ] 参数 JSON 基线两份(字段逐一相同,含 display/allocation/metadata/constraints)
      + loader(schema v1 校验、env 目录回退、defaults/suggest/clamp/write_tuned)
- [ ] 行业轮动:再平衡先于摄入、暖机闸门、月字段比较、稳定排序 tie-break、
      FLAT 先 LONG 后且组内字母序、留任不动、equity 每次现取
- [ ] ORB:09:30 硬编码、严格 `<` 区间窗口、锁定幂等、中途接入跳过整会话、
      EOD ≥ 含端点、止损优先止盈、会话边界防御性清仓、entry 禁止于 EOD 后
- [ ] 错误处理:配置校验消息关键字(测试 match 锚定);SG 正常路径无 panic;
      发布路径异常只记日志

---

## 6. Open questions(源码中存在歧义之处)

1. **sector_rotation 的 `timezone` 参数完全未使用**(月翻转用 UTC 日期,
   signal.py:132)。Go 是否保留这个"死参数"以维持 state_dict/config 形状?
   本规格默认:保留字段、保持未使用(MUST-MATCH 形状),另以 [IMPROVE] 开关提供
   本地时区日期判定。
2. **月翻转只比较 month 数字**(signal.py:133-136):恰好相隔 12 个月的数据缺口不触发
   再平衡。这是有意为之还是疏漏?默认按原样复刻。
3. **`old <= 0` 的不一致**:`_compute_rebalance_signals` 把该符号剔除出排名
   (signal.py:176-177),`evaluate_intent` 则记 0.0 回报参与排名(signal.py:276-279)。
   两处行为都需照抄,但同一符号在两个视图中的 rank 可能不同——UI 与交易可能短暂不一致。
4. **ORB 盘前 bar 污染区间**:任何 `local_ts < range_end` 的 bar(含 09:30 前)都计入
   开盘区间(signal.py:152-155)。生产数据源是否保证只推盘中 bar?若 Go 数据管道含
   盘前数据,需决定开关默认值。
5. **bar.ts 的开盘/收盘时间语义**:源码注释(signal.py:146-151)说明若部署方用
   收盘时间戳,边界 bar 被保守地排除;"如果想包含请改 `<=`"。Go 数据层用哪种时间戳
   约定?需要在数据摄入规格中钉死,否则区间组成会差一根 bar。
6. **reason 字符串中 float 的渲染**:Python f-string 对 float 用 repr 最短表示
   (`2.0` → `"2.0"`);Go `%g` 会渲染成 `"2"`。若验收以 reason 字符串逐字节比对,
   Go 需实现 Python 风格的 float repr(如 strconv 'f' 与最短表示组合 + 强制保留
   至少一位小数)。是否把 reason 比对纳入 gate?
7. **Decimal 序列化刻度**:`102.0 × 0.99 = 100.980`(三位小数)——state_dict 与
   reason 中渲染为 `"100.980"`。Go decimal 库需保留乘法刻度;若验收只做数值等价
   (parse 后比较),可放宽为数值相等。需要 gate 明确"字节相等"还是"数值相等"。
8. **ORB 的 hyperopt 接入**:JSON 声明了 search 范围,但 `search_spaces.py` 的
   StrategyName 不含 intraday_breakout。Go 版优化器是否补上 ORB(属于 IMPROVE),
   还是严格复刻"声明但不搜索"?默认严格复刻。
9. **`_last_seen_close` 与 `_intent_generation` 不持久化**:重启后 intent 的
   generation 从 1 重新计数、BUY/FORMING 判定暂缺 last close。下游(UI 以 generation
   排序去重)是否依赖跨重启单调性?若是,Go 可在 [IMPROVE] 下持久化。
10. **sector_rotation 仓位与真实成交脱钩**:`_current_positions` 永不对账;
    Runner 平仓用真实净头寸但开仓数量信 SG。若部分成交,下月 FLAT 的 reason 中
    `was {held_qty} sh` 会失真(只影响文案,不影响下单数量)。是否需要对账钩子?
