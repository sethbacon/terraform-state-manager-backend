package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/repolink"
)

const testOrgID = "11111111-1111-1111-1111-111111111111"

// fakeDiscoverer is a configurable Discoverer for handler tests.
type fakeDiscoverer struct {
	configured bool
	candidates []repolink.Candidate
	err        error
}

func (f *fakeDiscoverer) Configured() bool { return f.configured }
func (f *fakeDiscoverer) Discover(context.Context, string) ([]repolink.Candidate, error) {
	return f.candidates, f.err
}

// newRepoLinkRouter builds a gin engine wiring the repo-link routes with an
// injected organization_id (simulating AuthMiddleware) and a sqlmock-backed
// repository pair.
func newRepoLinkRouter(t *testing.T, orgID string, disc repolink.Discoverer) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	sourceRepo := repositories.NewStateSourceRepository(db)
	linkRepo := repositories.NewSourceRepoLinkRepository(db)
	h := NewRepoLinkHandlers(sourceRepo, linkRepo, disc)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("organization_id", orgID)
		c.Next()
	})
	r.GET("/sources/:id/repo-link", h.GetRepoLink)
	r.PUT("/sources/:id/repo-link", h.SetRepoLink)
	r.DELETE("/sources/:id/repo-link", h.DeleteRepoLink)
	r.POST("/sources/:id/repo-link/discover", h.DiscoverRepoLink)
	return r, mock
}

func expectSourceLookup(mock sqlmock.Sqlmock, sourceID, orgID, name string) {
	cols := []string{
		"id", "organization_id", "name", "source_type", "config", "is_active",
		"last_tested_at", "last_test_status", "created_by", "created_at", "updated_at",
	}
	rows := sqlmock.NewRows(cols).AddRow(
		sourceID, orgID, name, "s3", []byte(`{}`), true,
		nil, nil, nil, time.Now(), time.Now(),
	)
	mock.ExpectQuery(`SELECT id, organization_id, name, source_type, config`).
		WithArgs(sourceID).WillReturnRows(rows)
}

