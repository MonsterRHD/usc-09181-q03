package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"example.com/09181/q003/internal/api"
	"example.com/09181/q003/internal/service"
	"example.com/09181/q003/internal/store"
)

// TestWiringHealth 验证 main 装配所用的三层组合（文件存储 + 引擎 + HTTP）可用。
func TestWiringHealth(t *testing.T) {
	svc := service.New(store.NewFileStore(filepath.Join(t.TempDir(), "state.json")))
	srv := api.NewServer(svc)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health 状态码 = %d，期望 200", rec.Code)
	}
}
