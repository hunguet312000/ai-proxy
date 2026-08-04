package recommendation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"literouter/internal/storage"
)

func TestRegisterRecommendationEndpoints(t *testing.T) {
	service := New(Config{Catalog: func(_ context.Context, _ string) ([]storage.CatalogModel, error) {
		return []storage.CatalogModel{{Provider: "codex", ID: "cx/model"}}, nil
	}})
	e := echo.New()
	service.Register(e)

	for _, path := range []string{"/api/recommendations", "/api/recommend"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path+"?limit=1", nil)
		e.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"model":"cx/model"`) {
			t.Fatalf("%s = %d %s", path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestRecommendationHandlerRejectsInvalidQuery(t *testing.T) {
	service := New(Config{Catalog: func(_ context.Context, _ string) ([]storage.CatalogModel, error) {
		return nil, nil
	}})
	e := echo.New()
	service.Register(e)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/recommendations?limit=bad", nil)
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}
