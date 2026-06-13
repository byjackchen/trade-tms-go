# SEPA 策略完整规格（Specific Entry Point Analysis — Minervini）

> 来源仓库（只读参考）：`/Users/byjackchen/codespace/trade-multi-strategies`
> 提取范围：`src/strategies/sepa/*` + `tests/strategies/sepa/*` + 直接依赖
> （`src/strategies/params/baseline/sepa.json`、`src/portfolio/context_refresher.py`、
> `src/data/earnings_actor.py`、`src/data/custom_data.py`）。
>
> 标签约定：
> - **[MUST-MATCH]** — Go 实现必须逐位复刻（含边界、NaN 处理、取整、顺序、字符串格式）。
> - **[IMPROVE]** — 原实现存在已知弱点；同时给出"原行为"与"建议改进"。Go 可改进，
>   但必须以配置/标志保留可回退到原行为的能力（用于对照回测）。
>
> 所有引用均为 `路径:行号`（相对 trade-multi-strategies 仓库根）。

---

## 0. 模块总览

```
strategies/sepa/
├── _indicators.py     纯函数技术指标（MA、MA 斜率、滚动高低点）
├── _swing.py          摆动高/低点检测（VCP 内部依赖）
├── trend_template.py  Trend Template 8 条规则
├── stage.py           Stan Weinstein 风格 Stage 分类器（1/2/3/4/unknown）
├── vcp.py             VCP（波动收缩形态）检测器
├── grade.py           A+/B/skip 评级（决定建仓与分批数）
├── intent.py          SEPASignalIntent（UI 快照类型）
├── signal.py          SEPASignalGenerator — 纯 Python 核心状态机
├── nautilus_runner.py 单标的 Nautilus 包装层
└── universe_runner.py 多标的 Universe runner（active_set + 订阅上限）
```

分层硬性约束（Eng-D2，`signal.py:1-13`）：`signal.py`、`intent.py`、`grade.py`、
`vcp.py`、`stage.py`、`trend_template.py`、`_swing.py`、`_indicators.py` 全部为纯
Python，零交易引擎依赖；引擎适配只存在于两个 runner 文件。
**[MUST-MATCH]** Go 侧必须保持同样分层：核心 SEPA 包不得 import broker/engine 包。

数据流（条目化）：

1. 日线 Bar 流入 `SEPASignalGenerator.on_bar()`（`signal.py:186`）。
2. 外部上下文经 setter 推入：`set_regime` / `set_market_cap` /
   `set_earnings_blackout` / `set_catalyst`（`signal.py:170-180`）。
3. 空仓 → 入场判定链（Stage → Trend Template → VCP → 突破 → Grade → 头寸计算）；
   持仓 → 仅硬止损判定（P2 范围）。
4. 产出 `[]Signal`（LONG/FLAT），由 runner 转译为市价单。

---

## 1. 核心数据类型

### 1.1 Bar（`signal.py:70-83`）

| 字段 | 类型 | 语义 |
|---|---|---|
| symbol | string | 标的代码 |
| ts | datetime（**tz-aware UTC**） | bar 时间戳 |
| open/high/low/close | Decimal | OHLC |
| volume | int | 成交量 |

**[MUST-MATCH]** 时间一律 tz-aware UTC（`signal.py:74`）。测试夹具用美东收盘
≈ 21:00 UTC（`tests/strategies/sepa/test_signal.py:76`）。
内部 kline 缓冲将 Decimal 转 float、volume 转 int 存储（`signal.py:360-369`）。

### 1.2 Signal（`signal.py:86-101`）

| 字段 | 类型 | 语义 |
|---|---|---|
| symbol | string | |
| ts | datetime | 触发 bar 的 ts |
| side | enum `LONG`/`FLAT`/`SHORT` | SHORT 未使用（long-only，`signal.py:67`） |
| target_qty | int（带符号目标仓位） | LONG 时为首批股数；FLAT 时为 0 |
| reason | string | 人类可读说明（格式见 §8.5） |
| confidence | float，默认 1.0 | 当前恒为 1.0 |
| grade | `"A+"`/`"B"`/nil | 入场与止损退出信号均携带 |
| stop_price | Decimal/nil | 入场时为新止损；止损退出时为被击穿的止损价 |

**[MUST-MATCH]** target-position 语义：runner 将 target_qty 翻译为增量订单
（`signal.py:90-92`）。

### 1.3 SEPASignalGeneratorConfig（`signal.py:104-127`）

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| symbol | string | 是 | |
| equity_provider | `func() Decimal` | 是（无默认值） | 每次 sizing 时调用，取**实时**权益（`signal.py:108-114`） |
| risk_pct | float | 是 | 单笔风险占权益百分比 |
| market_cap_min_usd | float | 是 | Trend Template 规则 8 下限 |
| hard_stop_pct | float | 是 | 硬止损百分比（7.5 表示 7.5%） |
| pivot_buffer_pct | float | 是 | pivot 下方缓冲百分比 |
| breakout_volume_multiple | float | 是 | 突破量能倍数 |
| vcp_lookback | int | 是 | 摆动点检测半窗宽（见 §5.1；JSON 描述有误，见 Open Questions） |
| history_max_bars | int | 是 | 内部 kline 缓冲上限 |
| timezone | string | 是 | 声明字段；**逻辑中未使用**，仅存储/序列化（测试断言其等于 `America/New_York`，`test_signal.py:295-297`） |

**[MUST-MATCH]** 构造校验（`signal.py:155-164`）：
- `equity_provider` 不可调用 → `TypeError`，错误信息含 `equity_provider`；
- `risk_pct` 必须满足 `0 < risk_pct <= 100`，否则 `ValueError` 含 `risk_pct`；
- `hard_stop_pct <= 0` → `ValueError` 含 `hard_stop_pct`；
- 构造时**绝不调用** `equity_provider()`（其闭包可能尚未就绪，`signal.py:156-158`）。

### 1.4 参数默认值与 hyperopt 搜索范围（`src/strategies/params/baseline/sepa.json`）

| 参数 | 默认值 | 类型 | 搜索范围 [low, high] | 描述 |
|---|---|---|---|---|
| risk_pct | 1.0 | float | [1.0, 4.0] | 单笔风险 % of equity |
| market_cap_min_usd | 500000000.0 | float | [250000000.0, 1000000000.0] | 规则 8 市值下限 |
| hard_stop_pct | 7.5 | float | [4.0, 12.0] | `stop = entry*(1-hard_stop_pct/100)` |
| pivot_buffer_pct | 1.5 | float | [0.5, 3.0] | `pivot*(1-pivot_buffer_pct/100)` |
| breakout_volume_multiple | 1.5 | float | [1.0, 2.5] | 突破 bar 量能倍数 |
| vcp_lookback | 5 | int | [3, 10] | 摆动检测半窗宽 |
| history_max_bars | 1000 | int | 不可调（search=null） | 缓冲上限 |
| timezone | "America/New_York" | str | 不可调 | |

