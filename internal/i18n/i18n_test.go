package i18n

import "testing"

func TestT(t *testing.T) {
	if T("en", "common.save") != "Save" || T("fr-CA", "common.save") != "Save" || T("en", "nope") != "nope" {
		t.Fatal("fallback broken")
	}
}
