package main

import (
	"aislide/internal/auth"
	"aislide/internal/config"
	"aislide/internal/revenuecat"
	"aislide/internal/service"
	"aislide/internal/store"
	"aislide/internal/transport"
	"context"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/time/rate"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	// Database
	db, err := store.Open(cfg.DatabasePath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if err := store.Migrate(db); err != nil {
		log.Fatal(err)
	}

	// Firebase ID token verification
	verifier, err := auth.NewFirebaseVerifier(ctx, cfg.FirebaseProjectID, cfg.FirebaseCredentialsFile)
	if err != nil {
		log.Fatal(err)
	}

	// Wire up the layers
	revenueCatClient := revenuecat.New(cfg.RevenueCatSecretAPIKey, cfg.RevenueCatProjectID, cfg.RevenueCatEntitlementID)

	userService := service.NewUserService(store.NewUserStore(db))
	bookService := service.NewBookService(store.NewBookStore(db))
	revenueCatService := service.NewRevenueCatService(userService, store.NewRevenueCatEventStore(db), revenueCatClient)

	authMiddleware := transport.NewAuthMiddleware(verifier, userService)
	userHandler := transport.NewUserHandler(userService)
	bookHandler := transport.NewBookHandler(bookService)
	revenueCatHandler := transport.NewRevenueCatHandler(revenueCatService, cfg.RevenueCatWebhookAuth)

	// Configure the routes
	r := gin.Default()

	// Client IP resolution. gin trusts every proxy by default, which means any
	// caller could set X-Forwarded-For and become whoever they like in the
	// access log and in anything keyed on IP. See ConfigureClientIP.
	if err := transport.ConfigureClientIP(r, cfg.TrustedProxies, cfg.ClientIPHeader); err != nil {
		log.Fatal(err)
	}

	// Rate limits, per authenticated Firebase UID.
	//
	// The refresh bucket is the tight one: every call is a RevenueCat API
	// request we pay for, and the app only needs it after a purchase, a
	// restore, or a cold start that looks stale. The group bucket is a
	// backstop against a client looping on any /v1 route.
	refreshLimiter := transport.NewUserRateLimiter(transport.RateLimitConfig{
		Rate:  rate.Every(10 * time.Second),
		Burst: 5,
	})
	groupLimiter := transport.NewUserRateLimiter(transport.RateLimitConfig{
		Rate:  5,
		Burst: 20,
	})

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Server to server: authenticated by the RevenueCat shared secret, so it
	// stays outside the Firebase-authenticated group. Deliberately not rate
	// limited: dropping a delivery costs a customer their entitlement until a
	// retry lands, and the shared secret already bounds who can reach it.
	revenueCatHandler.RegisterRoutes(r)

	// Everything under /v1 needs a valid Firebase ID token, and the limiter
	// sits behind RequireAuth because it keys on the verified UID.
	v1 := r.Group("/v1", authMiddleware.RequireAuth(), groupLimiter.Middleware())
	userHandler.RegisterRoutes(v1)
	bookHandler.RegisterRoutes(v1)
	// POST /v1/me/premium/refresh: the app's recovery path when a webhook is
	// lost, and what it calls right after a purchase or a restore.
	revenueCatHandler.RegisterUserRoutes(v1, refreshLimiter.Middleware())

	// Start listening
	log.Printf("aislide api listening on :%s (db: %s, entitlement: %s)", cfg.Port, cfg.DatabasePath, cfg.RevenueCatEntitlementID)
	log.Fatal(r.Run(":" + cfg.Port))
}