策略级元数据：`allocation.capital_pct = 0.40`，`active = true`（`sepa.json:7-10`）；
`schema_version = 1`；`constraints = []`。

**[MUST-MATCH]** 参数加载器语义（`params/loader.py`）：
- 解析顺序：显式 `params_dir` > 环境目录 `TMS_STRATEGY_PARAMS_DIR`（仅当该目录下
  存在 `sepa.json`）> 包内 baseline（`loader.py:77-96`）。
- `search` 只允许出现在 numeric 类型（float/int）上，否则加载报错（`loader.py:163-168`）。
- hyperopt 取样时参数名带前缀：`sepa.risk_pct` 等（`loader.py:209`）；float 用
  `suggest_float`、int 用 `suggest_int`；约束按声明顺序施加 clamp_high/clamp_low
  （`loader.py:223-229`，SEPA 当前无约束）。
- `suggest_with` 只返回有 search 的键；静态默认靠 `defaults_dict` 合并
  （`loader.py:192-204`）。

---

## 2. 指标原语（`_indicators.py`）

### 2.1 ma（`_indicators.py:13-15`）

**[MUST-MATCH]** `ma(klines, period)` = `close` 列的简单滚动均值，
`min_periods == period`（pandas 默认）：窗口不满时为 NaN。

### 2.2 ma_slope_pct（`_indicators.py:18-31`）

**[MUST-MATCH]** 公式与边界：

```
若 len(klines) < period + lookback           → 返回 0.0
last = MA(period) 的最后一个值
prev = MA(period) 在位置 [-1 - lookback] 的值     # 即 lookback 根 bar 之前
若 prev == 0 或 prev/last 为 NaN              → 返回 0.0
返回 (last - prev) / prev * 100.0             # 单位：百分比
```

默认 lookback=20。注意长度门槛是 `period + lookback`（不是 `period + lookback - 1`），
且 prev 取的是 `-1-lookback` 位置（21 根前的 MA 与当前 MA 之差除以 20 根跨度的语义
差 1 根——按源码原样复刻）。

### 2.3 ma_uptrend_days（`_indicators.py:34-52`）

**[MUST-MATCH]** MA(period) 连续上升的尾部天数：

```
若 len(klines) < period + 2 → 0
series = MA(period).dropna()
若空 → 0
diffs = series.diff().fillna(0)      # 第一个 diff（NaN）按 0 处理 → 视为"未上升"
从尾部向前数 diffs > 0 的连续个数（严格 >0；遇到 <=0 即停）
```

仅作诊断输出（Trend Template 不强制 MA200 上升 ≥1 个月这条原版规则，
`trend_template.py:17-19`）。

### 2.4 rolling_high / rolling_low（`_indicators.py:55-62`）

**[MUST-MATCH]** `high` 列 / `low` 列的 N=252 滚动最大/最小，`min_periods == window`
（窗口不满 → NaN，由调用方做回退，见 §3.2）。

---

## 3. Trend Template — 8 条规则（`trend_template.py`）

### 3.1 规则与阈值

入参：日线 klines（需 `close`、`high`、`low` 列）+ 外部提供的 `market_cap_usd`。
常量（`trend_template.py:37-40`）：

| 常量 | 值 |
|---|---|
| DEFAULT_MARKET_CAP_MIN_USD | 500_000_000.0 |
| _HIGH_LOW_WINDOW | 252 |
| _HIGH_TOLERANCE | 0.25 |
| _LOW_PREMIUM | 0.30 |

**[MUST-MATCH]** 8 条规则（全部以最后一根 bar 计，`trend_template.py:136-165`）：

| # | 规则 | 精确判定 |
|---|---|---|
| 1 | Price > MA50 | `close > ma50`（严格 >） |
| 2 | Price > MA150 | `close > ma150` |
| 3 | Price > MA200 | `close > ma200` |
| 4 | MA50 > MA150 | `ma50 > ma150` |
| 5 | MA150 > MA200 | `ma150 > ma200` |
| 6 | 距 52 周高点 ≤25% | `close >= high_52w * (1.0 - 0.25)`（**>=**） |
| 7 | 高于 52 周低点 ≥30% | `low_52w > 0` 时 `close >= low_52w * (1.0 + 0.30)`；`low_52w <= 0` → **False**（`trend_template.py:155`） |
| 8 | 市值下限 | `market_cap_usd >= market_cap_min_usd`（**>=**，等于也过，`test_trend_template.py:75` 用 `min-1` 验证失败） |

`passed` = 全部 8 条 AND（`trend_template.py:70-80`）；
`passing_rules` = 通过条数 0..8（`trend_template.py:83-96`，供 intent 评分用）。

### 3.2 历史不足与 NaN 处理

**[MUST-MATCH]**
- `len(klines) < 200`：规则 1–7 一律 False；**规则 8 仍按市值实际判定**；
  诊断字段 close = 最后收盘（若 n==0 则 0.0），ma50/150/200、high/low_52w 均 0.0，
  uptrend_days=0（`trend_template.py:113-134`）。
- 200 ≤ n < 252：rolling 252 高/低为 NaN → 回退为整段 `high.max()` / `low.min()`
  （expanding 语义，`trend_template.py:143-150`）。
- MA 值经 `float()` 强转；n ≥ 200 时 MA50/150/200 必然非 NaN。

### 3.3 诊断字段

**[MUST-MATCH]** `TrendTemplateResult` 携带：close、ma50、ma150、ma200、high_52w、
low_52w、market_cap_usd、ma200_uptrend_days（`trend_template.py:59-67`）。
测试基准（`test_trend_template.py:139-148`）：252 根 100→200 线性上升 →
close==200.0、high_52w==200.0、low_52w==100.0。

---

## 4. Stage 分类器（`stage.py`）

### 4.1 常量（`stage.py:29-35`）

