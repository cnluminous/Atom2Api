package main

import (
	"crypto/rand"
	"errors"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"
)

type ModelView struct {
	ID                  string   `json:"id"`
	Alias               string   `json:"alias"`
	Upstream            string   `json:"upstream"`
	ProviderType        string   `json:"provider_type"`
	BaseURL             string   `json:"base_url"`
	ContextWindow       int      `json:"context_window"`
	Enabled             bool     `json:"enabled"`
	AccountCount        int      `json:"account_count"`
	Accounts            []string `json:"accounts"`
	Plans               []string `json:"plans"`
	Manual              bool     `json:"manual"`
	ResponsesChatCompat bool     `json:"responses_chat_compat"`
}

type ModelRoute struct {
	Requested           string
	Upstream            string
	Alias               string
	ResponsesChatCompat bool
	Model               CodingPlanModel
	Account             Account
	Token               string
}

type ModelRouter struct {
	store    *Store
	mu       sync.Mutex
	next     map[string]int
	bindings map[routeBindingKey]string
}

type routeBindingKey struct {
	APIKeyID string
	Model    string
}

type modelCandidate struct {
	account Account
	model   CodingPlanModel
	token   string
}

func NewModelRouter(store *Store) *ModelRouter {
	return &ModelRouter{store: store, next: map[string]int{}, bindings: map[routeBindingKey]string{}}
}

func (r *ModelRouter) Catalog() []ModelView {
	type aggregate struct {
		view     ModelView
		accounts map[string]struct{}
		plans    map[string]struct{}
	}
	aggregates := map[string]*aggregate{}
	accounts := r.store.Accounts()
	for _, account := range accounts {
		for _, model := range account.Models {
			if !model.PlanAvailable || model.DisplayModelName == "" {
				continue
			}
			entry := aggregates[model.DisplayModelName]
			if entry == nil {
				entry = &aggregate{
					view: ModelView{
						ID: model.DisplayModelName, Alias: model.DisplayModelName, Upstream: model.DisplayModelName,
						ProviderType: model.ProviderType, BaseURL: model.BaseURL, ContextWindow: model.ContextWindow, Enabled: true,
					},
					accounts: map[string]struct{}{}, plans: map[string]struct{}{},
				}
				aggregates[model.DisplayModelName] = entry
			}
			entry.accounts[account.ID] = struct{}{}
			if account.Plan.Plan != nil && account.Plan.Plan.PlanName != "" {
				entry.plans[account.Plan.Plan.PlanName] = struct{}{}
			}
		}
	}
	settings := r.store.ModelSettings()
	for upstream, setting := range settings {
		if _, exists := aggregates[upstream]; exists || !setting.Manual {
			continue
		}
		baseURL := defaultGatewayURL
		if r.store.config != nil {
			baseURL = r.store.config.Snapshot().GatewayURL
		}
		entry := &aggregate{
			view: ModelView{
				ID: upstream, Alias: upstream, Upstream: upstream, ProviderType: "openai",
				BaseURL: baseURL, ContextWindow: 64000, Enabled: true, Manual: true,
			},
			accounts: map[string]struct{}{}, plans: map[string]struct{}{},
		}
		for _, account := range accounts {
			entry.accounts[account.ID] = struct{}{}
			if account.Plan.Plan != nil && account.Plan.Plan.PlanName != "" {
				entry.plans[account.Plan.Plan.PlanName] = struct{}{}
			}
		}
		aggregates[upstream] = entry
	}
	result := make([]ModelView, 0, len(aggregates))
	for upstream, entry := range aggregates {
		entry.view.ResponsesChatCompat = defaultResponsesChatCompat(upstream)
		if setting, ok := settings[upstream]; ok {
			entry.view.Alias = setting.Alias
			entry.view.Enabled = setting.Enabled
			entry.view.ResponsesChatCompat = setting.ResponsesChatCompat
		}
		entry.view.Accounts = []string{}
		entry.view.Plans = []string{}
		for id := range entry.accounts {
			entry.view.Accounts = append(entry.view.Accounts, id)
		}
		for plan := range entry.plans {
			entry.view.Plans = append(entry.view.Plans, plan)
		}
		sort.Strings(entry.view.Accounts)
		sort.Strings(entry.view.Plans)
		entry.view.AccountCount = len(entry.view.Accounts)
		result = append(result, entry.view)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Alias < result[j].Alias })
	return result
}

