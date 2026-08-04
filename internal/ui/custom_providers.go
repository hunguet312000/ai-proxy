package ui

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"literouter/internal/storage"
)

// CustomProviderHooks exposes the storage operations the UI needs. Reload is called
// after every mutation so a change takes effect on the next request instead of
// waiting for a restart.
type CustomProviderHooks struct {
	List   func(context.Context) ([]storage.CustomProvider, error)
	Create func(context.Context, storage.CustomProvider) (storage.CustomProvider, error)
	Update func(context.Context, storage.CustomProvider) (storage.CustomProvider, error)
	Delete func(context.Context, string) error

	AddKey    func(ctx context.Context, providerID, label, apiKey string) (storage.CustomProviderKey, error)
	ToggleKey func(ctx context.Context, keyID string, enabled bool) error
	DeleteKey func(ctx context.Context, keyID string) error

	Reload func()
}

func (s *Service) registerCustomProviderRoutes(e *echo.Echo) {
	e.GET("/ui/providers/custom", s.listCustomProvidersHandler)
	e.POST("/ui/providers/custom", s.createCustomProviderHandler)
	e.POST("/ui/providers/custom/:id", s.updateCustomProviderHandler)
	e.DELETE("/ui/providers/custom/:id", s.deleteCustomProviderHandler)
	e.POST("/ui/providers/custom/:id/keys", s.addCustomProviderKeyHandler)
	e.POST("/ui/providers/custom/keys/:keyID/toggle", s.toggleCustomProviderKeyHandler)
	e.DELETE("/ui/providers/custom/keys/:keyID", s.deleteCustomProviderKeyHandler)
}

// loadCustomProviders is the read used by the page. A failure renders an empty list
// rather than breaking the whole Providers tab, since the built-in providers on the
// same page are unaffected by it.
func (s *Service) loadCustomProviders(ctx context.Context) []storage.CustomProvider {
	if s.customProviders.List == nil {
		return nil
	}
	providers, err := s.customProviders.List(ctx)
	if err != nil {
		return nil
	}
	return providers
}

// renderCustomProviders returns the fragment htmx swaps in after a mutation, which
// is how every other list on the dashboard updates itself.
func (s *Service) renderCustomProviders(c echo.Context, status int, message string) error {
	providers := s.loadCustomProviders(c.Request().Context())
	data := viewData{
		CustomProviders:      providers,
		CustomProviderModels: s.customProviderModels(c.Request().Context(), providers),
		CustomError:          message,
	}
	// Both pages swap into #custom-providers, so the handler renders whichever
	// fragment the caller is showing. view=detail comes from the provider page.
	template := "custom-providers"
	if c.QueryParam("view") == "detail" {
		template = "custom-provider-keys"
		if selected := findCustomProvider(providers, customProviderScope(c, providers)); selected != nil {
			data.CustomProvider = selected
			data.CustomProviderModels = s.customProviderModels(c.Request().Context(),
				[]storage.CustomProvider{*selected})
		}
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	c.Response().WriteHeader(status)
	return s.tab.ExecuteTemplate(c.Response(), template, data)
}

// customProviderScope works out which provider the detail page is showing. The
// provider id is on the path for provider-scoped routes; for key routes only the key
// id is available, so the owning provider is found through it.
func customProviderScope(c echo.Context, providers []storage.CustomProvider) string {
	if id := strings.TrimSpace(c.Param("id")); id != "" {
		return id
	}
	keyID := strings.TrimSpace(c.Param("keyID"))
	if keyID == "" {
		return ""
	}
	for _, provider := range providers {
		for _, key := range provider.Keys {
			if key.ID == keyID {
				return provider.ID
			}
		}
	}
	// A just-deleted key no longer resolves, so fall back to the referring page.
	return customProviderPrefixFromReferer(c)
}

// customProviderPrefixFromReferer recovers the open provider from the page URL, which
// is the only hint left once the record the request named has been deleted.
func customProviderPrefixFromReferer(c echo.Context) string {
	referer := c.Request().Header.Get("Referer")
	if referer == "" {
		return ""
	}
	if index := strings.LastIndex(referer, "/ui/providers/"); index >= 0 {
		return strings.Trim(referer[index+len("/ui/providers/"):], "/")
	}
	return ""
}

func findCustomProvider(providers []storage.CustomProvider, key string) *storage.CustomProvider {
	if key == "" {
		return nil
	}
	for index := range providers {
		if providers[index].ID == key || providers[index].Prefix == key {
			return &providers[index]
		}
	}
	return nil
}

// wantsHTML reports whether the caller is the dashboard. Keeping the JSON shape for
// everyone else means the same endpoints stay usable from a shell.
func wantsHTML(c echo.Context) bool {
	if c.Request().Header.Get("HX-Request") != "" {
		return true
	}
	return strings.Contains(c.Request().Header.Get(echo.HeaderAccept), echo.MIMETextHTML)
}

// customProviderModels groups the catalog entries registered under each custom
// prefix. Models are stored in the same catalog as the built-in ones, so the picker
// and the context-window resolver pick them up with no extra plumbing.
func (s *Service) customProviderModels(ctx context.Context, providers []storage.CustomProvider) map[string][]storage.CatalogModel {
	if s.modelsHook.List == nil || len(providers) == 0 {
		return nil
	}
	grouped := make(map[string][]storage.CatalogModel, len(providers))
	for _, definition := range providers {
		models, err := s.modelsHook.List(ctx, definition.Prefix)
		if err != nil {
			continue
		}
		grouped[definition.Prefix] = models
	}
	return grouped
}

func (s *Service) customProvidersReady() error {
	if s.customProviders.List == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "custom providers are unavailable")
	}
	return nil
}

