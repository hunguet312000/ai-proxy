package gateway

import (
	"context"
	"errors"
	"testing"

	"literouter/internal/translator"
)

func imageTurn(model string) translator.AnthropicRequest {
	return translator.AnthropicRequest{
		Model: model,
		Messages: []translator.AnthropicMessage{{Role: "user", Content: []translator.AnthropicContent{
			{Type: "text", Text: "what is wrong here?"},
			{Type: "image", Source: &translator.AnthropicSource{
				Type: "base64", MediaType: "image/png", Data: "iVBORw0KGgo=",
			}},
		}}},
	}
}

func textTurn(model string) translator.AnthropicRequest {
	return translator.AnthropicRequest{
		Model:    model,
		Messages: []translator.AnthropicMessage{{Role: "user", Content: []translator.AnthropicContent{{Type: "text", Text: "hi"}}}},
	}
}

// The rule is off until a model is declared text-only, so an existing setup where every
// model reads images keeps behaving exactly as it did.
func TestImageRouteIsInertUntilAModelIsDeclaredTextOnly(t *testing.T) {
	service := New(Options{})
	service.SetImageRoute("vision/model", nil)
	decision, err := service.routeModel(context.Background(), imageTurn("cheap/small"), 1_000)
	if decision.overrides() || err != nil {
		t.Fatalf("got (%+v, %v), want no override and no error", decision, err)
	}
}

func TestImageRouteMovesImageTurnsOffATextOnlyModel(t *testing.T) {
	service := New(Options{})
	service.SetImageRoute("vision/model", []string{"text/only"})

	decision, err := service.routeModel(context.Background(), imageTurn("text/only"), 1_000)
	if err != nil || !decision.overrides() || decision.Model != "vision/model" {
		t.Fatalf("got (%q, %q, %v, %v), want vision/model", decision.Model, decision.Reason, decision.overrides(), err)
	}
	if decision.Reason != "image on a text-only model" {
		t.Fatalf("reason = %q", decision.Reason)
	}
	// No image, no reroute: the model is text-only and the turn is text.
	if decision, err := service.routeModel(context.Background(), textTurn("text/only"), 1_000); decision.overrides() || err != nil {
		t.Fatalf("a text turn was rerouted (%+v err=%v)", decision, err)
	}
	// A model that was never declared text-only keeps its image turns, however cheap it is.
	if decision, err := service.routeModel(context.Background(), imageTurn("other/model"), 1_000); decision.overrides() || err != nil {
		t.Fatalf("an undeclared model was rerouted (%+v err=%v)", decision, err)
	}
}

// Refusing beats forwarding: the upstream would reject this anyway, after the whole
// conversation had been uploaded, with wording that never mentions the attachment.
func TestImageTurnWithNowhereToGoIsRefusedBeforeTheUpload(t *testing.T) {
	service := New(Options{})
	service.SetImageRoute("", []string{"text/only"})
	_, err := service.routeModel(context.Background(), imageTurn("text/only"), 1_000)
	if !errors.Is(err, errImageModelUnavailable) {
		t.Fatalf("err = %v, want errImageModelUnavailable", err)
	}
}

// Capability is applied to whatever the size and plan rules settled on, not to the model
// the client asked for — otherwise plan mode could hand a screenshot to a text-only model
// and the correction would never see it.
func TestImageCapabilityAppliesToThePlanModelToo(t *testing.T) {
	service := New(Options{})
	service.SetPlanModel("text/only-planner")
	service.SetImageRoute("vision/model", []string{"text/only-planner"})
	// The plan marker goes first and the image last: only an image in the newest turn is the
	// one being asked about, so that is where it has to be for this rule to apply.
	request := translator.AnthropicRequest{
		Model: "cheap/small",
		Messages: []translator.AnthropicMessage{
			{Role: "user", Content: []translator.AnthropicContent{{Type: "text", Text: planEnteredMarker}}},
			{Role: "assistant", Content: []translator.AnthropicContent{{Type: "text", Text: "planning"}}},
		},
	}
	request.Messages = append(request.Messages, imageTurn("cheap/small").Messages...)
	decision, err := service.routeModel(context.Background(), request, 1_000)
	if err != nil || !decision.overrides() || decision.Model != "vision/model" {
		t.Fatalf("got (%q, %q, %v, %v), want the plan model corrected to vision/model", decision.Model, decision.Reason, decision.overrides(), err)
	}
}

