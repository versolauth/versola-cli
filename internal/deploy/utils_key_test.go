package deploy

import (
	"strings"
	"testing"
)

const (
	firstPrivate  = `{"kty":"EC","crv":"P-256","x":"AAA","y":"BBB","d":"SECRET1","kid":"utils","alg":"ES256"}`
	firstPublic   = `{"kty":"EC","crv":"P-256","x":"AAA","y":"BBB","kid":"utils","alg":"ES256"}`
	secondPrivate = `{"kty":"EC","crv":"P-256","x":"CCC","y":"DDD","d":"SECRET2","kid":"utils","alg":"ES256"}`
)

func TestPublicJWKDropsThePrivateHalf(t *testing.T) {
	got, err := publicJWK(firstPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "SECRET1") || strings.Contains(got, `"d"`) {
		t.Errorf("private half leaked into %s", got)
	}
	if !sameJWKPublicKey(got, firstPublic) {
		t.Errorf("public coordinates changed: %s", got)
	}
	if _, err := publicJWK(firstPublic); err == nil {
		t.Error("a JWK without \"d\" must be refused as a private key")
	}
	if _, err := publicJWK("not json"); err == nil {
		t.Error("a non-JWK must be refused")
	}
}

func TestReconcileUtilsKey(t *testing.T) {
	t.Run("first run stores this run's pair", func(t *testing.T) {
		plan, err := reconcileUtilsKey(firstPrivate, "", "")
		if err != nil || !plan.store || plan.private != firstPrivate || plan.warning != "" || !sameJWKPublicKey(plan.centralPublic, firstPublic) {
			t.Errorf("got %+v, %v", plan, err)
		}
	})
	t.Run("a stored key wins over a regenerated one, and central is offered its public half", func(t *testing.T) {
		plan, err := reconcileUtilsKey(secondPrivate, firstPrivate, "")
		if err != nil || plan.store || plan.private != firstPrivate || plan.warning != "" || !sameJWKPublicKey(plan.centralPublic, firstPublic) {
			t.Errorf("got %+v, %v", plan, err)
		}
	})
	t.Run("a consistent stored pair needs nothing", func(t *testing.T) {
		plan, err := reconcileUtilsKey(secondPrivate, firstPrivate, firstPublic)
		if err != nil || plan.store || plan.warning != "" {
			t.Errorf("got %+v, %v", plan, err)
		}
	})
	t.Run("a public key stored without a private one is warned about, not papered over", func(t *testing.T) {
		plan, err := reconcileUtilsKey(secondPrivate, "", firstPublic)
		if err != nil || !plan.store || plan.warning == "" {
			t.Errorf("got %+v, %v", plan, err)
		}
	})
	t.Run("a stored private key that disagrees with central's public key is warned about", func(t *testing.T) {
		plan, err := reconcileUtilsKey(firstPrivate, secondPrivate, firstPublic)
		if err != nil || plan.store || plan.private != secondPrivate || plan.warning == "" {
			t.Errorf("got %+v, %v", plan, err)
		}
	})
	t.Run("versola-tools without the pair and nothing stored does nothing", func(t *testing.T) {
		plan, err := reconcileUtilsKey("", "", "")
		if err != nil || plan.private != "" || plan.store {
			t.Errorf("got %+v, %v", plan, err)
		}
	})
	t.Run("an unusable stored key fails the run", func(t *testing.T) {
		if _, err := reconcileUtilsKey(firstPrivate, "garbage", ""); err == nil {
			t.Error("expected an error")
		}
	})
}
