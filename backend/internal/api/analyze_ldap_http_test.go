package api

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func analyzeRouter() *gin.Engine {
	r := gin.New()
	r.POST("/api/v1/analyze", AnalyzeUploadHandler())
	return r
}

func TestAnalyzeUpload_RawBody(t *testing.T) {
	r := analyzeRouter()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/analyze",
		strings.NewReader(minState(7, "lin-1", "aws_instance.web")))
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	for _, want := range []string{`"analysis"`, `"resources"`, `"rum":1`, "aws_instance"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("response missing %s", want)
		}
	}
}

func TestAnalyzeUpload_MultipartFile(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "app.tfstate")
	_, _ = fw.Write([]byte(minState(3, "lin-2", "aws_vpc.main")))
	_ = mw.Close()

	r := analyzeRouter()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/analyze", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "aws_vpc") {
		t.Fatalf("multipart: status = %d (%s)", w.Code, w.Body.String())
	}
}

func TestAnalyzeUpload_InvalidState(t *testing.T) {
	r := analyzeRouter()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/analyze", strings.NewReader("not json")))
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid state: status = %d, want 422", w.Code)
	}
}

func TestLDAPLogin_DisabledGuard(t *testing.T) {
	// LDAP disabled → clear 400 before any credential processing.
	h, _ := newReconcileEnv(t, nil) // all providers disabled
	r := gin.New()
	r.POST("/api/v1/auth/ldap/login", h.LDAPLoginHandler())

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/ldap/login",
		strings.NewReader(`{"username":"alice","password":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not enabled") {
		t.Errorf("disabled LDAP: status = %d (%s)", w.Code, w.Body.String())
	}
}