func defaultResponsesChatCompat(upstream string) bool {
	return strings.EqualFold(strings.TrimSpace(upstream), "GLM-5.2")
}

func (r *ModelRouter) Resolve(requested string, key APIKey) (ModelRoute, error) {
	return r.ResolveWithStrategy(requested, key, normalizeRouteStrategy(key.RouteStrategy))
}

func (r *ModelRouter) ResolveWithStrategy(requested string, key APIKey, strategy string) (ModelRoute, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return ModelRoute{}, errors.New("model is required")
	}
	var selected *ModelView
	for _, model := range r.Catalog() {
		if model.Enabled && (model.Alias == requested || model.Upstream == requested) {
			copy := model
			selected = &copy
			break
		}
	}
	if selected == nil {
		return ModelRoute{}, errors.New("model is not available")
	}
	if len(key.AllowedModels) > 0 && !containsString(key.AllowedModels, requested) && !containsString(key.AllowedModels, selected.Upstream) && !containsString(key.AllowedModels, selected.Alias) {
		return ModelRoute{}, errors.New("API key is not allowed to use this model")
	}
	var candidates []modelCandidate
	for _, accountView := range r.store.Accounts() {
		if !accountView.Enabled || accountView.Status != "active" || quotaExhausted(accountView.Plan, accountView.LastSyncAt) {
			continue
		}
		account, token, _, err := r.store.Account(accountView.ID)
		if err != nil {
			continue
		}
		if !accountEligibleForModel(account, selected.Upstream) {
			continue
		}
		if selected.Manual {
			candidates = append(candidates, modelCandidate{
				account: account,
				model: CodingPlanModel{
					DisplayModelName: selected.Upstream, BaseURL: selected.BaseURL,
					ProviderType: selected.ProviderType, ContextWindow: selected.ContextWindow, PlanAvailable: true,
				},
				token: token,
			})
			continue
		}
		for _, model := range account.Models {
			if model.PlanAvailable && model.DisplayModelName == selected.Upstream {
				candidates = append(candidates, modelCandidate{account: account, model: model, token: token})
				break
			}
		}
	}
	candidates = prioritizeModelCandidates(candidates, selected.Upstream)
	if len(candidates) == 0 {
		return ModelRoute{}, errors.New("no active account has quota for this model")
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].account.ConsecutiveErr < candidates[j].account.ConsecutiveErr
	})
	strategy = normalizeRouteStrategy(strategy)
	index := 0
	persistBinding := false
	if strategy == RouteStrategyFill {
		bindingKey := routeBindingKey{APIKeyID: key.ID, Model: selected.Upstream}
		assignments := r.store.APIKeyRouteAssignments(selected.Upstream)
		r.mu.Lock()
		boundAccountID := r.bindings[bindingKey]
		if boundAccountID == "" && key.RouteBindings != nil {
			boundAccountID = key.RouteBindings[selected.Upstream].AccountID
		}
		for candidateIndex, candidate := range candidates {
			if candidate.account.ID == boundAccountID {
				index = candidateIndex
				break
			}
		}
		if boundAccountID == "" || candidates[index].account.ID != boundAccountID {
			for binding, accountID := range r.bindings {
				if binding.Model == selected.Upstream {
					assignments[binding.APIKeyID] = accountID
				}
			}
			delete(assignments, key.ID)
			index = leastUsedCandidateIndex(candidates, assignments, r.next[selected.Upstream])
			r.next[selected.Upstream]++
			persistBinding = true
		}
		r.bindings[bindingKey] = candidates[index].account.ID
		r.mu.Unlock()
		if persistBinding && key.ID != "" {
			_ = r.store.SetAPIKeyRouteBinding(key.ID, selected.Upstream, candidates[index].account.ID)
		}
	} else {
		index = randomCandidateIndex(len(candidates))
	}
	choice := candidates[index]
	return ModelRoute{
		Requested: requested, Upstream: selected.Upstream, Alias: selected.Alias,
		ResponsesChatCompat: selected.ResponsesChatCompat,
		Model:               choice.model, Account: choice.account, Token: choice.token,
	}, nil
}

