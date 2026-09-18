// Package service 实现观察站的核心引擎：
// 接收银行报表与人工佐证、逐约束逐期次生成状态、去重通知、
// 违约冻结相关提款、指定角色复核，并写入不可变审计链。
package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"example.com/09181/q003/internal/domain"
)

// Store 是引擎依赖的持久化接口（JSON 文件或内存实现均可）。
// 所有读方法必须返回深拷贝，避免调用方绕过引擎直接改状态。
type Store interface {
	Load() (*Snapshot, error)
	Save(*Snapshot) error
}

// Snapshot 全量状态。历史集合（状态/通知/审计）只追加。
type Snapshot struct {
	Counter     int64                          `json:"counter"`
	Contracts   map[string]*domain.Contract    `json:"contracts"`
	Covenants   map[string]*domain.Covenant    `json:"covenants"`
	Drawdowns   map[string]*domain.Drawdown    `json:"drawdowns"`
	Filings     map[string]*domain.FilingChain `json:"filings"` // key: 合同|期次
	Evidence    []*domain.Evidence             `json:"evidence"`
	Statuses    []*domain.StatusRecord         `json:"statuses"` // 不可变
	Notifs      []*domain.Notification         `json:"notifications"`
	Freezes     []*domain.FreezeOrder          `json:"freezes"`
	Reviews     []*domain.ReviewTask           `json:"reviews"`
	Audit       []*domain.AuditEntry           `json:"audit"`
}

func NewSnapshot() *Snapshot {
	return &Snapshot{
		Contracts: map[string]*domain.Contract{},
		Covenants: map[string]*domain.Covenant{},
		Drawdowns: map[string]*domain.Drawdown{},
		Filings:   map[string]*domain.FilingChain{},
	}
}

// Service 引擎。now 可注入以便测试跨月/跨时区/重启场景。
type Service struct {
	store Store
	mu    sync.Mutex
	now   func() time.Time
}

func New(store Store) *Service {
	return &Service{store: store, now: time.Now}
}

// WithClock 注入时钟（测试用）。
func (s *Service) WithClock(now func() time.Time) *Service { s.now = now; return s }

// begin 串行化读—改—写事务：write=true 时在提交时落盘。
func (s *Service) begin(write bool) (*Snapshot, func(), error) {
	s.mu.Lock()
	snap, err := s.store.Load()
	if err != nil {
		s.mu.Unlock()
		return nil, func() {}, err
	}
	var once sync.Once
	commit := func() {
		once.Do(func() {
			if write {
				_ = s.store.Save(snap)
			}
			s.mu.Unlock()
		})
	}
	return snap, commit, nil
}

func (s *Service) nextID(snap *Snapshot, prefix string) string {
	snap.Counter++
	return fmt.Sprintf("%s-%06d", prefix, snap.Counter)
}

func (s *Service) audit(snap *Snapshot, actorID, actorRole, action, targetType, targetID, summary string, links map[string]string) {
	snap.Audit = append(snap.Audit, &domain.AuditEntry{
		ID:         s.nextID(snap, "AUD"),
		Time:       s.now().UTC(),
		ActorID:    actorID,
		ActorRole:  actorRole,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Summary:    summary,
		Links:      links,
	})
}

// ---------- 命令侧 ----------

// Actor 操作人（角色决定权限）。
type Actor struct {
	ID   string
	Role string
}

var adminRoles = map[string]bool{"ADMIN": true, "TREASURY_ADMIN": true, "资金委员会管理员": true}

// CreateContract 登记银团贷款合同。
func (s *Service) CreateContract(id, name, borrower, baseCurrency string, actor Actor) (*domain.Contract, error) {
	snap, commit, err := s.begin(true)
	if err != nil {
		return nil, err
	}
	defer commit()
	if id == "" {
		return nil, fmt.Errorf("合同 ID 不能为空")
	}
	if _, ok := snap.Contracts[id]; ok {
		return nil, fmt.Errorf("合同 %s 已存在", id)
	}
	c := &domain.Contract{ID: id, Name: name, Borrower: borrower, BaseCurrency: baseCurrency, CreatedAt: s.now().UTC()}
	snap.Contracts[id] = c
	s.audit(snap, actor.ID, actor.Role, domain.ActionContractCreated, "CONTRACT", id,
		fmt.Sprintf("登记合同 %s（借款人 %s，币种 %s）", name, borrower, baseCurrency), nil)
	return c, nil
}

// RegisterDrawdown 登记一笔提款，状态默认可用。
func (s *Service) RegisterDrawdown(id, contractID string, dom domain.Domain, amount float64, currency, purpose string, actor Actor) (*domain.Drawdown, error) {
	snap, commit, err := s.begin(true)
	if err != nil {
		return nil, err
	}
	defer commit()
	if _, ok := snap.Contracts[contractID]; !ok {
		return nil, fmt.Errorf("合同 %s 不存在", contractID)
	}
	if !domain.ValidDomain(dom) {
		return nil, fmt.Errorf("非法条款域 %q", dom)
	}
	if _, ok := snap.Drawdowns[id]; ok {
		return nil, fmt.Errorf("提款 %s 已存在", id)
	}
	d := &domain.Drawdown{ID: id, ContractID: contractID, Domain: dom, Amount: amount,
		Currency: currency, Purpose: purpose, Status: domain.DrawdownAvailable, CreatedAt: s.now().UTC()}
	snap.Drawdowns[id] = d
	s.audit(snap, actor.ID, actor.Role, domain.ActionDrawdownAdded, "DRAWDOWN", id,
		fmt.Sprintf("登记提款 %s（域 %s，金额 %.2f %s）", id, dom, amount, currency),
		map[string]string{"contract_id": contractID})
	return d, nil
}

// CreateCovenantInput 创建约束入参。
type CreateCovenantInput struct {
	ID                  string
	ContractID          string
	Dom                 domain.Domain
	Name                string
	OwnerRole           string
	ReviewerRole        string
	Frequency           domain.Frequency
	ReportingOffsetDays int
	DeadlineHour        int
	DeadlineMinute      int
	DeadlineTZ          string
	Metric              domain.MetricSpec
	Basis               domain.Basis
	GraceDays           int
}

