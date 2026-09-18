// Package api 提供观察站的 HTTP 接口：
// 合同/提款/约束登记、条款修订、报表与佐证上传、状态生成、
// 期次解释、复核决定、未决复核与下一截止查询、审计链导出。
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"example.com/09181/q003/internal/domain"
	"example.com/09181/q003/internal/service"
)

type Server struct {
	svc *service.Service
	mux *http.ServeMux
}

func NewServer(svc *service.Service) *Server {
	s := &Server{svc: svc, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) routes() {
	s.mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("/api/contracts", s.handleContracts)
	s.mux.HandleFunc("/api/contracts/", s.handleContractSub)
	s.mux.HandleFunc("/api/covenants", s.handleCovenants)
	s.mux.HandleFunc("/api/covenants/", s.handleCovenantSub)
	s.mux.HandleFunc("/api/filings", s.handleFilings)
	s.mux.HandleFunc("/api/evidence", s.handleEvidence)
	s.mux.HandleFunc("/api/generate", s.handleGenerate)
	s.mux.HandleFunc("/api/reviews/pending", s.handlePendingReviews)
	s.mux.HandleFunc("/api/reviews/", s.handleReviewSub)
	s.mux.HandleFunc("/api/audit", s.handleAudit)
}

type errBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, errBody{Error: err.Error()})
}

func actor(r *http.Request) service.Actor {
	return service.Actor{ID: r.Header.Get("X-Actor-Id"), Role: r.Header.Get("X-Actor-Role")}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// ---------- 合同与提款 ----------

type contractReq struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Borrower     string `json:"borrower"`
	BaseCurrency string `json:"base_currency"`
}

func (s *Server) handleContracts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, &apiError{"仅支持 POST"})
		return
	}
	var in contractReq
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	c, err := s.svc.CreateContract(in.ID, in.Name, in.Borrower, in.BaseCurrency, actor(r))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

type apiError struct{ msg string }

func (e *apiError) Error() string { return e.msg }

func (s *Server) handleContractSub(w http.ResponseWriter, r *http.Request) {
	// /api/contracts/{id}/drawdowns | /api/contracts/{id}/upcoming-deadline
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/contracts/"), "/")
	if len(parts) != 2 {
		fail(w, http.StatusNotFound, &apiError{"路径不存在"})
		return
	}
	id, sub := parts[0], parts[1]
	switch sub {
	case "drawdowns":
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, &apiError{"仅支持 POST"})
			return
		}
		var in struct {
			ID       string  `json:"id"`
			Domain   string  `json:"domain"`
			Amount   float64 `json:"amount"`
			Currency string  `json:"currency"`
			Purpose  string  `json:"purpose"`
		}
		if err := decode(r, &in); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		d, err := s.svc.RegisterDrawdown(in.ID, id, domain.Domain(in.Domain), in.Amount, in.Currency, in.Purpose, actor(r))
		if err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, d)
	case "upcoming-deadline":
		if r.Method != http.MethodGet {
			fail(w, http.StatusMethodNotAllowed, &apiError{"仅支持 GET"})
			return
		}
		from := time.Now()
		if t := r.URL.Query().Get("from"); t != "" {
			parsed, err := time.Parse(time.RFC3339, t)
			if err != nil {
				fail(w, http.StatusBadRequest, &apiError{"from 必须为 RFC3339 时刻"})
				return
			}
			from = parsed
		}
		cid, pk, dl, ok, err := s.svc.UpcomingDeadline(id, from)
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"found": ok, "covenant_id": cid, "period_key": pk, "deadline": dl})
	default:
		fail(w, http.StatusNotFound, &apiError{"路径不存在"})
	}
}

// ---------- 约束与修订 ----------

type metricReq struct {
	Code           string  `json:"code"`
	Name           string  `json:"name"`
	HigherIsBetter bool    `json:"higher_is_better"`
	Threshold      float64 `json:"threshold"`
	WarnDelta      float64 `json:"warn_delta"`
	Unit           string  `json:"unit"`
}