| 常量 | 值 |
|---|---|
| _MIN_BARS | 220（MA200 + 20 斜率回看） |
| _MOMENTUM_WINDOW | 60 |
| _RECENT_MOMENTUM_THRESHOLD | 5.0（%） |
| _SLOPE_BULL_THRESHOLD | 1.0 |
| _SLOPE_BEAR_THRESHOLD | -1.0 |
| _SLOPE_BASE_THRESHOLD | 0.5 |
| _ABOVE_MA_HISTORY_FRACTION | 0.7 |

### 4.2 算法（`stage.py:38-89`）

**[MUST-MATCH]** 按以下**顺序**短路求值；返回 `"1" | "2" | "3" | "4" | "unknown"`：

```
若 len < 220 → "unknown"
ma150 = MA150 最后值；ma200 = MA200 最后值；last = 最后收盘
slope = ma_slope_pct(period=200, lookback=20)

# 60 根动量
若 len(close) >= 120:
    recent_avg = close 最后 60 根均值
    prior_avg  = close[-120:-60] 均值
    momentum = (recent_avg - prior_avg) / prior_avg * 100        # float
否则: momentum = slope                                            # 回退

1) Stage 2: last > ma150 > ma200（链式严格比较）AND slope > 1.0 AND momentum > 5.0
2) Stage 4: last < ma150 < ma200 AND slope < -1.0
3) Stage 3: last > ma200 AND slope > 1.0 AND momentum < 5.0
4) Stage 3 兜底: mean(close.tail(200) > MA200.tail(200)) > 0.7 AND |slope| <= 1.0
   # 注意：MA200 在前 199 根上为 NaN，NaN 比较为 False 计入分母——
   # 当总长 < 399 时该分数被天然压低（≤ (len-199)/200）。按源码原样复刻。
5) Stage 1: |slope| < 0.5
否则 → "unknown"
```

momentum 落在边界（==5.0）时既不满足 Stage 2 也不满足 Stage 3 第一式
（两处都是严格不等）。**[MUST-MATCH]**

测试锚点（`test_stage.py`）：250 根 100→200 线性 → "2"；200→100 → "4"；
110 根升至 180 + 140 根横盘 → "3"；均值 100、σ=0.5 噪声 → "1"；100 根 → "unknown"。

**[IMPROVE]** 第 4 步兜底的 NaN-as-False 行为（`stage.py:81-82`）在 len<399 时
几乎不可能触发（分数上限 (len-199)/200 ≤ 0.7 需 len≥339）。原行为：直接比较含
NaN 段。建议改进：仅对 MA 有效段计算分数；保留 `--legacy-stage-fraction` 开关回退。

---

## 5. 摆动点检测（`_swing.py`）

### 5.1 算法（`_swing.py:23-42`）

**[MUST-MATCH]**
- 入参 lookback（即 config `vcp_lookback`）= 半窗宽；窗口 = `2*lookback+1` 根，居中。
- 仅扫描 `i ∈ [lookback, len-lookback)`——**首尾各 lookback 根永不成为摆动点**
 （隐式 look-ahead guard：确认一个摆动点需要其后 lookback 根 bar）。
- swing high：`highs[i] == window.max()` 且 `window.argmax() == lookback`
  （argmax 取**最左**极值 → 平台期取最左根；若最左极值不在中心则拒绝）。
- swing low 对称：`lows[i] == window.min()` 且 `argmin() == lookback`。
- 同一根 bar 可同时是 high 和 low（OHLC 同值的孤立尖峰理论上可双触发）。
- 输出按 idx 升序（`out.sort(key=idx)`；同 idx 时 high 先于 low——append 顺序
  high 在前且 Python sort 稳定）。
- price 取 `float(highs[i])` / `float(lows[i])`；date 取 DataFrame 索引值。

测试锚点（`test_vcp.py:43-62`）：11 根 `[100..104,110,104..100]`、lookback=5 →
恰好 1 个 high，idx=5，price=110.0；输出 idx 严格有序。

---

## 6. VCP 检测器（`vcp.py`）

### 6.1 常量（`vcp.py:29-33`）与入参默认

| 名称 | 值 | 说明 |
|---|---|---|
| _DRYUP_BASELINE_THRESHOLD | 0.7 | 末段量 < 70% 基线量 |
| _BASELINE_LOOKBACK | 50 | 基线量回看根数 |
| _DEFAULT_BASE_MIN_DAYS | 25 | 底部最短（≈5 周） |
| _DEFAULT_BASE_MAX_DAYS | 150 | 底部最长（≈30 周） |
| lookback（入参） | 5 | 传 config.vcp_lookback |
| min_contractions | 2 | |
| max_last_contraction | 10.0 | 末段收缩深度上限 % |

### 6.2 算法步骤（`vcp.py:54-165`）

**[MUST-MATCH]** 全流程：

1. `len(klines) < 30` → nil（`vcp.py:69-70`）。
2. `find_swing_points(klines, lookback)`。
3. **配对收缩段**（`vcp.py:75-86`）：按摆动序遍历；遇 high 记下
   `(last_high, last_high_idx)`（后出现的 high **覆盖**前一个未配对 high）；
   遇 low 且有未配对 high → 计算
   `depth = (last_high - low) / last_high * 100`；
   仅当 `0.5 < depth < 50`（双侧**严格**）才收录四元组
   `(depth, low_price, low_idx, high_idx)`；无论是否收录，**配对后清空** last_high
   （一个 high 只配一个 low；深度出界的配对也消耗该 high）。
4. 收缩段总数 `< min_contractions` → nil。
5. **收敛尾**（`vcp.py:93-101`）：从最新收缩段向旧回溯，只要更旧段
   `depth >= 尾部最新加入段的 depth`（**>=**，允许相等）就并入；首次violation停止。
   反转回时间正序。尾长 `< min_contractions` → nil。
6. 末段深度 `last_depth = tail[-1].depth > max_last_contraction` → nil。
7. **pivot**（`vcp.py:107-115`）：`last_low_idx` 之后（**不含**该 low 那根）的
   `high` 列最大值；若其后无 bar，回退为整个 klines 最后一根的 high。
8. **底部长度**（`vcp.py:117-121`）：`base_len = len(klines) - 1 - tail[0].low_idx`
  （**位置差**，非日历天）；不满足 `base_length_min <= base_len <= base_length_max`
   （闭区间）→ nil。