// CreateCovenant 法务/财务/业务负责人登记自己域内的可计算约束（首版条款）。
func (s *Service) CreateCovenant(in CreateCovenantInput, actor Actor) (*domain.Covenant, error) {
	snap, commit, err := s.begin(true)
	if err != nil {
		return nil, err
	}
	defer commit()
	if err := validateCovenantInput(in, snap); err != nil {
		return nil, err
	}
	c := &domain.Covenant{
		ID: in.ID, ContractID: in.ContractID, Domain: in.Dom, Name: in.Name,
		OwnerRole: in.OwnerRole, ReviewerRole: in.ReviewerRole, Frequency: in.Frequency,
		ReportingOffsetDays: in.ReportingOffsetDays, DeadlineHour: in.DeadlineHour,
		DeadlineMinute: in.DeadlineMinute, DeadlineTZ: in.DeadlineTZ,
		CreatedAt: s.now().UTC(),
	}
	c.Versions = []domain.CovenantVersion{{
		VersionID: s.nextID(snap, "CV"), Sequence: 1,
		Metric: in.Metric, Basis: in.Basis, GraceDays: in.GraceDays,
		ImpactScope: "首版条款，覆盖全部期次", ChangedBy: actor.ID, CreatedAt: s.now().UTC(),
	}}
	snap.Covenants[in.ID] = c
	s.audit(snap, actor.ID, actor.Role, domain.ActionCovenantCreated, "COVENANT", in.ID,
		fmt.Sprintf("创建约束 %s（域 %s，指标 %s，阈值 %.4f，周期 %s，宽限 %d 天，口径 %s v%d）",
			in.Name, in.Dom, in.Metric.Code, in.Metric.Threshold, in.Frequency, in.GraceDays,
			in.Basis.Currency, in.Basis.Version),
		map[string]string{"contract_id": in.ContractID, "version_id": c.Versions[0].VersionID})
	return c, nil
}

func validateCovenantInput(in CreateCovenantInput, snap *Snapshot) error {
	if _, ok := snap.Contracts[in.ContractID]; !ok {
		return fmt.Errorf("合同 %s 不存在", in.ContractID)
	}
	if !domain.ValidDomain(in.Dom) {
		return fmt.Errorf("非法条款域 %q", in.Dom)
	}
	if !domain.ValidFrequency(in.Frequency) {
		return fmt.Errorf("非法观察周期 %q", in.Frequency)
	}
	if in.OwnerRole == "" || in.ReviewerRole == "" {
		return fmt.Errorf("责任角色与复核角色不能为空")
	}
	if in.ReportingOffsetDays < 0 {
		return fmt.Errorf("报表截止偏移天数不能为负")
	}
	if in.DeadlineHour < 0 || in.DeadlineHour > 23 || in.DeadlineMinute < 0 || in.DeadlineMinute > 59 {
		return fmt.Errorf("截止时刻非法")
	}
	if _, err := time.LoadLocation(in.DeadlineTZ); err != nil {
		return fmt.Errorf("截止时区 %q 无法解析: %w", in.DeadlineTZ, err)
	}
	if in.Metric.Code == "" {
		return fmt.Errorf("指标代码不能为空")
	}
	if in.Basis.Currency == "" {
		return fmt.Errorf("币种不能为空")
	}
	if in.ID == "" {
		return fmt.Errorf("约束 ID 不能为空")
	}
	if _, exists := snap.Covenants[in.ID]; exists {
		return fmt.Errorf("约束 %s 已存在", in.ID)
	}
	return nil
}

// AmendCovenant 管理员修订条款：追加版本，记录生效时间与影响范围，绝不就地改写。
func (s *Service) AmendCovenant(covenantID string, metric domain.MetricSpec, basis domain.Basis, graceDays int,
	effectiveFrom time.Time, impactScope, reason string, actor Actor) (*domain.CovenantVersion, error) {
	snap, commit, err := s.begin(true)
	if err != nil {
		return nil, err
	}
	defer commit()
	c, ok := snap.Covenants[covenantID]
	if !ok {
		return nil, fmt.Errorf("约束 %s 不存在", covenantID)
	}
	if !adminRoles[actor.Role] {
		return nil, fmt.Errorf("仅管理员可修订条款（当前角色 %s）", actor.Role)
	}
	if strings.TrimSpace(impactScope) == "" {
		return nil, fmt.Errorf("修订必须填写影响范围")
	}
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("修订必须填写原因")
	}
	last := c.Versions[len(c.Versions)-1]
	if !effectiveFrom.After(last.EffectiveFrom) && !last.EffectiveFrom.IsZero() {
		return nil, fmt.Errorf("新生效时间 %s 必须晚于当前版本生效时间 %s",
			effectiveFrom.Format(time.RFC3339), last.EffectiveFrom.Format(time.RFC3339))
	}
	nv := domain.CovenantVersion{
		VersionID: s.nextID(snap, "CV"), Sequence: last.Sequence + 1,
		Metric: metric, Basis: basis, GraceDays: graceDays,
		EffectiveFrom: effectiveFrom.UTC(), ImpactScope: impactScope,
		Reason: reason, ChangedBy: actor.ID, CreatedAt: s.now().UTC(),
	}
	c.Versions = append(c.Versions, nv)
	s.audit(snap, actor.ID, actor.Role, domain.ActionCovenantAmended, "COVENANT_VERSION", nv.VersionID,
		fmt.Sprintf("约束 %s 修订至 v%d，生效时间 %s，影响范围：%s；原因：%s",
			c.ID, nv.Sequence, effectiveFrom.UTC().Format(time.RFC3339), impactScope, reason),
		map[string]string{"covenant_id": c.ID, "effective_from": effectiveFrom.UTC().Format(time.RFC3339)})
	return &nv, nil
}

// UploadFilingInput 上传银行报表入参。
type UploadFilingInput struct {
	ContractID string
	PeriodKey  string // 允许跨月/跨季任意期次；为空则按提交时刻归属
	FileName   string
	Content    []byte
	Values     map[string]float64
	SubmittedAt time.Time // 银行报表载明的提交时刻（用于跨时区截止判定）
	SourceTZ   string
	Submitter  string
	Note       string
	Kind       string // 留空则自动判定 ORIGINAL/BACKFILL/REVISION
}

