// Package domain 定义融资契约观察站的领域模型：
// 把贷款合同拆成可计算约束（指标、观察周期、宽限期、币种口径、责任人），
// 并定义期次、报表版本、每期状态、通知、冻结、复核与审计记录。
package domain

import (
	"fmt"
	"time"
	_ "time/tzdata" // 内嵌时区数据库，保证容器内跨时区截止时刻可计算
)

// Domain 条款域：法务维护偿债、财务维护担保、业务维护资金用途。
type Domain string

const (
	DebtService   Domain = "DEBT_SERVICE"    // 偿债
	Security      Domain = "SECURITY"        // 担保
	UseOfProceeds Domain = "USE_OF_PROCEEDS" // 资金用途
)

func ValidDomain(d Domain) bool {
	return d == DebtService || d == Security || d == UseOfProceeds
}

// Frequency 观察周期。
type Frequency string

const (
	Monthly   Frequency = "MONTHLY"
	Quarterly Frequency = "QUARTERLY"
)

func ValidFrequency(f Frequency) bool { return f == Monthly || f == Quarterly }

// Status 每期状态。历史状态只追加、不覆盖。
type Status string

const (
	StatusPending                Status = "PENDING"                  // 尚未到截止时刻
	StatusOverdue                Status = "OVERDUE"                  // 已过截止、宽限期内未收到合格报表
	StatusCompliant              Status = "COMPLIANT"                // 达标
	StatusNearThreshold          Status = "NEAR_THRESHOLD"           // 接近阈值（只提醒一次）
	StatusBreachInGrace          Status = "BREACH_IN_GRACE"          // 突破阈值，宽限期内可补救
	StatusConfirmedDefault       Status = "CONFIRMED_DEFAULT"        // 确认违约：冻结相关提款并待指定角色复核
	StatusRevisionPendingReview  Status = "REVISION_PENDING_REVIEW"  // 违约后收到修订/补发，需复核撤回
	StatusFalseAlarmWithdrawn    Status = "FALSE_ALARM_WITHDRAWN"    // 复核认定误报并撤回
	StatusCured                  Status = "CURED"                   // 复核认可补救
	StatusDefaultAcknowledged    Status = "DEFAULT_ACKNOWLEDGED"     // 复核确认违约成立
)

// 通知种类。去重键为 种类|约束|期次，保证“接近阈值只提醒一次”。
const (
	KindOverdue        = "OVERDUE_REPORT"        // 报表逾期
	KindNearWarning    = "NEAR_WARNING"          // 近阈值预警
	KindBreachGrace    = "BREACH_IN_GRACE"       // 宽限期内违约
	KindDefault        = "DEFAULT_CONFIRMED"     // 违约确认
	KindBackfill       = "BACKFILL_RECEIVED"     // 补发/修订使状态恢复
	KindRevisionReview = "REVISION_UNDER_REVIEW" // 违约后修订待复核
	KindWithdrawn      = "FALSE_ALARM_WITHDRAWN" // 误报撤回
	KindCured          = "BREACH_CURED"          // 违约补救成立
)

// 审计动作。
const (
	ActionContractCreated = "CONTRACT_CREATED"
	ActionDrawdownAdded   = "DRAWDOWN_REGISTERED"
	ActionCovenantCreated = "COVENANT_CREATED"
	ActionCovenantAmended = "COVENANT_AMENDED"
	ActionFilingUploaded  = "FILING_UPLOADED"
	ActionFilingDuplicate = "FILING_DUPLICATE_IGNORED"
	ActionEvidenceAdded   = "EVIDENCE_UPLOADED"
	ActionStatusGenerated = "PERIOD_STATUS_GENERATED"
	ActionNotified        = "NOTIFICATION_RAISED"
	ActionFreezeApplied   = "DRAWDOWN_FREEZE_APPLIED"
	ActionFreezeReleased  = "DRAWDOWN_FREEZE_RELEASED"
	ActionReviewOpened    = "REVIEW_OPENED"
	ActionReviewDecided   = "REVIEW_DECIDED"
)

