package service_test

import (
	"path/filepath"
	"testing"
	"time"

	"example.com/09181/q003/internal/domain"
	"example.com/09181/q003/internal/service"
	"example.com/09181/q003/internal/store"
)

// ---- 测试夹具 ----

type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time { return c.t }

func utc(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
}

type fixture struct {
	t     *testing.T
	svc   *service.Service
	clk   *fixedClock
	admin service.Actor
	legal service.Actor
	fin   service.Actor
	biz   service.Actor
	rev1  service.Actor
	rev2  service.Actor
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := &fixedClock{t: utc(2026, 1, 1, 0, 0)}
	svc := service.New(store.NewMemoryStore()).WithClock(clk.now)
	f := &fixture{
		t: t, svc: svc, clk: clk,
		admin: service.Actor{ID: "u-admin", Role: "ADMIN"},
		legal: service.Actor{ID: "u-legal", Role: "LEGAL_HEAD"},
		fin:   service.Actor{ID: "u-fin", Role: "FINANCE_HEAD"},
		biz:   service.Actor{ID: "u-biz", Role: "BUSINESS_HEAD"},
		rev1:  service.Actor{ID: "u-risk", Role: "RISK_COMMITTEE"},
		rev2:  service.Actor{ID: "u-cro", Role: "CRO"},
	}
	if _, err := svc.CreateContract("LN-1", "海外子公司银团贷款", "OverseasSub", "USD", f.admin); err != nil {
		t.Fatalf("创建合同失败: %v", err)
	}
	return f
}

// monthlyDSCR 月度偿债覆盖率约束：期末 +15 天 17:00（上海）截止，宽限 5 天。
// 阈值 1.20（越大越好），近阈值带宽 0.05。
func (f *fixture) monthlyDSCR(id string, reviewer string, offset int) *domain.Covenant {
	c, err := f.svc.CreateCovenant(service.CreateCovenantInput{
		ID: id, ContractID: "LN-1", Dom: domain.DebtService, Name: "偿债覆盖率 " + id,
		OwnerRole: "LEGAL_HEAD", ReviewerRole: reviewer, Frequency: domain.Monthly,
		ReportingOffsetDays: offset, DeadlineHour: 17, DeadlineMinute: 0,
		DeadlineTZ: "Asia/Shanghai", GraceDays: 5,
		Metric: domain.MetricSpec{Code: "DSCR_" + id, Name: "偿债覆盖率", HigherIsBetter: true,
			Threshold: 1.20, WarnDelta: 0.05},
		Basis: domain.Basis{Currency: "USD", FXConvention: "期末中间价", Version: 1},
	}, f.legal)
	if err != nil {
		f.t.Fatalf("创建约束 %s 失败: %v", id, err)
	}
	return c
}

func (f *fixture) drawdown(id string, dom domain.Domain) {
	if _, err := f.svc.RegisterDrawdown(id, "LN-1", dom, 5_000_000, "USD", "营运资金", f.biz); err != nil {
		f.t.Fatalf("登记提款失败: %v", err)
	}
}

func (f *fixture) filing(period, file string, submitted time.Time, vals map[string]float64) (*domain.FilingVersion, bool) {
	fv, dup, err := f.svc.UploadFiling(service.UploadFilingInput{
		ContractID: "LN-1", PeriodKey: period, FileName: file,
		Content: []byte(file + "::" + period), Values: vals,
		SubmittedAt: submitted, SourceTZ: "UTC", Submitter: "agent-bank",
	}, f.fin)
	if err != nil {
		f.t.Fatalf("上传报表失败: %v", err)
	}
	return fv, dup
}

func (f *fixture) gen(covenantID, period string) *domain.StatusRecord {
	r, err := f.svc.GenerateOne(covenantID, period)
	if err != nil {
		f.t.Fatalf("生成状态失败 (%s,%s): %v", covenantID, period, err)
	}
	return r
}