// UploadFiling 接收银行报表，返回版本；同一文件哈希重复上传不新增版本、不改版本关系。
func (s *Service) UploadFiling(in UploadFilingInput, actor Actor) (*domain.FilingVersion, bool, error) {
	snap, commit, err := s.begin(true)
	if err != nil {
		return nil, false, err
	}
	defer commit()
	if _, ok := snap.Contracts[in.ContractID]; !ok {
		return nil, false, fmt.Errorf("合同 %s 不存在", in.ContractID)
	}
	if len(in.Values) == 0 {
		return nil, false, fmt.Errorf("报表必须包含至少一个指标值")
	}
	if in.SubmittedAt.IsZero() {
		in.SubmittedAt = s.now()
	}
	in.SubmittedAt = in.SubmittedAt.UTC()

	periodKey := in.PeriodKey
	if periodKey == "" {
		// 报表归属期次以提交时刻的月/季为准（取合同内任一约束的周期仅用于归属）
		periodKey = s.inferPeriodKey(snap, in.ContractID, in.SubmittedAt)
	}
	key := domain.FilingKey(in.ContractID, periodKey)
	chain := snap.Filings[key]
	if chain == nil {
		chain = &domain.FilingChain{ContractID: in.ContractID, PeriodKey: periodKey}
		snap.Filings[key] = chain
	}

	hash := hashBytes(in.Content)
	if hash == "" {
		hash = hashOfValues(in.FileName, in.Values, in.SubmittedAt)
	}
	for _, v := range chain.Versions {
		if v.ContentHash == hash {
			s.audit(snap, actor.ID, actor.Role, domain.ActionFilingDuplicate, "FILING_VERSION", v.ID,
				fmt.Sprintf("同一文件重复上传被忽略：%s（哈希 %s，期次 %s）", in.FileName, shortHash(hash), periodKey),
				map[string]string{"contract_id": in.ContractID, "period_key": periodKey})
			return &v, true, nil // duplicate=true，版本关系保持不变
		}
	}

	kind := in.Kind
	var supersedes string
	latest := chain.Latest()
	if kind == "" {
		switch {
		case latest == nil:
			// 期次首传：晚于任一覆盖约束的截止时刻即为补发，否则为按期首传。
			if s.filingLateForAny(snap, in.ContractID, periodKey, in.SubmittedAt) {
				kind = domain.FilingBackfill
			} else {
				kind = domain.FilingOriginal
			}
		default:
			// 已有版本：新提交一律视为对既有版本的修订（迟到信息由每期 FilingLate 单独记录）。
			kind = domain.FilingRevision
		}
	}
	if latest != nil {
		supersedes = latest.ID
	}
	fv := domain.FilingVersion{
		ID: s.nextID(snap, "FIL"), Sequence: len(chain.Versions) + 1,
		Kind: kind, Supersedes: supersedes, FileName: in.FileName, ContentHash: hash,
		Values: cloneValues(in.Values), SubmittedAt: in.SubmittedAt, SourceTZ: in.SourceTZ,
		Submitter: in.Submitter, Note: in.Note, UploadedAt: s.now().UTC(),
	}
	chain.Versions = append(chain.Versions, fv)
	s.audit(snap, actor.ID, actor.Role, domain.ActionFilingUploaded, "FILING_VERSION", fv.ID,
		fmt.Sprintf("接收报表 %s（期次 %s，版本 %d，种类 %s，提交时刻 %s%s）",
			in.FileName, periodKey, fv.Sequence, kindLabel(fv.Kind),
			fv.SubmittedAt.Format(time.RFC3339), tzNote(in.SourceTZ)),
		map[string]string{"contract_id": in.ContractID, "period_key": periodKey,
			"supersedes": supersedes})
	return &fv, false, nil
}

func (s *Service) inferPeriodKey(snap *Snapshot, contractID string, t time.Time) string {
	freq := domain.Monthly
	for _, c := range snap.Covenants {
		if c.ContractID == contractID {
			freq = c.Frequency
			break
		}
	}
	p, _ := domain.PeriodFor(freq, t)
	return p.Key
}

// filingLateForAny 判定提交时刻是否晚于该合同任一覆盖该期次的约束的截止。
func (s *Service) filingLateForAny(snap *Snapshot, contractID, periodKey string, submitted time.Time) bool {
	for _, c := range snap.Covenants {
		if c.ContractID != contractID {
			continue
		}
		periods, err := expandPeriods(c.Frequency, periodKey)
		if err != nil {
			continue
		}
		for _, p := range periods {
			if submitted.After(c.Deadline(p)) {
				return true
			}
		}
	}
	return false
}

// AddEvidence 接收人工佐证（银行系统外的凭证、说明、往来函件）。
func (s *Service) AddEvidence(contractID, periodKey, fileName string, content []byte, note string, actor Actor) (*domain.Evidence, error) {
	snap, commit, err := s.begin(true)
	if err != nil {
		return nil, err
	}
	defer commit()
	if _, ok := snap.Contracts[contractID]; !ok {
		return nil, fmt.Errorf("合同 %s 不存在", contractID)
	}
	if periodKey == "" {
		return nil, fmt.Errorf("期次不能为空")
	}
	e := &domain.Evidence{
		ID: s.nextID(snap, "EV"), ContractID: contractID, PeriodKey: periodKey,
		FileName: fileName, ContentHash: hashBytes(content), Note: note,
		UploadedBy: actor.ID, UploadedAt: s.now().UTC(),
	}
	snap.Evidence = append(snap.Evidence, e)
	s.audit(snap, actor.ID, actor.Role, domain.ActionEvidenceAdded, "EVIDENCE", e.ID,
		fmt.Sprintf("接收人工佐证 %s（期次 %s）", fileName, periodKey),
		map[string]string{"contract_id": contractID, "period_key": periodKey})
	return e, nil
}