// MetricSpec 指标的计算口径。
type MetricSpec struct {
	Code           string  `json:"code"`            // 例如 DSCR、COLLATERAL_COVERAGE、USE_OF_PROCEEDS_RATIO
	Name           string  `json:"name"`            // 中文名
	HigherIsBetter bool    `json:"higher_is_better"` // 指标方向：true 越大越安全（如偿债覆盖率）
	Threshold      float64 `json:"threshold"`       // 违约阈值
	WarnDelta      float64 `json:"warn_delta"`      // 近阈值带宽；0 表示不设近阈值提醒
	Unit           string  `json:"unit,omitempty"`
}

// Basis 币种与换算口径。口径本身带版本，历史计算快照所用版本。
type Basis struct {
	Currency     string `json:"currency"`      // 报送币种，如 USD
	FXConvention string `json:"fx_convention"` // 换算约定，如“期末中国外汇交易中心中间价”
	Version      int    `json:"version"`       // 口径版本，从 1 起
}

// CovenantVersion 条款版本。管理员修订即追加新版本，绝不就地改写。
type CovenantVersion struct {
	VersionID     string     `json:"version_id"`
	Sequence      int        `json:"sequence"` // 从 1 起
	Metric        MetricSpec `json:"metric"`
	Basis         Basis      `json:"basis"`
	GraceDays     int        `json:"grace_days"`
	EffectiveFrom time.Time  `json:"effective_from"` // 生效时间（零值表示首版，始终有效）
	ImpactScope   string     `json:"impact_scope"`   // 影响范围（必填）
	Reason        string     `json:"reason"`
	ChangedBy     string     `json:"changed_by"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Covenant 可计算约束。
type Covenant struct {
	ID                  string            `json:"id"`
	ContractID          string            `json:"contract_id"`
	Domain              Domain            `json:"domain"`
	Name                string            `json:"name"`
	OwnerRole           string            `json:"owner_role"`    // 责任角色（法务/财务/业务负责人）
	ReviewerRole        string            `json:"reviewer_role"` // 违约确认后的指定复核角色
	Frequency           Frequency         `json:"frequency"`
	ReportingOffsetDays int               `json:"reporting_offset_days"` // 报表截止 = 期末 + N 天
	DeadlineHour        int               `json:"deadline_hour"`
	DeadlineMinute      int               `json:"deadline_minute"`
	DeadlineTZ          string            `json:"deadline_tz"` // 报表截止时区，如 Asia/Shanghai
	Versions            []CovenantVersion `json:"versions"`
	CreatedAt           time.Time         `json:"created_at"`
}

// VersionEffective 选取在给定时点（按期末归属）生效的条款版本：
// 历史期次永远使用其期末之前生效的版本，新口径不会追溯覆盖历史计算。
func (c Covenant) VersionEffective(at time.Time) CovenantVersion {
	v := c.Versions[0]
	for _, cand := range c.Versions {
		if !cand.EffectiveFrom.IsZero() && !cand.EffectiveFrom.After(at) && cand.Sequence >= v.Sequence {
			v = cand
		}
	}
	return v
}

// Period 观察期次。End 为期末日历日 00:00（UTC），用于跨月/跨季推算。
type Period struct {
	Key   string    `json:"key"` // 月：2026M03；季：2026Q1
	Start time.Time `json:"start"`
	End   time.Time `json:"end"` // 该期最后一个日历日
}

// PeriodFor 返回 t 所在的观察期次。
func PeriodFor(f Frequency, t time.Time) (Period, error) {
	y, m, _ := t.Date()
	switch f {
	case Monthly:
		start := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		end := time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC)
		return Period{Key: fmt.Sprintf("%04dM%02d", y, int(m)), Start: start, End: end}, nil
	case Quarterly:
		q := (int(m)-1)/3 + 1
		qm := time.Month((q-1)*3 + 1)
		start := time.Date(y, qm, 1, 0, 0, 0, 0, time.UTC)
		end := time.Date(y, qm+3, 0, 0, 0, 0, 0, time.UTC)
		return Period{Key: fmt.Sprintf("%04dQ%d", y, q), Start: start, End: end}, nil
	default:
		return Period{}, fmt.Errorf("未知观察周期 %q", f)
	}
}

// PeriodFromKey 由期次键解析期次（如 2026M03、2026Q1）。
func PeriodFromKey(f Frequency, key string) (Period, error) {
	var y, n int
	var probe time.Time
	switch f {
	case Monthly:
		if _, err := fmt.Sscanf(key, "%4dM%2d", &y, &n); err != nil || n < 1 || n > 12 {
			return Period{}, fmt.Errorf("非法月度期次键 %q", key)
		}
		probe = time.Date(y, time.Month(n), 15, 0, 0, 0, 0, time.UTC)
	case Quarterly:
		if _, err := fmt.Sscanf(key, "%4dQ%1d", &y, &n); err != nil || n < 1 || n > 4 {
			return Period{}, fmt.Errorf("非法季度期次键 %q", key)
		}
		probe = time.Date(y, time.Month((n-1)*3+2), 15, 0, 0, 0, 0, time.UTC)
	default:
		return Period{}, fmt.Errorf("未知观察周期 %q", f)
	}
	return PeriodFor(f, probe)
}

// NextPeriod 返回下一个期次（用于推算下一截止时刻）。
func NextPeriod(f Frequency, p Period) (Period, error) {
	switch f {
	case Monthly:
		return PeriodFor(f, p.End.AddDate(0, 0, 2))
	case Quarterly:
		return PeriodFor(f, p.End.AddDate(0, 0, 5))
	default:
		return Period{}, fmt.Errorf("未知观察周期 %q", f)
	}
}

func (c Covenant) location() *time.Location {
	loc, err := time.LoadLocation(c.DeadlineTZ)
	if err != nil {
		return time.UTC
	}
	return loc
}

// Deadline 报表接收截止时刻：期末日历日 + N 天，按合同时区的当日 HH:MM 换算为绝对时刻。
// 例：2026M03、N=15、17:00 Asia/Shanghai → 2026-04-15 17:00+08:00。
func (c Covenant) Deadline(p Period) time.Time {
	due := p.End.AddDate(0, 0, c.ReportingOffsetDays)
	y, m, d := due.Date()
	return time.Date(y, m, d, c.DeadlineHour, c.DeadlineMinute, 0, 0, c.location())
}

// GraceEnd 宽限期结束时刻（截止时刻 + GraceDays 个当地日历日）。
func (c Covenant) GraceEnd(p Period) time.Time {
	due := p.End.AddDate(0, 0, c.ReportingOffsetDays+c.GraceDays)
	y, m, d := due.Date()
	return time.Date(y, m, d, c.DeadlineHour, c.DeadlineMinute, 0, 0, c.location())
}

// Classify 按指标方向与近阈值带宽判定数值落点。
func Classify(m MetricSpec, v float64) Status {
	if m.HigherIsBetter {
		if v < m.Threshold {
			return StatusBreachInGrace
		}
		if m.WarnDelta > 0 && v < m.Threshold+m.WarnDelta {
			return StatusNearThreshold
		}
		return StatusCompliant
	}
	if v > m.Threshold {
		return StatusBreachInGrace
	}
	if m.WarnDelta > 0 && v > m.Threshold-m.WarnDelta {
		return StatusNearThreshold
	}
	return StatusCompliant
}

// Contract 银团贷款合同。
type Contract struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Borrower     string    `json:"borrower"`
	BaseCurrency string    `json:"base_currency"`
	CreatedAt    time.Time `json:"created_at"`
}

// 提款状态。
const (
	DrawdownAvailable = "AVAILABLE" // 可提款
	DrawdownRequested = "REQUESTED" // 已申请待发放
	DrawdownFrozen    = "FROZEN"    // 冻结
	DrawdownDisbursed = "DISBURSED" // 已发放
)

// Drawdown 提款（按条款域关联，违约时只冻结“相关提款”）。
type Drawdown struct {
	ID         string  `json:"id"`
	ContractID string  `json:"contract_id"`
	Domain     Domain  `json:"domain"`
	Amount     float64 `json:"amount"`
	Currency   string  `json:"currency"`
	Purpose    string  `json:"purpose,omitempty"`
	Status     string  `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

// 报表版本种类。
const (
	FilingOriginal = "ORIGINAL" // 按期首传
	FilingBackfill = "BACKFILL" // 过截止后的补发
	FilingRevision = "REVISION" // 对已上传报表的修订
)

// FilingVersion 银行报表的一个版本。同一逻辑报表（合同+期次）形成版本链。
type FilingVersion struct {
	ID          string             `json:"id"`
	Sequence    int                `json:"sequence"`
	Kind        string             `json:"kind"`
	Supersedes  string             `json:"supersedes,omitempty"` // 被修订的上一版本 ID
	FileName    string             `json:"file_name"`
	ContentHash string             `json:"content_hash"` // 同一文件重复上传据此识别
	Values      map[string]float64 `json:"values"`       // 指标代码 → 数值（一份报表可覆盖多条约束）
	SubmittedAt time.Time          `json:"submitted_at"` // 银行报表载明的提交时刻（绝对时间）
	SourceTZ    string             `json:"source_tz,omitempty"`
	Submitter   string             `json:"submitter,omitempty"`
	Note        string             `json:"note,omitempty"`
	UploadedAt  time.Time          `json:"uploaded_at"` // 观察站签收时刻
}

// FilingChain 同一合同同一期次的报表版本链。
type FilingChain struct {
	ContractID string          `json:"contract_id"`
	PeriodKey  string          `json:"period_key"`
	Versions   []FilingVersion `json:"versions"`
}

func (c *FilingChain) Latest() *FilingVersion {
	if c == nil || len(c.Versions) == 0 {
		return nil
	}
	return &c.Versions[len(c.Versions)-1]
}

// Evidence 人工佐证材料。
type Evidence struct {
	ID          string    `json:"id"`
	ContractID  string    `json:"contract_id"`
	PeriodKey   string    `json:"period_key"`
	FileName    string    `json:"file_name"`
	ContentHash string    `json:"content_hash"`
	Note        string    `json:"note,omitempty"`
	UploadedBy  string    `json:"uploaded_by"`
	UploadedAt  time.Time `json:"uploaded_at"`
}

// StatusRecord 某条约束某期次的一次状态计算结果（不可变，只追加）。
type StatusRecord struct {
	ID                string    `json:"id"`
	Sequence          int       `json:"sequence"`
	CovenantID        string    `json:"covenant_id"`
	ContractID        string    `json:"contract_id"`
	PeriodKey         string    `json:"period_key"`
	Status            Status    `json:"status"`
	AsOf              time.Time `json:"as_of"`
	MetricCode        string    `json:"metric_code"`
	Value             float64   `json:"value,omitempty"`
	HasValue          bool      `json:"has_value"`
	Threshold         float64   `json:"threshold"`
	WarnDelta         float64   `json:"warn_delta"`
	HigherIsBetter    bool      `json:"higher_is_better"`
	CovenantVersionID string    `json:"covenant_version_id"` // 计算所用条款版本
	Basis             Basis     `json:"basis"`                // 计算所用币种口径快照
	FilingVersionID   string    `json:"filing_version_id,omitempty"`
	FilingKind        string    `json:"filing_kind,omitempty"`
	FilingLate        bool      `json:"filing_late"` // 相对该约束自己的截止时刻
	Deadline          time.Time `json:"deadline"`
	GraceEnd          time.Time `json:"grace_end"`
	EvidenceCount     int       `json:"evidence_count"`
	ReviewID          string    `json:"review_id,omitempty"`
	Explanation       string    `json:"explanation"` // 面向审计的人类可读解释
	CreatedAt         time.Time `json:"created_at"`
}

// Notification 通知。同一去重键只产生一条。
type Notification struct {
	ID             string     `json:"id"`
	DedupKey       string     `json:"dedup_key"`
	ContractID     string     `json:"contract_id"`
	CovenantID     string     `json:"covenant_id"`
	PeriodKey      string     `json:"period_key"`
	Kind           string     `json:"kind"`
	Severity       string     `json:"severity"` // INFO/WARN/HIGH/CRITICAL
	Message        string     `json:"message"`
	StatusRecordID string     `json:"status_record_id,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	WithdrawnAt    *time.Time `json:"withdrawn_at,omitempty"` // 误报撤回时标记原通知
	WithdrawnReason string    `json:"withdrawn_reason,omitempty"`
}

// FreezeOrder 提款冻结令；撤回误报/认可补救时释放。
type FreezeOrder struct {
	ID             string            `json:"id"`
	ContractID     string            `json:"contract_id"`
	CovenantID     string            `json:"covenant_id"`
	PeriodKey      string            `json:"period_key"`
	StatusRecordID string            `json:"status_record_id"`
	ReviewID       string            `json:"review_id"`
	Reason         string            `json:"reason"`
	DrawdownIDs    []string          `json:"drawdown_ids"`
	PreviousStatus map[string]string `json:"previous_status"` // 释放时恢复原状态
	CreatedAt      time.Time         `json:"created_at"`
	ReleasedAt     *time.Time        `json:"released_at,omitempty"`
	ReleasedBy     string            `json:"released_by,omitempty"`
	ReleaseReason  string            `json:"release_reason,omitempty"`
}

// 复核任务状态。
const (
	ReviewPending   = "PENDING"
	ReviewWithdrawn = "WITHDRAWN" // 认定误报
	ReviewConfirmed = "CONFIRMED" // 违约成立
	ReviewCured     = "CURED"      // 补救成立
)

// 复核决定。
const (
	DecisionWithdraw = "WITHDRAW_FALSE_ALARM"
	DecisionConfirm  = "CONFIRM_DEFAULT"
	DecisionCure     = "ACCEPT_CURE"
)

// ReviewTask 指定角色复核任务，必须持久化、跨重启可追踪。
type ReviewTask struct {
	ID             string     `json:"id"`
	ContractID     string     `json:"contract_id"`
	CovenantID     string     `json:"covenant_id"`
	PeriodKey      string     `json:"period_key"`
	StatusRecordID string     `json:"status_record_id"`
	RequiredRole   string     `json:"required_role"`
	OwnerRole      string     `json:"owner_role"`
	Status         string     `json:"status"`
	OpenedAt       time.Time  `json:"opened_at"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	Resolver       string     `json:"resolver,omitempty"`
	Decision       string     `json:"decision,omitempty"`
	Note           string     `json:"note,omitempty"`
}

// AuditEntry 审计记录：任何关键动作只追加，含互相链接的 ID 以保证解释一致。
type AuditEntry struct {
	ID         string            `json:"id"`
	Time       time.Time         `json:"time"`
	ActorID    string            `json:"actor_id,omitempty"`
	ActorRole  string            `json:"actor_role,omitempty"`
	Action     string            `json:"action"`
	TargetType string            `json:"target_type"`
	TargetID   string            `json:"target_id"`
	Summary    string            `json:"summary"`
	Links      map[string]string `json:"links,omitempty"`
}

// 键工具：报表与佐证按 合同+期次 归集，状态历史按 约束+期次 归集。
func FilingKey(contractID, periodKey string) string  { return contractID + "|" + periodKey }
func StatusKey(covenantID, periodKey string) string  { return covenantID + "|" + periodKey }
