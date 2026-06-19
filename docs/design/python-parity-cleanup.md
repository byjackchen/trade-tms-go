# Design: 彻底清理 Python / parity 残留

状态: **已定方向 / 待实施**（本文档仅计划，未改代码） · 作者: byjackchen · 日期: 2026-06-17

## 0. 决策背景

旧 Python repo 已**彻底废弃**。多次设计因"与 Python parity"约束承担了不必要负担
（最近一例见 [intent-to-signal-rename.md](intent-to-signal-rename.md)）。
**从现在起一切以本库功能为核心迭代**，不再以 Python 为参照系。本计划清除 repo 内
所有把 Python/parity 当作设计约束或真理来源的注释、说明、文档，**以及只为 parity
存在的代码**（第二层）。

### 决策原则（2026-06-17，本轮锁定）
1. **彻底，不留尾巴**：契约改名、测试改名、文档删除一律执行到底，不保留"别名/
   归档/暂留标识符"之类的过渡尾巴。
2. **一次性迁移，不考虑向后兼容**：任何 DB/标识符/JSON/CLI 变更做**一次定**，
   无兼容窗口、无双写、无旧值别名。
3. 清理分**两层**：L1 = 注释/文档 prose（纯文本）；L2 = **只为 parity 存在的代码**
   （死字段、数值规避、兼容档）——L2 才是真正的"尾巴"，必须一并清。

## 1. 爆炸半径（已排除 `.claude/worktrees/`）

关键词：`parity` / `python` / `MUST-MATCH` / `nautilus` / `dataclass` / `CPython`
/ `.py` / `src/strategies` / `mirror` / `reference`。