// Declared by prefix so one entry covers a whole provider, matching how windows and output
// caps are keyed everywhere else in the package.
func TestTextOnlyMatchesByPrefix(t *testing.T) {
	service := New(Options{})
	service.SetImageRoute("vision/model", []string{"fpt-ai"})
	decision, err := service.routeModel(context.Background(), imageTurn("fpt-ai/llama-3-70b"), 1_000)
	if err != nil || !decision.overrides() || decision.Model != "vision/model" {
		t.Fatalf("got (%q, %v, %v), want vision/model for a prefix match", decision.Model, decision.overrides(), err)
	}
}

// Declaring the vision model itself text-only is contradictory; following it would bounce
// the turn straight back to the same check.
func TestAContradictoryDeclarationDoesNotLoop(t *testing.T) {
	service := New(Options{})
	service.SetImageRoute("vision/model", []string{"vision/model"})
	if decision, err := service.routeModel(context.Background(), imageTurn("vision/model"), 1_000); decision.overrides() || err != nil {
		t.Fatalf("got (%+v, err=%v), want the vision role to win", decision, err)
	}
}

func TestRequestHasImageFindsSystemAttachments(t *testing.T) {
	request := textTurn("m")
	request.System = []translator.AnthropicContent{
		{Type: "text", Text: "you are helpful"},
		{Type: "image", Source: &translator.AnthropicSource{Type: "base64", MediaType: "image/png", Data: "x"}},
	}
	if !requestHasImage(request) {
		t.Fatal("an image in the system block was missed")
	}
	if requestHasImage(textTurn("m")) {
		t.Fatal("a text-only request was reported as carrying an image")
	}
}

// The nested case is the common one: an image reaches a transcript when Read opens an image
// file or an MCP tool returns a screenshot, and both arrive inside a tool result rather than
// as a top-level block. Checking only top-level blocks missed every one of them.
func TestImageInsideAToolResultIsDetected(t *testing.T) {
	request := textTurn("text/only")
	request.Messages = append(request.Messages, translator.AnthropicMessage{
		Role: "user", Content: []translator.AnthropicContent{{
			Type: "tool_result", ToolUseID: "t1",
			Content: []any{
				map[string]any{"type": "text", "text": "screenshot:"},
				map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": "image/png", "data": "x",
				}},
			},
		}},
	})
	if !requestHasImage(request) {
		t.Fatal("an image nested in a tool result was missed")
	}

	service := New(Options{})
	service.SetImageRoute("vision/model", []string{"text/only"})
	decision, err := service.routeModel(context.Background(), request, 1_000)
	if err != nil || !decision.overrides() || decision.Model != "vision/model" {
		t.Fatalf("got (%q, %q, %v, %v), want vision/model", decision.Model, decision.Reason, decision.overrides(), err)
	}
}

// A tool result whose text merely mentions the word does not count; only a real image block
// does, or every transcript discussing screenshots would reroute.
func TestToolResultTextMentioningImagesIsNotAnImage(t *testing.T) {
	request := textTurn("text/only")
	request.Messages = append(request.Messages, translator.AnthropicMessage{
		Role: "user", Content: []translator.AnthropicContent{{
			Type: "tool_result", ToolUseID: "t1",
			Content: []any{map[string]any{"type": "text", "text": `{"type":"image"} appears in this file`}},
		}},
	})
	if requestHasImage(request) {
		t.Fatal("text mentioning an image block was treated as one")
	}
}

