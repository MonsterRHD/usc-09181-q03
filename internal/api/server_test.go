package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"example.com/09181/q003/internal/api"
	"example.com/09181/q003/internal/service"
	"example.com/09181/q003/internal/store"
)

func do(t *testing.T, h http.Handler, method, path string, body any, role string) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if role != "" {
		req.Header.Set("X-Actor-Id", "tester")
		req.Header.Set("X-Actor-Role", role)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应不是 JSON: %v (body=%s)", err, rec.Body.String())
		}
	}
	return rec.Code, out
}

func TestEndToEndHTTP(t *testing.T) {
	svc := service.New(store.NewMemoryStore())
	h := api.NewServer(svc).Handler()

	// 健康检查。
	if code, body := do(t, h, "GET", "/health", nil, ""); code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("health 异常: code=%d body=%v", code, body)
	}

	// 登记合同。
	if code, _ := do(t, h, "POST", "/api/contracts", map[string]any{
		"id": "LN-1", "name": "银团贷款", "borrower": "子公司", "base_currency": "USD",
	}, "ADMIN"); code != http.StatusCreated {
		t.Fatalf("创建合同返回 %d", code)
	}

	// 登记提款。
	if code, _ := do(t, h, "POST", "/api/contracts/LN-1/drawdowns", map[string]any{
		"id": "DD-1", "domain": "USE_OF_PROCEEDS", "amount": 1000000,
		"currency": "USD", "purpose": "采购",
	}, "BUSINESS_HEAD"); code != http.StatusCreated {
		t.Fatalf("登记提款返回 %d", code)
	}

	// 创建资金用途约束（月度，月末+10 天 18:00 纽约截止，宽限 3 天）。
	if code, body := do(t, h, "POST", "/api/covenants", map[string]any{
		"id": "C-UOP", "contract_id": "LN-1", "domain": "USE_OF_PROCEEDS",
		"name": "资金用途合规率", "owner_role": "BUSINESS_HEAD", "reviewer_role": "RISK_COMMITTEE",
		"frequency": "MONTHLY", "reporting_offset_days": 10,
		"deadline_hour": 18, "deadline_minute": 0, "deadline_tz": "America/New_York",
		"grace_days": 3,
		"metric": map[string]any{"code": "UOP", "name": "用途合规率",
			"higher_is_better": false, "threshold": 0.05, "warn_delta": 0.01},
		"basis": map[string]any{"currency": "USD", "fx_convention": "期末中间价", "version": 1},
	}, "BUSINESS_HEAD"); code != http.StatusCreated {
		t.Fatalf("创建约束返回 %d: %v", code, body)
	}

	// 同一文件重复上传：第二次返回 200 且 duplicate=true。
	filing := map[string]any{
		"contract_id": "LN-1", "period_key": "2026M03", "file_name": "uop.csv",
		"content_hash": "hash-abc", "values": map[string]float64{"UOP": 0.02},
		"submitted_at": "2026-04-08T18:00:00Z", "source_tz": "UTC",
	}
	if code, body := do(t, h, "POST", "/api/filings", filing, "FINANCE_HEAD"); code != http.StatusCreated {
		t.Fatalf("首次上传返回 %d: %v", code, body)
	}
	if code, body := do(t, h, "POST", "/api/filings", filing, "FINANCE_HEAD"); code != http.StatusOK || body["duplicate"] != true {
		t.Fatalf("重复上传返回 code=%d body=%v", code, body)
	}

	// 生成期次状态：0.02 ≤ 阈值 0.05，达标。
	code, body := do(t, h, "POST", "/api/generate",
		map[string]any{"contract_id": "LN-1", "period_key": "2026M03"}, "FINANCE_HEAD")
	if code != http.StatusOK {
		t.Fatalf("生成状态返回 %d: %v", code, body)
	}
	recs, _ := body["records"].([]any)
	if len(recs) != 1 {
		t.Fatalf("状态记录数 = %d", len(recs))
	}
	rec, _ := recs[0].(map[string]any)
	if rec["status"] != "COMPLIANT" {
		t.Fatalf("状态 = %v，期望 COMPLIANT", rec["status"])
	}

	// 期次解释视图含版本链。
	code, body = do(t, h, "GET", "/api/covenants/C-UOP/periods/2026M03/explain", nil, "")
	if code != http.StatusOK {
		t.Fatalf("期次解释返回 %d: %v", code, body)
	}
	chain, _ := body["filing_chain"].(map[string]any)
	versions, _ := chain["versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("报表版本数 = %d，期望 1", len(versions))
	}

	// 无未决复核。
	if code, body = do(t, h, "GET", "/api/reviews/pending", nil, ""); code != http.StatusOK {
		t.Fatalf("未决复核返回 %d", code)
	}
	if list, _ := body["reviews"].([]any); len(list) != 0 {
		t.Fatalf("不应有未决复核: %d", len(list))
	}

	// 审计链非空且只追加。
	if code, body = do(t, h, "GET", "/api/audit", nil, ""); code != http.StatusOK {
		t.Fatalf("审计返回 %d", code)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) < 5 {
		t.Fatalf("审计链过短: %d", len(entries))
	}
}

func TestHTTPErrorCases(t *testing.T) {
	svc := service.New(store.NewMemoryStore())
	h := api.NewServer(svc).Handler()

	// 方法不允许。
	if code, _ := do(t, h, "GET", "/api/contracts", nil, ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/contracts 应 405，得到 %d", code)
	}
	// 未知路径。
	if code, _ := do(t, h, "GET", "/api/unknown", nil, ""); code != http.StatusNotFound {
		t.Fatalf("未知路径应 404，得到 %d", code)
	}
	// 非法 JSON。
	req := httptest.NewRequest("POST", "/api/contracts", bytes.NewReader([]byte("{bad")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应 400，得到 %d", rec.Code)
	}
}