9. `final_duration = max(1, last_low_idx - last_high_idx)`（末段 high→low 根数）。
10. **量能干涸**（`vcp.py:126-148`）：
    - 基线段 = `klines[max(0, last_low_idx-50) : max(1, last_low_idx)]` 的
      volume 均值（空段 → 0.0）；
    - 末段 = `klines[last_high_idx : last_low_idx+1]` 的 volume 均值；
    - `vol_dryup_ratio = final_avg / baseline_avg`（基线 ≤0 → **1.0**）；
    - 逐段相对干涸：对 tail 每段取 `klines[max(0, low_idx-5) : low_idx+1]`
      （固定 6 根窗，**与 lookback 无关**）的均量，要求严格递减
      （`avg >= prev` 即失败）；
    - `volume_dryup = relative_dryup AND vol_dryup_ratio < 0.7`。
11. **质量分**（`vcp.py:150-153`）：
    `score = min(1.0, 0.25*len(tail) + 0.4*(1 - last_depth/10) + (volume_dryup ? 0.2 : 0))`
    —— 注意无下限 clamp；但因 last_depth ≤ 10，第二项 ∈ [0, ~0.38]，整体非负。
12. **输出取整**（`vcp.py:155-165`）：contractions 各深度 `round(x, 2)`（Python
    banker's rounding——round-half-even）；last_contraction_pct `round(.,2)`；
    quality_score `round(.,2)`；vol_dryup_ratio `round(.,3)`；pivot_price 不取整。

**[MUST-MATCH]** `VCPSnapshot` 字段（`vcp.py:37-51`）：code、contractions
（旧→新）、last_contraction_pct、pivot_price、base_length_days、volume_dryup、
quality_score、vol_dryup_ratio、final_contraction_duration_days。

测试锚点（`test_vcp.py`）：双段收缩 -10% → -6.4%（lookback=4）可检出，
pivot ≥ 118.0，score ∈ [0,1]；纯线性上升 → nil；逐段加深（-5,-10,-20）→ nil
（收敛尾只剩 1 段）；末段 -12% > 10% → nil；强干涸夹具 → volume_dryup=true 且
ratio < 0.7。

**[IMPROVE]** `base_length_days` 与 `final_contraction_duration_days` 都是 bar
位置差而非日历天，命名有歧义；`_BASELINE_LOOKBACK=50` 注释称"50-day baseline"
但单位也是 bar。原行为：bar 计数。建议改进：Go 字段命名为 `*_bars` 并在 JSON
序列化处保留旧键名以兼容 UI。

**[IMPROVE]** Python `round()` 为 round-half-even。Go 的
`math.Round` 是 half-away-from-zero。原行为：half-even。建议改进：实现
`roundHalfEven(x, digits)` 工具函数保证逐位一致（对照测试必需），不建议改语义。

---

## 7. Grade 评级（`grade.py`）

### 7.1 输入（`grade.py:17-23`）

`SetupInputs{ trend_template_pass, earnings_pass, stage, catalyst,
vcp_contraction_count, regime }`。

### 7.2 规则（`grade.py:26-42`）

**[MUST-MATCH]** 顺序判定：

```
1) regime == "bear" 或 stage != "2"                  → "skip"
2) !(trend_template_pass AND earnings_pass)          → "skip"
3) vcp_contraction_count < 2                          → "skip"
4) catalyst AND count >= 3 AND regime == "bull"       → "A+"
5) 否则                                               → "B"
```

要点：
- 只有 `"bear"` 被一票否决；`"neutral"`、`"warning"`、甚至冷启动的 `"unknown"`
  都可给出 "B"。**[MUST-MATCH]**
- "A+" 三条件缺一不可（催化剂 + ≥3 段收缩 + bull）。
- `earnings_pass = !earnings_blackout`（调用方取反，`signal.py:239`）。

**[IMPROVE]** regime=="unknown"（Actor 尚未推送）时允许 B 级入场是宽松行为
（`signal.py` SG 默认 `_regime = "unknown"`，`signal.py:150`；注意
`nautilus_runner.py:138-140` 注释声称默认 "neutral"，与代码不符）。原行为：
unknown 不拦截。建议改进：Go 增加配置 `requireKnownRegime`（默认 false=原行为），
true 时 unknown/空 regime 视同 skip。

---

## 8. SEPASignalGenerator 状态机（`signal.py`）

### 8.1 内部状态（`signal.py:140-153`）

| 字段 | 初值 | 说明 |
|---|---|---|
| _klines | 空 DataFrame | OHLCV 历史（float/int 列，DatetimeIndex） |
| _position | 0 | 当前持仓股数 |
| _entry_price | Decimal(0) | |
| _stop_price | Decimal(0) | |
| _pivot_price | Decimal(0) | |
| _grade | nil | |
| _intent_generation | 0 | evaluate_intent 单调计数 |
| _regime | "unknown" | 外部 setter 推入 |
| _market_cap_usd | 0.0 | |
| _earnings_blackout | false | |
| _catalyst | false | |

### 8.2 on_bar 主流程（`signal.py:186-193`）

**[MUST-MATCH]**
1. `bar.symbol != config.symbol` → 返回 `[]`，**不追加历史**（`test_signal.py:162-166`）。
2. 追加 bar 到 `_klines`（§8.7）。
3. `_position == 0` → `_maybe_enter`；否则 → `_maybe_exit`。
   同一根 bar 不会既入场又出场。

### 8.3 入场判定链 `_maybe_enter`（`signal.py:199-254`）

**[MUST-MATCH]** 按序短路，任一失败返回 `[]`：

1. **Warmup 门槛**：`len(_klines) < 200` → 拒（`signal.py:200-201`）。
   注意 Stage 需要 220 根才能非 unknown，所以实际首个可入场 bar ≥ 第 220 根。
2. **Stage == "2"**（在含当前 bar 的 `_klines` 上分类）。
3. **Trend Template 8 条全过**（含当前 bar；market_cap 来自 setter，
   下限来自 config）。
4. **VCP**：`prior = _klines.iloc[:-1]`（**剔除当前 bar**——防止突破 bar 自身的
   high 抬高 pivot 导致 `close <= pivot` 恒假，`signal.py:217-221` —— 关键
   look-ahead/自指 guard）；`len(prior) < 30` → 拒；
   `detect_vcp(prior, code=symbol, lookback=vcp_lookback)`（min_contractions、
   max_last_contraction、base 长度界都用 §6.1 默认值）→ nil 则拒。
5. **突破**：`float(bar.close) <= vcp.pivot_price` → 拒（必须**严格大于** pivot）；
   再查量能（§8.4）。
6. **Grade**：构造 `SetupInputs{tt.passed, !_earnings_blackout, stage, _catalyst,
   len(vcp.contractions), _regime}`；`grade == "skip"` → 拒。
7. 计算止损与头寸并发出 LONG（§8.5）。

