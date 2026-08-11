package model

import "time"

// User is our local record of a Firebase account. Firebase owns identity,
// this table owns entitlements (premium) and app-side metadata.
type User struct {
	FirebaseUID  string     `json:"firebase_uid"`
	IsPremium    bool       `json:"is_premium"`
	PremiumUntil *time.Time `json:"premium_until"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// HasActivePremium reports whether the user may use premium features right now.
// A nil PremiumUntil with IsPremium set means a non-expiring entitlement
// (lifetime purchase), so it stays active.
func (u *User) HasActivePremium() bool {
	if u == nil || !u.IsPremium {
		return false
	}

	if u.PremiumUntil == nil {
		return true
	}

	return time.Now().Before(*u.PremiumUntil)
}