func leastUsedCandidateIndex(candidates []modelCandidate, assignments map[string]string, offset int) int {
	counts := make(map[string]int, len(candidates))
	for _, accountID := range assignments {
		counts[accountID]++
	}
	minimum := -1
	indices := make([]int, 0, len(candidates))
	for index, candidate := range candidates {
		count := counts[candidate.account.ID]
		if minimum == -1 || count < minimum {
			minimum = count
			indices = indices[:0]
			indices = append(indices, index)
			continue
		}
		if count == minimum {
			indices = append(indices, index)
		}
	}
	return indices[offset%len(indices)]
}

func prioritizeModelCandidates(candidates []modelCandidate, upstream string) []modelCandidate {
	family := modelFamily(upstream)
	if family == "" {
		return candidates
	}
	preferred := make([]modelCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		tier := accountPlanTier(candidate.account)
		if family == "deepseek" && tier == "lite" {
			preferred = append(preferred, candidate)
		}
		if family == "glm" && (tier == "pro" || tier == "max") {
			preferred = append(preferred, candidate)
		}
	}
	if family == "deepseek" && len(preferred) > 0 {
		return preferred
	}
	if family == "glm" && len(preferred) > 0 {
		return preferred
	}
	return candidates
}

func modelFamily(upstream string) string {
	name := strings.ToLower(strings.TrimSpace(upstream))
	if strings.Contains(name, "deepseek") {
		return "deepseek"
	}
	if strings.Contains(name, "glm-5.2") {
		return "glm"
	}
	return ""
}

func accountEligibleForModel(account Account, upstream string) bool {
	// The synchronized upstream catalog is authoritative for this account.
	// Keep legacy tier rules only for models absent from that catalog.
	for _, model := range account.Models {
		if model.DisplayModelName == upstream {
			return model.PlanAvailable
		}
	}
	if accountPlanTier(account) == "lite" && modelFamily(upstream) != "deepseek" {
		return false
	}
	if modelFamily(upstream) == "glm" {
		tier := accountPlanTier(account)
		return tier == "pro" || tier == "max"
	}
	return true
}

func accountPlanTier(account Account) string {
	return strings.ToLower(planTier(account.Plan.Plan))
}

func randomCandidateIndex(size int) int {
	if size <= 1 {
		return 0
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(size)))
	if err != nil {
		return int(time.Now().UnixNano() % int64(size))
	}
	return int(value.Int64())
}

func (r *ModelRouter) ForgetAPIKeyRoutes(keyID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for binding := range r.bindings {
		if binding.APIKeyID == keyID {
			delete(r.bindings, binding)
		}
	}
}

func quotaExhausted(status CodingPlanStatus, syncedAt *time.Time) bool {
	elapsed := int64(0)
	if syncedAt != nil {
		elapsed = int64(time.Since(*syncedAt).Seconds())
		if elapsed < 0 {
			elapsed = 0
		}
	}
	for _, window := range status.RateLimitWindows {
		if window.ShowEnable == 1 && window.QuotaExhausted && window.SecondsUntilReset > elapsed {
			return true
		}
	}
	if !status.WindowQuotaExhausted {
		return false
	}
	return status.CurrentUsage == nil || status.CurrentUsage.SecondsUntilReset > elapsed
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