### 8.4 突破量能 `_breakout_volume_ok`（`signal.py:256-273`）

**[MUST-MATCH]**

```
base_lookback = 60                       # 硬编码 60（非 50！）
len(_klines) < 61 → false
base_avg = _klines.volume[-(61):-1].mean()    # 不含当前 bar 的前 60 根均量
base_avg <= 0 → false
bar_vol = _klines.volume 最后一根（== 当前 bar）
返回 bar_vol > breakout_volume_multiple * base_avg     # 严格 >
```

分母**排除当日**是刻意行为（`signal.py:259-262`：包含当日会被突破量自身抬高
分母）。注意：模块 docstring（`signal.py:20`）与 JSON 描述写的是"1.5x 50-day
avg"，**代码实际是 60 根**——以代码为准。见 Open Questions Q2。

### 8.5 建仓 `_build_long_entry`（`signal.py:275-311`）

**[MUST-MATCH]**

```
entry_f = float(bar.close)
pivot_f = float(vcp.pivot_price)
hard_stop  = round(entry_f * (1 - hard_stop_pct/100), 4)      # half-even, 4 位
pivot_stop = round(pivot_f * (1 - pivot_buffer_pct/100), 4)
stop_f = max(hard_stop, pivot_stop)
tranches = grade == "A+" ? 3 : 2
shares = _compute_first_tranche_shares(entry_f, stop_f, tranches)
shares <= 0 → 返回 []（不入场、不留状态）
```

状态落账：`_position = shares`、`_entry_price = bar.close`（Decimal 原值）、
`_stop_price = Decimal(str(stop_f))`、`_pivot_price = Decimal(str(pivot_f))`、
`_grade = grade`。

reason 字符串格式（`signal.py:296-300`，UI 与日志依赖）：

```
SEPA {grade} :: stage=2, TT pass, VCP {n} contractions (last {last_pct}%), pivot ${pivot:.2f} -> close ${close:.2f}, stop ${stop:.2f}
```

其中 `{last_pct}` 是已 round(.,2) 的浮点的 Python str 形式（如 `4.24`），
`%.2f` 为 pivot/close/stop。
信号：`Signal{symbol, ts=bar.ts, LONG, target_qty=shares, reason, grade,
stop_price=_stop_price}`，confidence 取默认 1.0。

**[MUST-MATCH]** P2 范围注记：tranches 仅用作除数——**只买第一批**，后续批次
加仓逻辑不存在（无 R-ladder、无 pyramid）。A+ 实际首批 = full_shares//3，
B = full_shares//2。

### 8.6 头寸计算 `_compute_first_tranche_shares`（`signal.py:313-326`）

**[MUST-MATCH]**

```
equity = float(equity_provider())        # 每次 sizing 实时调用，绝不缓存
risk_dollar = equity * risk_pct / 100
stop_distance = entry - stop
stop_distance <= 0 → 0
full_shares = int(risk_dollar // stop_distance)    # floor 除
返回 full_shares // max(1, tranches)               # 再 floor 除
```

两次整除导致 ≤ ~1% 的份额损耗（`test_signal.py:349-363` 容差
`max(2, expected//100)`）。equity 翻倍 → 股数近似翻倍（线性）。

### 8.7 历史缓冲 `_append_bar`（`signal.py:360-376`）

**[MUST-MATCH]**
- 每 bar 追加一行：open/high/low/close 转 float、volume 转 int，索引 =
  `pd.Timestamp(bar.ts)`。
- 超过 `history_max_bars` 时保留最后 N 根（追加后裁剪）。
- **无去重**：同一时间戳重复 on_bar 会追加两行。

**[IMPROVE]** 重复时间戳无去重/无乱序检测。原行为：盲目 append。建议改进：
Go 实现可在 ts ≤ 上一根 ts 时记录 warning 并按配置选择 drop/replace（默认保持
append 以对齐回测）。

### 8.8 出场 `_maybe_exit`（`signal.py:332-354`）— P2 仅硬止损

**[MUST-MATCH]**
- 触发条件：`bar.close <= _stop_price`（Decimal 比较，**<=**，等于也触发）。
- 触发后**先暂存** stop/grade，再清零全部头寸状态（_position/_entry/_stop/
  _pivot → 0，_grade → nil），随后发出：
  `Signal{symbol, ts, FLAT, target_qty=0,`
  `reason="SEPA stop hit at ${stop:.2f} :: close ${close:.2f}", grade=旧grade,`
  `stop_price=旧stop}`。
- `close > stop` → `[]`，持仓不变。
- 没有移动止损、没有时间退出、没有强弱卖出（注释言明 P3+，`signal.py:329`）。
- regime 转 bear、earnings blackout 开启**不会**触发持仓退出——只挡新入场。

**[IMPROVE]** 与 `evaluate_intent` 的 STOP_HIT 判定不一致：on_bar 用
`close <= stop`（`signal.py:333`），intent 用 `close < stop`（严格 <，
`signal.py:432`）。close == stop 时实际会被平仓但 intent 显示 HOLD。原行为：
两处不同。建议改进：Go 内部统一为 `<=` 并加注释；若做逐位对照需保留两个谓词。

### 8.9 warmup_from_history（`signal.py:378-392`）

**[MUST-MATCH]**
- 入参为 date-indexed OHLCV DataFrame（与 on_bar 同形）；nil 或空 → no-op。
- 取尾部 `history_max_bars` 行，按列序 `[open, high, low, close, volume]` 复制
  存为 `_klines`（多余列丢弃，列序固定）。
- **不跑任何信号逻辑**——纯状态预热（跳过 ~200 根 warmup 尾）。
- 必须在首个 on_bar 之前调用；调用方传"运行窗口开始之前"的历史
  （`universe_runner.py:148-149`），保证无 look-ahead。

### 8.10 evaluate_intent（`signal.py:398-513`、`intent.py`）

返回 `SEPASignalIntent`（`intent.py:30-56`）：

| 字段 | 类型 | 说明 |
|---|---|---|
| symbol / state / strength / proximity_to_trigger_pct / updated_at / generation | 基础 | strength ∈ [0,100] |
| strategy_id | 恒 "sepa" | `intent.py:27` |
| grade | int（0-100，= passing_rules/8*100 截断） | 注意与 Grade("A+/B") 不同物 |
| trend_template_pass | bool | |
| base_age_days / base_depth_pct / volume_dryup | VCP 诊断 / nil | |
| pivot_price / stop_price | Decimal / nil | |
| rs_rank | nil（未实现） | |

