package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/tadjov/tusker/internal/api/dto"
	apiserver "github.com/tadjov/tusker/internal/api/server"
	"github.com/tadjov/tusker/internal/apperr"
	"github.com/tadjov/tusker/pkg/pagination"
)

func TestSearchHandlers_RejectOutOfBoundsRequests(t *testing.T) {
	longQuery := strings.Repeat("a", dto.MaxQueryLength+1)

	tests := []struct {
		name string
		req  *http.Request
	}{
		{
			name: "query string too long",
			req:  httptest.NewRequest(http.MethodGet, "/v1/articles/search?q="+url.QueryEscape(longQuery), nil),
		},
		{
			name: "query string page too large",
			req:  httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/articles/search?q=climate&size=%d", pagination.PageMaxSize+1), nil),
		},
		{
			name: "semantic query too long",
			req:  httptest.NewRequest(http.MethodGet, "/v1/articles/semantic_search?q="+url.QueryEscape(longQuery), nil),
		},
		{
			name: "structured query too long",
			req: jsonRequest(http.MethodPost, "/v1/articles/_search",
				fmt.Sprintf(`{"query":{"match":{"field":"title","query":%q}}}`, longQuery)),
		},
		{
			name: "structured page too large",
			req: jsonRequest(http.MethodPost, "/v1/articles/_search",
				fmt.Sprintf(`{"size":%d,"query":{"match":{"field":"title","query":"climate"}}}`, pagination.PageMaxSize+1)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			(&apiserver.Server{Echo: e}).SetupValidator()
			e.HTTPErrorHandler = apperr.GlobalErrorHandler()
			NewSearchRouter(e, stubFtsSearcher{caps: elasticsearchCaps}, WithSemanticSearcher(stubSemanticSearcher{})).Bind()

			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, tt.req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}

func jsonRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	return req
}