// GenerateAll 对某合同某期次的全部约束生成状态（季度报告场景：一次评估多条约束）。
func (s *Service) GenerateAll(contractID, periodKey string) ([]*domain.StatusRecord, error) {
	snap, commit, err := s.begin(true)
	if err != nil {
		return nil, err
	}
	defer commit()
	if _, ok := snap.Contracts[contractID]; !ok {
		return nil, fmt.Errorf("合同 %s 不存在", contractID)
	}
	var out []*domain.StatusRecord
	ids := make([]string, 0)
	for id, c := range snap.Covenants {
		if c.ContractID == contractID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := snap.Covenants[id]
		// 同一合同可混用月/季周期：季度报告同时评估季度约束与季内三个月度期次，
		// 月度报告则评估月度约束及包含该月的季度期次。
		periods, err := expandPeriods(c.Frequency, periodKey)
		if err != nil {
			return nil, fmt.Errorf("期次 %s 无法用于约束 %s（%s）: %w", periodKey, c.ID, c.Frequency, err)
		}
		for _, p := range periods {
			rec, gerr := s.generateOne(snap, c, p.Key)
			if gerr != nil {
				return nil, gerr
			}
			out = append(out, rec)
		}
	}
	return out, nil
}

// expandPeriods 把请求期次键展开为目标周期下应评估的期次：
// 同周期直接返回；季度键 + 月度周期 → 季内三个月；月度键 + 季度周期 → 所在季度。
func expandPeriods(freq domain.Frequency, key string) ([]domain.Period, error) {
	switch freq {
	case domain.Monthly:
		if p, err := domain.PeriodFromKey(domain.Monthly, key); err == nil {
			return []domain.Period{p}, nil
		}
		qp, err := domain.PeriodFromKey(domain.Quarterly, key)
		if err != nil {
			return nil, err
		}
		var out []domain.Period
		for m := qp.Start; !m.After(qp.End); m = m.AddDate(0, 1, 0) {
			mp, _ := domain.PeriodFor(domain.Monthly, m)
			out = append(out, mp)
		}
		return out, nil
	case domain.Quarterly:
		if p, err := domain.PeriodFromKey(domain.Quarterly, key); err == nil {
			return []domain.Period{p}, nil
		}
		mp, err := domain.PeriodFromKey(domain.Monthly, key)
		if err != nil {
			return nil, err
		}
		qp, _ := domain.PeriodFor(domain.Quarterly, mp.Start.AddDate(0, 0, 15))
		return []domain.Period{qp}, nil
	default:
		return nil, fmt.Errorf("未知周期 %q", freq)
	}
}

// GenerateOne 对单条约束生成期次状态。
func (s *Service) GenerateOne(covenantID, periodKey string) (*domain.StatusRecord, error) {
	snap, commit, err := s.begin(true)
	if err != nil {
		return nil, err
	}
	defer commit()
	c, ok := snap.Covenants[covenantID]
	if !ok {
		return nil, fmt.Errorf("约束 %s 不存在", covenantID)
	}
	return s.generateOne(snap, c, periodKey)
}

func (s *Service) generateOne(snap *Snapshot, c *domain.Covenant, periodKey string) (*domain.StatusRecord, error) {
	p, err := domain.PeriodFromKey(c.Frequency, periodKey)
	if err != nil {
		return nil, fmt.Errorf("期次 %s 与约束 %s 的周期 %s 不匹配", periodKey, c.ID, c.Frequency)
	}
	now := s.now().UTC()
	v := c.VersionEffective(p.End) // 历史期次使用期末生效版本，新口径不追溯
	deadline := c.Deadline(p)
	graceEnd := c.GraceEnd(p)
	chain := snap.Filings[domain.FilingKey(c.ContractID, periodKey)]
	latest := chain.Latest()
	evCount := s.countEvidence(snap, c.ContractID, periodKey)

	// 未决复核期次：只在收到“新版本报表”时重算（达标则转修订待复核，仍违约则幂等保持）；
	// 没有新版本则保持等待，绝不重复触发冻结/通知。
	openRv := s.openReview(snap, c.ID, periodKey)
	if openRv != nil {
		lastRec := s.latestRecord(snap, c.ID, periodKey)
		newVersion := latest != nil && (lastRec == nil || latest.ID != lastRec.FilingVersionID)
		if !newVersion {
			rec := s.cloneRecord(snap, lastRec, now, "存在未决复核且无新版本报表，状态保持等待指定角色复核决定")
			s.appendRecord(snap, rec)
			return rec, nil
		}
	}
	if openRv == nil {
		if term := s.terminalRecord(snap, c.ID, periodKey); term != nil {
			rec := s.cloneRecord(snap, term, now, "期次已终局（"+string(term.Status)+"），历史结果不因新口径或重算而改变")
			s.appendRecord(snap, rec)
			return rec, nil
		}
	}

	var status domain.Status
	var explanation string
	var value float64
	hasValue := false
	var filingID, filingKind string
	late := false

	if latest != nil {
		val, present := latest.Values[v.Metric.Code]
		filingID = latest.ID
		filingKind = latest.Kind
		late = latest.SubmittedAt.After(deadline)
		if present {
			value = val
			hasValue = true
			status = domain.Classify(v.Metric, val)
			switch status {
			case domain.StatusNearThreshold:
				explanation = fmt.Sprintf("报表 %s 指标 %s=%.4f 接近阈值 %.4f（带宽 %.4f），币种口径 %s v%d；提交%s",
					latest.FileName, v.Metric.Code, val, v.Metric.Threshold, v.Metric.WarnDelta,
					v.Basis.Currency, v.Basis.Version, lateText(late, deadline))
			case domain.StatusBreachInGrace:
				if now.After(graceEnd) {
					status = domain.StatusConfirmedDefault
					explanation = fmt.Sprintf("指标 %s=%.4f %s阈值 %.4f，宽限期已于 %s 届满未治愈，按 %s v%d 口径确认违约",
						v.Metric.Code, val, breachWord(v.Metric.HigherIsBetter), v.Metric.Threshold,
						graceEnd.Format(time.RFC3339), v.Basis.Currency, v.Basis.Version)
				} else {
					explanation = fmt.Sprintf("指标 %s=%.4f %s阈值 %.4f，处于宽限期（截止 %s，宽限届满 %s）",
						v.Metric.Code, val, breachWord(v.Metric.HigherIsBetter), v.Metric.Threshold,
						deadline.Format(time.RFC3339), graceEnd.Format(time.RFC3339))
				}
			default:
				explanation = fmt.Sprintf("报表 %s 指标 %s=%.4f 满足阈值 %.4f，口径 %s v%d，状态达标",
					latest.FileName, v.Metric.Code, val, v.Metric.Threshold, v.Basis.Currency, v.Basis.Version)
			}
			if late && status != domain.StatusConfirmedDefault {
				explanation += "；报表迟于截止时刻（" + latest.Kind
				if latest.Kind == domain.FilingBackfill {
					explanation += "补发"
				} else if latest.Kind == domain.FilingRevision {
					explanation += "修订"
				}
				explanation += "）"
			}
		} else {
			status, explanation = s.noDataStatus(now, deadline, graceEnd, "最新报表版本未包含该指标值")
		}
	} else {
		status, explanation = s.noDataStatus(now, deadline, graceEnd, "尚未收到银行报表")
	}
	if evCount > 0 {
		explanation += fmt.Sprintf("；已附人工佐证 %d 份", evCount)
	}

	// 违约确认后收到补发/修订（含达标新值）→ 不自动恢复，挂到同一复核任务转“修订待复核”。
	prev := s.latestRecord(snap, c.ID, periodKey)
	wasDefault := openRv != nil || (prev != nil && prev.Status == domain.StatusConfirmedDefault)
	if wasDefault && latest != nil && hasValue && status != domain.StatusConfirmedDefault {
		status = domain.StatusRevisionPendingReview
		explanation = fmt.Sprintf("违约确认后收到%s版本（%s，指标 %s=%.4f），需 %s 复核后决定撤回误报、确认违约或认可补救",
			kindLabel(filingKind), latest.FileName, v.Metric.Code, value, c.ReviewerRole)
	}

	rec := &domain.StatusRecord{
		CovenantID: c.ID, ContractID: c.ContractID, PeriodKey: periodKey,
		Status: status, AsOf: now, MetricCode: v.Metric.Code, Value: value, HasValue: hasValue,
		Threshold: v.Metric.Threshold, WarnDelta: v.Metric.WarnDelta, HigherIsBetter: v.Metric.HigherIsBetter,
		CovenantVersionID: v.VersionID, Basis: v.Basis,
		FilingVersionID: filingID, FilingKind: filingKind, FilingLate: late,
		Deadline: deadline, GraceEnd: graceEnd, EvidenceCount: evCount, Explanation: explanation,
		CreatedAt: now,
	}
	if openRv != nil {
		rec.ReviewID = openRv.ID // 新版本重算仍挂在原复核任务上，不另开任务
	}
	rec.ID = s.nextID(snap, "ST")
	s.appendRecord(snap, rec)
	s.applySideEffects(snap, rec, c)
	return rec, nil
}