// A subagent that read one image file among fifty must not die of it. With no vision model
// configured, tool images are replaced with an explicit placeholder and the turn continues —
// degraded and honest beats dead.
func TestToolImagesAreDroppedRatherThanKillingTheTurn(t *testing.T) {
	service := New(Options{})
	service.SetImageRoute("", []string{"text/only"})
	request := textTurn("text/only")
	request.Messages = append(request.Messages, translator.AnthropicMessage{
		Role: "user", Content: []translator.AnthropicContent{{
			Type: "tool_result", ToolUseID: "t1",
			Content: []any{
				map[string]any{"type": "text", "text": "Image file contents:"},
				map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": "image/png", "data": "PNGDATA",
				}},
			},
		}},
	})
	decision, err := service.routeModel(context.Background(), request, 1_000)
	if err != nil {
		t.Fatalf("err = %v, want the turn to survive", err)
	}
	if !decision.StripImages || decision.Model != "" {
		t.Fatalf("got %+v, want StripImages with no model change", decision)
	}

	stripped, count := stripUnreadableImages(request)
	if count != 1 {
		t.Fatalf("stripped %d images, want 1", count)
	}
	nested := stripped.Messages[1].Content[0].Content.([]any)
	entry := nested[1].(map[string]any)
	if entry["type"] != "text" || entry["text"] != imagePlaceholder {
		t.Fatalf("image was not replaced by the placeholder: %+v", entry)
	}
	// The caller's own request must be untouched, because the passthrough path forwards its
	// original bytes.
	original := request.Messages[1].Content[0].Content.([]any)
	if original[1].(map[string]any)["type"] != "image" {
		t.Fatal("stripping mutated the caller's request")
	}
}

// An attachment is the request itself, so it is refused rather than quietly dropped — the
// user is right there and can act on the message.
func TestAttachedImagesAreStillRefusedNotDropped(t *testing.T) {
	service := New(Options{})
	service.SetImageRoute("", []string{"text/only"})
	if _, err := service.routeModel(context.Background(), imageTurn("text/only"), 1_000); !errors.Is(err, errImageModelUnavailable) {
		t.Fatalf("err = %v, want a refusal for an attached image", err)
	}
}

// The price/performance rule. An image stays in the transcript forever because the client
// resends the history every turn, so routing on "contains an image" put every later turn of
// the session on the vision model and paid for the image's base64 again each time. Only the
// newest turn counts; older images are replaced so the session returns to the model it was
// configured to run on.
func TestOnlyTheNewestTurnFollowsAnImageToTheVisionModel(t *testing.T) {
	service := New(Options{})
	service.SetImageRoute("vision/model", []string{"text/only"})

	request := imageTurn("text/only")
	decision, err := service.routeModel(context.Background(), request, 1_000)
	if err != nil || decision.Model != "vision/model" {
		t.Fatalf("the turn carrying the image: got %+v (%v), want vision/model", decision, err)
	}

	// Two turns later the image is history and the question is about something else.
	request.Messages = append(request.Messages,
		translator.AnthropicMessage{Role: "assistant", Content: []translator.AnthropicContent{{Type: "text", Text: "It shows a red square."}}},
		translator.AnthropicMessage{Role: "user", Content: []translator.AnthropicContent{{Type: "text", Text: "now rename the function"}}},
	)
	decision, err = service.routeModel(context.Background(), request, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Model != "" {
		t.Fatalf("later turn routed to %q; it belongs on the model the client asked for", decision.Model)
	}
	if !decision.StripImages {
		t.Fatal("the stale image must be replaced, or its base64 is paid for on every later turn")
	}
	// What the vision model said about it survives as text, which is the part a text model can
	// actually use.
	stripped, count := stripUnreadableImages(request)
	if count != 1 {
		t.Fatalf("stripped %d, want the one stale image", count)
	}
	if stripped.Messages[0].Content[1].Type != "text" || stripped.Messages[0].Content[1].Text != imagePlaceholder {
		t.Fatalf("stale image not replaced: %+v", stripped.Messages[0].Content[1])
	}
	if stripped.Messages[1].Content[0].Text != "It shows a red square." {
		t.Fatal("the description of the image was lost")
	}
}

// A stale image never causes a refusal: the user is not asking about it now, so there is
// nothing to act on and killing the turn would be gratuitous.
func TestStaleImagesNeverRefuseEvenWithNoVisionModel(t *testing.T) {
	service := New(Options{})
	service.SetImageRoute("", []string{"text/only"})
	request := imageTurn("text/only")
	request.Messages = append(request.Messages,
		translator.AnthropicMessage{Role: "assistant", Content: []translator.AnthropicContent{{Type: "text", Text: "ok"}}},
		translator.AnthropicMessage{Role: "user", Content: []translator.AnthropicContent{{Type: "text", Text: "carry on"}}},
	)
	decision, err := service.routeModel(context.Background(), request, 1_000)
	if err != nil {
		t.Fatalf("err = %v, want the turn to survive", err)
	}
	if !decision.StripImages || decision.Model != "" {
		t.Fatalf("got %+v, want a strip with no model change", decision)
	}
}