func (f *fixture) view(covenantID, period string) *service.PeriodView {
	v, err := f.svc.ExplainPeriod(covenantID, period)
	if err != nil {
		f.t.Fatalf("查询期次视图失败: %v", err)
	}
	return v
}

func mustStatus(t *testing.T, r *domain.StatusRecord, want domain.Status) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("状态 = %s，期望 %s；解释：%s", r.Status, want, r.Explanation)
	}
}

func countNotif(v *service.PeriodView, kind string) int {
	n := 0
	for _, x := range v.Notifications {
		if x.Kind == kind {
			n++
		}
	}
	return n
}

// ---- 场景一：跨月指标 + 补发报表 + 近阈值只提醒一次 ----

func TestCrossMonthBackfillAndNearWarnOnce(t *testing.T) {
	f := newFixture(t)
	f.monthlyDSCR("C-DSCR", "RISK_COMMITTEE", 15)

	// 2026M03 截止 = 2026-04-15 17:00 上海 = 09:00 UTC；宽限届满 = 04-20 09:00 UTC。
	// 截止后、宽限期内：先逾期。
	f.clk.t = utc(2026, 4, 16, 0, 0)
	mustStatus(t, f.gen("C-DSCR", "2026M03"), domain.StatusOverdue)

	// 补发：提交时刻晚于截止，版本种类必须是 BACKFILL，且保留版本链。
	fv, dup := f.filing("2026M03", "dscr-m03.pdf", utc(2026, 4, 16, 2, 0),
		map[string]float64{"DSCR_C-DSCR": 1.23})
	if dup || fv.Kind != domain.FilingBackfill || fv.Sequence != 1 {
		t.Fatalf("补发版本判定错误: %+v dup=%v", fv, dup)
	}

	// 1.23 落在近阈值带 [1.20,1.25)：近阈值，且只提醒一次。
	mustStatus(t, f.gen("C-DSCR", "2026M03"), domain.StatusNearThreshold)
	f.gen("C-DSCR", "2026M03")
	f.gen("C-DSCR", "2026M03")
	v := f.view("C-DSCR", "2026M03")
	if n := countNotif(v, domain.KindNearWarning); n != 1 {
		t.Fatalf("近阈值通知数 = %d，期望 1（去重失败）", n)
	}
	if n := countNotif(v, domain.KindOverdue); n != 1 {
		t.Fatalf("逾期通知数 = %d，期望 1", n)
	}

	// 再次补发达标值：状态恢复，补发收讫通知只发一条。
	f.filing("2026M03", "dscr-m03-fixed.pdf", utc(2026, 4, 17, 3, 0),
		map[string]float64{"DSCR_C-DSCR": 1.30})
	mustStatus(t, f.gen("C-DSCR", "2026M03"), domain.StatusCompliant)
	f.gen("C-DSCR", "2026M03")
	v = f.view("C-DSCR", "2026M03")
	if n := countNotif(v, domain.KindBackfill); n != 1 {
		t.Fatalf("补发收讫通知数 = %d，期望 1", n)
	}
	if len(v.FilingChain.Versions) != 2 || v.FilingChain.Versions[1].Supersedes != v.FilingChain.Versions[0].ID {
		t.Fatalf("报表版本链关系错误: %+v", v.FilingChain)
	}
	// 历史记录只追加：逾期 1 + 预警 3（保持态也追加）+ 达标 2，共 6 条全部保留。
	if len(v.History) != 6 {
		t.Fatalf("状态历史条数 = %d，期望 6（历史不得被覆盖）", len(v.History))
	}
}

// ---- 跨时区截止：同一绝对时刻，不因时区口径错过通知 ----