// appendRecord 追加不可变状态记录并写审计，不触发任何副作用。
func (s *Service) appendRecord(snap *Snapshot, rec *domain.StatusRecord) {
	rec.Sequence = s.countStatus(snap, rec.CovenantID, rec.PeriodKey) + 1
	snap.Statuses = append(snap.Statuses, rec)
	c := snap.Covenants[rec.CovenantID]
	s.audit(snap, "", "", domain.ActionStatusGenerated, "STATUS", rec.ID,
		"生成期次状态："+string(rec.Status)+"｜"+rec.Explanation,
		map[string]string{"covenant_id": rec.CovenantID, "period_key": rec.PeriodKey,
			"contract_id": rec.ContractID})
}

// applySideEffects 依据新状态驱动通知/冻结/复核。
// 仅在真实状态迁移时调用；保持态克隆不得调用，避免重复冻结或重复通知。
func (s *Service) applySideEffects(snap *Snapshot, rec *domain.StatusRecord, c *domain.Covenant) {
	switch rec.Status {
	case domain.StatusOverdue:
		s.notify(snap, rec, domain.KindOverdue, "WARN",
			fmt.Sprintf("【报表逾期】约束 %s 期次 %s 已过截止 %s 未收到合格报表，宽限届满 %s",
				c.Name, rec.PeriodKey, rec.Deadline.Format(time.RFC3339), rec.GraceEnd.Format(time.RFC3339)))
	case domain.StatusNearThreshold:
		// 近阈值提醒：同约束同期次只发一次；即使数值在预警带内反复波动也不重复。
		s.notify(snap, rec, domain.KindNearWarning, "INFO",
			fmt.Sprintf("【近阈值预警】约束 %s 期次 %s 指标 %s=%.4f 接近阈值 %.4f",
				c.Name, rec.PeriodKey, rec.MetricCode, rec.Value, rec.Threshold))
	case domain.StatusBreachInGrace:
		s.notify(snap, rec, domain.KindBreachGrace, "HIGH",
			fmt.Sprintf("【宽限期违约】约束 %s 期次 %s 指标 %s=%.4f 突破阈值 %.4f，请于 %s 前补救",
				c.Name, rec.PeriodKey, rec.MetricCode, rec.Value, rec.Threshold, rec.GraceEnd.Format(time.RFC3339)))
	case domain.StatusConfirmedDefault:
		s.onConfirmedDefault(snap, rec, c)
	case domain.StatusRevisionPendingReview:
		s.notify(snap, rec, domain.KindRevisionReview, "HIGH",
			fmt.Sprintf("【修订待复核】约束 %s 期次 %s 违约后收到新版本报表，等待 %s 复核",
				c.Name, rec.PeriodKey, c.ReviewerRole))
	}

	// 补发/修订使非违约期次恢复达标时留一条信息通知（去重：每约束期次一条）。
	if (rec.Status == domain.StatusCompliant || rec.Status == domain.StatusCured) &&
		(rec.FilingKind == domain.FilingBackfill || rec.FilingKind == domain.FilingRevision) {
		s.notify(snap, rec, domain.KindBackfill, "INFO",
			fmt.Sprintf("【补发/修订收讫】约束 %s 期次 %s 已收到%s报表，指标达标",
				c.Name, rec.PeriodKey, kindLabel(rec.FilingKind)))
	}
}

