package model_test

import (
	"aislide/internal/model"
	"testing"
	"time"
)

func TestHasActivePremium(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	cases := []struct {
		name string
		user *model.User
		want bool
	}{
		{"nil user", nil, false},
		{"free user", &model.User{}, false},
		{"premium without expiry", &model.User{IsPremium: true}, true},
		{"premium not expired", &model.User{IsPremium: true, PremiumUntil: &future}, true},
		{"premium expired", &model.User{IsPremium: true, PremiumUntil: &past}, false},
		{"flag off but date in future", &model.User{IsPremium: false, PremiumUntil: &future}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.user.HasActivePremium(); got != tc.want {
				t.Errorf("HasActivePremium() = %v, want %v", got, tc.want)
			}
		})
	}
}