| 类别 | 规模 | 处理 |
|---|---|---|
| Go **注释/文档 prose** | ~2921 行命中 / ~443 文件 | **删改**（主目标，C1） |
| Go **代码/字符串** | ~282 行命中 | **逐项语义判断**（C2/C3，含契约面） |
| 整篇 parity 文档 | `docs/parity.md`(233)、`docs/concept-alignment.md`(253) | **归档/删除**（C4） |
| spec/其他 md | ~45 个 md 含命中（含 docs/spec/*） | **改 prose**（C4） |
| parity 测试 + fixture | 4 个 `*parity*_test.go` + 9 个 `*_parit*.json` | **保留行为，去 parity 框架语义**（C3） |
| UI ts/tsx | 16+16 文件含命中 | 多为无关词，**逐查**（C4） |

> 注释占绝大多数（~91%），是纯文本清理；真正需要决策的是 §3 的契约面。

## 2. 清理原则

1. **删除"以 Python 为真理来源"的表述**：`[MUST-MATCH]`、`mirroring Python's X`、
   `the Python reference`、`frozen dataclass`、`src/strategies/...py:NN`、
   `CPython never fuses` 之类。
2. **保留仍成立的技术理由，剥离 parity 归因**：若注释解释的是*为什么代码这样写*
   且理由独立于 Python 仍成立（如同 bar 收盘成交语义、跨平台数值确定性），
   **保留技术理由、删掉"为了和 Python 对齐"的归因**，重述为"本库的定义/不变式"。
   ——但若某段代码**只**为 parity 存在（理由随 Python 一起消失），归 L2 删除。
3. **测试去 parity 叙事**：golden/regression 测试保留数值基线（钉住本库行为），
   测试名/注释从"对齐 Python"重述为"本库 golden 回归"。
4. **契约面也彻底改、一次定**（原则①②）：用户可见标识符/CLI/JSON 值若带旧世界
   命名（如 `nautilus-compat`），**本轮直接改到位、无别名窗口**（见 §3）。

## 3. L2 — 只为 parity 存在的代码（真正的尾巴，逐项处理）

> 这一层 agent 初稿低估了。它们不是注释，是代码；按原则①必须清。

### N1 — `nautilus-compat` fill profile
- 出现：CLI `--fill-profile nautilus-compat`（**且为默认值** `cmd/tms/backtest.go:79`）、
  `engine.ProfileNautilusCompat`、`exec.NautilusCompatModel`、API 校验串、
  hyperopt objective、JSON 字段。
- 现状：命名自 Nautilus，但**行为本库自有**（同 bar 收盘成交、零滑点零佣金），
  对快速确定性回测仍有用 → **保留行为，但彻底改名，无别名**。
- 处理：标识符/CLI 值/JSON 全部改为 `close-fill`（建议名），一次定，旧值不保留。
- **附带决策**：默认 fill profile 是否从 `nautilus-compat` 改为 `realistic`
  （production-faithful）？建议改 —— 见 §6.Q1。

### N2 — 死字段（永远 nil/0 的冻结保留字段）→ 直接删除
- `domain.SignalIntent` 家族：`RSRank`(reserved never set)、`HalfLifeDays`(always 0.0)、
  `ATRAtOpen`(always nil)；`orb/types.go` 对应字段。
- 仅为镜像 Python dataclass 形状而留，无任何 producer。→ **删除字段 + 相关注释**。
  （与 [intent-to-signal-rename.md](intent-to-signal-rename.md) 的 Signal 家族改名合并做。）

### N3 — parity-only 数值规避（**已定 Q2：保留机制，仅改归因**）
- `metrics/metrics.go`：math/big 精确算术，注释称"reproduce CPython mean/pstdev
  BIT-FOR-BIT"。
- `domain/intent.go StrengthFromRank`：显式 `float64()` 抑制 FMA，注释称"CPython never fuses"。
- **裁决（已定）**：团队**需要跨平台（arm64 mac ↔ x86 CI/服务器）bit 级可复现**的
  回测/hyperopt 结果。这些机制的独立价值正是跨平台确定性，与 Python 无关。
  → **保留机制不动**，仅把注释从"reproduce CPython / CPython never fuses"
  **改述为"本库跨平台数值确定性要求（arm64 vs x86 FMA 差异）"**。
  数值 golden **不变，无需 re-baseline**。

### N4 — 帮助/错误文本里的 Python 归因
- `cmd/tms/import.go` `Long:` "Reads the Python reference repo's parquet layout"
  → 改述为"本库支持的 parquet 输入格式"。
- `data/sharadar/client.go` "Python SDK's ~1M row cap" → 实为 Nasdaq Data Link
  **外部 API** 限制，保留事实、改为"the data provider's ~1M row cap"。

## 4. L1 — 注释/文档清理范围（C1–C3）

- **C1 Go 注释 prose**（~2921 行 / ~443 文件）：按 §2.1/§2.2 删改。重灾区
  doc.go / 各 strategy / engine / exec / riskgate / metrics。
- **C2 parity 测试 + fixture**（彻底改，原则①）：
  - 文件改名：`*parity*_test.go → *golden*_test.go`，`*_parity.json → *_golden.json`。
  - 测试函数/注释：`TestXxxParity → TestXxxGolden`，去"对齐 Python"叙事。
  - **数值断言与 testdata 保持不变**（行为基线不动；但 N1/N3 若改了默认/数值，
    需同步 re-baseline，见 §5.PR-4）。
- **C3 文档**（彻底删，原则①，不归档）：
  - `docs/parity.md`、`docs/concept-alignment.md`：**直接删除**（已无参照对象，
    git 历史留存即可，不进 `docs/archive/`）。
  - `docs/spec/*.md`、`docs/reference/architecture.md` 等：删 `[MUST-MATCH]`/Python 引用段，
    spec 重述为"本库定义"。

## 5. 实施阶段（PR 切分，全部承诺执行）

1. **PR-1 文档层（C3）**：删 parity.md、concept-alignment.md；清 docs/spec prose。零代码风险。
2. **PR-2 Go 注释 prose（C1 + N4）**：批量删改注释与归因文本。`go build ./...` 验证。逐包提交。
3. **PR-3 L2 死字段 + 测试改名（N2 + C2）**：删死字段，parity 测试/fixture 改名。
   `go test ./...` 全绿（N2 改字段可能动到序列化测试，同步修）。
4. **PR-4 契约一次定（N1 + 默认值）**：`nautilus-compat → close-fill` 一次改到位
   （CLI/标识符/JSON，无别名）；默认 profile 改为 `realistic`（Q1）。破坏性但
   **一次性完成**（原则②，无兼容窗口）。
   - ⚠️ 默认从 close-fill 改 realistic 会改变"未显式传 profile"的回测结果；
     凡依赖旧默认的 golden 需 re-baseline（**仅此项**；N3 数值机制保留故不涉及）。
5. **收尾守卫**：加 CI grep gate（Q3），全仓 `python/parity/MUST-MATCH/
   nautilus/CPython/dataclass` 命中归零（除 N3 技术注释允许清单）。

## 6. 决策（已定，2026-06-17）

- **Q1 ✅ 改**：默认 fill profile `nautilus-compat`(→`close-fill`) 改为 `realistic`
  （production-faithful）。显式传 `close-fill` 的调用方不受影响。
- **Q2 ✅ 保留**：需要跨平台 bit 级可复现 → big.Rat 精确算术 + FMA 抑制**机制保留**，
  仅改注释归因为"跨平台数值确定性"，数值 golden 不动（见 N3）。
- **Q3 ✅ 加**：CI grep 守卫，防 parity 词汇回流（类似 depguard）。

## 7. 相关
- 术语归位计划：[intent-to-signal-rename.md](intent-to-signal-rename.md)
  （其 §4 也依赖"parity 不再是约束"这一前提；两计划方向一致）。