**[MUST-MATCH]** 判定顺序（每次调用先 `_intent_generation += 1`；注意 docstring
自称"不变更状态"但 generation 确实自增——`signal.py:407`，测试断言严格单调
`test_intent.py:125-130`）：

```
base_kwargs: pivot_price = _pivot_price>0 ? 之 : nil；stop_price 同理
1) _klines 空或 len < 50              → NO_SETUP, strength=0, tt_pass=false, grade=0
2) 持仓 且 _stop_price>0 且 last_close < stop（严格<）
                                       → STOP_HIT, strength=0, tt_pass=false, grade=0
3) 持仓                                → HOLD, strength=50.0, tt_pass=true(硬编码), grade=0
4) 空仓：tt = evaluate_trend_template(全量 _klines, ...)
   tt_grade = int(passing_rules / 8 * 100)        # int() 截断：7/8 → 87
   len>=30 时 vcp = detect_vcp(全量 _klines, ...)  # 注意：这里不剔除当前 bar
   4a) !tt.passed                      → NO_SETUP, strength=tt_grade, grade=tt_grade
   4b) _pivot_price>0 且 last_close >= pivot
                                       → BUY, proximity=(close-pivot)/pivot*100,
                                         附 VCP 诊断三元组
   4c) 否则                            → FORMING；proximity 仅当 pivot>0 时给出
                                         （可为负），附 VCP 诊断
last_close = Decimal(str(_klines.close.iloc[-1]))
```

实务说明：`_pivot_price` 只在真实入场时写入并在出场时清零，因此 4b 的 BUY
态在自然流程中难以出现（测试通过手工 arm pivot 验证，`test_intent.py:84-101`）。
**[MUST-MATCH]** 保持该语义——pivot 不在 intent 路径中即时计算。

tt_grade 截断锚点：8/8→100；7/8→87（`int(87.5)`）；6/8→75。

### 8.11 state_summary（`signal.py:515-539`）

**[MUST-MATCH]** 恰好 11 个键（测试做集合相等断言，`test_signal.py:377-390`）：
`symbol, regime, market_cap_usd, in_blackout, position_qty, entry_price,
stop_price, current_grade, vcp_detected, pivot_price, bars_in_history`。
- flat 时 entry_price/stop_price/pivot_price/current_grade 均 nil，
  vcp_detected=false；
- 持仓时 entry/stop/pivot 为 **str(Decimal)**（JSON 安全），
  current_grade ∈ {"A+","B"}，`vcp_detected = (持仓) AND pivot>0`；
- 整体必须可直接 JSON 序列化（无 Decimal/datetime 对象）。

### 8.12 state_dict / load_state（`signal.py:545-601`）

**[MUST-MATCH]** `state_dict()` 结构：

```jsonc
{
  "config": {
    "symbol", 
    "equity_at_snapshot": float(equity_provider()),   // 保存时实时取一次，仅审计用
    "risk_pct", "market_cap_min_usd", "hard_stop_pct", "pivot_buffer_pct",
    "breakout_volume_multiple", "vcp_lookback", "history_max_bars", "timezone"
  },
  "context":  { "regime", "market_cap_usd", "earnings_blackout", "catalyst" },
  "position": { "shares", "entry_price": str(Decimal), "stop_price": str(...),
                "pivot_price": str(...), "grade": "A+"|"B"|null },
  "klines":   <reset_index().to_dict(orient="list")>   // 列式 dict，索引列名 "index"
}
```

关键点：
- `config` 中**没有** `account_size` 键（测试显式断言不存在，
  `test_signal.py:329-334`）；equity_provider 本身不序列化。
- `load_state()` 只恢复 context/position/klines，**不恢复 config**（caller 用新
  config 构造再 load）；缺省值：regime→"unknown"、cap→0.0、blackout→false、
  catalyst→false、shares→0、价格→Decimal("0")、grade→原样 get（可 nil）。
- klines 恢复：若列中有 `"index"` 则 set_index 并 `to_datetime`；空/缺失 →
  空 DataFrame。
- 往返不变量：position、stop、entry、len(klines) 全等
  （`test_signal.py:305-318`）。

**[IMPROVE]** 若 `_klines` 的索引带名字（如 warmup 传入的 df 索引名为 "ts"），
`reset_index()` 产生的列名将不是 "index"，load_state 会把日期当普通数据列、
索引丢失。原行为：依赖索引无名。建议改进：Go 侧序列化时显式写 `"index"` 键；
反序列化兼容 "index"/"ts" 两种列名。

---

## 9. 外部上下文（regime / market cap / earnings blackout）

SG 是被动接收方；以下为上游计算语义，Go 侧 context 服务必须等价。

### 9.1 Market regime（`portfolio/context_refresher.py:50-103`）

常量：`_REGIME_MIN_BARS=200`，`_REGIME_SLOPE_WINDOW=30`，
`_REGIME_SLOPE_FLAT_PCT=0.0`（`context_refresher.py:43-47`）。

**[MUST-MATCH]** `compute_regime(spy_history, as_of)`：

```
1) as_of 非空 → 先过滤 frame 至 date <= as_of（DatetimeIndex 或 'date' 列均兼容）
   —— 显式防 look-ahead（context_refresher.py:62-65）
2) len < 200 → "neutral"
3) ma200 = close 的 rolling(200, min_periods=200) 均值；last_ma NaN → "neutral"
4) last_close < last_ma                       → "bear"      （严格 <；等于不算）
5) len(ma200) < 31 → "warning"（保守）
6) ma_then = ma200[-31]；NaN 或 0 → "warning"
7) slope_pct = (last_ma - ma_then)/ma_then；> 0.0 → "bull"；否则 → "warning"
```

注意 regime 的斜率窗口是 **30**（与 Stage 的 20 不同），阈值是 **0**（与 Stage
的 ±1.0% 不同）。

### 9.2 Earnings blackout（`context_refresher.py:150-184`、`data/earnings_actor.py`）

**[MUST-MATCH]**
- `EARNINGS_BLACKOUT_DAYS = 5`（`context_refresher.py:40`）。
- 判定：`as_of` 与任一财报日的差落在 **±5 个日历天闭区间**
  （`lo = as_of - 5d, hi = as_of + 5d`，`dates >= lo AND dates <= hi`）→ True。
- 数据源：SHARADAR/EVENTS，eventcode 22，列 `ticker` + `report_date`；
  loader 只返回 True 项，缺席即 False。
