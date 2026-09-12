package admin

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/usage"
)

func TestUsageRequiresAdminAndValidDates(t *testing.T) {
	h := testAdmin(t)
	store, err := usage.Open(filepath.Join(t.TempDir(), "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.Usage = store
	if w := adminRequest(h, "GET", "/admin/api/usage", "", nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	s := loginAs(t, h)
	for _, query := range []string{"from=bad", "from=2026-02-30", "from=2026-09-12&to=2026-09-01"} {
		if w := adminRequest(h, "GET", "/admin/api/usage?"+query, "", s); w.Code != 400 {
			t.Fatal(query, w.Code)
		}
	}
	if w := adminRequest(h, "GET", "/admin/api/usage", "", s); w.Code != 200 {
		t.Fatal(w.Code)
	}
}

func TestCreditRefreshPersistsActualMeterUsage(t *testing.T) {
	h := testAdmin(t)
	path := filepath.Join(t.TempDir(), "usage.json")
	store, err := usage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.Usage = store
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":75,"CycleCapacityUsed":25}]}}}}`))
	}))
	defer up.Close()
	h.cfg.Upstream.BillingBaseCN = up.URL
	h.cfg.Pool.Add(&auth.Auth{UID: "test-account", AccessToken: "test", ExpiresAt: 9999999999})
	w := adminRequest(h, "POST", "/admin/api/accounts/test-account/credits", "{}", loginAs(t, h))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	reloaded, err := usage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	credits := reloaded.Credits()
	if len(credits) != 1 || credits[0].Used == nil || *credits[0].Used != 25 || credits[0].Remain != 75 {
		t.Fatal(credits)
	}
}