func (s *Service) onConfirmedDefault(snap *Snapshot, rec *domain.StatusRecord, c *domain.Covenant) {
	s.notify(snap, rec, domain.KindDefault, "CRITICAL",
		fmt.Sprintf("【违约确认】约束 %s 期次 %s 已确认违约，立即冻结同域未发放提款并转 %s 复核",
			c.Name, rec.PeriodKey, c.ReviewerRole))

	// 开启指定角色复核任务（同约束期次只开一个，重启后仍可追踪）。
	rv := s.openReview(snap, rec.CovenantID, rec.PeriodKey)
	if rv == nil {
		rv = &domain.ReviewTask{
			ID: s.nextID(snap, "REV"), ContractID: rec.ContractID, CovenantID: rec.CovenantID,
			PeriodKey: rec.PeriodKey, StatusRecordID: rec.ID,
			RequiredRole: c.ReviewerRole, OwnerRole: c.OwnerRole,
			Status: domain.ReviewPending, OpenedAt: s.now().UTC(),
		}
		snap.Reviews = append(snap.Reviews, rv)
		s.audit(snap, "", "", domain.ActionReviewOpened, "REVIEW", rv.ID,
			fmt.Sprintf("开启复核任务：约束 %s 期次 %s，指定角色 %s", c.Name, rec.PeriodKey, c.ReviewerRole),
			map[string]string{"status_id": rec.ID, "covenant_id": rec.CovenantID})
	}
	order.ReviewID = rv.ID
	rec.ReviewID = rv.ID

	// 同约束期次已有生效冻结令（如未决复核期间新版本仍判违约的重算）：保持幂等不叠加。
	for _, old := range snap.Freezes {
		if old.CovenantID == rec.CovenantID && old.PeriodKey == rec.PeriodKey && old.ReleasedAt == nil {
			s.audit(snap, "", "", domain.ActionFreezeApplied, "FREEZE", old.ID,
				fmt.Sprintf("约束 %s 期次 %s 违约状态持续，既有冻结令 %s 继续有效",
					c.Name, rec.PeriodKey, old.ID),
				map[string]string{"review_id": rv.ID, "status_id": rec.ID, "covenant_id": rec.CovenantID})
			return
		}
	}

	// 冻结“相关提款”：同合同同条款域、尚未发放的提款。
	// 已被其他生效冻结令覆盖的提款同样列入本令（PreviousStatus 留空），
	// 这样“两条约束同时违约、一条撤回”时提款仍被另一冻结令覆盖，不会提前解冻。
	order := &domain.FreezeOrder{
		ID: s.nextID(snap, "FRZ"), ContractID: rec.ContractID, CovenantID: rec.CovenantID,
		PeriodKey: rec.PeriodKey, StatusRecordID: rec.ID,
		Reason: fmt.Sprintf("约束 %s 期次 %s 确认违约", c.Name, rec.PeriodKey),
		PreviousStatus: map[string]string{}, CreatedAt: s.now().UTC(),
	}
	ddIDs := make([]string, 0)
	for _, d := range snap.Drawdowns {
		if d.ContractID == rec.ContractID && d.Domain == c.Domain &&
			(d.Status == domain.DrawdownAvailable || d.Status == domain.DrawdownRequested) {
			order.PreviousStatus[d.ID] = d.Status
			d.Status = domain.DrawdownFrozen
			ddIDs = append(ddIDs, d.ID)
		} else if d.ContractID == rec.ContractID && d.Domain == c.Domain && d.Status == domain.DrawdownFrozen {
			order.PreviousStatus[d.ID] = "" // 已被他令冻结，原始状态以首令为准
			ddIDs = append(ddIDs, d.ID)
		}
	}
	sort.Strings(ddIDs)
	order.DrawdownIDs = ddIDs
	snap.Freezes = append(snap.Freezes, order)
	newly := 0
	for _, ps := range order.PreviousStatus {
		if ps != "" {
			newly++
		}
	}
	s.audit(snap, "", "", domain.ActionFreezeApplied, "FREEZE", order.ID,
		fmt.Sprintf("冻结覆盖 %d 笔相关提款（新冻结 %d 笔，域 %s）：%s", len(ddIDs), newly, c.Domain, strings.Join(ddIDs, ", ")),
		map[string]string{"review_id": rv.ID, "status_id": rec.ID, "covenant_id": rec.CovenantID})
}

// DecideReview 指定角色对未决复核作出决定。
func (s *Service) DecideReview(reviewID, decision, note string, actor Actor) (*domain.ReviewTask, *domain.StatusRecord, error) {
	snap, commit, err := s.begin(true)
	if err != nil {
		return nil, nil, err
	}
	defer commit()
	var rv *domain.ReviewTask
	for _, r := range snap.Reviews {
		if r.ID == reviewID {
			rv = r
			break
		}
	}
	if rv == nil {
		return nil, nil, fmt.Errorf("复核任务 %s 不存在", reviewID)
	}
	if rv.Status != domain.ReviewPending {
		return nil, nil, fmt.Errorf("复核任务 %s 已结案（%s）", reviewID, rv.Status)
	}
	if actor.Role != rv.RequiredRole {
		return nil, nil, fmt.Errorf("仅指定复核角色 %s 可决定（当前角色 %s）", rv.RequiredRole, actor.Role)
	}
	c := snap.Covenants[rv.CovenantID]
	now := s.now().UTC()
	rv.ResolvedAt = &now
	rv.Resolver = actor.ID
	rv.Decision = decision
	rv.Note = note

	var finalStatus domain.Status
	var summary, notifKind string
	switch decision {
	case domain.DecisionWithdraw:
		rv.Status = domain.ReviewWithdrawn
		finalStatus = domain.StatusFalseAlarmWithdrawn
		notifKind = domain.KindWithdrawn
		summary = fmt.Sprintf("复核认定误报：约束 %s 期次 %s 的违约确认撤回，释放冻结提款", c.Name, rv.PeriodKey)
	case domain.DecisionConfirm:
		rv.Status = domain.ReviewConfirmed
		finalStatus = domain.StatusDefaultAcknowledged
		summary = fmt.Sprintf("复核确认违约成立：约束 %s 期次 %s，冻结维持有效", c.Name, rv.PeriodKey)
	case domain.DecisionCure:
		rv.Status = domain.ReviewCured
		finalStatus = domain.StatusCured
		notifKind = domain.KindCured
		summary = fmt.Sprintf("复核认可补救：约束 %s 期次 %s 恢复达标，释放冻结提款", c.Name, rv.PeriodKey)
	default:
		return nil, nil, fmt.Errorf("未知复核决定 %q", decision)
	}

	// 释放冻结（仅误报撤回与认可补救）。
	// 同一提款可能被同域多条约束的冻结令同时覆盖：仅当不存在其他生效冻结令时才解冻，
	// 避免“两条约束同时违约、一条撤回”时把另一条违约对应的提款错误放出。
	if decision == domain.DecisionWithdraw || decision == domain.DecisionCure {
		for _, o := range snap.Freezes {
			if o.ReviewID == rv.ID && o.ReleasedAt == nil {
				o.ReleasedAt = &now
				o.ReleasedBy = actor.ID
				o.ReleaseReason = summary
				for _, did := range o.DrawdownIDs {
					if s.otherActiveFreeze(snap, o, did) {
						continue // 仍被另一生效冻结令覆盖，维持冻结
					}
					if d, ok := snap.Drawdowns[did]; ok && d.Status == domain.DrawdownFrozen {
						d.Status = o.PreviousStatus[did]
						if d.Status == "" {
							d.Status = domain.DrawdownAvailable
						}
					}
				}
				s.audit(snap, actor.ID, actor.Role, domain.ActionFreezeReleased, "FREEZE", o.ID,
					summary, map[string]string{"review_id": rv.ID})
			}
		}
	}

	// 撤回误报：标记原违约通知为已撤回（不删除，通知历史可追溯）。
	if decision == domain.DecisionWithdraw {
		for _, n := range snap.Notifs {
			if n.CovenantID == rv.CovenantID && n.PeriodKey == rv.PeriodKey &&
				(n.Kind == domain.KindDefault || n.Kind == domain.KindBreachGrace) && n.WithdrawnAt == nil {
				nc := now
				n.WithdrawnAt = &nc
				n.WithdrawnReason = "复核认定误报：" + note
			}
		}
	}

	prev := s.latestRecord(snap, rv.CovenantID, rv.PeriodKey)
	rec := s.cloneRecord(snap, prev, now, summary+mapNote(note))
	rec.Status = finalStatus
	rec.ReviewID = rv.ID
	s.appendRecord(snap, rec)
	if notifKind != "" {
		s.notify(snap, rec, notifKind, "WARN", "【复核结论】"+summary)
	}
	s.audit(snap, actor.ID, actor.Role, domain.ActionReviewDecided, "REVIEW", rv.ID,
		summary+mapNote(note), map[string]string{"status_id": rec.ID, "decision": decision})
	return rv, rec, nil
}

