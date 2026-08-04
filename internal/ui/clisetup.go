package ui

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/labstack/echo/v4"

	"literouter/internal/clisetup"
)

const defaultLiteRouterBaseURL = "http://127.0.0.1:8317"

func requestBaseURL(request *http.Request) string {
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(strings.Split(request.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded == "http" || forwarded == "https" {
		scheme = forwarded
	}
	return scheme + "://" + request.Host
}

func (s *Service) cliSetupHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	tool := clisetup.Tool(c.Param("tool"))
	action := clisetup.Action(c.Param("action"))
	baseURL := c.FormValue("base_url")
	if baseURL == "" {
		baseURL = requestBaseURL(c.Request())
	}
	req := clisetup.Request{
		Tool: tool, Action: action, BaseURL: baseURL, Token: s.apiToken,
		Model: c.FormValue("model"), SubagentModel: c.FormValue("subagent_model"),
		FableModel: c.FormValue("fable_model"), OpusModel: c.FormValue("opus_model"),
		SonnetModel: c.FormValue("sonnet_model"), HaikuModel: c.FormValue("haiku_model"),
		Effort: c.FormValue("effort"), MaxContext: c.FormValue("max_context"),
	}
	// "auto" is resolved here rather than inside clisetup, which has no way to reach the
	// gateway's view of the catalog. Resolved on every apply, so the number written to
	// the client tracks whatever has been learned since the last one.
	if s.settings.ContextCeiling != nil {
		models := []string{
			req.Model, req.SubagentModel, req.FableModel, req.OpusModel, req.SonnetModel, req.HaikuModel,
		}
		// The plan override belongs in the tally even though it is not on this form. The
		// client is never told the router swapped the model, so its window belief has to
		// hold for whatever actually serves the turn — plan turns included.
		if s.settings.GetPlanModel != nil {
			if planModel, planErr := s.settings.GetPlanModel(c.Request().Context()); planErr == nil {
				models = append(models, planModel)
			}
		}
		req.CatalogContextWindow = s.settings.ContextCeiling(c.Request().Context(), models)
	}

	// Optional legacy script download.
	if c.QueryParam("download") == "1" || c.FormValue("download") == "1" {
		artifact, err := clisetup.Generate(req)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		response := c.Response()
		response.Header().Set(echo.HeaderContentType, "text/x-shellscript; charset=utf-8")
		response.Header().Set("Content-Disposition", `attachment; filename="`+artifact.Filename+`"`)
		response.Header().Set(echo.HeaderCacheControl, "no-store")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		return c.Blob(http.StatusOK, "text/x-shellscript; charset=utf-8", artifact.Content)
	}

	// 9router-style: apply/reset directly on host home.
	result, err := clisetup.ApplyDirect(req)
	if err != nil {
		// Helpful Docker hint when home isn't mounted.
		msg := err.Error()
		if os.Getenv("LITEROUTER_OAUTH_CALLBACK_CONTAINER") == "true" || fileMissingHomeHint(err) {
			msg = err.Error() + " — mount host $HOME at /host-home and set HOME=/host-home (see docker-compose.yml)"
		}
		return echo.NewHTTPError(http.StatusBadRequest, msg)
	}

	// Remembered on reset as well as on apply, and that is the point: Reset strips
	// LiteRouter's keys out of the host client's config, so without a copy here the
	// selection would be gone and re-applying would mean retyping every field. Empty
	// drafts are skipped so a reset submitted from a blank form cannot wipe a good one.
	if tool == clisetup.ToolClaude && s.settings.SetCLIDraft != nil {
		if draft := req.Draft(); draft.HasSelection() {
			// Bookkeeping: the host config was already written, so a failed save must not
			// report the whole operation as failed.
			_ = s.settings.SetCLIDraft(c.Request().Context(), draft)
		}
	}

	// HTMX / fetch JSON-friendly HTML flash.
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	c.Response().Header().Set("Cache-Control", "no-store")
	note := ""
	if result.Path != "" {
		note = " · " + result.Path
	}
	if action == clisetup.Reset && tool == clisetup.ToolClaude {
		note = " · selection kept here, press Apply to restore" + note
	}
	_, err = fmt.Fprintf(c.Response(), `<div class="cli-flash ok"><strong>%s</strong><span>%s</span></div>`,
		htmlEscape(result.Message), htmlEscape(note))
	return err
}

func fileMissingHomeHint(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "home") || strings.Contains(msg, "permission denied") || strings.Contains(msg, "read-only")
}

func htmlEscape(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return replacer.Replace(s)
}
