package recommendation

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
)

// Register adds the recommendation endpoints to an Echo instance.
func (s *Service) Register(e *echo.Echo) {
	e.GET("/api/recommendations", s.handler)
	e.GET("/api/recommend", s.handler)
}

func (s *Service) handler(c echo.Context) error {
	query, err := parseQuery(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	response, err := s.Recommend(c.Request().Context(), query)
	if err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, err.Error())
	}
	return c.JSON(http.StatusOK, response)
}

func parseQuery(c echo.Context) (Query, error) {
	query := Query{
		Provider: c.QueryParam("provider"),
		Model:    c.QueryParam("model"),
		Task:     c.QueryParam("task"),
	}
	for name, target := range map[string]*int{
		"limit":       &query.Limit,
		"min_context": &query.MinContext,
	} {
		value := strings.TrimSpace(c.QueryParam(name))
		if value == "" {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return Query{}, fmt.Errorf("%s must be an integer", name)
		}
		*target = parsed
	}
	return normalizeQuery(query)
}
