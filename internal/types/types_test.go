package types

import (
	"net/http/httptest"
	"testing"
)

// TestWriteData 验证成功响应统一形状（技术设计 §2.1）：
// {"data":{...}} + HTTP 2xx + application/json; charset=utf-8。
func TestWriteData(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteData(rr, 200, map[string]string{"k": "v"})

	if rr.Code != 200 {
		t.Errorf("WriteData status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != ContentTypeJSON {
		t.Errorf("WriteData Content-Type = %q, want %q", got, ContentTypeJSON)
	}
	wantBody := `{"data":{"k":"v"}}`
	if got := rr.Body.String(); got != wantBody {
		t.Errorf("WriteData body = %s, want %s", got, wantBody)
	}
}

// TestWriteError 验证错误响应统一形状（技术设计 §2.1）：
// {"error":{"code","message"}} + HTTP 4xx/5xx + application/json; charset=utf-8。
func TestWriteError(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteError(rr, 404, "not_found", "请求的资源不存在")

	if rr.Code != 404 {
		t.Errorf("WriteError status = %d, want 404", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != ContentTypeJSON {
		t.Errorf("WriteError Content-Type = %q, want %q", got, ContentTypeJSON)
	}
	wantBody := `{"error":{"code":"not_found","message":"请求的资源不存在"}}`
	if got := rr.Body.String(); got != wantBody {
		t.Errorf("WriteError body = %s, want %s", got, wantBody)
	}
}