func TestTimezoneDeadlineAbsoluteInstant(t *testing.T) {
	f := newFixture(t)
	f.monthlyDSCR("C-TZ", "RISK_COMMITTEE", 15)

	// 04-15 08:59 UTC = 16:59 上海：截止前一分钟，不得报逾期。
	f.clk.t = utc(2026, 4, 15, 8, 59)
	mustStatus(t, f.gen("C-TZ", "2026M03"), domain.StatusPending)

	// 09:01 UTC = 17:01 上海：刚过截止，立即逾期通知。
	f.clk.t = utc(2026, 4, 15, 9, 1)
	mustStatus(t, f.gen("C-TZ", "2026M03"), domain.StatusOverdue)
	if n := countNotif(f.view("C-TZ", "2026M03"), domain.KindOverdue); n != 1 {
		t.Fatalf("跨时区逾期通知数 = %d，期望 1", n)
	}
}

// ---- 同一文件重复上传：不新增版本、不改版本关系 ----

func TestDuplicateUploadIgnored(t *testing.T) {
	f := newFixture(t)
	f.monthlyDSCR("C-DUP", "RISK_COMMITTEE", 15)

	fv1, dup1 := f.filing("2026M03", "same.pdf", utc(2026, 4, 14, 8, 0),
		map[string]float64{"DSCR_C-DUP": 1.40})
	fv2, dup2 := f.filing("2026M03", "same.pdf", utc(2026, 4, 14, 9, 0),
		map[string]float64{"DSCR_C-DUP": 1.40})
	if dup1 || !dup2 || fv1.ID != fv2.ID {
		t.Fatalf("重复上传识别错误 dup1=%v dup2=%v id1=%s id2=%s", dup1, dup2, fv1.ID, fv2.ID)
	}
	chain := f.view("C-DUP", "2026M03").FilingChain
	if len(chain.Versions) != 1 {
		t.Fatalf("重复上传新增了版本，版本数 = %d", len(chain.Versions))
	}

	// 截止前第二次上传不同内容 → REVISION 而非补发。
	fv3, dup3 := f.filing("2026M03", "same-revised.pdf", utc(2026, 4, 14, 10, 0),
		map[string]float64{"DSCR_C-DUP": 1.42})
	if dup3 || fv3.Kind != domain.FilingRevision || fv3.Supersedes != fv1.ID {
		t.Fatalf("截止前修订判定错误: %+v dup=%v", fv3, dup3)
	}
}

// ---- 场景二：两条约束同时触发违约 ----