type basisReq struct {
	Currency     string `json:"currency"`
	FXConvention string `json:"fx_convention"`
	Version      int    `json:"version"`
}

type covenantReq struct {
	ID                  string    `json:"id"`
	ContractID          string    `json:"contract_id"`
	Domain              string    `json:"domain"`
	Name                string    `json:"name"`
	OwnerRole           string    `json:"owner_role"`
	ReviewerRole        string    `json:"reviewer_role"`
	Frequency           string    `json:"frequency"`
	ReportingOffsetDays int       `json:"reporting_offset_days"`
	DeadlineHour        int       `json:"deadline_hour"`
	DeadlineMinute      int       `json:"deadline_minute"`
	DeadlineTZ          string    `json:"deadline_tz"`
	Metric              metricReq `json:"metric"`
	Basis               basisReq  `json:"basis"`
	GraceDays           int       `json:"grace_days"`
}

func (s *Server) handleCovenants(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, &apiError{"仅支持 POST"})
		return
	}
	var in covenantReq
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	c, err := s.svc.CreateCovenant(service.CreateCovenantInput{
		ID: in.ID, ContractID: in.ContractID, Dom: domain.Domain(in.Domain), Name: in.Name,
		OwnerRole: in.OwnerRole, ReviewerRole: in.ReviewerRole,
		Frequency: domain.Frequency(in.Frequency), ReportingOffsetDays: in.ReportingOffsetDays,
		DeadlineHour: in.DeadlineHour, DeadlineMinute: in.DeadlineMinute, DeadlineTZ: in.DeadlineTZ,
		Metric: domain.MetricSpec{
			Code: in.Metric.Code, Name: in.Metric.Name, HigherIsBetter: in.Metric.HigherIsBetter,
			Threshold: in.Metric.Threshold, WarnDelta: in.Metric.WarnDelta, Unit: in.Metric.Unit,
		},
		Basis: domain.Basis{
			Currency: in.Basis.Currency, FXConvention: in.Basis.FXConvention, Version: in.Basis.Version,
		},
		GraceDays: in.GraceDays,
	}, actor(r))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) handleCovenantSub(w http.ResponseWriter, r *http.Request) {
	// /api/covenants/{id}/amendments | /api/covenants/{id}/periods/{key}/explain
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/covenants/"), "/")
	switch {
	case len(parts) == 2 && parts[1] == "amendments":
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, &apiError{"仅支持 POST"})
			return
		}
		var in struct {
			Metric        metricReq `json:"metric"`
			Basis         basisReq  `json:"basis"`
			GraceDays     int       `json:"grace_days"`
			EffectiveFrom string    `json:"effective_from"`
			ImpactScope   string    `json:"impact_scope"`
			Reason        string    `json:"reason"`
		}
		if err := decode(r, &in); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		eff, err := time.Parse(time.RFC3339, in.EffectiveFrom)
		if err != nil {
			fail(w, http.StatusBadRequest, &apiError{"effective_from 必须为 RFC3339 时刻"})
			return
		}
		v, err := s.svc.AmendCovenant(parts[0],
			domain.MetricSpec{Code: in.Metric.Code, Name: in.Metric.Name, HigherIsBetter: in.Metric.HigherIsBetter,
				Threshold: in.Metric.Threshold, WarnDelta: in.Metric.WarnDelta, Unit: in.Metric.Unit},
			domain.Basis{Currency: in.Basis.Currency, FXConvention: in.Basis.FXConvention, Version: in.Basis.Version},
			in.GraceDays, eff, in.ImpactScope, in.Reason, actor(r))
		if err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, v)
	case len(parts) == 4 && parts[1] == "periods" && parts[3] == "explain":
		if r.Method != http.MethodGet {
			fail(w, http.StatusMethodNotAllowed, &apiError{"仅支持 GET"})
			return
		}
		v, err := s.svc.ExplainPeriod(parts[0], parts[2])
		if err != nil {
			fail(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	default:
		fail(w, http.StatusNotFound, &apiError{"路径不存在"})
	}
}

