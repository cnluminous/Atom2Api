package main

import "testing"

func TestLiteCatalogModelRoutesWithoutTierRejection(t *testing.T) {
	_, store := newTestStore(t)
	addRoutingAccount(t, store, "lite-catalog", "CodingPlan Lite-体验版", "glm5.3-flash", "GLM-5.2", "mimo-v2.5")
	router := NewModelRouter(store)
	for _, model := range []string{"glm5.3-flash", "GLM-5.2", "mimo-v2.5"} {
		route, err := router.Resolve(model, APIKey{})
		if err != nil || route.Account.ID != "lite-catalog" {
			t.Fatalf("%s: route=%s error=%v", model, route.Account.ID, err)
		}
	}
}

func TestCatalogPermissionOverridesLegacyTier(t *testing.T) {
	for _, name := range []string{"glm5.3-flash", "GLM-5.2", "mimo-v2.5", "qwen3.8-27b"} {
		for _, available := range []bool{true, false} {
			account := Account{Plan: CodingPlanStatus{Plan: &PlanInfo{PlanName: "CodingPlan Lite-体验版"}}, Models: []CodingPlanModel{{DisplayModelName: name, PlanAvailable: available}}}
			if got := accountEligibleForModel(account, name); got != available {
				t.Fatalf("%s permission=%v: got %v", name, available, got)
			}
		}
	}
	account := Account{Plan: CodingPlanStatus{Plan: &PlanInfo{PlanName: "CodingPlan Lite-体验版"}}}
	if accountEligibleForModel(account, "unknown-model") {
		t.Fatal("unknown model must retain legacy restriction")
	}
}

func TestGLMPriorityPreservesEligibleLiteFallback(t *testing.T) {
	candidates := []modelCandidate{{account: Account{Plan: CodingPlanStatus{Plan: &PlanInfo{PlanName: "CodingPlan Lite-体验版"}}}}}
	if len(prioritizeModelCandidates(candidates, "GLM-5.2")) != 1 {
		t.Fatal("priority must not discard an already eligible account")
	}
}