func TestTwoCovenantsDefaultTogether(t *testing.T) {
	f := newFixture(t)
	f.monthlyDSCR("C-A", "RISK_COMMITTEE", 15)
	// 同域第二条月度约束（利息保障倍数，阈值 2.0），复核人不同。
	if _, err := f.svc.CreateCovenant(service.CreateCovenantInput{
		ID: "C-B", ContractID: "LN-1", Dom: domain.DebtService, Name: "利息保障倍数",
		OwnerRole: "LEGAL_HEAD", ReviewerRole: "CRO", Frequency: domain.Monthly,
		ReportingOffsetDays: 15, DeadlineHour: 17, DeadlineTZ: "Asia/Shanghai", GraceDays: 5,
		Metric: domain.MetricSpec{Code: "ICR", Name: "利息保障倍数", HigherIsBetter: true,
			Threshold: 2.0, WarnDelta: 0.1},
		Basis: domain.Basis{Currency: "USD", Version: 1},
	}, f.legal); err != nil {
		t.Fatalf("创建 C-B 失败: %v", err)
	}
	f.drawdown("DD-1", domain.DebtService)

	// 宽限期满后仍无任何报表：两条约束同时确认违约。
	f.clk.t = utc(2026, 4, 21, 0, 0)
	recs, err := f.svc.GenerateAll("LN-1", "2026M03")
	if err != nil {
		t.Fatalf("批量生成失败: %v", err)
	}
	for _, r := range recs {
		mustStatus(t, r, domain.StatusConfirmedDefault)
	}

	snap, _ := f.svc.View()
	// 同一提款被两张生效冻结令共同覆盖（首令记录原状态，次令作为持续覆盖）：
	// 正是这种覆盖关系保证“一条撤回、另一条仍违约”时提款不会被放出。
	active := 0
	covered := 0
	for _, o := range snap.Freezes {
		if o.ReleasedAt == nil {
			active++
			if len(o.DrawdownIDs) == 1 && o.DrawdownIDs[0] == "DD-1" {
				covered++
			}
		}
	}
	if active != 2 || covered != 2 {
		t.Fatalf("生效冻结令 = %d（期望 2），覆盖 DD-1 的 = %d（期望 2）", active, covered)
	}
	if snap.Drawdowns["DD-1"].Status != domain.DrawdownFrozen {
		t.Fatalf("提款未冻结: %s", snap.Drawdowns["DD-1"].Status)
	}
	pending, _ := f.svc.PendingReviews()
	if len(pending) != 2 {
		t.Fatalf("未决复核数 = %d，期望 2", len(pending))
	}

	// 非指定角色不能复核：对任一任务，用另一约束的指定角色决策都必须被拒。
	for _, p := range pending {
		other := f.rev1
		if p.RequiredRole == "RISK_COMMITTEE" {
			other = f.rev2
		}
		if _, _, err := f.svc.DecideReview(p.ID, domain.DecisionWithdraw, "越权尝试", other); err == nil {
			t.Fatalf("任务 %s（指定角色 %s）不应接受 %s 的复核", p.ID, p.RequiredRole, other.Role)
		}
	}

	// C-A 复核认定误报并撤回：因 C-B 的违约冻结仍生效，提款必须保持冻结。
	var ra, rb string
	for _, r := range pending {
		switch r.CovenantID {
		case "C-A":
			ra = r.ID
		case "C-B":
			rb = r.ID
		}
	}
	if _, _, err := f.svc.DecideReview(ra, domain.DecisionWithdraw, "银行口径差错", f.rev1); err != nil {
		t.Fatalf("C-A 撤回失败: %v", err)
	}
	snap, _ = f.svc.View()
	if snap.Drawdowns["DD-1"].Status != domain.DrawdownFrozen {
		t.Fatalf("另一违约仍生效时提款被错误解冻: %s", snap.Drawdowns["DD-1"].Status)
	}
	mustStatus(t, f.view("C-A", "2026M03").Current, domain.StatusFalseAlarmWithdrawn)

	// C-B 复核确认违约成立：冻结继续维持，期次进入终局。
	if _, _, err := f.svc.DecideReview(rb, domain.DecisionConfirm, "违约属实", f.rev2); err != nil {
		t.Fatalf("C-B 确认失败: %v", err)
	}
	snap, _ = f.svc.View()
	if snap.Drawdowns["DD-1"].Status != domain.DrawdownFrozen {
		t.Fatalf("确认成立后提款不应解冻")
	}
	mustStatus(t, f.view("C-B", "2026M03").Current, domain.StatusDefaultAcknowledged)
	if f.view("C-B", "2026M03").Freeze.ReleasedAt != nil {
		t.Fatal("确认成立的冻结令不应被释放")
	}

	// 终局后再生成：历史不被重算覆盖，也不产生第二个复核任务。
	f.gen("C-B", "2026M03")
	if p, _ := f.svc.PendingReviews(); len(p) != 0 {
		t.Fatalf("终局后仍有未决复核 %d 个", len(p))
	}
}

// ---- 场景三：撤回误报全链路（违约→修订→复核撤回→解冻）----