func doReq(t *testing.T, r *gin.Engine, method, path string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(payload)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// --- GET ------------------------------------------------------------------

func TestGetRepoLink_Found(t *testing.T) {
	r, mock := newRepoLinkRouter(t, testOrgID, repolink.NewStubDiscoverer())
	sourceID := uuid.NewString()

	expectSourceLookup(mock, sourceID, testOrgID, "prod")
	linkCols := []string{
		"id", "organization_id", "source_id", "ado_organization_url", "ado_project",
		"ado_repo", "ado_pipeline_id", "discovery_method", "created_at", "updated_at",
	}
	mock.ExpectQuery(`SELECT id, organization_id, source_id, ado_organization_url, ado_project`).
		WithArgs(sourceID).
		WillReturnRows(sqlmock.NewRows(linkCols).AddRow(
			"link-1", testOrgID, sourceID, "https://dev.azure.com/acme", "infra",
			"tf-network", 7, "manual", time.Now(), time.Now(),
		))

	w := doReq(t, r, http.MethodGet, "/sources/"+sourceID+"/repo-link", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Data struct {
			ADORepo string `json:"ado_repo"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "tf-network", resp.Data.ADORepo)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetRepoLink_NoLink_Returns404(t *testing.T) {
	r, mock := newRepoLinkRouter(t, testOrgID, repolink.NewStubDiscoverer())
	sourceID := uuid.NewString()

	expectSourceLookup(mock, sourceID, testOrgID, "prod")
	mock.ExpectQuery(`SELECT id, organization_id, source_id, ado_organization_url, ado_project`).
		WithArgs(sourceID).
		WillReturnRows(sqlmock.NewRows([]string{})) // no rows

	w := doReq(t, r, http.MethodGet, "/sources/"+sourceID+"/repo-link", nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetRepoLink_SourceNotFound_Returns404(t *testing.T) {
	r, mock := newRepoLinkRouter(t, testOrgID, repolink.NewStubDiscoverer())
	sourceID := uuid.NewString()

	mock.ExpectQuery(`SELECT id, organization_id, name, source_type, config`).
		WithArgs(sourceID).
		WillReturnRows(sqlmock.NewRows([]string{})) // source missing

	w := doReq(t, r, http.MethodGet, "/sources/"+sourceID+"/repo-link", nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetRepoLink_CrossOrg_Returns404(t *testing.T) {
	r, mock := newRepoLinkRouter(t, testOrgID, repolink.NewStubDiscoverer())
	sourceID := uuid.NewString()

	// Source belongs to a different org; handler must report 404, not leak it.
	expectSourceLookup(mock, sourceID, "22222222-2222-2222-2222-222222222222", "other")

	w := doReq(t, r, http.MethodGet, "/sources/"+sourceID+"/repo-link", nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetRepoLink_InvalidID_Returns400(t *testing.T) {
	r, _ := newRepoLinkRouter(t, testOrgID, repolink.NewStubDiscoverer())
	w := doReq(t, r, http.MethodGet, "/sources/not-a-uuid/repo-link", nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

// --- PUT ------------------------------------------------------------------

func TestSetRepoLink_Success(t *testing.T) {
	r, mock := newRepoLinkRouter(t, testOrgID, repolink.NewStubDiscoverer())
	sourceID := uuid.NewString()

	expectSourceLookup(mock, sourceID, testOrgID, "prod")
	mock.ExpectQuery(`INSERT INTO source_repo_links`).
		WithArgs(
			testOrgID, sourceID, "https://dev.azure.com/acme", "infra", "tf-network",
			7, "manual",
		).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).
			AddRow("link-1", time.Now(), time.Now()))

	w := doReq(t, r, http.MethodPut, "/sources/"+sourceID+"/repo-link", map[string]interface{}{
		"ado_organization_url": "https://dev.azure.com/acme",
		"ado_project":          "infra",
		"ado_repo":             "tf-network",
		"ado_pipeline_id":      7,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Data struct {
			ID              string `json:"id"`
			DiscoveryMethod string `json:"discovery_method"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "link-1", resp.Data.ID)
	require.Equal(t, "manual", resp.Data.DiscoveryMethod)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSetRepoLink_MissingRequiredField_Returns400(t *testing.T) {
	r, mock := newRepoLinkRouter(t, testOrgID, repolink.NewStubDiscoverer())
	sourceID := uuid.NewString()

	expectSourceLookup(mock, sourceID, testOrgID, "prod")

	// ado_repo omitted -> binding:"required" fails, no INSERT expected.
	w := doReq(t, r, http.MethodPut, "/sources/"+sourceID+"/repo-link", map[string]interface{}{
		"ado_organization_url": "https://dev.azure.com/acme",
		"ado_project":          "infra",
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSetRepoLink_NonPositivePipeline_Returns400(t *testing.T) {
	r, mock := newRepoLinkRouter(t, testOrgID, repolink.NewStubDiscoverer())
	sourceID := uuid.NewString()

	expectSourceLookup(mock, sourceID, testOrgID, "prod")

	w := doReq(t, r, http.MethodPut, "/sources/"+sourceID+"/repo-link", map[string]interface{}{
		"ado_organization_url": "https://dev.azure.com/acme",
		"ado_project":          "infra",
		"ado_repo":             "tf-network",
		"ado_pipeline_id":      -1,
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

// --- DELETE ---------------------------------------------------------------

func TestDeleteRepoLink_Success(t *testing.T) {
	r, mock := newRepoLinkRouter(t, testOrgID, repolink.NewStubDiscoverer())
	sourceID := uuid.NewString()

	expectSourceLookup(mock, sourceID, testOrgID, "prod")
	mock.ExpectExec(`DELETE FROM source_repo_links WHERE source_id = \$1`).
		WithArgs(sourceID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := doReq(t, r, http.MethodDelete, "/sources/"+sourceID+"/repo-link", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

// --- discover -------------------------------------------------------------

func TestDiscoverRepoLink_Unconfigured_Returns503(t *testing.T) {
	// Stub discoverer => not configured => 503 before any source lookup.
	r, mock := newRepoLinkRouter(t, testOrgID, repolink.NewStubDiscoverer())
	sourceID := uuid.NewString()

	w := doReq(t, r, http.MethodPost, "/sources/"+sourceID+"/repo-link/discover", nil)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	// No DB calls should have occurred.
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDiscoverRepoLink_Configured_ReturnsCandidates(t *testing.T) {
	disc := &fakeDiscoverer{
		configured: true,
		candidates: []repolink.Candidate{
			{ADORepo: "tf-network", ADOPipelineID: intptr(11), Score: 1.0},
		},
	}
	r, mock := newRepoLinkRouter(t, testOrgID, disc)
	sourceID := uuid.NewString()

	expectSourceLookup(mock, sourceID, testOrgID, "tf-network")

	w := doReq(t, r, http.MethodPost, "/sources/"+sourceID+"/repo-link/discover", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Candidates []repolink.Candidate `json:"candidates"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Candidates, 1)
	require.Equal(t, "tf-network", resp.Candidates[0].ADORepo)
	require.NoError(t, mock.ExpectationsWereMet())
}

func intptr(i int) *int { return &i }