// ---------- 报表、佐证、生成 ----------

type filingReq struct {
	ContractID  string             `json:"contract_id"`
	PeriodKey   string             `json:"period_key"`
	FileName    string             `json:"file_name"`
	ContentHash string             `json:"content_hash"` // 银行原始文件哈希（无原始文件时可用内容指纹）
	Values      map[string]float64 `json:"values"`
	SubmittedAt string             `json:"submitted_at"`
	SourceTZ    string             `json:"source_tz"`
	Submitter   string             `json:"submitter"`
	Note        string             `json:"note"`
	Kind        string             `json:"kind"`
}

func (s *Server) handleFilings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, &apiError{"仅支持 POST"})
		return
	}
	var in filingReq
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	submitted := time.Time{}
	if in.SubmittedAt != "" {
		t, err := time.Parse(time.RFC3339, in.SubmittedAt)
		if err != nil {
			fail(w, http.StatusBadRequest, &apiError{"submitted_at 必须为 RFC3339 时刻"})
			return
		}
		submitted = t
	}
	// 报文以哈希指纹代表文件内容；同哈希重复上传在服务层被识别并忽略。
	fv, duplicate, err := s.svc.UploadFiling(service.UploadFilingInput{
		ContractID: in.ContractID, PeriodKey: in.PeriodKey, FileName: in.FileName,
		Content: []byte(in.ContentHash), Values: in.Values, SubmittedAt: submitted,
		SourceTZ: in.SourceTZ, Submitter: in.Submitter, Note: in.Note, Kind: in.Kind,
	}, actor(r))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	code := http.StatusCreated
	if duplicate {
		code = http.StatusOK
	}
	writeJSON(w, code, map[string]any{"version": fv, "duplicate": duplicate})
}

type evidenceReq struct {
	ContractID string `json:"contract_id"`
	PeriodKey  string `json:"period_key"`
	FileName   string `json:"file_name"`
	ContentHash string `json:"content_hash"`
	Note       string `json:"note"`
}

func (s *Server) handleEvidence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, &apiError{"仅支持 POST"})
		return
	}
	var in evidenceReq
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	e, err := s.svc.AddEvidence(in.ContractID, in.PeriodKey, in.FileName, []byte(in.ContentHash), in.Note, actor(r))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

type generateReq struct {
	ContractID string `json:"contract_id"`
	PeriodKey  string `json:"period_key"`
}

func (s *Server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, &apiError{"仅支持 POST"})
		return
	}
	var in generateReq
	if err := decode(r, &in); err != nil && err.Error() != "EOF" {
		fail(w, http.StatusBadRequest, err) // 坏 JSON 明确 400；仅空 body 才回退查询参数
		return
	}
	if in.ContractID == "" || in.PeriodKey == "" {
		in.ContractID = r.URL.Query().Get("contract_id")
		in.PeriodKey = r.URL.Query().Get("period_key")
	}
	if in.ContractID == "" || in.PeriodKey == "" {
		fail(w, http.StatusBadRequest, &apiError{"必须提供 contract_id 与 period_key"})
		return
	}
	recs, err := s.svc.GenerateAll(in.ContractID, in.PeriodKey)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": recs})
}

// ---------- 复核 ----------

func (s *Server) handlePendingReviews(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, &apiError{"仅支持 GET"})
		return
	}
	list, err := s.svc.PendingReviews()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": list})
}

func (s *Server) handleReviewSub(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/reviews/"), "/")
	if len(parts) != 2 || parts[1] != "decision" {
		fail(w, http.StatusNotFound, &apiError{"路径不存在"})
		return
	}
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, &apiError{"仅支持 POST"})
		return
	}
	var in struct {
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	rv, rec, err := s.svc.DecideReview(parts[0], in.Decision, in.Note, actor(r))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"review": rv, "status_record": rec})
}

// ---------- 审计 ----------

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, &apiError{"仅支持 GET"})
		return
	}
	trail, err := s.svc.AuditTrail()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": trail})
}