func TestWithdrawFalseAlarmReleasesFreeze(t *testing.T) {
	f := newFixture(t)
	f.monthlyDSCR("C-FA", "RISK_COMMITTEE", 15)
	f.drawdown("DD-2", domain.DebtService)

	// 宽限期内收到突破阈值报表：先进入宽限期违约。
	f.clk.t = utc(2026, 4, 16, 0, 0)
	f.filing("2026M03", "bad.pdf", utc(2026, 4, 16, 0, 0),
		map[string]float64{"DSCR_C-FA": 1.10})
	mustStatus(t, f.gen("C-FA", "2026M03"), domain.StatusBreachInGrace)

	// 宽限届满：确认违约 → 冻结 + 复核任务。
	f.clk.t = utc(2026, 4, 21, 0, 0)
	mustStatus(t, f.gen("C-FA", "2026M03"), domain.StatusConfirmedDefault)
	v := f.view("C-FA", "2026M03")
	if v.Review == nil || v.Review.Status != domain.ReviewPending || v.Freeze == nil {
		t.Fatal("违约后未开启复核或未冻结")
	}

	// 违约后收到修订达标报表：不得自动恢复，转“修订待复核”。
	f.filing("2026M03", "good.pdf", utc(2026, 4, 22, 0, 0),
		map[string]float64{"DSCR_C-FA": 1.30})
	mustStatus(t, f.gen("C-FA", "2026M03"), domain.StatusRevisionPendingReview)
	snap, _ := f.svc.View()
	if snap.Drawdowns["DD-2"].Status != domain.DrawdownFrozen {
		t.Fatal("修订待复核期间提款不应提前解冻")
	}

	// 指定角色复核认定误报：撤回、解冻、原违约通知标记撤回。
	if _, _, err := f.svc.DecideReview(v.Review.ID, domain.DecisionWithdraw,
		"银行重算后 DSCR 为 1.30", f.rev1); err != nil {
		t.Fatalf("撤回失败: %v", err)
	}
	snap, _ = f.svc.View()
	if snap.Drawdowns["DD-2"].Status != domain.DrawdownAvailable {
		t.Fatalf("撤回后提款未恢复: %s", snap.Drawdowns["DD-2"].Status)
	}
	v = f.view("C-FA", "2026M03")
	mustStatus(t, v.Current, domain.StatusFalseAlarmWithdrawn)
	if v.Freeze.ReleasedAt == nil {
		t.Fatal("冻结令未标记释放")
	}
	var defaultNotif *domain.Notification
	for _, n := range v.Notifications {
		if n.Kind == domain.KindDefault {
			defaultNotif = n
		}
	}
	if defaultNotif == nil || defaultNotif.WithdrawnAt == nil {
		t.Fatal("原违约通知未标记撤回")
	}
	if countNotif(v, domain.KindWithdrawn) != 1 {
		t.Fatal("缺少误报撤回通知")
	}
}

// ---- 条款修订：生效时间 + 影响范围；历史计算不被新口径覆盖 ----

