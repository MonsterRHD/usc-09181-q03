# 跨境融资契约观察站

把海外子公司银团贷款合同中的**指标、观察周期、宽限期、币种口径、责任人**拆成可计算约束；接收银行报表与人工佐证后逐期生成状态，负责通知、提款冻结与复核留痕。

## 要解决的问题

- 法务、财务、业务分别维护偿债、资金用途、担保条款，时区与口径不一，上一季度报告被错过通知；
- 报表迟到、补发、修订、跨时区截止、同一文件重复上传，必须保持版本关系；
- 接近阈值只提醒一次；确认违约后冻结**相关**提款并要求指定角色复核；
- 历史计算不能被新口径覆盖；管理员改条款必须留下生效时间与影响范围；隔日重启仍可追踪未决复核和下一次截止。

## 设计要点

| 关注点 | 做法 |
|---|---|
| 时间 | 内部统一 UTC 毫秒；截止按**条款时区的挂钟时间**解释（Intl 往返换算，含夏令时）；宽限按自然日 |
| 币种 | 金额以最小记账单位 **BigInt 整数**存储（JPY 0 位小数等）；汇率 1e8 定点，乘法换算 half-up，记录舍入残差 |
| 汇率口径 | 评估锚定期末（`fxPolicy=period_end`），只取锚点前最后一条不可变汇率；新汇率不回溯历史计算 |
| 条款版本 | 每次签发/修订追加版本行；修订必须给 `effectiveFromWall` + `impactScope`（`future_periods` / `current_and_future`） |
| 报表版本链 | `original → backfill → revision`，以 `rootId` 串联；sha256 相同的重复上传只登记 `duplicate`，不进计算 |
| 每期状态 | `indeterminate`（缺数据，不按 0 误判）/ `compliant` / `near_threshold` / `breach`；评估带输入指纹，输入未变则复用，不产生新版本或重复告警 |
| 通知 | 宽限提醒、超宽限迟到告警、接近阈值提醒、违约告警、复核任务；按 `类型:条款:期` 去重；违约确认后自动撤回早先的接近提醒 |
| 冻结与复核 | 同批次可同时触发多条约束 → 一个复核任务、按条款类别冻结相关提款；复核结论 `confirmed`（维持冻结）或 `false_positive`（撤回告警+解冻，评估以"误报撤回"新版本留痕） |
| 修订闭环 | 修订报表使未确认违约恢复时，自动撤回告警/解冻并按剩余违约收口复核；**已人工确认的违约不自动解除** |
| 持久化 | 每次变更原子写盘（tmp+rename）；BigInt 以 `$bigint` 包装；全量审计日志（人、时点、动作、影响） |

## 运行

零依赖（仅需 Node ≥ 20）：

```bash
npm test          # 6 个测试文件、10 个用例，含四类验收场景
npm start         # HTTP 服务，默认 :8080，数据文件 data/state.json（DATA_FILE 可覆盖）
```

HTTP 接口（身份由 `x-user-id` 头给出，角色以登记为准）：

```
POST /admin/setup            初始化授信框架（报告币种、基准时区、复核角色）
POST /parties                登记参与方（legal/finance/business/facility_admin/covenant_reviewer）
POST /metrics                定义指标口径（指标键集合）
POST /fx                     录入汇率（不可变追加，asOfWall + 时区）
POST /covenants              签发条款
POST /covenants/:id/amend    管理员修订（生效时间 + 影响范围）
POST /periods/open           开立观察期跟踪器
POST /tick                   截止巡检（宽限/迟到通知）
POST /reports                银行报表（original/backfill/revision，sha256 去重）
POST /evidence               人工佐证（可更正/撤回）
POST /periods/:end/assess    整批评估（可 only 指定条款子集）
POST /drawdowns              提款申请（相关冻结拦截）
POST /reviews/:id/resolve    指定角色复核（confirmed / false_positive）
GET  /assessments/:id/explain 审计解释（口径、汇率版本、数据来源、条款版本）
GET  /pending                未决复核 + 各期截止（重启后追踪用）
GET  /status
```

## 验收场景（见 `test/`）

1. `01` **跨月指标**：英镑子公司与美元总部报表分月到达，期末汇率换算合并为季度状态；接近阈值反复评估只提醒一次；事后新汇率不覆盖历史。
2. `02` **补发报表**：伦敦/新加坡两地挂钟截止的宽限与迟到通知；迟到补发进入版本链；同文件重复上传只记 duplicate；修订产生新评估版本而历史保留。
3. `03` **两条约束同时触发 + 冻结 + 重启 + 修订**：同批次双违约 → 一个复核、相关类别提款冻结（无关类别放行）；隔日重开进程未决复核与下一次截止仍在、冻结仍生效；管理员修订自新观察期生效、不追溯当期。
4. `04` **撤回误报**：银行科目误记导致的违约经复核官撤回 → 告警 withdrawn、冻结 lifted、提款恢复；误报终态不随重跑复活；正确报表补发后重新合规，违约→撤回→修订→合规全程审计可解释。

## 目录

```
src/time.js      时区挂钟/截止/宽限/观察期日历
src/money.js     BigInt 金额、定点汇率、换算与舍入残差
src/store.js     原子持久化、BigInt JSON、审计日志
src/station.js   领域主体：条款版本、报表/佐证版本链、评估引擎、通知、冻结、复核
src/server.js    零依赖 HTTP 入口
test/            单元测试 + 四个验收场景与标准场景装配
```