- EarningsActor 在 SPY 日线心跳上对每个跟踪 ticker 重算；**首次观察必发布**
  （即使 False），之后仅在翻转时发布（`earnings_actor.py:79-86`）。
- as_of 取心跳 bar 的 `ts_event` 折算 UTC 日期（`earnings_actor.py:117`）。

与 SEPA 的互锁：blackout=true → `earnings_pass=false` → grade=skip → **仅阻断
新入场**；既有持仓不受影响（无强制平仓）。**[MUST-MATCH]**

### 9.3 Market cap（`context_refresher.py:106-148`）

**[MUST-MATCH]** `load_sf1_market_caps`：SHARADAR/SF1，按 `dimension=="MRT"`
过滤（若有该列），`datekey <= as_of`、`marketcap` 非空且 `> 0`，每 ticker 取
datekey 最新一行，值转 `Decimal(str(marketcap))`。SG 收到后存 float。

### 9.4 更新分发（custom data）

**[MUST-MATCH]**（`custom_data.py:28-79`、`universe_runner.py:161-193`、
`test_universe_runner.py:125-164`）
- `RegimeUpdate{value}`：全局——广播给**所有** SG。value ∈
  {"bull","bear","neutral","warning"}。
- `MarketCapUpdate{ticker, value}` / `EarningsBlackoutUpdate{ticker, value}`：
  按 ticker 路由到单个 SG；未知 ticker **静默忽略**（不报错）。
- 单标的 runner 通过 `_runner_ticker()` 过滤非本标的更新
  （`nautilus_runner.py:147-152`）。

---

## 10. 单标的 Runner（`nautilus_runner.py`）

**[MUST-MATCH]** 语义要点（Go 对应一层薄适配器）：
- Config 字段与 SG 一致再加 `instrument_id`/`bar_type`；**没有**
  `account_size`、`initial_market_cap_usd`、`initial_regime`
  （`test_runner_config.py:94-114`）。
- `to_sg_config(symbol, equity_provider)`：字段一一透传（runner-only 字段不
  转发）；产物为 frozen（`test_runner_config.py:47-91`）。
- equity_provider = 闭包，实时读 venue 账户 USD 总额
  （`nautilus_runner.py:123-127`）；构造期不调用。
- on_start：订阅 bar + 3 类 context 更新；on_stop：平掉本标的全部仓位。
- 信号转译 `_submit_for_signal`（`nautilus_runner.py:175-208`）：先过组合
  gate（拒则丢弃+记日志）；LONG → 市价买 target_qty（GTC）；FLAT → 读净仓位，
  0 则 no-op，否则反向市价单平全部；SHORT 不支持。

---

## 11. Universe Runner（`universe_runner.py`）

多标的版本；每个 bar_type 一个独立 SG（同一份调参 knob，
`universe_runner.py:98-115`）。

### 11.1 架构常量

**[MUST-MATCH]** `active_cap = 20`（U-D2）、`subscription_cap = 30`（U-D3）
为默认值（`universe_runner.py:57-58`），非 hyperopt 调参对象；config frozen
（`test_universe_runner.py:22-33`）。运行时可用 `_active_cap_override` /
`_subscription_cap_override` 覆盖（`universe_runner.py:216-218,243-244`）。

### 11.2 日界检测与 pre-open（`universe_runner.py:199-239`）

**[MUST-MATCH]**
- 在 bar 分发**之前**比较 `bar.ts.date()` 与 `_last_processed_date`；
- 首根 bar 只记日期、**不触发** pre-open（day 0 screener 无数据）；
- 日期前进（严格 >）→ 触发一次 `on_pre_open(new_date)`；同日多 ticker 不重复触发；
- `on_pre_open`：`screener.top_k(k=active_cap, as_of=target_date)` →
  映射回本 universe 的 InstrumentId（screener 可能带外部 ticker，丢弃之）→
  **整体替换** active_set → `_enforce_subscription_cap()`。

### 11.3 Bar 路由（`universe_runner.py:341-359`）

**[MUST-MATCH]**
- screener.update(bar)：**每 bar、每 ticker、无条件**；
- SG 仅当 `instrument ∈ active_set` **或** `sg._position != 0`（U-D10：被轮出
  但仍持仓的标的继续被驱动直至离场）才喂 bar；
- on_bar 总序：to_my_bar → pre-open 检测 → 记录 last_close → 路由 →
  发布 state summary（`universe_runner.py:319-335`）。

### 11.4 订阅上限强制（U-D12，`universe_runner.py:252-283`）

**[MUST-MATCH]**
- 循环：`|active_set ∪ holdings| <= cap` 即停；
- 可逐出集合 = `holdings - active_set`（active_set 神圣不可逐出）；空则记
  warning 并退出（防死循环，`test_universe_runner.py:388-401`）;
- 排序键 `(trend_template_count(symbol), symbol字符串)` 升序，逐出第一个
 （分数最低；平分按 symbol 字典序）——每轮逐出一个再重算；
- 逐出 = `_submit_flat`：净仓位为 0 时仅防御性清 `sg._position = 0`；否则
  反向市价单平仓（`universe_runner.py:285-313`）。

### 11.5 warmup_ticker（`universe_runner.py:140-155`）

**[MUST-MATCH]** 对未知 instrument / 空 df 为 no-op；否则同时预热 SG
（`warmup_from_history`）与 screener（`screener.warmup(symbol, df)`）。

### 11.6 信号提交

**[MUST-MATCH]** 与单标的版相同的转译；未知 symbol 在触碰 order_factory 之前
静默返回（`universe_runner.py:370-371`、`test_universe_runner.py:411-429`）。

---

## 12. Look-ahead / 自指防护汇总

| # | 防护 | 位置 | 标签 |
|---|---|---|---|
| 1 | 入场 VCP 检测剔除当前 bar（防突破 bar 自定义 pivot） | `signal.py:217-221` | [MUST-MATCH] |
| 2 | 突破量能分母剔除当日 | `signal.py:259-269` | [MUST-MATCH] |
| 3 | 摆动点需后置 lookback 根确认（首尾各 lookback 根不出点） | `_swing.py:34` | [MUST-MATCH] |
| 4 | compute_regime 的 as_of 预过滤 | `context_refresher.py:62-74` | [MUST-MATCH] |
| 5 | warmup 数据必须严格早于运行窗口（调用方契约） | `universe_runner.py:148-149` | [MUST-MATCH] |
| 6 | pre-open 用前一日收盘后的 screener 状态选 top-K（bar 分发前触发） | `universe_runner.py:199-214` | [MUST-MATCH] |
| 7 | `evaluate_intent` 的 VCP 用**全量** klines（含当前 bar）——与入场路径不同；intent 仅供 UI，非交易判定 | `signal.py:463-470` | [MUST-MATCH]（刻意差异，勿"修复"） |