func TestAmendmentDoesNotRewriteHistory(t *testing.T) {
	f := newFixture(t)
	cov := f.monthlyDSCR("C-AM", "RISK_COMMITTEE", 15)

	// M03 在 v1（阈值 1.20）下达标：值 1.25。
	f.clk.t = utc(2026, 4, 16, 0, 0)
	f.filing("2026M03", "m03.pdf", utc(2026, 4, 16, 0, 0),
		map[string]float64{"DSCR_C-AM": 1.25})
	mustStatus(t, f.gen("C-AM", "2026M03"), domain.StatusCompliant)

	// 非管理员不能修订；管理员修订缺影响范围也被拒绝。
	_, err := f.svc.AmendCovenant("C-AM",
		domain.MetricSpec{Code: "DSCR", HigherIsBetter: true, Threshold: 1.30, WarnDelta: 0.05},
		domain.Basis{Currency: "USD", Version: 2}, 5,
		utc(2026, 5, 1, 0, 0), "仅 M05 起生效", "银行要求提阈值", f.legal)
	if err == nil {
		t.Fatal("非管理员修订应被拒绝")
	}
	_, err = f.svc.AmendCovenant("C-AM",
		domain.MetricSpec{Code: "DSCR", HigherIsBetter: true, Threshold: 1.30, WarnDelta: 0.05},
		domain.Basis{Currency: "USD", Version: 2}, 5,
		utc(2026, 5, 1, 0, 0), "  ", "原因", f.admin)
	if err == nil {
		t.Fatal("缺影响范围的修订应被拒绝")
	}

	// v2：阈值 1.30，2026-05-01 生效，只影响该日之后期次。
	if _, err := f.svc.AmendCovenant("C-AM",
		domain.MetricSpec{Code: "DSCR_C-AM", Name: "偿债覆盖率", HigherIsBetter: true, Threshold: 1.30, WarnDelta: 0.05},
		domain.Basis{Currency: "USD", FXConvention: "期末中间价", Version: 2}, 5,
		utc(2026, 5, 1, 0, 0), "2026M05 起阈值提至 1.30；历史期次不重算", "银团年度修订", f.admin); err != nil {
		t.Fatalf("管理员修订失败: %v", err)
	}

	// 历史期次重算：仍用 v1，保持达标；记录里快照阈值 1.20。
	r := f.gen("C-AM", "2026M03")
	mustStatus(t, r, domain.StatusCompliant)
	if r.CovenantVersionID == cov.Versions[0].VersionID {
		// 期望仍是首版
	} else {
		t.Fatalf("历史期次被新口径覆盖: 版本 %s", r.CovenantVersionID)
	}
	if r.Threshold != 1.20 || r.Basis.Version != 1 {
		t.Fatalf("历史记录口径快照错误: 阈值 %.2f 口径 v%d", r.Threshold, r.Basis.Version)
	}

	// M05 期末在生效日之后：1.32 在 v2（阈值 1.30，带宽 0.05）下落入近阈值带；
	// 同一数值在 v1 下本会达标，用于证明口径版本差异。
	f.clk.t = utc(2026, 6, 16, 0, 0)
	f.filing("2026M05", "m05.pdf", utc(2026, 6, 16, 0, 0),
		map[string]float64{"DSCR_C-AM": 1.32})
	r = f.gen("C-AM", "2026M05")
	if r.Threshold != 1.30 || r.Basis.Version != 2 {
		t.Fatalf("新期次未采用 v2: 阈值 %.2f 口径 v%d", r.Threshold, r.Basis.Version)
	}
	mustStatus(t, r, domain.StatusNearThreshold)

	// 审计链必须包含修订动作及其生效时间/影响范围。
	trail, _ := f.svc.AuditTrail()
	found := false
	for _, a := range trail {
		if a.Action == domain.ActionCovenantAmended &&
			a.Links["effective_from"] == "2026-05-01T00:00:00Z" {
			found = true
		}
	}
	if !found {
		t.Fatal("审计链缺少条款修订记录")
	}
}

// ---- 场景四：隔日重启后未决复核与下一截止仍可追踪（文件持久化）----

func TestRestartPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	run := func(clock time.Time) (*service.Service, *fixedClock) {
		clk := &fixedClock{t: clock}
		svc := service.New(store.NewFileStore(path)).WithClock(clk.now)
		return svc, clk
	}

	// 第一次启动：建合同、约束、提款，走到违约确认 + 未决复核。
	svc, clk := run(utc(2026, 1, 1, 0, 0))
	a := service.Actor{ID: "admin", Role: "ADMIN"}
	if _, err := svc.CreateContract("LN-9", "重启测试贷款", "Sub", "USD", a); err != nil {
		t.Fatal(err)
	}
	la := service.Actor{ID: "legal", Role: "LEGAL_HEAD"}
	if _, err := svc.CreateCovenant(service.CreateCovenantInput{
		ID: "C-9", ContractID: "LN-9", Dom: domain.Security, Name: "担保覆盖率",
		OwnerRole: "FINANCE_HEAD", ReviewerRole: "RISK_COMMITTEE", Frequency: domain.Quarterly,
		ReportingOffsetDays: 30, DeadlineHour: 17, DeadlineTZ: "Asia/Shanghai", GraceDays: 10,
		Metric: domain.MetricSpec{Code: "COV", HigherIsBetter: true, Threshold: 1.1, WarnDelta: 0.02},
		Basis:  domain.Basis{Currency: "USD", Version: 1},
	}, la); err != nil {
		t.Fatal(err)
	}
	bz := service.Actor{ID: "biz", Role: "BUSINESS_HEAD"}
	if _, err := svc.RegisterDrawdown("DD-9", "LN-9", domain.Security, 1e6, "USD", "", bz); err != nil {
		t.Fatal(err)
	}
	// 2026Q1 截止 = 04-30 17:00 上海，宽限 +10 天。
	clk.t = utc(2026, 5, 11, 0, 0) // 已过宽限届满
	r, err := svc.GenerateOne("C-9", "2026Q1")
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, r, domain.StatusConfirmedDefault)

	// 隔日重新启动（新进程、新 Service 实例，同一状态文件）。
	svc2, _ := run(utc(2026, 5, 12, 9, 0))
	pending, err := svc2.PendingReviews()
	if err != nil || len(pending) != 1 {
		t.Fatalf("重启后未决复核丢失: %v (n=%d)", err, len(pending))
	}
	if pending[0].CovenantID != "C-9" || pending[0].RequiredRole != "RISK_COMMITTEE" {
		t.Fatalf("未决复核内容错误: %+v", pending[0])
	}
	snap, err := svc2.View()
	if err != nil || snap.Drawdowns["DD-9"].Status != domain.DrawdownFrozen {
		t.Fatalf("重启后冻结状态丢失: %v", err)
	}
	// 下一截止可追踪：Q2 截止 = 2026-07-30 17:00 上海 = 09:00 UTC。
	_, pk, dl, ok, err := svc2.UpcomingDeadline("LN-9", utc(2026, 5, 12, 0, 0))
	if err != nil || !ok || pk != "2026Q2" {
		t.Fatalf("下一截止查询错误: ok=%v key=%s err=%v", ok, pk, err)
	}
	want := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	if !dl.Equal(want) {
		t.Fatalf("下一截止 = %s，期望 %s", dl, want)
	}

	// 重启后复核仍可正常结案并解冻。
	rev := service.Actor{ID: "risk", Role: "RISK_COMMITTEE"}
	if _, _, err := svc2.DecideReview(pending[0].ID, domain.DecisionCure, "担保已补足", rev); err != nil {
		t.Fatalf("重启后复核结案失败: %v", err)
	}
	snap, _ = svc2.View()
	if snap.Drawdowns["DD-9"].Status != domain.DrawdownAvailable {
		t.Fatalf("补救认可后未解冻: %s", snap.Drawdowns["DD-9"].Status)
	}
}

// ---- 季度报告：季度键一次展开季度约束 + 季内三个月度期次 ----

func TestQuarterlyReportExpandsPeriods(t *testing.T) {
	f := newFixture(t)
	f.monthlyDSCR("C-M", "RISK_COMMITTEE", 15)
	// 再加一条季度约束。
	if _, err := f.svc.CreateCovenant(service.CreateCovenantInput{
		ID: "C-Q", ContractID: "LN-1", Dom: domain.Security, Name: "季度担保覆盖率",
		OwnerRole: "FINANCE_HEAD", ReviewerRole: "CRO", Frequency: domain.Quarterly,
		ReportingOffsetDays: 30, DeadlineHour: 17, DeadlineTZ: "Asia/Shanghai", GraceDays: 10,
		Metric: domain.MetricSpec{Code: "COV", HigherIsBetter: true, Threshold: 1.1},
		Basis:  domain.Basis{Currency: "USD", Version: 1},
	}, f.fin); err != nil {
		t.Fatal(err)
	}
	// 用季度键生成：月度约束覆盖 M01/M02/M03，季度约束覆盖 Q1。
	recs, err := f.svc.GenerateAll("LN-1", "2026Q1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range recs {
		got[r.CovenantID+"/"+r.PeriodKey] = true
	}
	for _, k := range []string{"C-M/2026M01", "C-M/2026M02", "C-M/2026M03", "C-Q/2026Q1"} {
		if !got[k] {
			t.Fatalf("季度报告缺少期次评估 %s，实际 %v", k, got)
		}
	}
}