// ---------- 读模型 / 查询侧 ----------

// PendingReviews 返回未决复核（隔日重启后仍可追踪）。
func (s *Service) PendingReviews() ([]*domain.ReviewTask, error) {
	snap, commit, err := s.begin(false)
	if err != nil {
		return nil, err
	}
	defer commit()
	out := make([]*domain.ReviewTask, 0)
	for _, r := range snap.Reviews {
		if r.Status == domain.ReviewPending {
			out = append(out, clone(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenedAt.Before(out[j].OpenedAt) })
	return out, nil
}

// UpcomingDeadline 下一报表截止：基于合同下尚未过截止时刻的最早期次。
func (s *Service) UpcomingDeadline(contractID string, from time.Time) (covenantID, periodKey string, deadline time.Time, ok bool, err error) {
	snap, commit, e := s.begin(false)
	if e != nil {
		return "", "", time.Time{}, false, e
	}
	defer commit()
	var best time.Time
	var bestID, bestKey string
	for _, c := range snap.Covenants {
		if c.ContractID != contractID {
			continue
		}
		p, _ := domain.PeriodFor(c.Frequency, from)
		// 从当前期次起向后找 4 个期次，取最近的未过截止。
		for i := 0; i < 4; i++ {
			d := c.Deadline(p)
			if d.After(from) && (!ok || d.Before(best)) {
				best = d
				bestID = c.ID
				bestKey = p.Key
				ok = true
			}
			np, perr := domain.NextPeriod(c.Frequency, p)
			if perr != nil {
				break
			}
			p = np
		}
	}
	return bestID, bestKey, best, ok, nil
}

// PeriodView 某约束某期次的完整解释视图（供审计页/接口）。
type PeriodView struct {
	Covenant     *domain.Covenant      `json:"covenant"`
	Period       domain.Period         `json:"period"`
	Deadline     time.Time             `json:"deadline"`
	GraceEnd     time.Time             `json:"grace_end"`
	Current      *domain.StatusRecord  `json:"current"`
	History      []*domain.StatusRecord `json:"history"`
	FilingChain  *domain.FilingChain   `json:"filing_chain"`
	Evidence     []*domain.Evidence    `json:"evidence"`
	Review       *domain.ReviewTask    `json:"review"`
	Freeze       *domain.FreezeOrder   `json:"freeze"`
	Notifications []*domain.Notification `json:"notifications"`
}

// ExplainPeriod 返回某约束某期次的状态、版本链与审计解释。
func (s *Service) ExplainPeriod(covenantID, periodKey string) (*PeriodView, error) {
	snap, commit, err := s.begin(false)
	if err != nil {
		return nil, err
	}
	defer commit()
	c, ok := snap.Covenants[covenantID]
	if !ok {
		return nil, fmt.Errorf("约束 %s 不存在", covenantID)
	}
	p, err := domain.PeriodFromKey(c.Frequency, periodKey)
	if err != nil {
		return nil, err
	}
	v := &PeriodView{
		Covenant: clone(c), Period: p, Deadline: c.Deadline(p), GraceEnd: c.GraceEnd(p),
		FilingChain: clone(snap.Filings[domain.FilingKey(c.ContractID, periodKey)]),
	}
	for _, r := range snap.Statuses {
		if r.CovenantID == covenantID && r.PeriodKey == periodKey {
			v.History = append(v.History, clone(r))
		}
	}
	if len(v.History) > 0 {
		v.Current = v.History[len(v.History)-1]
	}
	for _, e := range snap.Evidence {
		if e.ContractID == c.ContractID && e.PeriodKey == periodKey {
			v.Evidence = append(v.Evidence, clone(e))
		}
	}
	for _, r := range snap.Reviews {
		if r.CovenantID == covenantID && r.PeriodKey == periodKey {
			v.Review = clone(r)
		}
	}
	for _, o := range snap.Freezes {
		if o.CovenantID == covenantID && o.PeriodKey == periodKey {
			v.Freeze = clone(o)
		}
	}
	for _, n := range snap.Notifs {
		if n.CovenantID == covenantID && n.PeriodKey == periodKey {
			v.Notifications = append(v.Notifications, clone(n))
		}
	}
	return v, nil
}

// AuditTrail 返回审计链（可按目标过滤）。
func (s *Service) AuditTrail() ([]*domain.AuditEntry, error) {
	snap, commit, err := s.begin(false)
	if err != nil {
		return nil, err
	}
	defer commit()
	return cloneSlice(snap.Audit), nil
}

// View 暴露给 HTTP 层的只读深拷贝。
func (s *Service) View() (*Snapshot, error) {
	snap, commit, err := s.begin(false)
	if err != nil {
		return nil, err
	}
	defer commit()
	return clone(snap), nil
}

// ---------- 内部工具 ----------

func (s *Service) notify(snap *Snapshot, rec *domain.StatusRecord, kind, severity, msg string) {
	key := strings.Join([]string{kind, rec.CovenantID, rec.PeriodKey}, "|")
	for _, n := range snap.Notifs {
		if n.DedupKey == key {
			return // 去重：近阈值等每种事件每约束期次只提醒一次
		}
	}
	n := &domain.Notification{
		ID: s.nextID(snap, "NTF"), DedupKey: key, ContractID: rec.ContractID,
		CovenantID: rec.CovenantID, PeriodKey: rec.PeriodKey, Kind: kind, Severity: severity,
		Message: msg, StatusRecordID: rec.ID, CreatedAt: s.now().UTC(),
	}
	snap.Notifs = append(snap.Notifs, n)
	s.audit(snap, "", "", domain.ActionNotified, "NOTIFICATION", n.ID, msg,
		map[string]string{"kind": kind, "covenant_id": rec.CovenantID, "period_key": rec.PeriodKey})
}

func (s *Service) noDataStatus(now, deadline, graceEnd time.Time, reason string) (domain.Status, string) {
	switch {
	case now.After(graceEnd):
		return domain.StatusConfirmedDefault,
			fmt.Sprintf("%s；宽限期已于 %s 届满，按未履报义务确认违约（截止 %s）", reason, graceEnd.Format(time.RFC3339), deadline.Format(time.RFC3339))
	case now.After(deadline):
		return domain.StatusOverdue,
			fmt.Sprintf("%s；已过截止时刻 %s，宽限届满 %s 前仍可补报", reason, deadline.Format(time.RFC3339), graceEnd.Format(time.RFC3339))
	default:
		return domain.StatusPending,
			fmt.Sprintf("%s；尚未到截止时刻 %s", reason, deadline.Format(time.RFC3339))
	}
}

func (s *Service) countEvidence(snap *Snapshot, contractID, periodKey string) int {
	n := 0
	for _, e := range snap.Evidence {
		if e.ContractID == contractID && e.PeriodKey == periodKey {
			n++
		}
	}
	return n
}

func (s *Service) countStatus(snap *Snapshot, covenantID, periodKey string) int {
	n := 0
	for _, r := range snap.Statuses {
		if r.CovenantID == covenantID && r.PeriodKey == periodKey {
			n++
		}
	}
	return n
}

func (s *Service) latestRecord(snap *Snapshot, covenantID, periodKey string) *domain.StatusRecord {
	var last *domain.StatusRecord
	for _, r := range snap.Statuses {
		if r.CovenantID == covenantID && r.PeriodKey == periodKey {
			last = r
		}
	}
	return last
}

func (s *Service) openReview(snap *Snapshot, covenantID, periodKey string) *domain.ReviewTask {
	for _, r := range snap.Reviews {
		if r.CovenantID == covenantID && r.PeriodKey == periodKey && r.Status == domain.ReviewPending {
			return r
		}
	}
	return nil
}

// otherActiveFreeze 判断某提款是否仍被“别的”生效冻结令覆盖。
func (s *Service) otherActiveFreeze(snap *Snapshot, self *domain.FreezeOrder, drawdownID string) bool {
	for _, o := range snap.Freezes {
		if o == self || o.ReleasedAt != nil {
			continue
		}
		for _, did := range o.DrawdownIDs {
			if did == drawdownID {
				return true
			}
		}
	}
	return false
}

// terminalRecord 已结案的终局状态：误报撤回/补救认可/违约确认成立。
func (s *Service) terminalRecord(snap *Snapshot, covenantID, periodKey string) *domain.StatusRecord {
	last := s.latestRecord(snap, covenantID, periodKey)
	if last == nil {
		return nil
	}
	switch last.Status {
	case domain.StatusFalseAlarmWithdrawn, domain.StatusCured, domain.StatusDefaultAcknowledged:
		return last
	}
	return nil
}

func (s *Service) cloneRecord(snap *Snapshot, src *domain.StatusRecord, now time.Time, explanation string) *domain.StatusRecord {
	if src == nil {
		return &domain.StatusRecord{AsOf: now, CreatedAt: now, Explanation: explanation}
	}
	cp := *src
	cp.ID = s.nextID(snap, "ST")
	cp.Sequence = 0 // appendRecord 会统一重排序号
	cp.AsOf = now
	cp.CreatedAt = now
	cp.Explanation = explanation
	return &cp
}

func hashBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hashOfValues(name string, vals map[string]float64, submitted time.Time) string {
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name).WriteString("|").WriteString(submitted.Format(time.RFC3339Nano))
	for _, k := range keys {
		fmt.Fprintf(&b, "|%s=%.10f", k, vals[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func cloneValues(in map[string]float64) map[string]float64 {
	if in == nil {
		return nil
	}
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func kindLabel(k string) string {
	switch k {
	case domain.FilingBackfill:
		return "补发"
	case domain.FilingRevision:
		return "修订"
	case domain.FilingOriginal:
		return "首次"
	default:
		return k
	}
}

func tzNote(tz string) string {
	if tz == "" {
		return ""
	}
	return "（来源时区 " + tz + "，已按绝对时刻比较）"
}

func lateText(late bool, deadline time.Time) string {
	if late {
		return "迟于截止 " + deadline.Format(time.RFC3339)
	}
	return "在截止 " + deadline.Format(time.RFC3339) + " 前"
}

func breachWord(higherIsBetter bool) string {
	if higherIsBetter {
		return "低于"
	}
	return "高于"
}

func mapNote(note string) string {
	if strings.TrimSpace(note) == "" {
		return ""
	}
	return "；复核说明：" + note
}