---

## 13. Warmup（200 根）语义

**[MUST-MATCH]**
- 交易分支硬门槛：`len(_klines) >= 200`（`signal.py:200`）；Stage 实际要求 220；
  VCP 要求 prior ≥ 30；突破量能要求 ≥ 61。综合：自然流式冷启动下，第 220 根 bar
  起才可能入场。
- `warmup_from_history` 可一次性注入 ≤ history_max_bars 根历史，绕过流式 warmup
  （不评估信号）；空/nil 安全（`test_signal.py:483-543`）。
- `evaluate_intent` 自身门槛是 50 根（低于交易门槛——UI 可早于交易给出 NO_SETUP
  之外的 grade 渐变）。

---

## 14. 验收测试锚点（Go 必须复现的端到端行为）

来自 `test_signal.py` 的 happy-path 夹具（261 根：200 根 50→115 线性 + 60 根
VCP 底 + 1 根突破，`test_signal.py:49-84`，`vcp_lookback=4`、risk_pct=1.0、
equity=100k、cap=10B）：

1. regime="bull" → 在**最后一根**（突破 bar）发出恰一组 LONG；grade ∈
   {A+, B}（catalyst=false 时为 B）；stop < entry close。
2. regime="bear" → 全程零信号。
3. market_cap=100M（< 500M）→ 零信号（TT 规则 8 拦截）。
4. blackout=true → 零信号（grade 拦截）。
5. 末根 close=117 < pivot(~118) → 零信号。
6. 末根量 800k（≈0.8x 均量）→ 零信号。
7. 入场后 close ≤ stop 的 bar → 恰一条 FLAT、target_qty=0、仓位清零。
8. close > stop → 无信号、持仓不变。
9. equity 2x/4x → 股数 ≈2x/4x（容差 `max(2, expected//100)`）。
10. state_dict 往返：position/stop/entry/len(klines) 全等。

---

## 15. 生产级要求（Go 侧补充，不改变语义）

原 Python 无显式并发/取消处理（单线程引擎回调）。Go 实现按验收线要求补齐，
全部属于"增强不减"，不改变信号语义：

- 每个 SG 实例的状态变更必须串行（per-symbol 单 goroutine 或互斥锁）；
- on_bar 路径禁止 panic：所有数组访问有界、除零有 guard（语义见各节）；
- context.Context 贯穿 runner 层（订阅循环、订单提交）；SG 纯计算层无需 ctx；
- 结构化日志字段至少含 symbol、ts、决策链短路点（被哪一关拒绝）——原版只在
  下单时打日志，建议增加 debug 级拒绝原因（[IMPROVE]：原行为静默拒绝，难排障；
  改进为可配置的 reject-reason 日志，默认关闭以免回测噪音）。

---

## 16. Open Questions

1. **`vcp_lookback` 的 JSON 描述错误**：`sepa.json:52` 写"Number of contraction
   segments to find"，实际是摆动检测半窗宽（`detect_vcp` 透传给
   `find_swing_points` 的 lookback，`signal.py:223-227`）。Go 按实际语义实现；
   hyperopt 范围 [3,10] 调的是窗宽。是否要修正文案（不影响行为）？
2. **突破量能基线 60 vs 50**：docstring（`signal.py:20`）与 JSON 描述
   （`sepa.json:46`）均称 50 日均量，代码硬编码 `base_lookback = 60`
   （`signal.py:264`）。规格按代码（60）为准；是否将 60 提升为可配置参数？
3. **`_breakout_volume_ok` 引用的 trade-minervini 出处**（`signal.py:260`）不在
   本仓库内，无法核对其 base 窗口究竟是 50 还是 60；按本仓库代码执行。
4. **regime="unknown" 允许 B 级入场**（§7.2 [IMPROVE]）：实盘中 Actor 首发布前
   的窗口极短，但回测若不接 RegimeActor 会全程 unknown 而照常入场。Go 默认值
   是否对齐（建议对齐 + 可选严格模式）？
5. **`nautilus_runner.py:138-140` 注释称 SG 默认 regime="neutral"**，代码实为
   `"unknown"`（`signal.py:150`）。规格以代码为准。
6. **evaluate_intent 的 BUY 态在自然流程中不可达**（pivot 仅在入场时写入、出场
   清零；空仓且 pivot>0 只会出现在持仓后……但持仓走 HOLD 分支）。唯一可达路径
   是外部直接写 `_pivot_price`（测试如此）。是否在 Go 中保留该死分支以维持
   字段级对照？（规格按保留处理。）
7. **`SEPASignalIntent.rs_rank` 恒为 nil**（`intent.py:56`）——RS ranking 未在
   本仓库 SEPA 路径实现。Go 保留字段、恒 null？
8. **state_dict 的 klines 索引列名脆弱性**（§8.12 [IMPROVE]）：是否采纳兼容
   读取（"index"/"ts"）？
9. **screener（`universe/sepa_screener.py`）的 top_k / trend_template_count /
   warmup 语义**本规格未展开（属 universe/screener 模块，应有独立 spec）。
   Universe runner 对其的依赖契约：`update(bar)`、`top_k(k, as_of)`（返回带
   `instrument_id` 的候选）、`trend_template_count(symbol) -> int`、
   `warmup(symbol, df)`、`tracked_count()`、`bars_seen(symbol)`。需要单独提取。
10. **`Signal.confidence` 恒 1.0**，无任何消费方差异化使用——Go 保留字段即可？
11. **timezone 配置项无任何行为**（仅声明 + 序列化 + 测试断言值）。是否在 Go 中
    用于日界判定（当前 universe runner 日界用 UTC date，`universe_runner.py:207`
    ——对 21:00 UTC 收盘 bar 恰好安全，但若数据源时间戳跨 UTC 午夜会错界）？
    按原样（UTC date）实现并标注风险。

---

## 17. 标签统计

- [MUST-MATCH]：52 处独立标注行为（不含文首约定与本节）
- [IMPROVE]：8 处（§4.2 Stage 兜底 NaN、§6.2 bar 计数命名、§6.2 round-half-even、
  §7.2 unknown regime、§8.7 重复时间戳、§8.8 止损谓词不一致、§8.12 klines 索引
  列名、§15 reject-reason 日志）
