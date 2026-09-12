package upstream

import (
	"net/http"
	"reflect"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestFetchModelsJoinsCLIFiltersAndPreservesDynamicMetadata(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusOK, `{
			"code": 0,
			"data": {
				"models": [
					{"id":"zero","name":"Zero","credits":"x0.00 credits"},
					{"id":"quarter","name":"Quarter","credits":"x0.25 credits","description":"English description","descriptionZh":"中文说明","supportsImages":true,"supportsToolCall":true,"tags":["coding","fast"],"reasoning":{"effort":"high","supportedEfforts":["low","high"],"canDisableThinking":true}},
					{"id":"full","name":"Full","credits":"×1.00"},
					{"id":"unknown","name":"Unknown","credits":"metered"},
					{"id":"missing","name":"Missing"},
					{"id":"auto","name":"Auto","credits":"x99 credits"},
					{"id":"disabled","name":"Disabled","credits":"x1.00","disabled":true},
					{"id":"non-cli","name":"Not CLI","credits":"x1.00"}
				],
				"agents": [
					{"name":"cli","models":["quarter","zero","quarter","auto","unknown","missing","disabled","not-present"]},
					{"name":"desktop","models":["non-cli"]},
					{"name":"cli","models":["full","zero"]}
				]
			}
		}`), nil
	})
	infos, err := c.FetchModels(&auth.Auth{AccessToken: "access-token", UID: "account"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	gotIDs := make([]string, 0, len(infos))
	byID := make(map[string]ModelInfo, len(infos))
	for _, info := range infos {
		gotIDs = append(gotIDs, info.ID)
		byID[info.ID] = info
	}
	if want := []string{"quarter", "zero", "auto", "unknown", "missing", "full"}; !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("CLI model ids = %v, want joined/deduped %v", gotIDs, want)
	}

	quarter := byID["quarter"]
	if quarter.Description != "中文说明" || !quarter.SupportsImages || !quarter.SupportsToolCall || !quarter.SupportsReasoning || !quarter.CanDisableThinking {
		t.Errorf("quarter capability metadata = %+v", quarter)
	}
	if !reflect.DeepEqual(quarter.Efforts, []string{"low", "high"}) || !reflect.DeepEqual(quarter.Tags, []string{"coding", "fast"}) {
		t.Errorf("quarter reasoning/tags = efforts=%v tags=%v", quarter.Efforts, quarter.Tags)
	}
	assertRate := func(id string, want float64) {
		t.Helper()
		got := byID[id]
		if got.CreditType != "relative" || got.CreditMultiplier == nil || *got.CreditMultiplier != want {
			t.Errorf("%s rate = multiplier=%v type=%q, want %v/relative", id, got.CreditMultiplier, got.CreditType, want)
		}
	}
	assertRate("zero", 0)
	assertRate("quarter", 0.25)
	assertRate("full", 1)
	for _, id := range []string{"unknown", "missing"} {
		if got := byID[id]; got.CreditMultiplier != nil || got.CreditType != "unknown" {
			t.Errorf("%s invalid/missing rate became known: %+v", id, got)
		}
	}
	if got := byID["auto"]; got.CreditMultiplier != nil || got.CreditType != "dynamic" {
		t.Errorf("auto rate = %+v, want dynamic with no numeric multiplier", got)
	}
	if _, exists := byID["disabled"]; exists {
		t.Error("disabled model was exposed")
	}
	if _, exists := byID["non-cli"]; exists {
		t.Error("non-CLI model was exposed")
	}
}