func (s *Service) listCustomProvidersHandler(c echo.Context) error {
	if err := s.customProvidersReady(); err != nil {
		return err
	}
	if wantsHTML(c) {
		return s.renderCustomProviders(c, http.StatusOK, "")
	}
	providers, err := s.customProviders.List(c.Request().Context())
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"providers": providers})
}

// customProviderForm reads a provider from either a form post or a JSON body, so the
// endpoints work from the dashboard and from a shell equally.
func customProviderForm(c echo.Context) (storage.CustomProvider, error) {
	contentType := c.Request().Header.Get(echo.HeaderContentType)
	if strings.HasPrefix(contentType, echo.MIMEApplicationJSON) {
		var provider storage.CustomProvider
		if err := c.Bind(&provider); err != nil {
			return storage.CustomProvider{}, echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
		}
		return provider, nil
	}
	enabled := true
	if raw := strings.TrimSpace(c.FormValue("enabled")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return storage.CustomProvider{}, echo.NewHTTPError(http.StatusBadRequest, "enabled must be true or false")
		}
		enabled = parsed
	}
	return storage.CustomProvider{
		Name:    c.FormValue("name"),
		Prefix:  c.FormValue("prefix"),
		Kind:    c.FormValue("kind"),
		APIType: c.FormValue("api_type"),
		BaseURL: c.FormValue("base_url"),
		Enabled: enabled,
	}, nil
}

func (s *Service) createCustomProviderHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if err := s.customProvidersReady(); err != nil {
		return err
	}
	provider, err := customProviderForm(c)
	if err != nil {
		return err
	}
	created, err := s.customProviders.Create(c.Request().Context(), provider)
	if err != nil {
		return s.customProviderFailure(c, err)
	}
	// A key may be supplied with the provider so one call is enough to make it usable.
	if apiKey := strings.TrimSpace(c.FormValue("api_key")); apiKey != "" && s.customProviders.AddKey != nil {
		if _, err := s.customProviders.AddKey(c.Request().Context(), created.ID, c.FormValue("key_label"), apiKey); err != nil {
			return s.customProviderFailure(c, err)
		}
	}
	s.reloadCustomProviders()
	if wantsHTML(c) {
		return s.renderCustomProviders(c, http.StatusOK, "")
	}
	return c.JSON(http.StatusOK, created)
}

func (s *Service) updateCustomProviderHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if err := s.customProvidersReady(); err != nil {
		return err
	}
	provider, err := customProviderForm(c)
	if err != nil {
		return err
	}
	provider.ID = c.Param("id")
	updated, err := s.customProviders.Update(c.Request().Context(), provider)
	if err != nil {
		return s.customProviderFailure(c, err)
	}
	s.reloadCustomProviders()
	if wantsHTML(c) {
		return s.renderCustomProviders(c, http.StatusOK, "")
	}
	return c.JSON(http.StatusOK, updated)
}

func (s *Service) deleteCustomProviderHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if err := s.customProvidersReady(); err != nil {
		return err
	}
	if err := s.customProviders.Delete(c.Request().Context(), c.Param("id")); err != nil {
		return s.customProviderFailure(c, err)
	}
	s.reloadCustomProviders()
	if wantsHTML(c) {
		return s.renderCustomProviders(c, http.StatusOK, "")
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) addCustomProviderKeyHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.customProviders.AddKey == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "custom providers are unavailable")
	}
	apiKey := strings.TrimSpace(c.FormValue("api_key"))
	if apiKey == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "api_key is required")
	}
	key, err := s.customProviders.AddKey(c.Request().Context(), c.Param("id"), c.FormValue("label"), apiKey)
	if err != nil {
		return s.customProviderFailure(c, err)
	}
	s.reloadCustomProviders()
	if wantsHTML(c) {
		return s.renderCustomProviders(c, http.StatusOK, "")
	}
	// The response deliberately carries only metadata; the key itself is never
	// echoed back, so it cannot end up in a log or a browser history entry.
	return c.JSON(http.StatusOK, key)
}

func (s *Service) toggleCustomProviderKeyHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.customProviders.ToggleKey == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "custom providers are unavailable")
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(c.FormValue("enabled")))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "enabled must be true or false")
	}
	if err := s.customProviders.ToggleKey(c.Request().Context(), c.Param("keyID"), enabled); err != nil {
		return s.customProviderFailure(c, err)
	}
	s.reloadCustomProviders()
	if wantsHTML(c) {
		return s.renderCustomProviders(c, http.StatusOK, "")
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) deleteCustomProviderKeyHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.customProviders.DeleteKey == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "custom providers are unavailable")
	}
	if err := s.customProviders.DeleteKey(c.Request().Context(), c.Param("keyID")); err != nil {
		return s.customProviderFailure(c, err)
	}
	s.reloadCustomProviders()
	if wantsHTML(c) {
		return s.renderCustomProviders(c, http.StatusOK, "")
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) reloadCustomProviders() {
	if s.customProviders.Reload != nil {
		s.customProviders.Reload()
	}
}

// customProviderFailure reports a failure in whichever form the caller can use. For
// the dashboard that means re-rendering the list with the message attached, because
// htmx swaps the response into the page and a bare error body would wipe the list.
func (s *Service) customProviderFailure(c echo.Context, err error) error {
	if !wantsHTML(c) {
		return customProviderError(err)
	}
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, storage.ErrCustomPrefixTaken):
		status = http.StatusConflict
	case errors.Is(err, sql.ErrNoRows):
		status = http.StatusNotFound
	}
	return s.renderCustomProviders(c, status, err.Error())
}

func customProviderError(err error) error {
	switch {
	case errors.Is(err, storage.ErrCustomPrefixTaken):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, sql.ErrNoRows):
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	default:
		// Validation failures land here; the message already says what to fix.
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
}
